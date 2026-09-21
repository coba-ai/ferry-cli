package keys

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kurenn/ferry-cli/internal/api"
	"github.com/kurenn/ferry-cli/internal/creds"
	"github.com/kurenn/ferry-cli/internal/fsx"
	"github.com/kurenn/ferry-cli/internal/noun"
	"github.com/kurenn/ferry-cli/internal/xdg"
)

// `--login`'s store, asserted here rather than through the fixture.
//
// `create_test.go` explains why: the recorder writes
// `API_KEY_TOKEN_PLACEHOLDER_1` in place of the token it captured, and that
// string matches neither token regex, so `creds.Put` refuses it by shape and
// no end-to-end run can reach a stored key. This test supplies a well-shaped
// token to the same function the command calls, so the path is exercised with
// the only thing about it changed being the value FERRY would have sent.
func TestStoreMintedWritesTheKeyBesideTheTokenThatMintedIt(t *testing.T) {
	const apiURL = "https://api.ferry.test"

	// 43 characters after the prefix, as `lib/ferry/tokens.rb` requires. The
	// length is padded rather than typed out: a token one character short is
	// refused by `creds.Classify`, and the failure would read as though the
	// store were broken.
	pat := "ferry_pat_" + body("INTERNALPAT")
	minted := "ferry_sk_sandbox_" + body("INTERNALSK")

	for _, token := range []string{pat, minted} {
		if _, err := creds.Classify(token); err != nil {
			t.Fatalf("the canary %q is not a token this CLI recognises: %v", token, err)
		}
	}

	home := t.TempDir()
	paths := xdg.Paths{ConfigDir: home, StateDir: home, Home: home}

	if err := paths.EnsureAll(fsx.OS()); err != nil {
		t.Fatalf("create %s: %v", home, err)
	}

	store := creds.NewStore(fsx.OS(), paths.CredentialsFile())

	file, err := store.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if err := file.Put(creds.DefaultProfile, apiURL, creds.Credential{Token: pat}); err != nil {
		t.Fatalf("seed the pat: %v", err)
	}

	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

	session := &noun.Session{
		Local: &noun.Local{
			Deps:    noun.Deps{FS: fsx.OS(), Now: func() time.Time { return now }},
			Globals: &noun.Globals{Profile: creds.DefaultProfile},
			Paths:   paths,
			Store:   store,
			File:    file,
		},
		Match: creds.Match{Class: creds.ClassPAT, Profile: creds.DefaultProfile, APIURL: apiURL},
	}

	token := minted
	key := api.APIKey{ID: "key_minted_1", Token: &token}

	if err := storeMinted(session, key); err != nil {
		t.Fatalf("storeMinted: %v", err)
	}

	// Read it back off disk rather than out of the in-memory document: the
	// claim is that the key survives the process, and an assertion against
	// `file` would hold for a Save that never happened.
	reread, err := creds.NewStore(fsx.OS(), filepath.Join(home, "credentials.json")).Load()
	if err != nil {
		t.Fatalf("reload: %v", err)
	}

	profile, ok := reread.Profiles[creds.DefaultProfile]
	if !ok {
		t.Fatal("no profile was written")
	}

	if profile.APIKey == nil {
		t.Fatal("the minted key was not stored")
	}

	if profile.APIKey.Token != minted {
		t.Errorf("the stored token is %q, want the minted one", profile.APIKey.Token)
	}

	if profile.APIKey.PrincipalID != key.ID {
		t.Errorf("principal_id is %q, want %q", profile.APIKey.PrincipalID, key.ID)
	}

	if profile.APIKey.Environment == nil || *profile.APIKey.Environment != creds.EnvSandbox {
		t.Errorf("environment is %v, want sandbox as the token's own shape says",
			profile.APIKey.Environment)
	}

	if !profile.APIKey.StoredAt.Equal(now) {
		t.Errorf("stored_at is %s, want the injected clock %s", profile.APIKey.StoredAt, now)
	}

	if profile.PAT == nil || profile.PAT.Token != pat {
		t.Error("the personal access token that minted the key was displaced by it")
	}

	if profile.APIURL != apiURL {
		t.Errorf("api_url is %q, want %q", profile.APIURL, apiURL)
	}
}

func body(tag string) string { return tag + strings.Repeat("0", 43-len(tag)) }

// A minted key that cannot be stored still reports that it exists, and never
// prints the token — including the branch where the 201 carried none.
func TestAKeyMintedWithoutATokenStillNamesItself(t *testing.T) {
	err := storeMinted(&noun.Session{
		Local: &noun.Local{Globals: &noun.Globals{Profile: creds.DefaultProfile}},
	}, api.APIKey{ID: "key_minted_2"})

	if err == nil {
		t.Fatal("a 201 with no token was stored as though it were a credential")
	}

	for _, want := range []string{"key_minted_2", "ferry keys revoke", "not recoverable"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the message does not say %q: %q", want, err)
		}
	}
}
