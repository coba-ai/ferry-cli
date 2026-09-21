package keys_test

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/kurenn/ferry/cli/internal/api"
	"github.com/kurenn/ferry/cli/internal/fixture"
	"github.com/kurenn/ferry/cli/internal/render"
)

// ---------------------------------------------------------------------------
// AC37 — the request body carries exactly the keys the caller supplied, and
// the 201's token is printed once.
// ---------------------------------------------------------------------------

// The recorded request is `{name, environment, scopes}` with no `expires_at`,
// and the fixture matches structurally (AC30), so a body carrying
// `"expires_at": null` would not be answered at all.
//
// That is the interesting half of AC37. `POST /v1/api_keys` refuses an unknown
// key outright, and an explicit null is a different request from an omission:
// the first asserts "this key never expires" and the second asks FERRY for its
// own default. Mutation M39 makes the CLI send the null.
func TestTheCreateBodyCarriesExactlyTheKeysTheCallerSupplied(t *testing.T) {
	server, home := loggedIn(t, "keys.create.sandbox.201")

	stdout, stderr, exit := run(t, invocation{
		home: home,
		args: []string{
			"create", "--api", server.URL(), "--env", "sandbox",
			"--name", "cli-recorded-key", "--scopes", "read,money:simulate",
		},
	})

	if exit != 0 {
		t.Fatalf("exit %d, want 0\nstdout: %s\nstderr: %s", exit, stdout, stderr)
	}

	if n := requestCount(server); n != 1 {
		t.Fatalf("the fixture saw %d requests, want 1", n)
	}

	req := server.Requests()[0]

	if !req.Matched() {
		t.Fatalf("no recording answered %s %s with body %s. The fixture compares the body "+
			"structurally (AC30), so this is the API refusing the request the CLI actually built.",
			req.Method, req.Path, req.Body)
	}

	assertBodyKeys(t, req.Body, "environment", "name", "scopes")
}

// And the other lifetimes, each asserted on the body the CLI built.
//
// These cannot go through the fixture: only the no-expiry body is recorded, so
// a request carrying `expires_at` is answered 599 by design. The assertion is
// on the bytes the client sent — which the fixture logs whether or not it
// matched (AC31) — and that is the layer the defect lives at.
func TestTheExpiryKeyIsPresentOnlyWhenTheCallerNamedALifetime(t *testing.T) {
	cases := []struct {
		name string
		args []string
		keys []string
		// expiresAt, when set, is the exact instant the body must carry.
		expiresAt string
	}{
		{
			name: "no lifetime omits the key",
			args: nil,
			keys: []string{"environment", "name", "scopes"},
		},
		{
			name:      "--expires-at is passed through verbatim",
			args:      []string{"--expires-at", "2026-12-31T23:59:59Z"},
			keys:      []string{"environment", "expires_at", "name", "scopes"},
			expiresAt: "2026-12-31T23:59:59Z",
		},
		{
			name:      "--expires-in 30d is resolved against the clock",
			args:      []string{"--expires-in", "30d"},
			keys:      []string{"environment", "expires_at", "name", "scopes"},
			expiresAt: fixedNow.Add(30 * 24 * time.Hour).Format(time.RFC3339),
		},
		{
			name:      "--expires-in 4w is four sevens of days",
			args:      []string{"--expires-in", "4w"},
			keys:      []string{"environment", "expires_at", "name", "scopes"},
			expiresAt: fixedNow.Add(28 * 24 * time.Hour).Format(time.RFC3339),
		},
		{
			name:      "--expires-in 12h is a Go duration",
			args:      []string{"--expires-in", "12h"},
			keys:      []string{"environment", "expires_at", "name", "scopes"},
			expiresAt: fixedNow.Add(12 * time.Hour).Format(time.RFC3339),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server, home := loggedIn(t, "keys.create.sandbox.201")

			args := append([]string{
				"create", "--api", server.URL(), "--env", "sandbox",
				"--name", "cli-recorded-key", "--scopes", "read,money:simulate",
			}, tc.args...)

			run(t, invocation{home: home, args: args})

			if n := requestCount(server); n != 1 {
				t.Fatalf("the fixture saw %d requests, want 1", n)
			}

			body := server.Requests()[0].Body

			assertBodyKeys(t, body, tc.keys...)

			if tc.expiresAt == "" {
				return
			}

			var sent struct {
				ExpiresAt string `json:"expires_at"`
			}

			if err := json.Unmarshal(body, &sent); err != nil {
				t.Fatalf("parse body: %v", err)
			}

			if sent.ExpiresAt != tc.expiresAt {
				t.Errorf("expires_at is %q, want %q", sent.ExpiresAt, tc.expiresAt)
			}
		})
	}
}

// Two ways to say one thing is a usage error rather than a silent precedence.
func TestExpiresAtAndExpiresInTogetherAreRefused(t *testing.T) {
	server, home := loggedIn(t, "keys.create.sandbox.201")

	_, stderr, exit := run(t, invocation{
		home: home,
		args: []string{
			"create", "--api", server.URL(), "--env", "sandbox",
			"--name", "k", "--scopes", "read",
			"--expires-at", "2026-12-31T23:59:59Z", "--expires-in", "30d",
		},
	})

	if exit != 2 {
		t.Errorf("exit %d, want 2", exit)
	}

	if n := requestCount(server); n != 0 {
		t.Errorf("the fixture saw %d requests", n)
	}

	if !strings.Contains(stderr, "--expires-at") || !strings.Contains(stderr, "--expires-in") {
		t.Errorf("the refusal names only one of the two: %q", stderr)
	}
}

// The required trio is required, each on its own.
func TestTheRequiredFieldsAreEachRequired(t *testing.T) {
	cases := []struct {
		missing string
		args    []string
		want    string
	}{
		{"--name", []string{"--env", "sandbox", "--scopes", "read"}, "--name is required"},
		{"--scopes", []string{"--env", "sandbox", "--name", "k"}, "--scopes is required"},
		{"--env", []string{"--name", "k", "--scopes", "read"}, "--env is required"},
	}

	for _, tc := range cases {
		t.Run("without "+tc.missing, func(t *testing.T) {
			server, home := loggedIn(t, "keys.create.sandbox.201")

			args := append([]string{"create", "--api", server.URL()}, tc.args...)

			_, stderr, exit := run(t, invocation{home: home, args: args})

			if exit != 2 {
				t.Errorf("exit %d, want 2 (stderr %q)", exit, stderr)
			}

			if n := requestCount(server); n != 0 {
				t.Errorf("the fixture saw %d requests", n)
			}

			if !strings.Contains(stderr, tc.want) {
				t.Errorf("the refusal is %q, want it to contain %q", stderr, tc.want)
			}
		})
	}
}

// The scope vocabulary, both directions against `api.Scopes`.
//
// The floor is the loop over `api.Scopes` itself: an empty list would make
// "every declared scope is accepted" true of a CLI that accepted nothing.
func TestTheScopeVocabularyIsExactlyTheContractsEnum(t *testing.T) {
	if len(api.Scopes) == 0 {
		t.Fatal("api.Scopes is empty; every assertion below would be vacuous")
	}

	for _, scope := range api.Scopes {
		server, home := loggedIn(t, "keys.create.sandbox.201")

		_, stderr, exit := run(t, invocation{
			home: home,
			args: []string{
				"create", "--api", server.URL(), "--env", "sandbox",
				"--name", "k", "--scopes", scope,
			},
		})

		// The fixture only has the two-scope body, so anything else is a
		// 599 and exit 5 — but never exit 2, which is what a scope the CLI
		// refused locally would produce.
		if exit == 2 {
			t.Errorf("the declared scope %q was refused locally: %q", scope, stderr)
		}

		if n := requestCount(server); n != 1 {
			t.Errorf("the declared scope %q produced %d requests, want 1", scope, n)
		}
	}

	// `keys:manage` is control-plane only and deliberately absent from the
	// enum, so it is the sharpest negative case there is.
	for _, scope := range []string{"keys:manage", "money:execute:all", "admin", "READ"} {
		server, home := loggedIn(t, "keys.create.sandbox.201")

		_, stderr, exit := run(t, invocation{
			home: home,
			args: []string{
				"create", "--api", server.URL(), "--env", "sandbox",
				"--name", "k", "--scopes", scope,
			},
		})

		if exit != 2 {
			t.Errorf("--scopes %s exits %d, want 2", scope, exit)
		}

		if n := requestCount(server); n != 0 {
			t.Errorf("--scopes %s produced %d requests; an undeclared scope must not reach FERRY", scope, n)
		}

		if !strings.Contains(stderr, scope) {
			t.Errorf("the refusal for %q does not name it: %q", scope, stderr)
		}
	}
}

// ---------------------------------------------------------------------------
// AC42, positive control — the create token is printed, once, in text mode.
// ---------------------------------------------------------------------------

// This is the half that proves the detector can fire. Without it, every
// "nothing printed a secret" assertion in `secrets_test.go` would be satisfied
// by a `FindSecrets` that recognises nothing.
func TestTheCreatedTokenIsPrintedExactlyOnceInTextMode(t *testing.T) {
	server, home := loggedIn(t, "keys.create.sandbox.201")

	before := render.RevealCount(render.SiteKeysCreateToken)

	stdout, stderr, exit := run(t, invocation{
		home: home,
		args: []string{
			"create", "--api", server.URL(), "--env", "sandbox",
			"--name", "cli-recorded-key", "--scopes", "read,money:simulate",
		},
	})

	if exit != 0 {
		t.Fatalf("exit %d, want 0\nstdout: %s\nstderr: %s", exit, stdout, stderr)
	}

	const token = "API_KEY_TOKEN_PLACEHOLDER_1"

	if got := strings.Count(stdout, token); got != 1 {
		t.Errorf("the token appears %d times on stdout, want exactly 1:\n%s", got, stdout)
	}

	if strings.Contains(stderr, token) {
		t.Errorf("the token was written to stderr as well:\n%s", stderr)
	}

	// The runtime census: the permitted site is what printed it, rather
	// than some other path that happens to produce the same text.
	if after := render.RevealCount(render.SiteKeysCreateToken); after != before+1 {
		t.Errorf("render.Reveal(%s) fired %d times, want 1", render.SiteKeysCreateToken, after-before)
	}

	// And the caution beside it, because a token printed without one is a
	// token a caller may not think to copy before the terminal scrolls.
	if !strings.Contains(stdout, "shown once; FERRY will not return it again") {
		t.Errorf("the token is printed with no caution that it is the only copy:\n%s", stdout)
	}
}

// ---------------------------------------------------------------------------
// AC37's `--login`.
// ---------------------------------------------------------------------------

// `--login` cannot be driven to a stored credential through the fixture.
//
// The recorder substitutes `API_KEY_TOKEN_PLACEHOLDER_1` for the token it
// captured (`spec/support/cli/recorder.rb`), and that string matches neither
// token regex of §1.2.2, so `creds.Put` refuses it by shape. That is the
// recorder behaving correctly — a real bearer token in a committed fixture
// would be worse — but it means no test in this package can watch a key go
// from the wire into the file. The store itself is asserted in
// `store_internal_test.go`, where a well-shaped token can be used; what is
// asserted here is the behaviour on the path the fixture can reach.
//
// Suggested amendment A340 records the gap.
func TestAMintedKeyThatCannotBeStoredSaysTheKeyExistsAnyway(t *testing.T) {
	server, home := loggedIn(t, "keys.create.sandbox.201")

	stdout, stderr, exit := run(t, invocation{
		home: home,
		args: []string{
			"create", "--api", server.URL(), "--env", "sandbox",
			"--name", "cli-recorded-key", "--scopes", "read,money:simulate", "--login",
		},
	})

	if exit != 1 {
		t.Fatalf("exit %d, want 1\nstdout: %s\nstderr: %s", exit, stdout, stderr)
	}

	// The three things an operator needs: that a key exists, which one, and
	// that this token is gone.
	for _, want := range []string{"the key was minted", "key_PLACEHOLDER_1", "not recoverable"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("the failure does not say %q:\n%s", want, stderr)
		}
	}

	if found := render.FindSecrets(stderr); len(found) > 0 {
		t.Errorf("the failure message carries secret-shaped text %q; an error stream is not one "+
			"of AC42's two sites", found)
	}

	// And the PAT that authenticated is untouched.
	if profile := storedProfile(t, home); profile.PAT == nil {
		t.Error("the personal access token was lost by a --login that failed")
	}
}

// Without `--login` nothing new is written: minting a key is not logging in
// with it, and a CLI that silently swapped the credential under the operator
// would change which key the next money command sends.
func TestWithoutLoginTheMintedKeyIsNotStored(t *testing.T) {
	server, home := loggedIn(t, "keys.create.sandbox.201")

	before := storedProfile(t, home)

	_, _, exit := run(t, invocation{
		home: home,
		args: []string{
			"create", "--api", server.URL(), "--env", "sandbox",
			"--name", "cli-recorded-key", "--scopes", "read,money:simulate",
		},
	})

	if exit != 0 {
		t.Fatalf("exit %d, want 0", exit)
	}

	after := storedProfile(t, home)

	if after.APIKey != nil {
		t.Error("a key was stored without --login")
	}

	if !reflect.DeepEqual(before.PAT, after.PAT) {
		t.Error("the stored personal access token changed under a command that was not asked to store anything")
	}
}

// A 403 mints nothing and stores nothing, `--login` or not.
func TestARefusedCreateStoresNothing(t *testing.T) {
	server := fixture.New(t, "keys.create.live_money.step_up.403")
	home := newHome(t)

	writeProfile(t, home, server.URL(), canaryPAT(t))

	stdout, stderr, exit := run(t, invocation{
		home: home,
		args: []string{
			"create", "--api", server.URL(), "--env", "live",
			"--name", "cli-recorded-key", "--scopes", "money:execute", "--login",
		},
	})

	if exit == 0 {
		t.Fatalf("exit 0 on a 403\nstdout: %s\nstderr: %s", stdout, stderr)
	}

	if profile := storedProfile(t, home); profile.APIKey != nil {
		t.Error("a key was stored for a create FERRY refused")
	}

	if found := render.FindSecrets(stdout + stderr); len(found) > 0 {
		t.Errorf("a refused create printed secret-shaped text: %q", found)
	}
}

// assertBodyKeys holds a request body's top-level key set to want, in both
// directions.
func assertBodyKeys(t *testing.T, body []byte, want ...string) {
	t.Helper()

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		t.Fatalf("parse request body: %v\n%s", err, body)
	}

	if len(fields) == 0 {
		t.Fatalf("the request body is empty; the comparison below is two empty sets:\n%s", body)
	}

	got := make([]string, 0, len(fields))
	for k := range fields {
		got = append(got, k)
	}

	sort.Strings(got)
	sort.Strings(want)

	if !reflect.DeepEqual(got, want) {
		t.Errorf("the request body carries\n  %v\nand the caller supplied\n  %v\n\nbody: %s", got, want, body)
	}
}
