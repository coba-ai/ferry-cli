// Package runs is the run ledger: the place the idempotency key of every
// money request lives, and the reason a process that dies mid-request does
// not cost a second transfer.
//
// The claim the whole package exists to make is PLAN §2 C1: for every money
// request the CLI sends, a record naming the key, the operation and the exact
// body bytes has been written, fsynced and renamed into place before
// http.Client.Do is called. [Handle.Begin] is that write; everything else is
// there so the write stays true afterwards.
//
// Two things this package deliberately does not hold:
//
//   - Consent. There is no state meaning "the human agreed" (C16). A step can
//     be awaiting_confirmation, declined, pending or unreachable; none of
//     those authorises a send, and a resume must obtain consent again in its
//     own invocation.
//   - An outcome table. [Outcome] here is a record of what some other package
//     decided, so that the ledger does not depend on internal/outcome and the
//     two can be built in the same wave.
package runs

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// Schema is the record version. PLAN §5.3 fixes it at 2.
const Schema = 2

// Step names. A run has at most one of each.
const (
	StepSimulate = "simulate"
	StepExecute  = "execute"
)

// Operation ids, as the API names them.
const (
	OpSimulate = "transfers_simulate"
	OpExecute  = "transfers_execute"
)

// ScrubbedToken is what replaces a plan token once it can no longer be used.
// It is a sentinel rather than null so a reader can tell "there was a token
// here" from "there never was one".
const ScrubbedToken = "[SCRUBBED]"

// State is a step's position in the state machine of PLAN §5.3.
type State string

const (
	// StateNotStarted is a minted step that has not been through Begin.
	StateNotStarted State = "not_started"
	// StateAwaitingConfirmation is written before a consent prompt is shown
	// and means nothing more than that: it is not consent (C16).
	StateAwaitingConfirmation State = "awaiting_confirmation"
	// StateDeclined is terminal. The step was refused by a human, by EOF, or
	// by a signal while the prompt was up, and nothing was sent.
	StateDeclined State = "declined"
	// StatePending means Begin has run: the request may or may not have left.
	StatePending State = "pending"
	// StateAnswered means a response was recorded.
	StateAnswered State = "answered"
	// StateTerminal means the outcome is settled and nothing will resume it.
	StateTerminal State = "terminal"
	// StateUnreachable is terminal and means the step can never legitimately
	// start: under --broadcast, a simulate that left via 202 issues no plan
	// token, so its execute step has nothing to execute (AC75, C8).
	StateUnreachable State = "unreachable"
)

// AllStates is the census of every State this package declares. It is the
// single list every caller and every test reads.
//
// It exists because Go offers no reflection over a string-constant enum: a
// test cannot ask the package what its states are. Without a census each test
// that iterates states iterates a hand-written literal, which is a one-
// directional subset check — it proves the states it lists exist, and says
// nothing about a state it does not list. A state added later would simply
// never enter the list, and for this enum that state is StateConfirmed: the
// exact defect C16 exists to forbid would ship with every control green.
//
// So that the census cannot itself drift out of date, state_census_test.go
// derives the declared states from this package's own syntax tree and asserts
// set equality with this function in both directions. Adding a state without
// adding it here fails; listing one here that the source does not declare
// fails too.
//
// The slice is built fresh on each call so a caller cannot mutate the census.
func AllStates() []State {
	return []State{
		StateNotStarted,
		StateAwaitingConfirmation,
		StateDeclined,
		StatePending,
		StateAnswered,
		StateTerminal,
		StateUnreachable,
	}
}

// Terminal reports whether nothing further will happen to a step in this
// state.
func (s State) Terminal() bool {
	switch s {
	case StateTerminal, StateDeclined, StateUnreachable:
		return true
	}
	return false
}

// MayHaveSent reports whether a request under this step's key may already
// have reached FERRY. It is the predicate that decides whether "no" is still
// an available answer: a step that may have sent can be resumed but never
// declined (AC92, V4).
func (s State) MayHaveSent() bool {
	switch s {
	case StatePending, StateAnswered, StateTerminal:
		return true
	}
	return false
}

// Plan is what a fresh simulate 201 said about the plan it issued. The token
// itself is not here; it is in Step.PlanToken, so there is exactly one field
// to scrub.
type Plan struct {
	ID        string    `json:"id"`
	ExpiresAt time.Time `json:"expires_at"`
}

// Response is what came back, stored so a resume and `runs show` can report
// it without asking FERRY again.
//
// Body holds the response bytes as received, minus the plan token (AC82).
// Unlike Step.Body there is no digest over it, so it is stored for reading
// rather than for proving: when the bytes are JSON they are stored as JSON so
// `runs show` and jq can reach into them, and when they are not they are
// stored as text under body_text.
type Response struct {
	Status     int               `json:"status"`
	Headers    map[string]string `json:"headers"`
	Body       []byte            `json:"body"`
	ReceivedAt time.Time         `json:"received_at"`
}

// responseJSON is Response's wire form. The split of body from body_text is
// what makes Response.MarshalJSON total.
//
// It has to be total. AC83 requires an HTML 502 to be recorded as pending/6,
// and pending is the class that means the money may have moved: if encoding
// the record could fail on the response bytes, the one outcome a caller most
// needs on disk would be the one that could not be written. A json.RawMessage
// body fails to marshal for any non-JSON byte string, so the raw field is
// used only when the bytes are in fact JSON.
type responseJSON struct {
	Status     int               `json:"status"`
	Headers    map[string]string `json:"headers"`
	Body       json.RawMessage   `json:"body"`
	BodyText   *string           `json:"body_text,omitempty"`
	ReceivedAt time.Time         `json:"received_at"`
}

var jsonNull = json.RawMessage("null")

func (r Response) MarshalJSON() ([]byte, error) {
	out := responseJSON{
		Status:     r.Status,
		Headers:    r.Headers,
		Body:       jsonNull,
		ReceivedAt: r.ReceivedAt,
	}
	switch {
	case len(r.Body) == 0:
		// Nothing came back, which is itself worth recording.
	case json.Valid(r.Body) && utf8.Valid(r.Body):
		out.Body = json.RawMessage(r.Body)
	default:
		// A proxy's HTML, a truncated body, or bytes that are not UTF-8 at
		// all. ToValidUTF8 is lossy, and that is the right trade here: no
		// digest is computed over these bytes, so recording something
		// readable beats refusing to record the send.
		text := strings.ToValidUTF8(string(r.Body), "\uFFFD")
		out.BodyText = &text
	}
	return json.Marshal(out)
}

func (r *Response) UnmarshalJSON(p []byte) error {
	var in responseJSON
	if err := json.Unmarshal(p, &in); err != nil {
		return err
	}
	r.Status, r.Headers, r.ReceivedAt = in.Status, in.Headers, in.ReceivedAt
	switch {
	case in.BodyText != nil:
		r.Body = []byte(*in.BodyText)
	case len(in.Body) == 0 || string(in.Body) == "null":
		r.Body = nil
	default:
		r.Body = []byte(in.Body)
	}
	return nil
}

// Outcome is the classification some other package produced, recorded
// verbatim. The ledger reads only ExitCode, and only to decide whether a step
// is resumable (classes 5 and 6) or settled.
type Outcome struct {
	Class       string `json:"class"`
	ExitCode    int    `json:"exit_code"`
	Money       string `json:"money"`
	SameKeySafe bool   `json:"same_key_safe"`
	Next        string `json:"next"`
}

// Resumable reports the two classes PLAN §5.3 lets a resume pick up.
func (o *Outcome) Resumable() bool {
	return o != nil && (o.ExitCode == 5 || o.ExitCode == 6)
}

// Body is the verbatim bytes of a request body.
//
// It marshals as a JSON *string*, not as embedded JSON, and that is a
// correction to PLAN §5.3's example record (amendment A310). encoding/json
// compacts and HTML-escapes anything a json.Marshaler returns, so a body
// stored as json.RawMessage comes back with its whitespace removed and its
// <, > and & escaped after the first rewrite of the record. Two money
// properties break if that happens: body_sha256 no longer matches the stored
// body, so AC11 reports every --body run corrupt; and a resume that resent
// the stored bytes would send different bytes under the same key, which
// FERRY answers 409 IDEMPOTENCY_KEY_REUSED / different_request. Encoding the
// bytes as a JSON string round-trips them exactly.
type Body []byte

// MarshalJSON implements json.Marshaler.
func (b Body) MarshalJSON() ([]byte, error) {
	if b == nil {
		return []byte("null"), nil
	}
	return json.Marshal(string(b))
}

// UnmarshalJSON implements json.Unmarshaler. It accepts the JSON string form
// this package writes and nothing else, so a hand-edited record that embeds
// the body as an object is a loud parse error rather than a silent digest
// mismatch.
func (b *Body) UnmarshalJSON(p []byte) error {
	if string(p) == "null" {
		*b = nil
		return nil
	}
	var s string
	if err := json.Unmarshal(p, &s); err != nil {
		return fmt.Errorf("runs: step body must be a JSON string of the verbatim request bytes: %w", err)
	}
	*b = Body(s)
	return nil
}

// SHA256 is the hex digest FERRY computes over the same bytes.
func (b Body) SHA256() string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Step is one HTTP request the run owns.
type Step struct {
	Name           string  `json:"name"`
	Operation      string  `json:"operation"`
	Method         string  `json:"method"`
	Path           string  `json:"path"`
	IdempotencyKey string  `json:"idempotency_key"`
	BodySHA256     *string `json:"body_sha256"`
	Body           Body    `json:"body"`

	// Plan and PlanToken are written on the execute step of a --broadcast run
	// and on nothing else. PlanToken is one of the token's two resting places
	// (C6, V6); the other is Body, once Begin has written the
	// {"plan_token": …} bytes, and that one is never scrubbed because
	// BodySHA256 must keep holding for a resume to resend the same request.
	Plan      *Plan   `json:"plan"`
	PlanToken *string `json:"plan_token"`

	State    State     `json:"state"`
	Attempts int       `json:"attempts"`
	Response *Response `json:"response"`
	Outcome  *Outcome  `json:"outcome"`
}

// HasPlanToken reports whether a live (unscrubbed) token is stored.
func (s *Step) HasPlanToken() bool {
	return s.PlanToken != nil && *s.PlanToken != "" && *s.PlanToken != ScrubbedToken
}

// Unresolved reports whether this step is one C18's orphan check cares
// about: awaiting_confirmation, pending, or answered with class 5 or 6.
func (s *Step) Unresolved() bool {
	switch s.State {
	case StateAwaitingConfirmation, StatePending:
		return true
	case StateAnswered:
		return s.Outcome.Resumable()
	}
	return false
}

// Run is the whole record.
type Run struct {
	SchemaVersion   int       `json:"schema"`
	RunID           string    `json:"run_id"`
	CreatedAt       time.Time `json:"created_at"`
	CLIVersion      string    `json:"cli_version"`
	Profile         string    `json:"profile"`
	APIURL          string    `json:"api_url"`
	Environment     string    `json:"environment"`
	CredentialClass string    `json:"credential_class"`
	TokenPrefix     string    `json:"token_prefix"`
	PrincipalID     string    `json:"principal_id"`
	Argv            []string  `json:"argv"`
	Steps           []*Step   `json:"steps"`
}

// Step returns the named step, or nil.
func (r *Run) Step(name string) *Step {
	for _, s := range r.Steps {
		if s.Name == name {
			return s
		}
	}
	return nil
}

// Unresolved returns the steps C18's orphan check cares about.
func (r *Run) Unresolved() []*Step {
	var out []*Step
	for _, s := range r.Steps {
		if s.Unresolved() {
			out = append(out, s)
		}
	}
	return out
}

// Prunable reports whether every step is terminal, which is the condition
// `runs prune` requires (AC57): declined and unreachable count, and an
// awaiting_confirmation step does not, because an unconfirmed plan is a
// decision somebody has not yet made.
func (r *Run) Prunable() bool {
	for _, s := range r.Steps {
		if !s.State.Terminal() {
			return false
		}
		if s.HasPlanToken() {
			return false
		}
	}
	return true
}

// StepKey is the idempotency key for a step of a run: <run>-<step>. It is
// printable ASCII well under the 255-byte limit of PLAN §1.2.3, and it is
// unique across processes without coordination because the run id is.
func StepKey(runID, step string) string { return runID + "-" + step }

// keyRE is the server's rule: 1 to 255 bytes of printable ASCII
// (command_request.rb:116-117).
var keyRE = regexp.MustCompile(`\A[\x20-\x7E]{1,255}\z`)

// ErrInvalidKey reports an idempotency key FERRY would refuse.
var ErrInvalidKey = errors.New("runs: idempotency key must be 1-255 bytes of printable ASCII")

// ValidKey reports whether a caller-supplied key is one FERRY will accept.
// Checking locally means a bad --idempotency-key is a usage error rather
// than a request.
func ValidKey(k string) bool { return keyRE.MatchString(k) }

// ErrBodyNotUTF8 reports a request body that is not valid UTF-8.
//
// RFC 8259 requires a JSON document to be UTF-8 and FERRY parses the body as
// JSON, so this refuses only bodies the server would refuse anyway — and it
// refuses them before anything is sent. The check is here rather than in the
// caller because the record encodes the body as a JSON string, and Go's
// encoder replaces invalid UTF-8 with U+FFFD: without this, such a body would
// be silently altered on disk and a resume would resend different bytes.
var ErrBodyNotUTF8 = errors.New("runs: request body is not valid UTF-8")

func checkBody(b []byte) error {
	if !utf8.Valid(b) {
		return ErrBodyNotUTF8
	}
	return nil
}
