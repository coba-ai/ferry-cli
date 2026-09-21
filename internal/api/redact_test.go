package api_test

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/kurenn/ferry/cli/internal/api"
	"github.com/kurenn/ferry/cli/internal/outcome"
)

// AC28 and C6. `--debug` output exists to be pasted into a bug report.
//
// The canaries are the control. Each is a string that appears nowhere else, so
// "the trace does not contain this" is a claim about the whole stream rather
// than about the one line the test happened to look at — and a redaction rule
// that is deleted, reordered or narrowed shows up as a canary in the output
// instead of as a subtly different string.

const (
	canaryPAT   = "ferry_pat_CANARYpersonalCANARY"
	canarySK    = "ferry_sk_CANARYsecretCANARY"
	canaryPlan  = "ferry_plan_CANARYplanCANARY"
	canaryToken = "CANARYbaretokenCANARY"
)

func assertNoCanaries(t *testing.T, what, s string) {
	t.Helper()

	for _, canary := range []string{canaryPAT, canarySK, canaryPlan, canaryToken} {
		if strings.Contains(s, canary) {
			t.Errorf("%s leaked the canary %q:\n%s", what, canary, s)
		}
	}
}

func TestRedactRemovesEveryCredentialShape(t *testing.T) {
	cases := []struct {
		name string
		in   string
		// keep is a fragment that must survive, so a rule cannot pass by
		// deleting the whole trace.
		keep string
	}{
		{
			name: "an Authorization header",
			in:   "Authorization: Bearer " + canaryPAT,
			keep: "Authorization:",
		},
		{
			name: "an Authorization header in another case",
			in:   "authorization: bearer " + canarySK,
			keep: "authorization:",
		},
		{
			// The whole value goes, not only the part that looks like a
			// token, so a scheme this CLI has never seen is still removed.
			name: "an Authorization header with an unfamiliar scheme",
			in:   "Authorization: Mystery " + canaryToken,
			keep: "Authorization:",
		},
		{
			name: "a bare secret key",
			in:   "the key was " + canarySK + " and it failed",
			keep: "ferry_sk_",
		},
		{
			name: "a bare personal access token",
			in:   "logged in as " + canaryPAT,
			keep: "ferry_pat_",
		},
		{
			name: "a plan token in a body",
			in:   `{"plan_token":"` + canaryPlan + `"}`,
			keep: "plan_token",
		},
		{
			name: "a JSON token member",
			in:   `{"token":"` + canaryToken + `"}`,
			keep: `"token"`,
		},
		{
			name: "a nested plan token member",
			in:   `{"plan":{"token":"` + canaryToken + `","expires_at":"2026-01-01T00:00:00Z"}}`,
			keep: "expires_at",
		},
		{
			name: "a token member with spaces around the colon",
			in:   `{"token" : "` + canaryToken + `"}`,
			keep: `"token"`,
		},
		{
			name: "a Go map rendering of a decoded body",
			in:   "map[object:simulation plan:map[token:" + canaryToken + " expires_at:2026]]",
			keep: "object:simulation",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := api.Redact(c.in)

			assertNoCanaries(t, "Redact", got)

			if !strings.Contains(got, api.Placeholder) {
				t.Errorf("nothing was marked as redacted:\n  %s", got)
			}

			if !strings.Contains(got, c.keep) {
				t.Errorf("redaction removed %q, which is not a secret:\n  %s", c.keep, got)
			}
		})
	}
}

// The prefix survives, so a trace still says *which kind* of credential was
// involved — which is most of what a bug report needs from it.
func TestRedactKeepsTheTokenPrefix(t *testing.T) {
	for prefix, in := range map[string]string{
		"ferry_sk_":   canarySK,
		"ferry_pat_":  canaryPAT,
		"ferry_plan_": canaryPlan,
	} {
		got := api.Redact("value " + in)

		if want := prefix + api.Placeholder; !strings.Contains(got, want) {
			t.Errorf("got %q, want it to contain %q", got, want)
		}
	}
}

// Text with nothing secret in it passes through unchanged. Without this, a
// redactor that replaced everything would pass every example above.
func TestRedactLeavesOrdinaryTextAlone(t *testing.T) {
	in := `> POST /v1/transfers
> Accept: application/json
< 403
< {"error":{"code":"POLICY_MAX_AMOUNT_PER_DAY","message":"over the daily limit"}}`

	if got := api.Redact(in); got != in {
		t.Errorf("ordinary text was altered:\n  in:  %s\n  out: %s", in, got)
	}
}

// The whole debug stream, end to end. This is the claim that matters: not
// "Redact works" but "nothing that reaches the trace has escaped it".
func TestDebugTraceRedactsEverything(t *testing.T) {
	var trace bytes.Buffer

	_, client := newServer(t, func(w http.ResponseWriter, r *http.Request, n int) {
		w.Header().Set("Ferry-Command-Id", "cmd_1")
		okJSON(w, 201, `{"object":"simulation","plan":{"token":"`+canaryPlan+`","expires_at":"2026-01-01T00:00:00Z"}}`)
	})

	client.Token = canarySK
	client.Debug = &trace

	// The body carries a `token` member and a plan token, which AC28 names,
	// alongside ordinary fields that must survive. What AC28 does *not*
	// promise is that an opaque string in a member with some other name is
	// redacted — nothing distinguishes it from a customer id — so no canary
	// is planted there and none is claimed.
	_, err := client.Do(context.Background(), api.Request{
		Op:             outcome.OpSimulateTransfer,
		Body:           []byte(`{"customer_id":"cus_1","plan_token":"` + canaryPlan + `","token":"` + canaryPAT + `"}`),
		IdempotencyKey: "key-1",
	})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}

	got := trace.String()

	if got == "" {
		t.Fatal("the debug writer received nothing; this example proves nothing")
	}

	assertNoCanaries(t, "the debug trace", got)

	// Non-vacuity in the other direction: the trace must still be a trace.
	for _, want := range []string{"POST", "/v1/transfers/simulate", "Idempotency-Key", "201", "cus_1"} {
		if !strings.Contains(got, want) {
			t.Errorf("the trace does not contain %q, so redaction may have eaten it:\n%s", want, got)
		}
	}
}

// Nothing is written when `--debug` is off. A redactor is a poor second line
// of defence for a stream that should not exist.
func TestNoTraceWithoutDebug(t *testing.T) {
	_, client := newServer(t, func(w http.ResponseWriter, r *http.Request, n int) {
		okJSON(w, 200, `{}`)
	})

	if client.Debug != nil {
		t.Fatal("the test client has Debug set")
	}

	if _, err := client.Do(context.Background(), api.Request{Op: outcome.OpGetMe}); err != nil {
		t.Fatalf("Do: %v", err)
	}
}

func TestRedactedHeaderValue(t *testing.T) {
	if got := api.RedactedHeaderValue("Authorization", "Bearer "+canarySK); got != api.Placeholder {
		t.Errorf("Authorization: got %q", got)
	}

	if got := api.RedactedHeaderValue("authorization", "Bearer "+canarySK); got != api.Placeholder {
		t.Errorf("a lower-case Authorization: got %q", got)
	}

	if got := api.RedactedHeaderValue("Accept", "application/json"); got != "application/json" {
		t.Errorf("an ordinary header was altered: %q", got)
	}

	// A header that is not `Authorization` but carries a token shape is
	// still redacted, because the rule is about shapes.
	got := api.RedactedHeaderValue("X-Something", canaryPlan)
	if strings.Contains(got, "CANARYplanCANARY") {
		t.Errorf("a token in a non-Authorization header survived: %q", got)
	}
}
