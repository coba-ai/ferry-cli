package api_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coba-ai/ferry-cli/internal/api"
	"github.com/coba-ai/ferry-cli/internal/outcome"
)

// The tests in this package drive a stdlib `httptest.Server` rather than
// `internal/fixture`. The fixture is U3's and does not exist yet, and these
// are unit tests of the client's own behaviour — what it puts on the wire and
// what it does with what comes back — for which a recording would add a file
// to keep in step without adding a claim. The recorded scenarios (AC30, AC76,
// AC77) test behaviour against real captured answers, which is a different
// question.

type capture struct {
	mu       sync.Mutex
	requests []recordedRequest
	handler  func(w http.ResponseWriter, r *http.Request, n int)
}

type recordedRequest struct {
	method string
	url    *url.URL
	header http.Header
	body   []byte
}

func newServer(t *testing.T, handler func(w http.ResponseWriter, r *http.Request, n int)) (*capture, *api.Client) {
	t.Helper()

	c := &capture{handler: handler}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)

		c.mu.Lock()
		c.requests = append(c.requests, recordedRequest{
			method: r.Method,
			url:    r.URL,
			header: r.Header.Clone(),
			body:   body,
		})
		n := len(c.requests)
		c.mu.Unlock()

		c.handler(w, r, n)
	}))

	t.Cleanup(srv.Close)

	return c, &api.Client{
		BaseURL:         srv.URL,
		Token:           "ferry_pat_CANARYtokenCANARY",
		UserAgentString: api.UserAgent("0.1.0", "abcdef0123456789"),
		Environment:     "sandbox",
		Clock:           &fakeClock{now: time.Unix(0, 0)},
	}
}

func (c *capture) at(t *testing.T, i int) recordedRequest {
	t.Helper()

	c.mu.Lock()
	defer c.mu.Unlock()

	if i >= len(c.requests) {
		t.Fatalf("wanted request %d; the server saw %d", i+1, len(c.requests))
	}

	return c.requests[i]
}

func (c *capture) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()

	return len(c.requests)
}

// fakeClock makes the retry budget a matter of arithmetic rather than of
// waiting. AC27's whole policy is about elapsed time, and a test that actually
// slept through a 60-second budget would be a test nobody runs.
type fakeClock struct {
	now    time.Time
	slept  []time.Duration
	frozen bool
}

func (f *fakeClock) Now() time.Time { return f.now }

func (f *fakeClock) Sleep(d time.Duration) {
	f.slept = append(f.slept, d)

	if !f.frozen {
		f.now = f.now.Add(d)
	}
}

func okJSON(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

func errorEnvelope(code string) string {
	b, _ := json.Marshal(map[string]any{
		"error": map[string]any{
			"code":                code,
			"message":             "refused",
			"retriable":           false,
			"retry_after_seconds": nil,
			"details":             nil,
			"request_id":          "req_1",
			"docs_url":            "https://example.test/errors",
		},
	})

	return string(b)
}

// AC15. Every request carries the four headers, and a request with a body
// carries `Content-Type` and the exact bytes it was given.
func TestRequestHeaders(t *testing.T) {
	cap, client := newServer(t, func(w http.ResponseWriter, r *http.Request, n int) {
		okJSON(w, 200, `{"object":"principal"}`)
	})

	if _, err := client.Do(context.Background(), api.Request{Op: outcome.OpGetMe}); err != nil {
		t.Fatalf("Do: %v", err)
	}

	got := cap.at(t, 0)

	if want := "Bearer ferry_pat_CANARYtokenCANARY"; got.header.Get("Authorization") != want {
		t.Errorf("Authorization: got %q, want %q", got.header.Get("Authorization"), want)
	}

	if got.header.Get("Accept") != "application/json" {
		t.Errorf("Accept: got %q", got.header.Get("Accept"))
	}

	// The shape, not the exact string: the version and the contract sha are
	// values this package does not own, but the shape is what a server-side
	// log has to be able to parse.
	ua := got.header.Get("User-Agent")

	uaRE := regexp.MustCompile(`^ferry-cli/\S+ \(\w+/\w+; go\S+\) contract/[0-9a-f]{8}$`)
	if !uaRE.MatchString(ua) {
		t.Errorf("User-Agent %q does not match AC15's shape", ua)
	}

	// A GET has no body, so it must not claim a content type.
	if ct := got.header.Get("Content-Type"); ct != "" {
		t.Errorf("a bodyless GET sent Content-Type: %q", ct)
	}
}

// AC15's second half, and M13's target. The bytes are carried, not rebuilt.
//
// The body here is deliberately not what `json.Marshal` of a Go map would
// produce — the keys are out of alphabetical order and there is incidental
// whitespace — so a client that re-marshals cannot accidentally produce the
// same bytes. That matters because the idempotency digest is over the bytes: a
// second attempt whose body was rebuilt is `IDEMPOTENCY_KEY_REUSED`, and by
// AC71 that lands on exit 3 telling the caller to fix a body they never
// changed.
func TestBodyIsSentByteForByte(t *testing.T) {
	body := []byte(`{"plan_token":  "ferry_plan_aaa", "zeta":1, "alpha":2}`)

	cap, client := newServer(t, func(w http.ResponseWriter, r *http.Request, n int) {
		okJSON(w, 201, `{"object":"transaction","status":"submitted"}`)
	})

	_, err := client.Do(context.Background(), api.Request{
		Op:             outcome.OpExecuteTransfer,
		Body:           body,
		IdempotencyKey: "key-1",
	})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}

	got := cap.at(t, 0)

	if string(got.body) != string(body) {
		t.Errorf("body was not sent byte-for-byte:\n  sent: %s\n  gave: %s", got.body, body)
	}

	if got.header.Get("Content-Type") != "application/json" {
		t.Errorf("a request with a body sent Content-Type %q", got.header.Get("Content-Type"))
	}

	if got.header.Get("Idempotency-Key") != "key-1" {
		t.Errorf("Idempotency-Key: got %q", got.header.Get("Idempotency-Key"))
	}
}

// AC16, enforced in both directions before the wire.
//
// The "no key on a money route" half is the one with teeth. A simulate or
// execute sent without a key has no replay protection at all, and the server
// answers `IDEMPOTENCY_KEY_REQUIRED` — by which time the CLI has proven it was
// willing to send it.
func TestIdempotencyKeyIsRequiredOnExactlyTheContractsOperations(t *testing.T) {
	cap, client := newServer(t, func(w http.ResponseWriter, r *http.Request, n int) {
		okJSON(w, 200, `{}`)
	})

	for _, route := range api.Routes {
		t.Run(string(route.Op), func(t *testing.T) {
			params := map[string]string{}
			if strings.Contains(route.Path, "{id}") {
				params["id"] = "x_1"
			}

			// Without a key.
			_, err := client.Do(context.Background(), api.Request{Op: route.Op, PathParams: params})

			if route.Idempotent && err == nil {
				t.Error("sent an idempotent operation with no Idempotency-Key")
			}

			if !route.Idempotent && err != nil {
				t.Errorf("refused a non-idempotent operation that carried no key: %v", err)
			}

			// With one.
			_, err = client.Do(context.Background(), api.Request{
				Op: route.Op, PathParams: params, IdempotencyKey: "k",
			})

			if !route.Idempotent && err == nil {
				t.Error("sent an Idempotency-Key on an operation the contract does not declare one for")
			}

			if route.Idempotent && err != nil {
				t.Errorf("refused an idempotent operation that carried a key: %v", err)
			}
		})
	}

	// Non-vacuity: the loop above must have actually sent something, or
	// every branch of it was an error path.
	if cap.count() == 0 {
		t.Error("no request reached the server; the loop proved nothing")
	}
}

// A refused request never reaches the wire. This is the claim that makes
// ErrUsage worth having: "we caught it" and "we sent it and the server caught
// it" are the same exit code and very different facts.
func TestRefusedRequestsAreNotSent(t *testing.T) {
	cap, client := newServer(t, func(w http.ResponseWriter, r *http.Request, n int) {
		okJSON(w, 200, `{}`)
	})

	refusals := []struct {
		name string
		req  api.Request
	}{
		{"an operation with no route", api.Request{Op: outcome.Operation("wallets_sweep")}},
		{"a money route with no key", api.Request{Op: outcome.OpExecuteTransfer}},
		{"a read route with a key", api.Request{Op: outcome.OpGetMe, IdempotencyKey: "k"}},
		{"a templated path with no parameter", api.Request{Op: outcome.OpGetCommand}},
		{"a templated path with an empty parameter", api.Request{
			Op: outcome.OpGetCommand, PathParams: map[string]string{"id": ""},
		}},
		{"a parameter the path does not have", api.Request{
			Op: outcome.OpGetMe, PathParams: map[string]string{"id": "1"},
		}},
	}

	for _, r := range refusals {
		t.Run(r.name, func(t *testing.T) {
			if _, err := client.Do(context.Background(), r.req); err == nil {
				t.Error("the request was accepted")
			}
		})
	}

	if n := cap.count(); n != 0 {
		t.Errorf("%d refused request(s) reached the server", n)
	}
}

// Path parameters are filled, and the templated path is what reaches the wire
// only after they are.
func TestPathExpansion(t *testing.T) {
	cap, client := newServer(t, func(w http.ResponseWriter, r *http.Request, n int) {
		okJSON(w, 200, `{}`)
	})

	_, err := client.Do(context.Background(), api.Request{
		Op:         outcome.OpGetCommand,
		PathParams: map[string]string{"id": "cmd_01HZ"},
		Query:      url.Values{"limit": []string{"20"}},
	})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}

	got := cap.at(t, 0)

	if got.url.Path != "/v1/commands/cmd_01HZ" {
		t.Errorf("path: got %q", got.url.Path)
	}

	if got.url.Query().Get("limit") != "20" {
		t.Errorf("query: got %q", got.url.RawQuery)
	}
}

// CS-13. A `poll` path that is absolute would let a response body send the
// CLI — carrying its `Authorization` header — to a host the profile never
// named.
func TestPollPathsMustBeRelative(t *testing.T) {
	client := &api.Client{BaseURL: "https://api.example.test"}

	for _, poll := range []string{
		"https://evil.test/v1/commands/cmd_1",
		"http://evil.test/v1/commands/cmd_1",
		"//evil.test/v1/commands/cmd_1",
	} {
		if _, err := client.ResolvePollPath(poll); err == nil {
			t.Errorf("followed the absolute poll path %q", poll)
		}
	}

	got, err := client.ResolvePollPath("/v1/commands/cmd_1")
	if err != nil {
		t.Fatalf("a relative poll path was refused: %v", err)
	}

	if want := "https://api.example.test/v1/commands/cmd_1"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// The seam into the decision table: a real answer becomes a real Input, with
// nothing dropped on the way.
func TestResultInputCarriesTheAnswersFacts(t *testing.T) {
	_, client := newServer(t, func(w http.ResponseWriter, r *http.Request, n int) {
		w.Header().Set("Ferry-Command-Id", "cmd_7")
		w.Header().Set("Idempotency-Replayed", "true")
		okJSON(w, 409, errorEnvelope("IDEMPOTENCY_KEY_IN_PROGRESS"))
	})

	res, err := client.Do(context.Background(), api.Request{
		Op: outcome.OpExecuteTransfer, Body: []byte(`{}`), IdempotencyKey: "k",
	})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}

	in := res.Input(false, "")

	if in.Op != outcome.OpExecuteTransfer {
		t.Errorf("op: got %q", in.Op)
	}

	if in.Status != 409 {
		t.Errorf("status: got %d", in.Status)
	}

	if in.Code() != "IDEMPOTENCY_KEY_IN_PROGRESS" {
		t.Errorf("code: got %q", in.Code())
	}

	if in.CommandID != "cmd_7" {
		t.Errorf("command id: got %q", in.CommandID)
	}

	if !in.Replayed {
		t.Error("the Idempotency-Replayed header was lost")
	}

	// And the whole round trip lands where AC20 says it does.
	if got := outcome.Classify(in); got.Exit != 6 {
		t.Errorf("a real 409 in-progress classified %s/%d, want pending/6", got.Class, got.Exit)
	}
}

// `details.command_id` stands in for the header when the header is absent, so
// AC71's "`next` naming `details.command_id`" holds for an answer that carries
// only the detail.
func TestCommandIDFallsBackToTheDetail(t *testing.T) {
	_, client := newServer(t, func(w http.ResponseWriter, r *http.Request, n int) {
		body, _ := json.Marshal(map[string]any{
			"error": map[string]any{
				"code": "IDEMPOTENCY_KEY_REUSED", "message": "m", "retriable": false,
				"retry_after_seconds": nil,
				"details":             map[string]any{"reason": "different_credential", "command_id": "cmd_detail"},
				"request_id":          "req", "docs_url": "d",
			},
		})

		okJSON(w, 409, string(body))
	})

	res, err := client.Do(context.Background(), api.Request{
		Op: outcome.OpExecuteTransfer, Body: []byte(`{}`), IdempotencyKey: "k",
	})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}

	in := res.Input(false, "")

	if in.CommandID != "cmd_detail" {
		t.Errorf("command id: got %q, want the value from details", in.CommandID)
	}

	got := outcome.Classify(in)

	if got.Exit != 7 {
		t.Errorf("classified %s/%d, want escalate/7", got.Class, got.Exit)
	}

	if !strings.Contains(got.Next, "cmd_detail") {
		t.Errorf("next does not name details.command_id:\n  %s", got.Next)
	}
}
