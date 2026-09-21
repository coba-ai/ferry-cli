package runs

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/kurenn/ferry-cli/internal/render"
	"github.com/kurenn/ferry-cli/internal/runs"
)

// AC57: no plan token is ever printed by `runs list` or `runs show`.
//
// The ledger keeps the token in the step's `body`, on purpose: the bytes are
// the proof that a resume resends the same request (AC11, AC45), and
// scrubbing them would destroy it. So redaction is a *display* obligation,
// and this file is where it is discharged.

// aRunHoldingATokenEverywhere is a record with a live plan token in every
// place one can end up, including a field invented after this test was
// written.
func aRunHoldingATokenEverywhere() *runs.Run {
	token := "ferry_plan_" + strings.Repeat("A", 32)
	plan := runs.Plan{ID: "qt_1", ExpiresAt: time.Unix(0, 0).UTC()}

	return &runs.Run{
		SchemaVersion: runs.Schema,
		RunID:         "01JQBN8Z5K0000000000000001",
		Profile:       "default",
		APIURL:        "https://api.example",
		Environment:   "sandbox",
		PrincipalID:   "key_1",

		// argv is a real leak path: `ferry transfers execute --plan
		// <token>` puts the token on the command line, and the record
		// stores the command line.
		Argv: []string{"ferry", "transfers", "execute", "--plan", token},

		Steps: []*runs.Step{{
			Name:           runs.StepExecute,
			Operation:      "transfers_execute",
			Method:         "POST",
			Path:           "/v1/transfers",
			IdempotencyKey: "01JQBN8Z5K0000000000000001-execute",
			State:          runs.StatePending,
			Attempts:       1,
			Body:           runs.Body([]byte(`{"plan_token":"` + token + `"}`)),
			Plan:           &plan,
			PlanToken:      &token,
			Response: &runs.Response{
				Status:  201,
				Headers: map[string]string{"X-Echo": token},
				Body:    []byte(`{"plan":{"token":"` + token + `"}}`),
			},
		}},
	}
}

// Both renderings are swept, in both modes, and the sweep is over the whole
// output rather than the fields this test thought of.
func TestNoRenderingPrintsAPlanToken(t *testing.T) {
	t.Parallel()

	record := aRunHoldingATokenEverywhere()

	// The floor. If `render.FindSecrets` cannot find the token in the
	// record itself, then finding none in the output proves nothing at all
	// — this is the control four shipped defects in this project were
	// missing.
	raw, err := json.Marshal(record)
	if err != nil {
		t.Fatalf("marshal the record: %v", err)
	}

	if got := render.FindSecrets(string(raw)); len(got) == 0 {
		t.Fatal("the detector finds no secret in a record built to hold five of them; " +
			"every assertion below would be vacuous")
	}

	for _, tc := range []struct {
		name   string
		render func() string
	}{
		{
			name: "show, text",
			render: func() string {
				var b strings.Builder

				writeRun(&b, record)

				return b.String()
			},
		},
		{
			name: "show, json",
			render: func() string {
				out, err := json.Marshal(view(record))
				if err != nil {
					t.Fatalf("marshal the view: %v", err)
				}

				return string(out)
			},
		},
		{
			name: "list, text",
			render: func() string {
				var b strings.Builder

				writeList(&b, []*runs.Run{record})

				return b.String()
			},
		},
		{
			name: "list, json",
			render: func() string {
				out, err := json.Marshal([]runView{view(record)})
				if err != nil {
					t.Fatalf("marshal the views: %v", err)
				}

				return string(out)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			out := tc.render()

			if got := render.FindSecrets(out); len(got) > 0 {
				t.Errorf("%d secret(s) reached the output: %v\n%s", len(got), got, out)
			}

			// And the raw token substring, in case the detector's pattern
			// is narrower than the token that is actually on disk.
			if strings.Contains(out, strings.Repeat("A", 32)) {
				t.Errorf("the token's body appears in the output\n%s", out)
			}
		})
	}
}

// M7: `body_sha256` must survive redaction.
//
// It is the digest a caller compares to prove the bytes a resume would
// resend are the bytes recorded. Redacting it because it looks like an
// opaque secret would remove the only integrity evidence in the record while
// leaving everything that looks reassuring.
func TestTheBodyDigestIsNotRedacted(t *testing.T) {
	t.Parallel()

	record := aRunHoldingATokenEverywhere()

	digest := record.Steps[0].Body.SHA256()
	record.Steps[0].BodySHA256 = &digest

	out, err := json.Marshal(view(record))
	if err != nil {
		t.Fatalf("marshal the view: %v", err)
	}

	if !strings.Contains(string(out), digest) {
		t.Errorf("body_sha256 %q is missing from the view; it is the proof a resume resends "+
			"the recorded bytes\n%s", digest, out)
	}

	var text strings.Builder

	writeRun(&text, record)

	if !strings.Contains(text.String(), digest) {
		t.Errorf("body_sha256 is missing from the text rendering\n%s", text.String())
	}
}

// The fields a caller needs in order to act survive redaction.
//
// A redactor that replaced the whole record with `[REDACTED]` would pass
// every assertion above. This is the other direction: what must still be
// there.
func TestRedactionKeepsWhatTheCallerNeeds(t *testing.T) {
	t.Parallel()

	record := aRunHoldingATokenEverywhere()

	out, err := json.Marshal(view(record))
	if err != nil {
		t.Fatalf("marshal the view: %v", err)
	}

	for _, want := range []string{
		record.RunID,
		record.Steps[0].IdempotencyKey,
		record.Profile,
		record.Environment,
		record.PrincipalID,
		string(runs.StatePending),
		"/v1/transfers",
	} {
		if !strings.Contains(string(out), want) {
			t.Errorf("%q is missing from the view; it is what the caller acts on\n%s", want, out)
		}
	}

	// The step's body is present and redacted, rather than removed. A
	// caller reading `runs show` needs to see that a body exists and what
	// shape it is; they must not see the token inside it.
	var view runView

	if err := json.Unmarshal(out, &view); err != nil {
		t.Fatalf("round-trip the view: %v", err)
	}

	if len(view.Steps) != 1 {
		t.Fatalf("the view has %d steps, want 1", len(view.Steps))
	}

	body := string(view.Steps[0].Body)

	if body == "" || body == "null" {
		t.Errorf("the step's body was dropped rather than redacted: %q", body)
	}

	if !strings.Contains(body, "plan_token") {
		t.Errorf("the body's shape was lost: %q", body)
	}

	if !strings.Contains(body, runs.RedactedToken) {
		t.Errorf("the body does not say it was redacted: %q", body)
	}
}

// A record with no token in it comes through unchanged.
//
// Redaction that fired on a record with nothing to redact would rewrite
// bodies a caller is comparing by eye, and the difference would look like a
// mismatch.
func TestARecordWithNoTokenIsUnchanged(t *testing.T) {
	t.Parallel()

	record := &runs.Run{
		SchemaVersion: runs.Schema,
		RunID:         "01JQBN8Z5K0000000000000002",
		Profile:       "default",
		Environment:   "sandbox",
		Argv:          []string{"ferry", "transfers", "create"},
		Steps: []*runs.Step{{
			Name:           runs.StepSimulate,
			IdempotencyKey: "01JQBN8Z5K0000000000000002-simulate",
			State:          runs.StateTerminal,
			Body:           runs.Body([]byte(`{"amount":{"value":"100.00"}}`)),
		}},
	}

	out, err := json.Marshal(view(record))
	if err != nil {
		t.Fatalf("marshal the view: %v", err)
	}

	var view runView

	if err := json.Unmarshal(out, &view); err != nil {
		t.Fatalf("round-trip the view: %v", err)
	}

	if got, want := string(view.Steps[0].Body), string(record.Steps[0].Body); got != want {
		t.Errorf("the body was rewritten with nothing to redact:\n got %s\nwant %s", got, want)
	}

	if strings.Contains(string(out), runs.RedactedToken) {
		t.Errorf("the view claims a redaction that did not happen\n%s", out)
	}
}
