package poll

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/kurenn/ferry-cli/internal/outcome"
)

// A394: `Command` had no Go struct, so `state`, `last_error` and
// `contradiction` had no Go-side pin. This package adds one, so this file
// pins it — in both directions, with a floor, the way
// `cli/internal/api/schema_pin_test.go` does.

// contractPath is the contract this struct claims to match.
func contractPath(t *testing.T) string {
	t.Helper()

	// The same path `internal/api`'s pin resolves: `internal/poll` →
	// `internal` → the repository root, then `contract/openapi.yaml`,
	// which is the copy this repository vendors from the Rails app
	// (A400, A401).
	path := filepath.Join(contractRoot, "contract", "openapi.yaml")

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the contract is not at %s: %v. This pin does not skip — a pin that "+
			"skips when it cannot find the document is a pin that stopped running and "+
			"said so in a place nobody reads.", path, err)
	}

	return path
}

// TestTheCommandStructIsPinnedToTheContract holds the struct's JSON tags and
// the schema's properties to each other.
//
// Both directions. A one-directional subset check has shipped in this
// project at least five times: "every field I declared is in the schema"
// passes when the schema grew a field, and "every schema property is
// declared" passes when the struct grew one that nothing serves. The
// unbound set below is the explicit record of the difference, so a property
// added to the contract reddens this rather than passing unnoticed.
func TestTheCommandStructIsPinnedToTheContract(t *testing.T) {
	t.Parallel()

	schema := commandSchema(t, contractPath(t))

	// The floor. A pin computed from an empty schema is true of any struct
	// at all, and a YAML reader that silently returned nothing is the way
	// that happens.
	if len(schema) < 10 {
		t.Fatalf("the Command schema has %d properties, which is too few to be the real one: %v",
			len(schema), sorted(schema))
	}

	declared := jsonTagsOf(t, Command{})

	if len(declared) == 0 {
		t.Fatal("the struct declares no JSON tags; this pin would be vacuous")
	}

	// Every property this struct binds must exist in the schema.
	for _, name := range declared {
		if !schema[name] {
			t.Errorf("Command binds %q, which openapi.yaml's Command does not declare. "+
				"A field bound to a property that does not exist is always its zero value, "+
				"and a decision made on it is a decision made on nothing.", name)
		}
	}

	// And every property the schema declares must be either bound or
	// listed as deliberately unbound.
	for name := range schema {
		if contains(declared, name) || unboundCommandProperties[name] != "" {
			continue
		}

		t.Errorf("openapi.yaml's Command declares %q, which this struct neither binds nor "+
			"lists in unboundCommandProperties. If nothing needs it, say so there; if "+
			"something does, bind it.", name)
	}

	// The unbound list itself, in the other direction: an entry naming a
	// property the schema no longer has is a permission granted for a
	// reason that has gone, and the next reader takes the list as the truth
	// about the contract.
	for name, why := range unboundCommandProperties {
		if !schema[name] {
			t.Errorf("unboundCommandProperties names %q (%q), which openapi.yaml's Command "+
				"no longer declares", name, why)
		}

		if contains(declared, name) {
			t.Errorf("%q is both bound by the struct and listed as unbound", name)
		}
	}
}

// unboundCommandProperties is every `Command` property this struct does not
// bind, with the reason.
//
// It is data rather than a comment so the pin can hold it to the contract.
var unboundCommandProperties = map[string]string{}

// TestTheStateEnumIsTheContractsEnum holds the states this CLI can name
// against the schema's enum.
//
// `outcome.KnownState` is the census, and a state outside it is `escalate` —
// "this CLI cannot say what it means" — rather than `pending`, which would
// poll forever. That fail-closed answer is only safe if the census is
// actually the contract's enum: too small and a legitimate state escalates,
// too large and a state the contract dropped is still polled.
func TestTheStateEnumIsTheContractsEnum(t *testing.T) {
	t.Parallel()

	states := commandStateEnum(t, contractPath(t))

	if len(states) < 5 {
		t.Fatalf("the contract's state enum has %d entries, too few to be the real one: %v",
			len(states), states)
	}

	for _, state := range states {
		if !outcome.KnownState(state) {
			t.Errorf("openapi.yaml declares the state %q and outcome.KnownState refuses it, "+
				"so a command in that state escalates as unreadable", state)
		}
	}

	// The other direction lives in `internal/outcome`, which owns the
	// census. What this package can check is that nothing here invents a
	// state of its own.
	for _, invented := range statesMentionedIn(t, "command.go", "poll.go") {
		if !contains(states, invented) {
			t.Errorf("this package names the state %q, which the contract's enum does not "+
				"contain", invented)
		}
	}
}

// A392, at the layer that decides it.
//
// `Command.Input` is the only place in this package that reads a command's
// fields for the table, and `last_error.code` is passed through rather than
// interpreted. This asserts the pass-through *and* that no classification
// here depends on it: the same command, with every error code in turn, must
// classify identically.
func TestTheErrorCodeDoesNotChangeTheClassification(t *testing.T) {
	t.Parallel()

	// The codes A392 names as the dangerous ones: read through the wire
	// table they mean "resend" or "keep polling", and a `failed_terminal`
	// command will never move again.
	codes := []string{
		"", "UPSTREAM_UNKNOWN", "RATE_LIMITED", "SERVICE_UNAVAILABLE",
		"UPSTREAM_BUSY", "INTERNAL_ERROR", "NOT_A_CODE_AT_ALL",
	}

	for _, state := range []string{"failed_terminal", "completed", "inflight"} {
		var first outcome.Outcome

		for i, code := range codes {
			cmd := Command{
				ID:        "cmd_1",
				Operation: string(outcome.OpExecuteTransfer),
				State:     state,
			}

			if code != "" {
				cmd.LastError = &LastError{Code: code}
			}

			got := outcome.ClassifyCommand(cmd.Input())

			// The pass-through: the table is given the code.
			if in := cmd.Input(); in.LastErrorCode != code {
				t.Errorf("Input() passed %q where last_error.code was %q", in.LastErrorCode, code)
			}

			if i == 0 {
				first = got

				continue
			}

			if got.Class != first.Class || got.Exit != first.Exit {
				t.Errorf("a %s command classified %s/%d with code %q and %s/%d without it; "+
					"the code is for an operator and never for a decision (A392)",
					state, got.Class, got.Exit, code, first.Class, first.Exit)
			}
		}

		// The floor: if `failed_terminal` classified `transient` or
		// `pending` for *every* code, the equality above would hold and
		// the property A392 protects would be broken.
		if state == "failed_terminal" {
			switch first.Class {
			case outcome.ClassTransient:
				t.Error("a failed_terminal command classifies transient, which tells the " +
					"caller to resend a command that will never move")
			case outcome.ClassPending:
				t.Error("a failed_terminal command classifies pending, which tells the " +
					"caller to keep polling a command that will never move")
			}
		}
	}
}

// A353: a `null` `result.body` is an absent body and not an unreadable
// command.
//
// `api.CommandResultBody.UnmarshalJSON` is called even for a JSON null,
// where it finds no `object` to discriminate on and fails. With a value-typed
// field that failure propagated to the whole command, and `poll.Watch`
// reported "200 with a body that could not be read" — `transient`, exit 5,
// "resend the identical request" — for a completed command carrying a
// contradiction, which is exit 7 and an operator. The pointer is what makes
// the null skip the unmarshaller.
func TestANullResultBodyIsReadable(t *testing.T) {
	t.Parallel()

	body := []byte(`{
	  "object": "command",
	  "id": "cmd_1",
	  "operation": "transfers_execute",
	  "state": "completed",
	  "attempts": 1,
	  "result": {"status": 201, "body": null},
	  "contradiction": {"kind": "second_transaction", "detected_by": "recovery"}
	}`)

	cmd, ok := DecodeCommand(body)
	if !ok {
		t.Fatal("a command with a null result.body could not be decoded")
	}

	if cmd.Result == nil || cmd.Result.Status != 201 {
		t.Errorf("result = %v, want status 201", cmd.Result)
	}

	if cmd.Result.Body != nil {
		t.Errorf("result.body = %v, want nil for a JSON null", cmd.Result.Body)
	}

	// And the contradiction survives, which is the field that decides.
	if cmd.Contradiction == nil {
		t.Fatal("the contradiction was lost")
	}

	if got := outcome.ClassifyCommand(cmd.Input()); got.Class != outcome.ClassEscalate {
		t.Errorf("classified %s/%d, want escalate: a contradiction dominates every state",
			got.Class, got.Exit)
	}
}

// `last_error`'s unbound keys survive a decode and re-encode.
//
// The contract declares it **open**: seven writers produce it and three merge
// onto what is there, so the keys present are the union of the writers that
// ran. Only `code` is bound, and dropping the rest would lose exactly the
// detail an operator opened `runs show` for.
func TestLastErrorKeepsItsUnboundKeys(t *testing.T) {
	t.Parallel()

	// The shape the recordings carry, from `execute.202.then_completed`.
	raw := []byte(`{
	  "code": "UPSTREAM_UNKNOWN",
	  "category": "unknown",
	  "body_class": "other",
	  "rejection_shaped": false,
	  "retry_after_seconds": 9
	}`)

	var err LastError
	if unmarshalErr := json.Unmarshal(raw, &err); unmarshalErr != nil {
		t.Fatalf("decode last_error: %v", unmarshalErr)
	}

	if err.Code != "UPSTREAM_UNKNOWN" {
		t.Errorf("code = %q, want UPSTREAM_UNKNOWN", err.Code)
	}

	// Keys() is sorted, so a record written from a decoded command is
	// stable rather than depending on Go's map iteration order.
	want := []string{"body_class", "category", "rejection_shaped", "retry_after_seconds"}

	if got := err.Keys(); !reflect.DeepEqual(got, want) {
		t.Errorf("Keys() = %v, want %v", got, want)
	}

	// Sorted twice is the same, which is the property a record depends on.
	if got := err.Keys(); !sort.StringsAreSorted(got) {
		t.Errorf("Keys() is not sorted: %v", got)
	}

	out, marshalErr := json.Marshal(err)
	if marshalErr != nil {
		t.Fatalf("encode last_error: %v", marshalErr)
	}

	var round map[string]any
	if unmarshalErr := json.Unmarshal(out, &round); unmarshalErr != nil {
		t.Fatalf("round-trip last_error: %v", unmarshalErr)
	}

	var original map[string]any
	if unmarshalErr := json.Unmarshal(raw, &original); unmarshalErr != nil {
		t.Fatalf("parse the original: %v", unmarshalErr)
	}

	if !reflect.DeepEqual(round, original) {
		t.Errorf("the round trip lost or changed keys:\n got %v\nwant %v", round, original)
	}
}

// RetryDelaySeconds is `max(retry_after_seconds, 1)`, and 5 when the server
// did not say.
//
// The nil fallback is the one that matters: a `0` would make the loop spin,
// and a `1` would poll five times as often as `ledger/replay.rb` expects for
// the two states that reach it in practice.
func TestRetryDelaySeconds(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		in   *int
		want int
	}{
		{name: "absent", in: nil, want: 5},
		{name: "zero", in: intp(0), want: 1},
		{name: "negative", in: intp(-30), want: 1},
		{name: "one", in: intp(1), want: 1},
		{name: "nine", in: intp(9), want: 9},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			cmd := Command{RetryAfterSeconds: tc.in}

			if got := cmd.RetryDelaySeconds(); got != tc.want {
				t.Errorf("RetryDelaySeconds() = %d, want %d", got, tc.want)
			}
		})
	}
}

func intp(n int) *int { return &n }

// jsonTagsOf reads a struct's JSON field names.
func jsonTagsOf(t *testing.T, v any) []string {
	t.Helper()

	typ := reflect.TypeOf(v)

	var out []string

	for i := 0; i < typ.NumField(); i++ {
		tag := typ.Field(i).Tag.Get("json")
		if tag == "" || tag == "-" {
			continue
		}

		name := strings.Split(tag, ",")[0]
		if name != "" {
			out = append(out, name)
		}
	}

	sort.Strings(out)

	return out
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}

	return false
}

func sorted(m map[string]bool) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}

	sort.Strings(out)

	return out
}
