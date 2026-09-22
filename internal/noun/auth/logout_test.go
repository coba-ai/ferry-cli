package auth_test

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/coba-ai/ferry-cli/internal/creds"
	"github.com/coba-ai/ferry-cli/internal/fixture"
	"github.com/coba-ai/ferry-cli/internal/noun/auth"
)

// ---------------------------------------------------------------------------
// AC36, second half — logout removes the named slot, or both, and sends
// nothing.
// ---------------------------------------------------------------------------

// The slot matrix, both directions: what was asked for is gone and what was
// not asked for is still there. A logout that cleared everything regardless of
// `--class` would pass the first half of each row on its own.
func TestLogoutRemovesExactlyTheClassNamed(t *testing.T) {
	// `kept` is a constant, not a function of the profile the run left
	// behind. An expectation derived from the result is not an expectation:
	// a logout that ignored `--class` and cleared both slots would make
	// "what is left is what should be left" true of itself. That mutation
	// (U4-6) survived until this was written down.
	cases := []struct {
		name  string
		class []string
		kept  []string
	}{
		{
			name:  "no class removes both",
			class: nil,
			kept:  []string{},
		},
		{
			name:  "--class api_key leaves the pat",
			class: []string{"--class", "api_key"},
			kept:  []string{"pat"},
		},
		{
			name:  "--class pat leaves the api key",
			class: []string{"--class", "pat"},
			kept:  []string{"api_key"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := fixture.New(t)
			home := newHome(t)

			writeProfile(t, home, server.URL(), sandboxKey(t), patToken(t))

			args := append([]string{"logout"}, tc.class...)

			stdout, stderr, exit := run(t, invocation{home: home, args: args})

			if exit != 0 {
				t.Fatalf("exit %d, want 0\nstdout: %s\nstderr: %s", exit, stdout, stderr)
			}

			if n := requestCount(server); n != 0 {
				t.Errorf("logout sent %d requests. FERRY is not told: a token dropped here is "+
					"still valid until it is revoked.", n)
			}

			profile := storedProfile(t, home)

			var got []string
			if profile.APIKey != nil {
				got = append(got, "api_key")
			}

			if profile.PAT != nil {
				got = append(got, "pat")
			}

			want := tc.kept

			if got == nil {
				got = []string{}
			}

			sort.Strings(got)
			sort.Strings(want)

			if !reflect.DeepEqual(got, want) {
				t.Errorf("after `%s` the profile holds %v, want %v", strings.Join(args, " "), got, want)
			}

			raw := string(readCredentials(t, home))

			for _, removed := range removedBy(tc.class) {
				token := sandboxKey(t)
				if removed == "pat" {
					token = patToken(t)
				}

				if strings.Contains(raw, token) {
					t.Errorf("the %s token is still in the credentials file after it was removed", removed)
				}
			}
		})
	}
}

// The `--class` vocabulary, held to `auth.Classes` in both directions.
//
// The floor matters: a `Classes` that lost its entries would make "every class
// is accepted" and "every non-class is refused" both vacuously true.
func TestTheClassVocabularyIsExactlyTheTwoCredentialClasses(t *testing.T) {
	declared := make([]string, 0, len(auth.Classes))
	for _, c := range auth.Classes {
		declared = append(declared, string(c))
	}

	sort.Strings(declared)

	want := []string{string(creds.ClassAPIKey), string(creds.ClassPAT)}
	sort.Strings(want)

	if !reflect.DeepEqual(declared, want) {
		t.Fatalf("auth.Classes is %v, want %v", declared, want)
	}

	for _, class := range declared {
		server := fixture.New(t)
		home := newHome(t)

		writeProfile(t, home, server.URL(), sandboxKey(t), patToken(t))

		if _, _, exit := run(t, invocation{home: home, args: []string{"logout", "--class", class}}); exit != 0 {
			t.Errorf("--class %s exits %d, want 0", class, exit)
		}
	}

	// And the spellings FERRY itself uses, which are not the store's, are
	// refused rather than silently doing nothing.
	for _, class := range []string{"personal_access_token", "apikey", "API_KEY", ""} {
		server := fixture.New(t)
		home := newHome(t)

		writeProfile(t, home, server.URL(), sandboxKey(t), patToken(t))

		args := []string{"logout", "--class", class}

		_, stderr, exit := run(t, invocation{home: home, args: args})

		if class == "" {
			// An explicitly empty --class is "no class": both slots go.
			if exit != 0 {
				t.Errorf("--class \"\" exits %d, want 0", exit)
			}

			continue
		}

		if exit != 2 {
			t.Errorf("--class %s exits %d, want 2", class, exit)
		}

		if !strings.Contains(stderr, "api_key") || !strings.Contains(stderr, "pat") {
			t.Errorf("the refusal for --class %s does not name the vocabulary: %q", class, stderr)
		}
	}
}

// Logging out of a profile that was never created is a refusal, not a
// zero-byte credentials file where there was none.
func TestLogoutOnAnUnknownProfileIsRefusedAndWritesNothing(t *testing.T) {
	home := newHome(t)

	_, stderr, exit := run(t, invocation{home: home, args: []string{"logout"}})

	if exit != 3 {
		t.Errorf("exit %d, want 3", exit)
	}

	if raw := readCredentials(t, home); raw != nil {
		t.Errorf("a credentials file was created by a logout that had nothing to remove:\n%s", raw)
	}

	if !strings.Contains(stderr, "nothing to remove") {
		t.Errorf("the refusal reads %q", stderr)
	}
}

// What was removed is published, so a script does not have to diff the file.
func TestTheLogoutDocumentNamesWhatItRemoved(t *testing.T) {
	server := fixture.New(t)
	home := newHome(t)

	writeProfile(t, home, server.URL(), sandboxKey(t), patToken(t))

	stdout, _, exit := run(t, invocation{home: home, args: []string{"logout", "--output", "json"}})

	if exit != 0 {
		t.Fatalf("exit %d, want 0", exit)
	}

	var doc struct {
		Response struct {
			Profile string   `json:"profile"`
			Removed []string `json:"removed"`
		} `json:"response"`
	}

	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("parse document: %v\n%s", err, stdout)
	}

	if doc.Response.Profile != creds.DefaultProfile {
		t.Errorf("profile is %q, want %q", doc.Response.Profile, creds.DefaultProfile)
	}

	want := []string{"api_key", "pat"}
	if !reflect.DeepEqual(doc.Response.Removed, want) {
		t.Errorf("removed is %v, want %v", doc.Response.Removed, want)
	}
}

// removedBy is the classes a `--class` argument list should have removed.
func removedBy(class []string) []string {
	if len(class) == 0 {
		return []string{"api_key", "pat"}
	}

	return []string{class[1]}
}
