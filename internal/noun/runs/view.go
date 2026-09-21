package runs

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/kurenn/ferry-cli/internal/render"
	"github.com/kurenn/ferry-cli/internal/runs"
)

// redact removes every secret-shaped run from text (AC57, C6).
//
// It uses `render.FindSecrets`, which is the detector AC42's behavioural half
// already depends on being able to fire, rather than a second regexp local to
// this package. One authority on "what a secret looks like": a redactor with
// its own pattern is a redactor that stops agreeing with the scanner the
// moment either is edited, and the one that would be edited is the scanner,
// because it is the one with tests.
//
// The plan token is the reason this exists. A run record holds one in two
// declared places — `plan_token`, and the `plan_token` inside the execute
// step's stored `body` (V6) — and `runs show` is the command most likely to
// be run on a shared terminal, pasted into an issue, or piped into a log.
func redact(text string) string {
	for _, secret := range render.FindSecrets(text) {
		text = strings.ReplaceAll(text, secret, runs.RedactedToken)
	}

	return text
}

func redactBytes(raw []byte) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}

	return json.RawMessage(redact(string(raw)))
}

// runView is a run as it may be shown: the whole record with every secret
// replaced.
//
// It is a separate type rather than a mutated copy of `runs.Run` because
// mutating the record and then rendering it is one missed `Save` away from
// writing the redacted form back to disk — and `body_sha256` is compared
// against `body`, so a redacted body saved over the real one makes the
// record corrupt and the run unresumable. A view cannot be saved.
type runView struct {
	Schema          int        `json:"schema"`
	RunID           string     `json:"run_id"`
	CreatedAt       time.Time  `json:"created_at"`
	CLIVersion      string     `json:"cli_version"`
	Profile         string     `json:"profile"`
	APIURL          string     `json:"api_url"`
	Environment     string     `json:"environment"`
	CredentialClass string     `json:"credential_class"`
	TokenPrefix     string     `json:"token_prefix"`
	PrincipalID     string     `json:"principal_id"`
	Argv            []string   `json:"argv"`
	Steps           []stepView `json:"steps"`
}

type stepView struct {
	Name           string  `json:"name"`
	Operation      string  `json:"operation"`
	Method         string  `json:"method"`
	Path           string  `json:"path"`
	IdempotencyKey string  `json:"idempotency_key"`
	BodySHA256     *string `json:"body_sha256"`

	// Body is the request bytes with secrets replaced. The digest above is
	// of the *real* bytes and is deliberately left alone: it is the proof
	// that a resume resends the same request, and a digest recomputed over
	// the redacted form would be a decoration (AC11, M7).
	Body json.RawMessage `json:"body"`

	Plan *runs.Plan `json:"plan"`

	// PlanToken is `[REDACTED]` whenever one is stored, and null when none
	// is. The distinction is worth keeping: "this run holds a live plan
	// token" is a fact an operator acts on, and collapsing it to null would
	// hide it.
	PlanToken *string `json:"plan_token"`

	State    string        `json:"state"`
	Attempts int           `json:"attempts"`
	Response *responseView `json:"response"`
	Outcome  *runs.Outcome `json:"outcome"`
}

type responseView struct {
	Status     int               `json:"status"`
	Headers    map[string]string `json:"headers"`
	Body       json.RawMessage   `json:"body"`
	ReceivedAt time.Time         `json:"received_at"`
}

func view(run *runs.Run) runView {
	out := runView{
		Schema:          run.SchemaVersion,
		RunID:           run.RunID,
		CreatedAt:       run.CreatedAt,
		CLIVersion:      run.CLIVersion,
		Profile:         run.Profile,
		APIURL:          run.APIURL,
		Environment:     run.Environment,
		CredentialClass: run.CredentialClass,
		TokenPrefix:     run.TokenPrefix,
		PrincipalID:     run.PrincipalID,

		// `ferry transfers execute --plan <token>` puts the token on the
		// command line, and the record stores the command line, so argv
		// is a leak path in its own right.
		//
		// It is swept here rather than in the text renderer, which is
		// where it was first done. A345: the text path redacted it and the
		// JSON path did not, so `--output json` — the mode whose output
		// gets piped into a log — printed the token while the mode a human
		// reads did not. One layer down is the layer where both get it.
		Argv: redactEach(run.Argv),
	}

	for _, step := range run.Steps {
		out.Steps = append(out.Steps, viewStep(step))
	}

	return out
}

// redactEach sweeps every element of a slice, returning nil for nil so the
// JSON stays `null` rather than becoming `[]`.
func redactEach(in []string) []string {
	if in == nil {
		return nil
	}

	out := make([]string, len(in))
	for i, s := range in {
		out[i] = redact(s)
	}

	return out
}

// redactHeaders sweeps the recorded response headers.
//
// The body and the `plan_token` field are the two places a token is expected
// and were the two that were redacted first. A header is neither, and it is
// the one nobody thinks of: nothing stops FERRY — or a proxy in front of it
// — echoing a request header back, and the recorded headers are printed
// verbatim by `runs show`. The values are swept rather than a list of header
// names being filtered, because the name that carries it is exactly the part
// that cannot be predicted.
//
// The names themselves are left alone. A header called `Ferry-Command-Id` is
// what a caller looks for, and a redacted key would hide the shape of the
// answer while protecting nothing.
func redactHeaders(headers map[string]string) map[string]string {
	if headers == nil {
		return nil
	}

	out := make(map[string]string, len(headers))
	for name, value := range headers {
		out[name] = redact(value)
	}

	return out
}

func viewStep(step *runs.Step) stepView {
	out := stepView{
		Name:           step.Name,
		Operation:      step.Operation,
		Method:         step.Method,
		Path:           step.Path,
		IdempotencyKey: step.IdempotencyKey,
		BodySHA256:     step.BodySHA256,
		Body:           redactBytes(step.Body),
		Plan:           step.Plan,
		State:          string(step.State),
		Attempts:       step.Attempts,
		Outcome:        step.Outcome,
	}

	if step.PlanToken != nil {
		// Both spellings collapse to the same rendered word: a scrubbed
		// token is already `[SCRUBBED]` on disk and a live one must never
		// leave this process, so the render says only "there is a token
		// field here".
		redacted := runs.RedactedToken
		out.PlanToken = &redacted
	}

	if step.Response != nil {
		out.Response = &responseView{
			Status:     step.Response.Status,
			Headers:    redactHeaders(step.Response.Headers),
			Body:       redactBytes(step.Response.Body),
			ReceivedAt: step.Response.ReceivedAt,
		}
	}

	return out
}

func encode(v any) (json.RawMessage, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, render.Fault(err, "the run record could not be encoded: %v", err)
	}

	// The belt to the view's braces. Every field above is either a
	// literal from the record or already redacted, so this should find
	// nothing; `redact_test.go` proves it *can* find something by feeding a
	// record whose token sits in a field no view field names, which is the
	// case a later schema change would create.
	return redactBytes(raw), nil
}
