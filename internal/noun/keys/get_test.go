package keys_test

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/kurenn/ferry-cli/internal/fixture"
	"github.com/kurenn/ferry-cli/internal/render"
)

func TestGetReadsOneKeyAndShowsNoToken(t *testing.T) {
	server, home := loggedIn(t, "keys.get.200")

	stdout, stderr, exit := run(t, invocation{
		home: home,
		args: []string{"get", "key_PLACEHOLDER_1", "--api", server.URL()},
	})

	if exit != 0 {
		t.Fatalf("exit %d, want 0\nstdout: %s\nstderr: %s", exit, stdout, stderr)
	}

	req := server.Requests()[0]

	if req.Method != "GET" || req.Path != "/v1/api_keys/key_PLACEHOLDER_1" {
		t.Errorf("get sent %s %s", req.Method, req.Path)
	}

	if !strings.Contains(stdout, "key_PLACEHOLDER_1") {
		t.Errorf("the key's id is not shown:\n%s", stdout)
	}

	// The 200 of `GET /v1/api_keys/{id}` carries no token, and neither does
	// the render. This is one of the paths AC42's "exactly two" excludes.
	if found := render.FindSecrets(stdout + stderr); len(found) > 0 {
		t.Errorf("`keys get` printed secret-shaped text %q", found)
	}
}

func TestGetWithoutAnIDIsAUsageError(t *testing.T) {
	server, home := loggedIn(t, "keys.get.200")

	_, stderr, exit := run(t, invocation{home: home, args: []string{"get", "--api", server.URL()}})

	if exit != 2 {
		t.Errorf("exit %d, want 2", exit)
	}

	if n := requestCount(server); n != 0 {
		t.Errorf("the fixture saw %d requests", n)
	}

	if !strings.Contains(stderr, "exactly 1 argument") {
		t.Errorf("the refusal reads %q", stderr)
	}
}

// A 404 is FERRY's answer and is reported as one: the http and error blocks
// are both present, and the code is the code FERRY sent.
func TestAnUnknownKeyIsReportedWithFERRYsOwnCode(t *testing.T) {
	server := fixture.New(t, "keys.get.404")
	home := newHome(t)

	writeProfile(t, home, server.URL(), canaryPAT(t))

	stdout, stderr, exit := run(t, invocation{
		home: home,
		args: []string{"get", "key_PLACEHOLDER_1", "--api", server.URL(), "--output", "json"},
	})

	if exit == 0 {
		t.Fatalf("exit 0 on a 404\nstdout: %s\nstderr: %s", stdout, stderr)
	}

	doc := decode(t, stdout)

	if doc.HTTP == nil {
		t.Error("the document carries no http block for an answer that did arrive")
	}

	if len(doc.Error) == 0 || string(doc.Error) == "null" {
		t.Fatalf("the document carries no error block:\n%s", stdout)
	}

	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(doc.Error, &envelope); err != nil {
		t.Fatalf("parse the error block: %v", err)
	}

	// The envelope is lifted verbatim, so all seven keys survive — including
	// `details`, which is still `type: object` in the contract (A321) and
	// would lose whatever a Go struct here did not name.
	got := make([]string, 0, len(envelope))
	for k := range envelope {
		got = append(got, k)
	}

	sort.Strings(got)

	want := []string{"code", "details", "docs_url", "message", "request_id", "retriable", "retry_after_seconds"}

	if !reflect.DeepEqual(got, want) {
		t.Errorf("the error block carries\n  %v\nand the envelope has\n  %v", got, want)
	}

	if doc.Outcome.ExitCode != exit {
		t.Errorf("the document says exit %d and the process exited %d; a program reading stdout "+
			"and a shell reading $? must not be told different things",
			doc.Outcome.ExitCode, exit)
	}
}

// `reason` is optional in `RevokeApiKeyRequest` and is omitted when the caller
// gave none: `{"reason": ""}` would record an empty reason as though one had
// been given.
//
// Only the with-a-reason row can be answered — U0 recorded
// `{"reason": "rotated"}` and the fixture matches structurally — so the
// no-reason row is asserted on the bytes the client sent, which the fixture
// logs whether or not a recording matched (AC31).
func TestRevokeSendsTheReasonOnlyWhenGiven(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want []string
	}{
		{"without a reason", nil, nil},
		{"with a reason", []string{"--reason", "rotated"}, []string{"reason"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server, home := loggedIn(t, "keys.revoke.200")

			args := append([]string{"revoke", "key_PLACEHOLDER_1", "--api", server.URL()}, tc.args...)

			run(t, invocation{home: home, args: args})

			if n := requestCount(server); n != 1 {
				t.Fatalf("the fixture saw %d requests, want 1", n)
			}

			req := server.Requests()[0]

			if req.Method != "POST" {
				t.Errorf("revoke sent %s, want POST", req.Method)
			}

			var fields map[string]json.RawMessage
			if err := json.Unmarshal(req.Body, &fields); err != nil {
				t.Fatalf("parse body: %v\n%s", err, req.Body)
			}

			got := make([]string, 0, len(fields))
			for k := range fields {
				got = append(got, k)
			}

			sort.Strings(got)

			want := tc.want
			if want == nil {
				want = []string{}
			}

			if len(got) == 0 {
				got = []string{}
			}

			if !reflect.DeepEqual(got, want) {
				t.Errorf("the revoke body carries %v, want %v (body: %s)", got, want, req.Body)
			}
		})
	}
}

// Revocation is idempotent, so repeating it is a 200 and not an error. The
// recording is the second call FERRY answered.
func TestRevokingAnAlreadyRevokedKeyIsNotAnError(t *testing.T) {
	server := fixture.New(t, "keys.revoke.again.200")
	home := newHome(t)

	writeProfile(t, home, server.URL(), canaryPAT(t))

	stdout, stderr, exit := run(t, invocation{
		home: home,
		args: []string{"revoke", "key_PLACEHOLDER_1", "--api", server.URL(), "--reason", "rotated again"},
	})

	if exit != 0 {
		t.Fatalf("exit %d, want 0\nstdout: %s\nstderr: %s", exit, stdout, stderr)
	}

	if !strings.Contains(stdout, "revoked") {
		t.Errorf("the output does not say the key is revoked:\n%s", stdout)
	}
}
