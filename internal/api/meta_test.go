package api_test

import (
	"net/http"
	"testing"

	"github.com/coba-ai/ferry-cli/internal/api"
)

// AC18. The six headers, and the one that must not be read as a number when it
// is absent.

func TestReadMetaReadsTheSixHeaders(t *testing.T) {
	h := http.Header{}
	h.Set("Ferry-Command-Id", "cmd_1")
	h.Set("Ferry-Environment", "sandbox")
	h.Set("Idempotency-Key", "key-1")
	h.Set("Idempotency-Replayed", "true")
	h.Set("Retry-After", "7")
	h.Set("WWW-Authenticate", `Bearer realm="ferry"`)

	m := api.ReadMeta(409, h, "sandbox")

	if m.Status != 409 {
		t.Errorf("status: got %d", m.Status)
	}

	if m.CommandID != "cmd_1" {
		t.Errorf("command id: got %q", m.CommandID)
	}

	if m.Environment != "sandbox" {
		t.Errorf("environment: got %q", m.Environment)
	}

	if m.IdempotencyKey != "key-1" {
		t.Errorf("idempotency key: got %q", m.IdempotencyKey)
	}

	if !m.Replayed {
		t.Error("Idempotency-Replayed: true was not read")
	}

	if m.RetryAfter == nil || *m.RetryAfter != 7 {
		t.Errorf("retry-after: got %v", m.RetryAfter)
	}

	if m.WWWAuthenticate != `Bearer realm="ferry"` {
		t.Errorf("www-authenticate: got %q", m.WWWAuthenticate)
	}

	if m.EnvironmentMismatch {
		t.Error("a matching environment was reported as a mismatch")
	}
}

// An absent `Retry-After` is nil, not zero.
//
// The two are different instructions. Nil means "the server said nothing", and
// §5.8's `max(retry_after_seconds, 1)` then waits a second; zero read as a
// number means "immediately", and a client that resends immediately against a
// rate limiter is a client that stays rate-limited.
func TestAbsentRetryAfterIsNil(t *testing.T) {
	m := api.ReadMeta(429, http.Header{}, "")

	if m.RetryAfter != nil {
		t.Errorf("an absent Retry-After became %v", *m.RetryAfter)
	}

	if got := m.RetryDelaySeconds(); got != 1 {
		t.Errorf("the delay for an absent Retry-After is %d seconds, want the 1-second floor", got)
	}
}

func TestRetryAfterFloorAndPassthrough(t *testing.T) {
	cases := []struct {
		header string
		want   int
	}{
		{"0", 1},   // A server asking for a hot loop is given the floor.
		{"1", 1},   //
		{"30", 30}, // Honoured as sent.
		{"-5", 1},  // Nonsense is floored rather than negated into an instant resend.
	}

	for _, c := range cases {
		h := http.Header{}
		h.Set("Retry-After", c.header)

		if got := api.ReadMeta(503, h, "").RetryDelaySeconds(); got != c.want {
			t.Errorf("Retry-After: %s gave a %d-second delay, want %d", c.header, got, c.want)
		}
	}
}

// A `Retry-After` this CLI cannot parse stays nil rather than becoming zero —
// the same reasoning as the absent case. RFC 7231 also permits an HTTP-date
// here; FERRY sends seconds, and a date would be a change worth waiting a
// second over rather than resending instantly over.
func TestUnparseableRetryAfterIsNil(t *testing.T) {
	h := http.Header{}
	h.Set("Retry-After", "Wed, 21 Oct 2026 07:28:00 GMT")

	m := api.ReadMeta(503, h, "")

	if m.RetryAfter != nil {
		t.Errorf("an unparseable Retry-After became %v", *m.RetryAfter)
	}

	if got := m.RetryDelaySeconds(); got != 1 {
		t.Errorf("delay: got %d, want the 1-second floor", got)
	}
}

// C9. The credential decides the environment, and the answer says which one it
// actually resolved to. A caller who believes they are on sandbox and is
// answered by live has moved real money before they read the body.
func TestEnvironmentMismatch(t *testing.T) {
	cases := []struct {
		name     string
		header   string
		expect   string
		mismatch bool
	}{
		{"live answer to a sandbox expectation", "live", "sandbox", true},
		{"sandbox answer to a live expectation", "sandbox", "live", true},
		{"agreement", "sandbox", "sandbox", false},
		// No expectation is not a disagreement: `ferry me` against a fresh
		// profile is how a caller finds out which environment they are on.
		{"no expectation", "live", "", false},
		// Nor is silence from the server: an answer with no
		// `Ferry-Environment` has not contradicted anything.
		{"no header", "", "sandbox", false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := http.Header{}
			if c.header != "" {
				h.Set("Ferry-Environment", c.header)
			}

			if got := api.ReadMeta(200, h, c.expect).EnvironmentMismatch; got != c.mismatch {
				t.Errorf("mismatch: got %v, want %v", got, c.mismatch)
			}
		})
	}
}

// `Idempotency-Replayed` is read as the literal `true` the contract declares,
// and not as "any value at all". A header whose value is `false` says the
// answer was fresh.
func TestReplayedIsReadStrictly(t *testing.T) {
	for _, c := range []struct {
		value string
		want  bool
	}{
		{"true", true},
		{"false", false},
		{"", false},
	} {
		h := http.Header{}
		if c.value != "" {
			h.Set("Idempotency-Replayed", c.value)
		}

		if got := api.ReadMeta(201, h, "").Replayed; got != c.want {
			t.Errorf("Idempotency-Replayed: %q gave %v, want %v", c.value, got, c.want)
		}
	}
}
