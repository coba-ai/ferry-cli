package api_test

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coba-ai/ferry-cli/internal/api"
	"github.com/coba-ai/ferry-cli/internal/outcome"
)

// AC23 and C3. The split is by whether bytes may have left the host, and the
// client measures it rather than reading it off an error string.
//
// These two examples are the reason the `httptrace` hook exists. Both fail
// with a context or connection error; one of them has put a money request on
// the wire and the other has not; and no error value distinguishes them. Get
// this wrong in the safe direction and a caller waits; wrong in the other and
// a caller is told "nothing was sent" about a transfer FERRY may already have
// sent upstream.

// A connection that is never established. The address is in 192.0.2.0/24 —
// TEST-NET-1, reserved by RFC 5737 and routable nowhere — so this does not
// depend on a name server or on what is listening locally.
func TestDialFailureIsClassifiedAsNothingSent(t *testing.T) {
	client := &api.Client{
		BaseURL: "http://192.0.2.1:9",
		Clock:   &fakeClock{now: time.Unix(0, 0)},
		HTTP: &http.Client{
			Transport: &http.Transport{
				DialContext: (&net.Dialer{Timeout: 50 * time.Millisecond}).DialContext,
			},
		},
	}

	res, err := client.Do(context.Background(), api.Request{
		Op: outcome.OpExecuteTransfer, Body: []byte(`{}`), IdempotencyKey: "k",
	})
	if err != nil {
		t.Fatalf("Do returned a usage error: %v", err)
	}

	if res.Transport == nil {
		t.Fatal("a dial failure produced no transport fault")
	}

	if res.Transport.Phase != outcome.PhaseDial {
		t.Errorf("phase: got %q, want %q", res.Transport.Phase, outcome.PhaseDial)
	}

	got := outcome.Classify(res.Input(false, ""))

	if got.Class != outcome.ClassTransient || got.Exit != 5 {
		t.Errorf("a dial failure on execute classified %s/%d, want transient/5", got.Class, got.Exit)
	}

	if !got.SameKeySafe {
		t.Error("nothing left the host, so the same key must be safe to resend")
	}
}

// A connection that is established, a request that is written, and a server
// that then goes away without answering.
//
// This is the case `Executor#send_and_record` makes dangerous: FERRY sends
// upstream before the caller sees a byte, so a request that reached the server
// may have moved money even though the caller has nothing to read.
func TestFailureAfterTheRequestWasWrittenIsClassifiedAsUnknown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)

		// Hijack and close without writing a response: the request is on
		// the wire, the answer never comes.
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijacking: %v", err)

			return
		}

		_ = conn.Close()
	}))
	defer srv.Close()

	client := &api.Client{
		BaseURL: srv.URL,
		Clock:   &fakeClock{now: time.Unix(0, 0)},
	}

	res, err := client.Do(context.Background(), api.Request{
		Op: outcome.OpExecuteTransfer, Body: []byte(`{"plan_token":"ferry_plan_x"}`), IdempotencyKey: "k",
	})
	if err != nil {
		t.Fatalf("Do returned a usage error: %v", err)
	}

	if res.Transport == nil {
		t.Fatal("a dropped connection after the write produced no transport fault")
	}

	if res.Transport.Phase != outcome.PhaseAfterWrite {
		t.Errorf("phase: got %q, want %q", res.Transport.Phase, outcome.PhaseAfterWrite)
	}

	got := outcome.Classify(res.Input(false, ""))

	if got.Class != outcome.ClassPending || got.Exit != 6 {
		t.Errorf("an after-write failure on execute classified %s/%d, want pending/6", got.Class, got.Exit)
	}

	if got.Money != outcome.MoneyUnknown {
		t.Errorf("money: got %q, want unknown", got.Money)
	}
}

// A transport fault is never resent by the retry loop, at any phase.
//
// Whether anything left the host is exactly what a money request cannot
// establish, and resending is the caller's decision to make through `runs
// resume` — with the run ledger in front of them — not this loop's to make
// silently.
func TestTransportFaultsAreNotRetried(t *testing.T) {
	clock := &fakeClock{now: time.Unix(0, 0)}

	client := &api.Client{
		BaseURL: "http://192.0.2.1:9",
		Clock:   clock,
		HTTP: &http.Client{
			Transport: &http.Transport{
				DialContext: (&net.Dialer{Timeout: 50 * time.Millisecond}).DialContext,
			},
		},
	}

	res, err := client.Do(context.Background(), api.Request{
		Op: outcome.OpExecuteTransfer, Body: []byte(`{}`), IdempotencyKey: "k",
	})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}

	if res.Attempts != 1 {
		t.Errorf("attempts: got %d, want 1", res.Attempts)
	}

	if len(clock.slept) != 0 {
		t.Errorf("the loop slept %v before giving up on a transport fault", clock.slept)
	}
}

// A truncated response body is an after-write fault, not a 200 with a short
// body. Bytes went both ways, so the request certainly reached the server.
func TestTruncatedResponseBodyIsAnAfterWriteFault(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1000")
		w.WriteHeader(201)
		_, _ = io.WriteString(w, `{"object":"tra`)

		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}

		_ = conn.Close()
	}))
	defer srv.Close()

	client := &api.Client{BaseURL: srv.URL, Clock: &fakeClock{now: time.Unix(0, 0)}}

	res, err := client.Do(context.Background(), api.Request{
		Op: outcome.OpExecuteTransfer, Body: []byte(`{}`), IdempotencyKey: "k",
	})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}

	if res.Transport == nil || res.Transport.Phase != outcome.PhaseAfterWrite {
		t.Fatalf("transport: got %+v, want an after-write fault", res.Transport)
	}

	if got := outcome.Classify(res.Input(false, "")); got.Exit != 6 {
		t.Errorf("classified %s/%d, want pending/6", got.Class, got.Exit)
	}
}
