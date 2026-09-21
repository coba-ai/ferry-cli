package keys_test

import (
	"strings"
	"testing"

	"github.com/kurenn/ferry/cli/internal/fixture"
)

// ---------------------------------------------------------------------------
// AC43 end to end — a pre-check refusal makes zero requests, and "zero" is
// read off the fixture's own log rather than inferred from an error.
// ---------------------------------------------------------------------------

// Every `ferry keys` verb accepts a personal access token and refuses an API
// key (`api_keys_controller.rb:27`). A profile holding only a key is refused
// locally, exit 3, with a sentence saying what to get.
//
// The server is loaded with the recording each verb would have used, so a
// request that *was* sent would have been answered — the test cannot pass
// because the fixture happened to refuse it. `Remaining` is then the proof
// that nothing was played.
func TestEveryKeysVerbRefusesAnAPIKeyWithoutSending(t *testing.T) {
	cases := []struct {
		verb     string
		args     []string
		scenario string
	}{
		{"list", []string{"list", "--limit", "2"}, "keys.list.page1.200"},
		{"get", []string{"get", "key_PLACEHOLDER_1"}, "keys.get.200"},
		{
			"create",
			[]string{"create", "--env", "sandbox", "--name", "cli-recorded-key", "--scopes", "read,money:simulate"},
			"keys.create.sandbox.201",
		},
		{"revoke", []string{"revoke", "key_PLACEHOLDER_1", "--reason", "rotated"}, "keys.revoke.200"},
	}

	for _, tc := range cases {
		t.Run(tc.verb, func(t *testing.T) {
			server := fixture.New(t, tc.scenario)
			home := newHome(t)

			writeProfile(t, home, server.URL(), canaryKey(t))

			args := append(append([]string{}, tc.args...), "--api", server.URL())

			stdout, stderr, exit := run(t, invocation{home: home, args: args})

			if exit != 3 {
				t.Fatalf("exit %d, want 3\nstdout: %s\nstderr: %s", exit, stdout, stderr)
			}

			if n := requestCount(server); n != 0 {
				t.Errorf("`ferry keys %s` sent %d request(s). AC43: the answer is computable from "+
					"the token's own shape, and sending it puts a live bearer credential in the "+
					"server's logs for a 403 this process already knew about.", tc.verb, n)
			}

			if left := server.Remaining()[tc.scenario]; left != 1 {
				t.Errorf("the recording %s has %d interaction(s) unplayed, want 1; the request "+
					"would have been answered, so the refusal is the CLI's and not the fixture's",
					tc.scenario, left)
			}

			for _, want := range []string{"personal access token", "ferry:pat:issue"} {
				if !strings.Contains(stderr, want) {
					t.Errorf("the refusal does not say %q:\n%s", want, stderr)
				}
			}
		})
	}
}

// The floor: the same commands against the right credential do send.
//
// Without it, "zero requests" above would be satisfied by a CLI that never
// sent anything at all.
func TestTheSameVerbsDoSendWithAPersonalAccessToken(t *testing.T) {
	cases := []struct {
		verb     string
		args     []string
		scenario string
	}{
		{"list", []string{"list", "--limit", "2"}, "keys.list.page1.200"},
		{"get", []string{"get", "key_PLACEHOLDER_1"}, "keys.get.200"},
		{
			"create",
			[]string{"create", "--env", "sandbox", "--name", "cli-recorded-key", "--scopes", "read,money:simulate"},
			"keys.create.sandbox.201",
		},
		{"revoke", []string{"revoke", "key_PLACEHOLDER_1", "--reason", "rotated"}, "keys.revoke.200"},
	}

	for _, tc := range cases {
		t.Run(tc.verb, func(t *testing.T) {
			server, home := loggedIn(t, tc.scenario)

			args := append(append([]string{}, tc.args...), "--api", server.URL())

			stdout, stderr, exit := run(t, invocation{home: home, args: args})

			if exit != 0 {
				t.Fatalf("exit %d, want 0\nstdout: %s\nstderr: %s", exit, stdout, stderr)
			}

			if n := requestCount(server); n != 1 {
				t.Errorf("`ferry keys %s` sent %d requests, want 1", tc.verb, n)
			}

			if left := server.Remaining()[tc.scenario]; left != 0 {
				t.Errorf("the recording %s has %d interaction(s) unplayed", tc.scenario, left)
			}
		})
	}
}

// FERRY_TOKEN does not get around the pre-check.
//
// This is the adversarial question "what makes a pre-check issue a request"
// with the most plausible answer: an escape hatch that replaces the credential
// without going through the class rule. It fills the slot of its own class and
// the rule is applied afterwards, so a key in the environment cannot make a
// PAT endpoint send one.
func TestAnEnvironmentTokenOfTheWrongClassIsStillRefused(t *testing.T) {
	server := fixture.New(t, "keys.list.page1.200")
	home := newHome(t)

	// The profile holds nothing, so the only credential in play is the one
	// in the environment.
	_, stderr, exit := run(t, invocation{
		home: home,
		args: []string{"list", "--limit", "2", "--api", server.URL()},
		env:  map[string]string{"FERRY_TOKEN": canaryKey(t)},
	})

	if exit != 3 {
		t.Fatalf("exit %d, want 3\nstderr: %s", exit, stderr)
	}

	if n := requestCount(server); n != 0 {
		t.Errorf("FERRY_TOKEN got an API key past the pre-check: %d request(s) were sent", n)
	}

	// And the floor: a PAT in the environment does reach the endpoint, so
	// the refusal above is about the class and not about the mechanism.
	patServer := fixture.New(t, "keys.list.page1.200")
	patHome := newHome(t)

	_, stderr, exit = run(t, invocation{
		home: patHome,
		args: []string{"list", "--limit", "2", "--api", patServer.URL()},
		env:  map[string]string{"FERRY_TOKEN": canaryPAT(t)},
	})

	if exit != 0 {
		t.Fatalf("a PAT in FERRY_TOKEN exits %d, want 0\nstderr: %s", exit, stderr)
	}

	if n := requestCount(patServer); n != 1 {
		t.Errorf("FERRY_TOKEN sent %d requests, want 1", n)
	}
}
