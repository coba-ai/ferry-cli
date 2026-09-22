package api_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/coba-ai/ferry-cli/internal/api"
	"github.com/coba-ai/ferry-cli/internal/outcome"
)

// AC27 and C14. What is resent, what is not, and what a resend must look like.
//
// The policy is stated twice on purpose: once as a predicate over an answer
// (Retryable), tested exhaustively below, and once as behaviour through the
// loop. The predicate is where a reader checks the rule; the loop is where the
// rule is actually obeyed, including the two properties that make a resend
// safe at all — the same bytes and the same key.

func envelopeWith(code string, retryAfter any) string {
	b, _ := json.Marshal(map[string]any{
		"error": map[string]any{
			"code": code, "message": "m", "retriable": true,
			"retry_after_seconds": retryAfter, "details": nil,
			"request_id": "req", "docs_url": "d",
		},
	})

	return string(b)
}

func TestRetryablePolicy(t *testing.T) {
	cases := []struct {
		name      string
		class     outcome.OpClass
		status    int
		code      string
		commandID string
		want      bool
	}{
		// The five AC27 enumerates.
		{"429 on money", outcome.OpMoneyExecute, 429, "RATE_LIMITED", "", true},
		{"503 UPSTREAM_BUSY with no command", outcome.OpMoneyExecute, 503, "UPSTREAM_BUSY", "", true},
		{"503 SERVICE_UNAVAILABLE", outcome.OpMoneyExecute, 503, "SERVICE_UNAVAILABLE", "", true},
		{"503 UPSTREAM_AUTH_UNAVAILABLE", outcome.OpMoneyExecute, 503, "UPSTREAM_AUTH_UNAVAILABLE", "", true},
		{"500 on a money request", outcome.OpMoneyExecute, 500, "INTERNAL_ERROR", "", true},

		// FERRY is already retrying this one itself; the caller polls.
		{"503 UPSTREAM_BUSY with a command", outcome.OpMoneyExecute, 503, "UPSTREAM_BUSY", "cmd_1", false},

		// 503s that are not transient.
		{"UPSTREAM_UNAVAILABLE", outcome.OpMoneyExecute, 503, "UPSTREAM_UNAVAILABLE", "", false},
		{"UPSTREAM_NOT_CONFIGURED", outcome.OpMoneyExecute, 503, "UPSTREAM_NOT_CONFIGURED", "", false},
		{"UPSTREAM_CREDENTIALS_INVALID", outcome.OpMoneyExecute, 503, "UPSTREAM_CREDENTIALS_INVALID", "", false},

		// No refusal FERRY computed from the request is resent: it will be
		// computed the same way again.
		{"400", outcome.OpMoneyExecute, 400, "VALIDATION_FAILED", "", false},
		{"401", outcome.OpMoneyExecute, 401, "TOKEN_EXPIRED", "", false},
		{"403 policy", outcome.OpMoneyExecute, 403, "POLICY_MAX_AMOUNT_PER_DAY", "", false},
		{"409 in progress", outcome.OpMoneyExecute, 409, "IDEMPOTENCY_KEY_IN_PROGRESS", "cmd_1", false},
		{"422", outcome.OpMoneySimulate, 422, "QUOTE_REJECTED", "", false},

		// CRITIQUE N5: the fixture's "nothing matched" marker. Retrying it
		// turns a clear test failure into a slow one.
		{"599 on money", outcome.OpMoneyExecute, 599, "", "", false},
		{"599 on a read", outcome.OpRead, 599, "", "", false},
		{"599 with an envelope", outcome.OpRead, 599, "SERVICE_UNAVAILABLE", "", false},

		// AC38: a key mint is never resent at any status. An unanswered
		// mint may have minted a key whose token this CLI no longer holds,
		// and a second attempt mints a second one.
		{"429 on keys create", outcome.OpControlCreate, 429, "RATE_LIMITED", "", false},
		{"503 on keys create", outcome.OpControlCreate, 503, "SERVICE_UNAVAILABLE", "", false},
		{"500 on keys create", outcome.OpControlCreate, 500, "INTERNAL_ERROR", "", false},

		// Reads: §5.6's "429/5xx → 5 after budget".
		{"429 on a read", outcome.OpRead, 429, "RATE_LIMITED", "", true},
		{"500 on a read", outcome.OpRead, 500, "INTERNAL_ERROR", "", true},
		{"an unknown 5xx on a read", outcome.OpRead, 504, "", "", true},
		{"an unknown 4xx on a read", outcome.OpRead, 418, "", "", false},

		// An unreadable 5xx on the money path is not on §5.8's list, and
		// this CLI does not extend the list on its own.
		{"an unknown 5xx on money", outcome.OpMoneyExecute, 504, "", "", false},

		// A success is never "retried", whatever else is true.
		{"200", outcome.OpRead, 200, "", "", false},
		{"201 on money", outcome.OpMoneyExecute, 201, "", "", false},
		{"202 on money", outcome.OpMoneyExecute, 202, "", "", false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := api.Retryable(c.class, c.status, c.code, c.commandID); got != c.want {
				t.Errorf("Retryable(%s, %d, %q, %q) = %v, want %v",
					c.class, c.status, c.code, c.commandID, got, c.want)
			}
		})
	}
}

// The loop resends the same bytes under the same key, and waits
// `max(retry_after_seconds, 1)` between attempts.
//
// The key assertion is M28's target. A retry that mints a new key is not a
// retry — it is a second, independent money request, and the server has no way
// to tell it is a duplicate.
func TestRetryResendsTheSameBytesUnderTheSameKey(t *testing.T) {
	body := []byte(`{"plan_token":  "ferry_plan_aaa", "zeta":1}`)

	cap, client := newServer(t, func(w http.ResponseWriter, r *http.Request, n int) {
		if n < 3 {
			w.Header().Set("Retry-After", "2")
			okJSON(w, 503, envelopeWith("SERVICE_UNAVAILABLE", 2))

			return
		}

		okJSON(w, 201, `{"object":"transaction","status":"submitted"}`)
	})

	clock := client.Clock.(*fakeClock)

	res, err := client.Do(context.Background(), api.Request{
		Op: outcome.OpExecuteTransfer, Body: body, IdempotencyKey: "key-fixed",
	})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}

	if res.Attempts != 3 {
		t.Errorf("attempts: got %d, want 3", res.Attempts)
	}

	if res.Meta.Status != 201 {
		t.Errorf("final status: got %d", res.Meta.Status)
	}

	if cap.count() != 3 {
		t.Fatalf("the server saw %d requests, want 3", cap.count())
	}

	for i := range 3 {
		got := cap.at(t, i)

		if string(got.body) != string(body) {
			t.Errorf("attempt %d sent different bytes:\n  %s", i+1, got.body)
		}

		if k := got.header.Get("Idempotency-Key"); k != "key-fixed" {
			t.Errorf("attempt %d sent the key %q, want the one fixed before the loop", i+1, k)
		}
	}

	want := []time.Duration{2 * time.Second, 2 * time.Second}
	if len(clock.slept) != len(want) {
		t.Fatalf("slept %v, want %v", clock.slept, want)
	}

	for i, d := range want {
		if clock.slept[i] != d {
			t.Errorf("sleep %d: got %s, want %s", i+1, clock.slept[i], d)
		}
	}
}

// The budget is a ceiling on total wall time, and the loop checks it before
// sleeping rather than after — sleeping past the budget and then returning
// would spend time to learn nothing.
func TestRetryStopsAtTheBudget(t *testing.T) {
	cap, client := newServer(t, func(w http.ResponseWriter, r *http.Request, n int) {
		w.Header().Set("Retry-After", "10")
		okJSON(w, 503, envelopeWith("SERVICE_UNAVAILABLE", 10))
	})

	client.RetryBudget = 25 * time.Second

	clock := client.Clock.(*fakeClock)

	res, err := client.Do(context.Background(), api.Request{
		Op: outcome.OpExecuteTransfer, Body: []byte(`{}`), IdempotencyKey: "k",
	})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}

	// Two sleeps of ten seconds fit inside twenty-five; a third would take
	// the elapsed time to thirty.
	if len(clock.slept) != 2 {
		t.Errorf("slept %v, want two sleeps inside a 25-second budget", clock.slept)
	}

	if cap.count() != 3 {
		t.Errorf("the server saw %d requests, want 3", cap.count())
	}

	if res.Meta.Status != 503 {
		t.Errorf("final status: got %d", res.Meta.Status)
	}

	// And the last word is still classified, not swallowed.
	if got := outcome.Classify(res.Input(false, "")); got.Exit != 5 {
		t.Errorf("the answer after the budget classified %s/%d, want transient/5", got.Class, got.Exit)
	}
}

// The budget's floor. A server that answers `Retry-After: 0` under load is
// asking for a hot loop; §5.8's `max(retry_after_seconds, 1)` refuses.
func TestRetryFloorsTheDelayAtOneSecond(t *testing.T) {
	_, client := newServer(t, func(w http.ResponseWriter, r *http.Request, n int) {
		if n < 2 {
			w.Header().Set("Retry-After", "0")
			okJSON(w, 429, envelopeWith("RATE_LIMITED", 0))

			return
		}

		okJSON(w, 200, `{}`)
	})

	clock := client.Clock.(*fakeClock)

	if _, err := client.Do(context.Background(), api.Request{Op: outcome.OpGetMe}); err != nil {
		t.Fatalf("Do: %v", err)
	}

	if len(clock.slept) != 1 || clock.slept[0] != time.Second {
		t.Errorf("slept %v, want a single one-second sleep", clock.slept)
	}
}

// AC38 through the loop, not only through the predicate: a key mint that is
// rate-limited is answered once and returned, so the count is 1.
func TestKeyCreateIsNeverRetried(t *testing.T) {
	cap, client := newServer(t, func(w http.ResponseWriter, r *http.Request, n int) {
		w.Header().Set("Retry-After", "1")
		okJSON(w, 429, envelopeWith("RATE_LIMITED", 1))
	})

	res, err := client.Do(context.Background(), api.Request{
		Op:   outcome.OpCreateAPIKey,
		Body: []byte(`{"name":"n","environment":"sandbox","scopes":["read"]}`),
	})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}

	if cap.count() != 1 {
		t.Errorf("a key mint was sent %d times", cap.count())
	}

	if res.Attempts != 1 {
		t.Errorf("attempts: got %d, want 1", res.Attempts)
	}

	if len(client.Clock.(*fakeClock).slept) != 0 {
		t.Error("the loop slept before deciding not to retry a key mint")
	}
}

// CRITIQUE N5, through the loop.
//
// Driven on a read, and on a 599 that carries a retriable envelope, because
// those are the two cases where the rule is load-bearing. A 599 with no
// envelope on a money request is already not retried — an unreadable 5xx on
// the money path is not on §5.8's list — so an example built that way would
// pass with the rule deleted, which is a control that cannot fail.
func TestFixtureUnmatchedIsNeverRetried(t *testing.T) {
	t.Run("on a read, where an unknown 5xx would otherwise be resent", func(t *testing.T) {
		cap, client := newServer(t, func(w http.ResponseWriter, r *http.Request, n int) {
			w.WriteHeader(api.FixtureUnmatched)
			_, _ = io.WriteString(w, `no recording matched this request`)
		})

		res, err := client.Do(context.Background(), api.Request{Op: outcome.OpGetMe})
		if err != nil {
			t.Fatalf("Do: %v", err)
		}

		if cap.count() != 1 {
			t.Errorf("a 599 was sent %d times", cap.count())
		}

		if res.Attempts != 1 {
			t.Errorf("attempts: got %d, want 1", res.Attempts)
		}
	})

	t.Run("carrying an envelope whose code is retriable", func(t *testing.T) {
		cap, client := newServer(t, func(w http.ResponseWriter, r *http.Request, n int) {
			w.Header().Set("Retry-After", "1")
			okJSON(w, api.FixtureUnmatched, envelopeWith("SERVICE_UNAVAILABLE", 1))
		})

		res, err := client.Do(context.Background(), api.Request{
			Op: outcome.OpExecuteTransfer, Body: []byte(`{}`), IdempotencyKey: "k",
		})
		if err != nil {
			t.Fatalf("Do: %v", err)
		}

		if cap.count() != 1 {
			t.Errorf("a 599 with a retriable code was sent %d times", cap.count())
		}

		if res.Attempts != 1 {
			t.Errorf("attempts: got %d, want 1", res.Attempts)
		}
	})
}

// `UPSTREAM_BUSY` with a command is not resent, because FERRY is already
// retrying it and the caller's next move is to poll.
func TestUpstreamBusyWithACommandIsNotRetried(t *testing.T) {
	cap, client := newServer(t, func(w http.ResponseWriter, r *http.Request, n int) {
		w.Header().Set("Ferry-Command-Id", "cmd_busy")
		w.Header().Set("Retry-After", "3")
		okJSON(w, 503, envelopeWith("UPSTREAM_BUSY", 3))
	})

	res, err := client.Do(context.Background(), api.Request{
		Op: outcome.OpExecuteTransfer, Body: []byte(`{}`), IdempotencyKey: "k",
	})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}

	if cap.count() != 1 {
		t.Errorf("sent %d times, want 1", cap.count())
	}

	if got := outcome.Classify(res.Input(false, "")); got.Exit != 6 {
		t.Errorf("classified %s/%d, want pending/6", got.Class, got.Exit)
	}
}

// The same code without a command *is* resent, so the two halves of the split
// are both exercised through the loop rather than only through the predicate.
func TestUpstreamBusyWithoutACommandIsRetried(t *testing.T) {
	cap, client := newServer(t, func(w http.ResponseWriter, r *http.Request, n int) {
		if n < 2 {
			w.Header().Set("Retry-After", "1")
			okJSON(w, 503, envelopeWith("UPSTREAM_BUSY", 1))

			return
		}

		okJSON(w, 201, `{"object":"transaction","status":"submitted"}`)
	})

	res, err := client.Do(context.Background(), api.Request{
		Op: outcome.OpExecuteTransfer, Body: []byte(`{}`), IdempotencyKey: "k",
	})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}

	if cap.count() != 2 {
		t.Errorf("sent %d times, want 2", cap.count())
	}

	if res.Meta.Status != 201 {
		t.Errorf("final status: got %d", res.Meta.Status)
	}
}
