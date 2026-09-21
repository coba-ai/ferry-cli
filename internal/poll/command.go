package poll

import (
	"encoding/json"
	"sort"

	"github.com/kurenn/ferry-cli/internal/api"
	"github.com/kurenn/ferry-cli/internal/outcome"
)

// Command is the `Command` schema of `openapi.yaml` — the body of
// `GET /v1/commands/{id}` and of every `202`.
//
// A394 recorded that this struct did not exist, so `state`, `last_error` and
// `contradiction` had no Go-side pin. It lives here rather than in
// `internal/api` because `internal/api` is U2's and the only reader of these
// fields is this loop. `schema_pin_test.go` in this package holds it to the
// document in both directions, with a floor, the way
// `api/schema_pin_test.go` holds the response bodies.
//
// # Which fields are pointers
//
// The same rule `api/responses.go` states: a property the contract types
// `[…, "null"]` is a pointer, because null and absent are answers and the
// zero value is not. `retry_after_seconds` is the one that would cost
// something — §5.7 sleeps `max(retry_after_seconds, 1)` and falls back to 5
// when it is null, and an `int` would make a null indistinguishable from a
// server asking for no wait at all.
type Command struct {
	Object         string  `json:"object"`
	ID             string  `json:"id"`
	Operation      string  `json:"operation"`
	State          string  `json:"state"`
	CreatedAt      *string `json:"created_at"`
	StateChangedAt *string `json:"state_changed_at"`
	Attempts       int     `json:"attempts"`

	// Poll is a path, never a URL. `api.Client.ResolvePollPath` refuses an
	// absolute one (CS-13); this loop does not follow it at all and polls
	// the operation's own route, so a body cannot redirect the CLI even if
	// that guard were removed.
	Poll string `json:"poll"`

	RetryAfterSeconds *int `json:"retry_after_seconds"`

	Result *CommandResult `json:"result"`

	// LastError is for the record and for an operator, never for a
	// decision (A392, and the contract says so in as many words). Its
	// `code` is not the `errors.md` vocabulary — the recordings carry
	// `UPSTREAM_UNKNOWN`, which the catalogue does not name — so this
	// package hands it to `outcome.ClassifyCommand` and reads nothing out
	// of it itself.
	LastError *LastError `json:"last_error"`

	// Contradiction non-null means stop, whatever `state` says (AC21).
	Contradiction *Contradiction `json:"contradiction"`

	TransactionID  *string `json:"transaction_id"`
	IdempotencyKey *string `json:"idempotency_key"`
}

// CommandResult is `Command.result`: populated once `completed` and only
// while still replayable, so a nil on a completed command means the answer
// aged out rather than that it failed.
type CommandResult struct {
	Status int `json:"status"`

	// Body is the discriminated union U2b typed (A393). Decoding it is what
	// keeps a `StoredSimulation` from being read as a `Transaction` with an
	// empty `status` — exit 0 where AC74 requires exit 4.
	//
	// It is a **pointer**, and that is load-bearing rather than stylistic.
	// `result.body` is null on a completed command whose stored answer aged
	// out of the replay window, and
	// `api.CommandResultBody.UnmarshalJSON` is called even for a JSON null,
	// where it finds no `object` to discriminate on and returns
	// `ErrResultBodyShape` (A353). A value here therefore made the whole
	// command unreadable, and `poll.Watch` reported "200 with a body that
	// could not be read" — exit 5, "resend the identical request" — for a
	// command carrying a contradiction, which is exit 7 and a human.
	Body *api.CommandResultBody `json:"body"`
}

// LastError is `Command.last_error`, which the contract declares **open**:
// seven writers produce it and three of them merge onto what is already
// there, so the keys present are the union of the writers that have run.
//
// Only `code` is bound, because only `code` is `required` and nothing in this
// CLI reads any of the rest. The fifteen properties the document lists are a
// vocabulary; `unboundLastErrorProperties` in the pin names every one this
// struct does not bind and is held equal to "the schema's properties minus
// this struct's fields" in both directions, so a property added to the
// contract reddens the pin rather than passing unnoticed through a
// one-directional subset check.
type LastError struct {
	Code string `json:"code"`

	// Rest is every other key, kept so `runs show` can print what an
	// operator needs without this CLI claiming to understand it.
	Rest map[string]json.RawMessage `json:"-"`
}

// UnmarshalJSON keeps the unbound keys rather than dropping them.
func (e *LastError) UnmarshalJSON(data []byte) error {
	var all map[string]json.RawMessage
	if err := json.Unmarshal(data, &all); err != nil {
		return err
	}

	*e = LastError{Rest: map[string]json.RawMessage{}}

	for key, raw := range all {
		if key == "code" {
			if err := json.Unmarshal(raw, &e.Code); err != nil {
				return err
			}

			continue
		}

		e.Rest[key] = raw
	}

	return nil
}

// Keys is the unbound keys, sorted, for a renderer that has to print them in
// a stable order.
func (e LastError) Keys() []string {
	out := make([]string, 0, len(e.Rest))
	for key := range e.Rest {
		out = append(out, key)
	}

	sort.Strings(out)

	return out
}

// MarshalJSON puts the unbound keys back, so a record written from a decoded
// command is the command.
func (e LastError) MarshalJSON() ([]byte, error) {
	all := make(map[string]json.RawMessage, len(e.Rest)+1)
	for key, raw := range e.Rest {
		all[key] = raw
	}

	code, err := json.Marshal(e.Code)
	if err != nil {
		return nil, err
	}

	all["code"] = code

	return json.Marshal(all)
}

// Contradiction is `Command.contradiction` — a closed schema with one writer,
// written once and never overwritten.
type Contradiction struct {
	Kind              string   `json:"kind"`
	TransactionOMSID  string   `json:"transaction_oms_id"`
	DetectedBy        string   `json:"detected_by"`
	PriorState        string   `json:"prior_state"`
	DetectedAt        string   `json:"detected_at"`
	TransactionOMSIDs []string `json:"transaction_oms_ids"`
}

// Input reduces a command to the tuple `outcome.ClassifyCommand` reads.
//
// Everything the table branches on is read here and nowhere else, which is
// what makes A392 enforceable: `last_error.code` is passed through to the
// table, which refuses to let a `failed_terminal` command classify
// `transient` or `pending` whatever code it carries. This package does not
// re-derive that rule and must not.
func (c Command) Input() outcome.CommandInput {
	in := outcome.CommandInput{
		Operation:     c.Operation,
		State:         c.State,
		Contradiction: c.Contradiction != nil,
		CommandID:     c.ID,
	}

	if c.TransactionID != nil {
		in.TransactionID = *c.TransactionID
	}

	if c.LastError != nil {
		in.LastErrorCode = c.LastError.Code
	}

	if c.Result != nil {
		in.Result = &outcome.CommandResult{Status: c.Result.Status}

		// A `transfers_simulate` command's stored body is a
		// StoredSimulation and structurally carries no `status` (A393), so
		// the field is read from the transaction arm only. Reading it from
		// whichever arm decoded would answer "" for a simulate, which
		// `classifyCompleted` would take for a transaction with no status.
		if c.Result.Body != nil && c.Result.Body.Transaction != nil {
			in.Result.TransactionStatus = c.Result.Body.Transaction.Status
		}
	}

	return in
}

// RetryDelaySeconds is §5.7 step 5: `max(retry_after_seconds, 1)`, and 5 when
// the server did not say.
//
// The nil fallback is 5 and not 1 because a command with no interval is one
// FERRY has no estimate for, and the two states that reach this in practice —
// `upstream_unknown` and `failed_retriable` — are the ones `ledger/replay.rb`
// gives 5 to.
func (c Command) RetryDelaySeconds() int {
	if c.RetryAfterSeconds == nil {
		return 5
	}

	if *c.RetryAfterSeconds < 1 {
		return 1
	}

	return *c.RetryAfterSeconds
}
