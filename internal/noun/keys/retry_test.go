package keys_test

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kurenn/ferry/cli/internal/api"
	"github.com/kurenn/ferry/cli/internal/noun"
)

// ---------------------------------------------------------------------------
// AC38 — `keys create` is never auto-retried, and a transport failure after
// the write is exit 6.
// ---------------------------------------------------------------------------

// deadServer accepts a request, reads it whole, and hangs up without
// answering.
//
// The hijack is what makes the phase right. `writeWatch` moves to
// `PhaseAfterWrite` when `httptrace` reports the request bytes written, and
// the handler only runs after that, so closing here is a failure the client
// can only classify as "the request may have been performed" — which is the
// exact situation AC38 exists for. A fixture cannot produce it: it answers
// every request it receives.
func deadServer(t *testing.T, hits *atomic.Int64) *httptest.Server {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)

		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)

			return
		}

		_ = conn.(*net.TCPConn).SetLinger(0)
		_ = conn.Close()
	}))

	t.Cleanup(server.Close)

	return server
}

// countingClock records every sleep the retry loop asks for without taking it.
//
// A test that let the real clock run would be asserting "no retry" by being
// fast, which is a race. This asserts it by the loop never asking to wait.
type countingClock struct {
	now    time.Time
	sleeps []time.Duration
}

func (c *countingClock) Now() time.Time { return c.now }

func (c *countingClock) Sleep(d time.Duration) {
	c.sleeps = append(c.sleeps, d)
	c.now = c.now.Add(d)
}

// One attempt, exit 6, and the sentence that tells the operator what to do
// before minting again.
//
// §6.2 names no mutation for AC38; the one this unit ran (U4-1 in the PR body)
// removes the `control_create` arm of `api.Retryable`. The attempt count below
// is what reddens.
func TestCreateIsNeverRetriedAfterTheRequestIsWritten(t *testing.T) {
	var hits atomic.Int64

	server := deadServer(t, &hits)
	home := newHome(t)

	writeProfile(t, home, server.URL, canaryPAT(t))

	clock := &countingClock{now: fixedNow}

	stdout, stderr, exit := run(t, invocation{
		home: home,
		args: []string{
			"create", "--api", server.URL, "--env", "sandbox",
			"--name", "cli-recorded-key", "--scopes", "read",
		},
		deps: func(d *noun.Deps) { d.Clock = clock },
	})

	if exit != 6 {
		t.Fatalf("exit %d, want 6 (pending: the request may have been performed)\n"+
			"stdout: %s\nstderr: %s", exit, stdout, stderr)
	}

	if got := hits.Load(); got != 1 {
		t.Errorf("the server was hit %d times. AC38: an unanswered mint may have minted a key "+
			"whose token this CLI no longer holds, and a second attempt mints a second one.", got)
	}

	if len(clock.sleeps) != 0 {
		t.Errorf("the retry loop waited %v before giving up; it should not have considered a retry at all",
			clock.sleeps)
	}

	if !strings.Contains(stderr, "ferry keys list") {
		t.Errorf("the message does not say to check before minting again:\n%s", stderr)
	}

	if !strings.Contains(stderr, "may have been minted") {
		t.Errorf("the message does not say the key may exist:\n%s", stderr)
	}
}

// unavailableServer answers a retryable 503 to everything, forever.
//
// The envelope carries all seven keys `DecodeEnvelope` requires: six of them
// and the CLI reads the answer as a body it cannot understand, which is a
// different branch of the table and would make this test measure something
// else.
func unavailableServer(t *testing.T, hits *atomic.Int64) *httptest.Server {
	t.Helper()

	const envelope = `{"error":{"code":"SERVICE_UNAVAILABLE",` +
		`"message":"FERRY is briefly unavailable.","retriable":true,"retry_after_seconds":1,` +
		`"details":null,"request_id":"req_test_1",` +
		`"docs_url":"https://docs.ferry.example/errors/SERVICE_UNAVAILABLE"}}`

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusServiceUnavailable)

		_, _ = w.Write([]byte(envelope))
	}))

	t.Cleanup(server.Close)

	return server
}

// The control that stops the test above from being vacuous, and the sharper
// statement of AC38 besides.
//
// `api.Client.Do` returns a transport fault without resending whatever the
// operation is, so "one attempt against a dead server" is true of every
// command and says nothing about `create` on its own. A retryable *status* is
// where the operation class is actually consulted: `Retryable` returns false
// for `control_create` at any status and true for a read at a 503. Both halves
// run against the same server, the same client and the same clock, and only
// the command differs.
//
// U4-1 removes the `control_create` arm of `api.Retryable`; the create half
// below is what reddens, and the read half is what proves the loop was alive.
func TestARetryableStatusIsRetriedForAReadAndNeverForACreate(t *testing.T) {
	t.Run("a read is retried", func(t *testing.T) {
		var hits atomic.Int64

		server := unavailableServer(t, &hits)
		home := newHome(t)

		writeProfile(t, home, server.URL, canaryPAT(t))

		clock := &countingClock{now: fixedNow}

		_, stderr, exit := run(t, invocation{
			home: home,
			args: []string{"list", "--api", server.URL, "--retry-budget", "5s"},
			deps: func(d *noun.Deps) { d.Clock = clock },
		})

		if exit == 0 {
			t.Fatalf("exit 0 against a server that only answers 503\nstderr: %s", stderr)
		}

		if got := hits.Load(); got < 2 {
			t.Errorf("the server was hit %d times; a 503 on a read is worth another try, and if the "+
				"loop never fired, the create half below would be true of every command", got)
		}

		if len(clock.sleeps) == 0 {
			t.Error("the retry loop never waited, so the budget was not exercised")
		}
	})

	t.Run("a create is not", func(t *testing.T) {
		var hits atomic.Int64

		server := unavailableServer(t, &hits)
		home := newHome(t)

		writeProfile(t, home, server.URL, canaryPAT(t))

		clock := &countingClock{now: fixedNow}

		_, stderr, exit := run(t, invocation{
			home: home,
			args: []string{
				"create", "--api", server.URL, "--env", "sandbox",
				"--name", "k", "--scopes", "read", "--retry-budget", "5s",
			},
			deps: func(d *noun.Deps) { d.Clock = clock },
		})

		if exit == 0 {
			t.Fatalf("exit 0 against a server that only answers 503\nstderr: %s", stderr)
		}

		if got := hits.Load(); got != 1 {
			t.Errorf("the server was hit %d times, want 1. AC38: a 503 is an answer FERRY may have "+
				"produced after minting, and a resend mints a second key.", got)
		}

		if len(clock.sleeps) != 0 {
			t.Errorf("the retry loop waited %v before a mint it must not repeat", clock.sleeps)
		}
	})
}

// And the other half of AC38's split: a connection that was never established
// minted nothing, so it is exit 5 and not exit 6.
//
// The distinction is the whole reason `api/transport.go` exists. Collapsing
// the two phases into one code would tell an operator to go and check for a
// key that a refused TCP connect could not possibly have created.
func TestACreateThatNeverConnectedIsTransientRatherThanPending(t *testing.T) {
	// A port nothing is listening on: the listener is opened to reserve a
	// port that is certainly free, then closed.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	addr := listener.Addr().String()

	if err := listener.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	home := newHome(t)
	url := "http://" + addr

	writeProfile(t, home, url, canaryPAT(t))

	clock := &countingClock{now: fixedNow}

	_, stderr, exit := run(t, invocation{
		home: home,
		args: []string{
			"create", "--api", url, "--env", "sandbox",
			"--name", "k", "--scopes", "read", "--retry-budget", "1s",
		},
		deps: func(d *noun.Deps) { d.Clock = clock },
	})

	if exit != 5 {
		t.Fatalf("exit %d, want 5 (transient: nothing left this host)\nstderr: %s", exit, stderr)
	}

	if !strings.Contains(stderr, "no key was minted") {
		t.Errorf("the message does not say the mint did not happen:\n%s", stderr)
	}
}

var _ api.Clock = (*countingClock)(nil)
