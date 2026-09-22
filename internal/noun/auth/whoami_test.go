package auth_test

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/coba-ai/ferry-cli/internal/creds"
	"github.com/coba-ai/ferry-cli/internal/fixture"
	"github.com/coba-ai/ferry-cli/internal/render"
)

// ---------------------------------------------------------------------------
// AC36 — whoami shows the stored prefix and last4 for each class and sends
// nothing; logout removes the named slot, or both.
// ---------------------------------------------------------------------------

// The server is named with no recordings on purpose: it answers 599 to
// anything, so a whoami that sent would be loud rather than merely wrong.
func TestWhoamiShowsBothSlotsAndSendsNothing(t *testing.T) {
	server := fixture.New(t)
	home := newHome(t)

	writeProfile(t, home, server.URL(), sandboxKey(t), patToken(t))

	stdout, stderr, exit := run(t, invocation{home: home, args: []string{"whoami", "--api", server.URL()}})

	if exit != 0 {
		t.Fatalf("exit %d, want 0\nstdout: %s\nstderr: %s", exit, stdout, stderr)
	}

	if n := requestCount(server); n != 0 {
		t.Errorf("whoami sent %d requests. Which credentials this machine holds is a local "+
			"question, and GET /v1/me could only answer for whichever one was presented.", n)
	}

	for _, want := range []string{
		creds.Prefix(sandboxKey(t)),
		last4(sandboxKey(t)),
		creds.Prefix(patToken(t)),
		last4(patToken(t)),
		"sandbox",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("whoami does not show %q:\n%s", want, stdout)
		}
	}
}

// The other direction: what whoami must *not* show is the token.
//
// Two assertions, because they fail for different reasons. The literal search
// catches this token; `render.FindSecrets` catches a token shape this test did
// not think of — a future canary, a PAT the implementation minted itself.
//
// Mutation M38 prints the token in `whoami`. It is caught three times over:
// here, by the AST sweep in `render/secrets_test.go` that requires a `.Token`
// read inside a print to be wrapped in `render.Reveal`, and by the behavioural
// sweep in `render/behaviour_test.go`.
func TestWhoamiNeverPrintsAStoredToken(t *testing.T) {
	server := fixture.New(t)
	home := newHome(t)

	writeProfile(t, home, server.URL(), sandboxKey(t), patToken(t))

	for _, output := range []string{"text", "json"} {
		t.Run(output, func(t *testing.T) {
			stdout, stderr, exit := run(t, invocation{
				home: home,
				args: []string{"whoami", "--api", server.URL(), "--output", output},
			})

			if exit != 0 {
				t.Fatalf("exit %d, want 0", exit)
			}

			transcript := stdout + stderr

			for _, token := range []string{sandboxKey(t), patToken(t)} {
				if strings.Contains(transcript, token) {
					t.Errorf("whoami printed a stored bearer token in %s mode:\n%s", output, transcript)
				}
			}

			if found := render.FindSecrets(transcript); len(found) > 0 {
				t.Errorf("whoami printed %d secret-shaped strings in %s mode: %q", len(found), output, found)
			}
		})
	}
}

// The JSON document's key set, in both directions, for the profile and for a
// slot. A field added here is a field somebody chose to publish; `token` being
// addable without a test going red is how it would get published by accident.
func TestTheWhoamiDocumentCarriesExactlyTheseKeys(t *testing.T) {
	server := fixture.New(t)
	home := newHome(t)

	writeProfile(t, home, server.URL(), sandboxKey(t), patToken(t))

	stdout, _, exit := run(t, invocation{
		home: home,
		args: []string{"whoami", "--api", server.URL(), "--output", "json"},
	})

	if exit != 0 {
		t.Fatalf("exit %d, want 0", exit)
	}

	var doc struct {
		Response map[string]json.RawMessage `json:"response"`
	}

	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("parse document: %v\n%s", err, stdout)
	}

	assertKeys(t, "the whoami response", doc.Response,
		"api_key", "api_url", "missing", "pat", "profile", "stored")

	var slot map[string]json.RawMessage
	if err := json.Unmarshal(doc.Response["api_key"], &slot); err != nil {
		t.Fatalf("parse api_key slot: %v", err)
	}

	assertKeys(t, "an api_key slot", slot,
		"environment", "principal_id", "stored_at", "token_last4", "token_prefix")
}

// `stored` and `missing` are complements and both are published, so a caller
// never has to read an absence as a decision.
func TestTheCensusOfSlotsIsComplete(t *testing.T) {
	cases := []struct {
		name    string
		tokens  []string
		stored  []string
		missing []string
	}{
		{"both", []string{"sk", "pat"}, []string{"api_key", "pat"}, []string{}},
		{"only an api key", []string{"sk"}, []string{"api_key"}, []string{"pat"}},
		{"only a pat", []string{"pat"}, []string{"pat"}, []string{"api_key"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := fixture.New(t)
			home := newHome(t)

			var tokens []string
			for _, kind := range tc.tokens {
				if kind == "sk" {
					tokens = append(tokens, sandboxKey(t))
				} else {
					tokens = append(tokens, patToken(t))
				}
			}

			writeProfile(t, home, server.URL(), tokens...)

			stdout, _, exit := run(t, invocation{
				home: home,
				args: []string{"whoami", "--api", server.URL(), "--output", "json"},
			})

			if exit != 0 {
				t.Fatalf("exit %d, want 0", exit)
			}

			var doc struct {
				Response struct {
					Stored  []string `json:"stored"`
					Missing []string `json:"missing"`
				} `json:"response"`
			}

			if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
				t.Fatalf("parse document: %v\n%s", err, stdout)
			}

			if !reflect.DeepEqual(doc.Response.Stored, tc.stored) {
				t.Errorf("stored is %v, want %v", doc.Response.Stored, tc.stored)
			}

			if !reflect.DeepEqual(doc.Response.Missing, tc.missing) {
				t.Errorf("missing is %v, want %v", doc.Response.Missing, tc.missing)
			}

			if len(doc.Response.Stored)+len(doc.Response.Missing) != 2 {
				t.Errorf("the census covers %d of 2 slots", len(doc.Response.Stored)+len(doc.Response.Missing))
			}
		})
	}
}

// An empty profile is exit 3 through the same pre-check every other command
// uses, not an empty render that reads like an answer.
func TestWhoamiOnAProfileWithNothingInItIsRefused(t *testing.T) {
	server := fixture.New(t)
	home := newHome(t)

	_, stderr, exit := run(t, invocation{home: home, args: []string{"whoami", "--api", server.URL()}})

	if exit != 3 {
		t.Errorf("exit %d, want 3", exit)
	}

	if n := requestCount(server); n != 0 {
		t.Errorf("the fixture saw %d requests", n)
	}

	if !strings.Contains(stderr, "auth login") {
		t.Errorf("the refusal does not say how to fix it: %q", stderr)
	}
}

func assertKeys(t *testing.T, what string, doc map[string]json.RawMessage, want ...string) {
	t.Helper()

	if len(doc) == 0 {
		t.Fatalf("%s is empty; the comparison below would be two empty sets", what)
	}

	got := make([]string, 0, len(doc))
	for k := range doc {
		got = append(got, k)
	}

	sort.Strings(got)
	sort.Strings(want)

	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s carries\n  %v\nand the declared set is\n  %v", what, got, want)
	}
}

func last4(token string) string { return token[len(token)-4:] }
