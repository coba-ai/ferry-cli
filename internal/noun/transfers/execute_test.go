package transfers_test

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// AC55 — `transfers execute --plan <token|->` sends exactly
// `{"plan_token": "…"}`.
//
// "Exactly" is the whole of it, and it defends C5. The execute endpoint
// takes the plan token and nothing else: the amount, the corridor and the
// parties were fixed when the plan was minted and priced, and anything
// alongside the token is a second statement of intent the server would have
// to reconcile with the first. A body that also carried `amount` would be a
// request whose two halves could disagree — and the half the server honours
// is the one that decides how much money moves.
//
// The assertion is on the **bytes the fixture received**, and A345 is the
// reason to say where it sits. U4 ran the same mutation against two layers
// and it survived green at the HTTP layer while the CLI had stopped
// encoding entirely. Here the direction is the other one: an assertion on
// the Go value handed to the encoder would survive a change *in* the
// encoder, and this claim is about what leaves the process.
func TestExecuteSendsThePlanTokenAndNothingElse(t *testing.T) {
	server, home := loggedIn(t, "execute.201.processing")

	stdout, stderr, exit := run(t, invocation{
		home: home,
		args: []string{"transfers", "execute", "--plan", recordedPlanToken, "--yes"},
	})

	if exit != 0 {
		t.Fatalf("exit = %d, want 0\n%s\n%s", exit, stdout, stderr)
	}

	if n := requestsTo(server, executePath); n != 1 {
		t.Fatalf("%d requests to %s, want 1", n, executePath)
	}

	assertExactlyThePlanToken(t, bodyOfRequestTo(t, server, executePath, 0), recordedPlanToken)
}

// The same claim for `--plan -`, which reads the token from stdin.
//
// A separate row because it is a separate path into the builder, and
// because of the newline. `echo $TOKEN | ferry transfers execute --plan -`
// is how a human will drive this, and a `\n` that survived into the JSON
// string would be a token the server rejects for a reason that looks
// nothing like whitespace.
func TestThePlanTokenCanComeFromStdinAndIsStillTheWholeBody(t *testing.T) {
	server, home := loggedIn(t, "execute.201.processing")

	stdout, stderr, exit := run(t, invocation{
		home:  home,
		args:  []string{"transfers", "execute", "--plan", "-", "--yes"},
		stdin: recordedPlanToken + "\n",
	})

	if exit != 0 {
		t.Fatalf("exit = %d, want 0\n%s\n%s", exit, stdout, stderr)
	}

	if n := requestsTo(server, executePath); n != 1 {
		t.Fatalf("%d requests to %s, want 1", n, executePath)
	}

	assertExactlyThePlanToken(t, bodyOfRequestTo(t, server, executePath, 0), recordedPlanToken)
}

// The floor under both rows above.
//
// `assertExactlyThePlanToken` compares two sets, and a set comparison is
// satisfied by two empty sets. This drives bodies that are *not* exactly
// the plan token through the same function and requires it to complain, so
// a pass above is the encoder's doing and not the assertion's.
func TestTheExactBodyAssertionRejectsAnythingElse(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{
			name: "an extra key",
			body: `{"plan_token":"pln_x","amount":"100.00"}`,
			want: "amount",
		},
		{
			name: "the wrong key",
			body: `{"token":"pln_x"}`,
			want: "plan_token",
		},
		{
			name: "the right key and the wrong token",
			body: `{"plan_token":"pln_y"}`,
			want: "pln_y",
		},
		{
			name: "an empty object",
			body: `{}`,
			want: "plan_token",
		},
		{
			name: "not an object at all",
			body: `"pln_x"`,
			want: "not a JSON object",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			spy := &recorder{}
			assertExactlyThePlanToken(spy, tc.body, "pln_x")

			if len(spy.messages) == 0 {
				t.Errorf("the assertion accepted %s, so it would accept it on the wire", tc.body)

				return
			}

			if !strings.Contains(strings.Join(spy.messages, "\n"), tc.want) {
				t.Errorf("the complaint does not name %q:\n%s",
					tc.want, strings.Join(spy.messages, "\n"))
			}
		})
	}

	// And the other half of the floor: the correct body passes. Without
	// this row the function could reject everything and the five above
	// would still be green.
	spy := &recorder{}
	assertExactlyThePlanToken(spy, `{"plan_token":"pln_x"}`, "pln_x")

	if len(spy.messages) != 0 {
		t.Errorf("the assertion rejected the correct body:\n%s", strings.Join(spy.messages, "\n"))
	}
}

// reporter is the part of *testing.T this assertion needs, so the floor can
// watch it complain without failing.
type reporter interface {
	Helper()
	Errorf(format string, args ...any)
}

type recorder struct{ messages []string }

func (r *recorder) Helper() {}

func (r *recorder) Errorf(format string, args ...any) {
	r.messages = append(r.messages, strings.TrimSpace(fmt.Sprintf(format, args...)))
}

func assertExactlyThePlanToken(t reporter, body, token string) {
	t.Helper()

	var got map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Errorf("the execute body is not a JSON object: %v\n%s", err, body)

		return
	}

	keys := make([]string, 0, len(got))
	for key := range got {
		keys = append(keys, key)
	}

	sort.Strings(keys)

	// Both directions at once: equality of the key set, not containment
	// either way. "plan_token is present" passes on the body this
	// criterion exists to forbid, and "every key is plan_token" passes on
	// an empty body.
	if !reflect.DeepEqual(keys, []string{"plan_token"}) {
		t.Errorf("the execute body's keys are %v, want exactly [plan_token]. Anything else "+
			"is a second statement of intent alongside the plan the server already "+
			"priced (C5).", keys)

		return
	}

	var sent string
	if err := json.Unmarshal(got["plan_token"], &sent); err != nil {
		t.Errorf("plan_token is not a JSON string: %v", err)

		return
	}

	if sent != token {
		t.Errorf("plan_token = %q, want %q", sent, token)
	}
}
