package keys_test

import (
	"encoding/json"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/kurenn/ferry-cli/internal/api"
	"github.com/kurenn/ferry-cli/internal/fixture"
	"github.com/kurenn/ferry-cli/internal/render"
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

	// The recorder's canary (A335/A340). It is transcribed rather than read
	// back out of the recording, because a test that draws the expected
	// value from the same file the server answered from would pass however
	// that file changed.
	const token = "ferry_sk_sandbox_RecordedCanaryNotARealKeyDoNotUse0000000001"

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

// `--login` drives a minted key from the wire into the credential file.
//
// This is the test A340 existed to unblock. The recorder used to substitute
// `API_KEY_TOKEN_PLACEHOLDER_1` for the token, which matched neither token
// regex of §1.2.2, so `creds.Put` refused it by shape and the path stopped
// one step short of the thing it exists to do. The recorder now emits a
// well-shaped canary, so the whole path is reachable.
func TestLoginStoresTheMintedKey(t *testing.T) {
	server, home := loggedIn(t, "keys.create.sandbox.201")

	before := storedProfile(t, home)

	stdout, stderr, exit := run(t, invocation{
		home: home,
		args: []string{
			"create", "--api", server.URL(), "--env", "sandbox",
			"--name", "cli-recorded-key", "--scopes", "read,money:simulate", "--login",
		},
	})

	if exit != 0 {
		t.Fatalf("exit %d, want 0\nstdout: %s\nstderr: %s", exit, stdout, stderr)
	}

	after := storedProfile(t, home)

	if after.APIKey == nil {
		t.Fatal("--login exited 0 and stored no API key, so the key it minted is only on the " +
			"screen — which is the failure A340 was about, moved rather than fixed")
	}

	const canary = "ferry_sk_sandbox_RecordedCanaryNotARealKeyDoNotUse0000000001"

	if got := after.APIKey.Token; got != canary {
		t.Errorf("the stored token is %q, want the minted one", got)
	}

	// The PAT that authenticated the mint is a different credential class
	// and must survive: losing it would leave an operator able to use the
	// key they just made and unable to make another.
	if after.PAT == nil {
		t.Error("--login discarded the personal access token that authenticated the mint")
	} else if before.PAT != nil && after.PAT.Token != before.PAT.Token {
		t.Error("--login replaced the personal access token rather than adding beside it")
	}
}

// And when the store fails, the operator is told the key exists anyway.
//
// Until A340 this happened by itself, because the recorded token was
// malformed — the test's fixture was a defect elsewhere, so fixing that
// defect took the fixture with it. The failure is deliberate now: the
// credential directory is made unwritable, so the atomic write cannot land.
// A key has been minted on the server by then and there is no second chance
// to read its token, which is why this cannot be a bare non-zero exit.
func TestAMintedKeyThatCannotBeStoredSaysTheKeyExistsAnyway(t *testing.T) {
	server, home := loggedIn(t, "keys.create.sandbox.201")

	if err := os.Chmod(home, 0o500); err != nil {
		t.Fatalf("making the credential directory unwritable: %v", err)
	}

	// Restored so the temporary directory can be cleaned up, and because a
	// test that leaves the filesystem altered fails its neighbours rather
	// than itself.
	t.Cleanup(func() { _ = os.Chmod(home, 0o700) })

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
