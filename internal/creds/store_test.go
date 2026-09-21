package creds_test

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kurenn/ferry-cli/internal/creds"
	"github.com/kurenn/ferry-cli/internal/fsx"
)

const (
	sandboxKey = "ferry_sk_sandbox_grpQ0HNk3bCwMxVf9aLtRzYuEjPdS7nHkX2gQmB4vTc"
	liveKey    = "ferry_sk_live_grpQ0HNk3bCwMxVf9aLtRzYuEjPdS7nHkX2gQmB4vTc"
	pat        = "ferry_pat_grpQ0HNk3bCwMxVf9aLtRzYuEjPdS7nHkX2gQmB4vTc"
	apiURL     = "https://ferry.internal.example"
)

func init() {
	// The fixtures must be the shapes the contract declares, or every
	// classification assertion below is about a string nothing would accept.
	for _, tok := range []string{sandboxKey, liveKey, pat} {
		if _, err := creds.Classify(tok); err != nil {
			panic("test fixture " + tok + " is not a valid token shape: " + err.Error())
		}
	}
}

func newStore(t *testing.T) (*creds.Store, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "credentials.json")
	return creds.NewStore(fsx.OS(), path), path
}

// AC7: written 0600 by temp+fsync+rename.
func TestSaveWritesMode0600AtomicallyWithFsync(t *testing.T) {
	dir := t.TempDir()
	f := fsx.NewFault(fsx.OS())
	path := filepath.Join(dir, "credentials.json")
	store := creds.NewStore(f, path)

	file := creds.NewFile()
	if err := file.Put("default", apiURL, creds.Credential{Token: sandboxKey, PrincipalID: "key_01j"}); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(file); err != nil {
		t.Fatalf("Save: %v", err)
	}

	want := []string{fsx.OpCreate, fsx.OpWrite, fsx.OpSync, fsx.OpClose, fsx.OpRename, fsx.OpSyncDir}
	if got := f.Names(); !slices.Equal(got, want) {
		t.Fatalf("write sequence:\n got %v\nwant %v", got, want)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("credentials file mode %#o, want 0600", got)
	}
}

// AC7: a file wider than 0600 is refused on read.
func TestLoadRefusesAFileWiderThan0600(t *testing.T) {
	for _, mode := range []os.FileMode{0o640, 0o604, 0o644, 0o666, 0o700 | 0o060} {
		store, path := newStore(t)
		if err := os.WriteFile(path, []byte(`{"schema":1,"profiles":{}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		_, err := store.Load()
		if !errors.Is(err, creds.ErrPermissionsTooWide) {
			t.Fatalf("mode %#o: err = %v, want ErrPermissionsTooWide", mode, err)
		}
		if !strings.Contains(err.Error(), "chmod 600") {
			t.Errorf("mode %#o: the refusal does not name the remedy: %v", mode, err)
		}
	}
}

// The refusal must not be reached by reading the file first; a token that has
// already been loaded into this process has already been spread.
func TestLoadDoesNotReadAWideFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "credentials.json")
	if err := os.WriteFile(path, []byte(`{"schema":1,"profiles":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	f := fsx.NewFault(fsx.OS())
	if _, err := creds.NewStore(f, path).Load(); !errors.Is(err, creds.ErrPermissionsTooWide) {
		t.Fatalf("err = %v", err)
	}
	for _, op := range f.Names() {
		if op == fsx.OpReadFile {
			t.Fatalf("the file was read before the mode was refused: %v", f.Names())
		}
	}
}

func TestLoadAcceptsExactly0600AndNarrower(t *testing.T) {
	for _, mode := range []os.FileMode{0o600, 0o400} {
		store, path := newStore(t)
		if err := os.WriteFile(path, []byte(`{"schema":1,"profiles":{}}`), mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Load(); err != nil {
			t.Fatalf("mode %#o refused: %v", mode, err)
		}
	}
}

func TestLoadOfAMissingFileIsAnEmptyDocument(t *testing.T) {
	store, _ := newStore(t)
	f, err := store.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(f.Profiles) != 0 {
		t.Fatalf("profiles = %v", f.Profiles)
	}
}

func TestRoundTrip(t *testing.T) {
	store, _ := newStore(t)
	file := creds.NewFile()
	stored := time.Date(2026, 9, 21, 10, 0, 0, 0, time.UTC)
	if err := file.Put("default", apiURL, creds.Credential{Token: sandboxKey, PrincipalID: "key_01j", StoredAt: stored}); err != nil {
		t.Fatal(err)
	}
	if err := file.Put("default", apiURL, creds.Credential{Token: pat, PrincipalID: "pat_01j", StoredAt: stored}); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(file); err != nil {
		t.Fatal(err)
	}

	back, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	p := back.Profiles["default"]
	if p.APIKey == nil || p.APIKey.Token != sandboxKey {
		t.Fatalf("api_key slot = %+v", p.APIKey)
	}
	if p.PAT == nil || p.PAT.Token != pat {
		t.Fatalf("pat slot = %+v", p.PAT)
	}
	if p.APIKey.Environment == nil || *p.APIKey.Environment != creds.EnvSandbox {
		t.Fatalf("api key environment = %v, want sandbox", p.APIKey.Environment)
	}
	if p.PAT.Environment != nil {
		t.Fatalf("a PAT belongs to no environment, got %v", *p.PAT.Environment)
	}
	if !p.APIKey.StoredAt.Equal(stored) {
		t.Fatalf("stored_at = %s", p.APIKey.StoredAt)
	}
}

// A PAT's null environment must survive the round trip as null rather than as
// the empty string; "no environment" and "unknown environment" are different
// facts and AC34 refuses --env on a PAT on the strength of the first.
func TestNullEnvironmentSurvivesAsNull(t *testing.T) {
	store, path := newStore(t)
	file := creds.NewFile()
	if err := file.Put("default", apiURL, creds.Credential{Token: pat}); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(file); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"environment": null`) {
		t.Fatalf("PAT environment is not written as null:\n%s", raw)
	}
}

// AC8: Put classifies by the contract's regexes, with no I/O.
func TestPutClassifiesByShapeWithNoIO(t *testing.T) {
	f := fsx.NewFault(fsx.OS())
	_ = creds.NewStore(f, filepath.Join(t.TempDir(), "credentials.json"))

	file := creds.NewFile()
	for _, tok := range []string{sandboxKey, liveKey, pat} {
		if err := file.Put("default", apiURL, creds.Credential{Token: tok}); err != nil {
			t.Fatalf("Put(%s): %v", tok[:12], err)
		}
	}
	if ops := f.Ops(); len(ops) != 0 {
		t.Fatalf("Put performed I/O: %v", ops)
	}

	p := file.Profiles["default"]
	if p.APIKey == nil || p.APIKey.Token != liveKey {
		t.Fatalf("the live key did not replace the sandbox key in the api_key slot: %+v", p.APIKey)
	}
	if p.PAT == nil || p.PAT.Token != pat {
		t.Fatalf("pat slot = %+v", p.PAT)
	}
}

func TestClassify(t *testing.T) {
	cases := map[string]struct {
		token string
		class creds.Class
		err   error
	}{
		"sandbox key":        {sandboxKey, creds.ClassAPIKey, nil},
		"live key":           {liveKey, creds.ClassAPIKey, nil},
		"pat":                {pat, creds.ClassPAT, nil},
		"empty":              {"", "", creds.ErrUnknownTokenShape},
		"unknown prefix":     {"ferry_zz_sandbox_" + strings.Repeat("a", 43), "", creds.ErrUnknownTokenShape},
		"unknown env":        {"ferry_sk_staging_" + strings.Repeat("a", 43), "", creds.ErrUnknownTokenShape},
		"secret too short":   {"ferry_pat_" + strings.Repeat("a", 42), "", creds.ErrUnknownTokenShape},
		"secret too long":    {"ferry_pat_" + strings.Repeat("a", 44), "", creds.ErrUnknownTokenShape},
		"non alphanumeric":   {"ferry_pat_" + strings.Repeat("a", 42) + "-", "", creds.ErrUnknownTokenShape},
		"trailing newline":   {pat + "\n", "", creds.ErrUnknownTokenShape},
		"newline injection":  {pat + "\nX-Evil: 1", "", creds.ErrUnknownTokenShape},
		"leading whitespace": {" " + pat, "", creds.ErrUnknownTokenShape},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			class, err := creds.Classify(tc.token)
			if !errors.Is(err, tc.err) {
				t.Fatalf("err = %v, want %v", err, tc.err)
			}
			if class != tc.class {
				t.Fatalf("class = %q, want %q", class, tc.class)
			}
		})
	}
}

// AC8: For returns the slot the requirement names, or ErrNoCredentialOfClass.
// Mutation M11 returns the PAT for an APIKey requirement.
func TestForReturnsOnlyTheRequiredClass(t *testing.T) {
	file := creds.NewFile()
	if err := file.Put("default", apiURL, creds.Credential{Token: pat}); err != nil {
		t.Fatal(err)
	}

	m, err := file.For("default", creds.RequirePAT, apiURL)
	if err != nil {
		t.Fatalf("RequirePAT: %v", err)
	}
	if m.Class != creds.ClassPAT || m.Credential.Token != pat {
		t.Fatalf("RequirePAT returned %+v", m)
	}

	m, err = file.For("default", creds.RequireAPIKey, apiURL)
	if !errors.Is(err, creds.ErrNoCredentialOfClass) {
		t.Fatalf("RequireAPIKey with only a PAT: err = %v, want ErrNoCredentialOfClass", err)
	}
	if m.Credential.Token != "" {
		t.Fatalf("a token was returned alongside the refusal: %q", m.Credential.Token)
	}
	if !strings.Contains(err.Error(), "api_key") {
		t.Errorf("the refusal does not name the class needed: %v", err)
	}

	// And the mirror image.
	file = creds.NewFile()
	if err := file.Put("default", apiURL, creds.Credential{Token: sandboxKey}); err != nil {
		t.Fatal(err)
	}
	if _, err := file.For("default", creds.RequirePAT, apiURL); !errors.Is(err, creds.ErrNoCredentialOfClass) {
		t.Fatalf("RequirePAT with only an API key: err = %v", err)
	}
	if m, err := file.For("default", creds.RequireEither, apiURL); err != nil || m.Class != creds.ClassAPIKey {
		t.Fatalf("RequireEither with only an API key: %+v %v", m, err)
	}
}

func TestForEitherPrefersTheAPIKey(t *testing.T) {
	file := creds.NewFile()
	if err := file.Put("default", apiURL, creds.Credential{Token: pat}); err != nil {
		t.Fatal(err)
	}
	if err := file.Put("default", apiURL, creds.Credential{Token: sandboxKey}); err != nil {
		t.Fatal(err)
	}
	m, err := file.For("default", creds.RequireEither, apiURL)
	if err != nil {
		t.Fatal(err)
	}
	if m.Class != creds.ClassAPIKey {
		t.Fatalf("RequireEither chose %s", m.Class)
	}
}

// AC9: a credential is bound to the API URL it was stored with; For with a
// different URL returns ErrAPIURLMismatch and not the token. Mutation M12
// ignores api_url in For.
func TestForRefusesADifferentAPIURLAndWithholdsTheToken(t *testing.T) {
	file := creds.NewFile()
	if err := file.Put("default", apiURL, creds.Credential{Token: sandboxKey}); err != nil {
		t.Fatal(err)
	}

	m, err := file.For("default", creds.RequireAPIKey, "https://elsewhere.example")
	if !errors.Is(err, creds.ErrAPIURLMismatch) {
		t.Fatalf("err = %v, want ErrAPIURLMismatch", err)
	}
	if m.Credential.Token != "" {
		t.Fatalf("the token was returned with the refusal: %q", m.Credential.Token)
	}
	if !strings.Contains(err.Error(), apiURL) || !strings.Contains(err.Error(), "elsewhere.example") {
		t.Errorf("the refusal names neither URL: %v", err)
	}

	// The bound URL, and an empty request meaning "whatever the profile is
	// bound to", both succeed.
	if _, err := file.For("default", creds.RequireAPIKey, apiURL); err != nil {
		t.Fatalf("the bound URL was refused: %v", err)
	}
	if m, err := file.For("default", creds.RequireAPIKey, ""); err != nil || m.APIURL != apiURL {
		t.Fatalf("an unspecified URL: %+v %v", m, err)
	}
}

// Same endpoint, different spelling. These must not be refused, or a caller
// who typed a trailing slash cannot use their own credential.
func TestAPIURLEquivalence(t *testing.T) {
	same := [][2]string{
		{"https://ferry.example", "https://ferry.example/"},
		{"https://Ferry.Example", "https://ferry.example"},
		{"HTTPS://ferry.example", "https://ferry.example"},
		{"https://ferry.example:443", "https://ferry.example"},
		{"http://ferry.example:80/v1", "http://ferry.example/v1"},
	}
	for _, p := range same {
		if !creds.SameAPIURL(p[0], p[1]) {
			t.Errorf("%q and %q should be the same endpoint", p[0], p[1])
		}
	}
	different := [][2]string{
		{"https://ferry.example", "http://ferry.example"},
		{"https://ferry.example", "https://ferry.example:8443"},
		{"https://ferry.example", "https://ferry.example.evil.test"},
		{"https://ferry.example/v1", "https://ferry.example/v2"},
		{"https://ferry.example", "https://ferry.example/v1"},
	}
	for _, p := range different {
		if creds.SameAPIURL(p[0], p[1]) {
			t.Errorf("%q and %q are different endpoints", p[0], p[1])
		}
	}
}

// AC9: a profile with no api_url cannot be created.
func TestPutRequiresAnAPIURL(t *testing.T) {
	file := creds.NewFile()
	if err := file.Put("default", "", creds.Credential{Token: sandboxKey}); !errors.Is(err, creds.ErrNoAPIURL) {
		t.Fatalf("err = %v, want ErrNoAPIURL", err)
	}
	if len(file.Profiles) != 0 {
		t.Fatalf("a profile was created without an api_url: %v", file.Profiles)
	}
}

func TestPutRefusesAHostileAPIURL(t *testing.T) {
	for name, raw := range map[string]string{
		"user info":                     "https://user:pass@ferry.example",
		"query":                         "https://ferry.example?token=x",
		"fragment":                      "https://ferry.example#x",
		"file":                          "file:///etc/passwd",
		"relative":                      "ferry.example",
		"no host":                       "https://",
		"scheme only for a unix socket": "unix:///var/run/ferry.sock",
	} {
		t.Run(name, func(t *testing.T) {
			file := creds.NewFile()
			err := file.Put("default", raw, creds.Credential{Token: sandboxKey})
			if err == nil {
				t.Fatalf("%q was accepted as an api_url", raw)
			}
			if len(file.Profiles) != 0 {
				t.Fatalf("a profile was created for a refused URL")
			}
		})
	}
}

// Rebinding a profile to another endpoint would carry a credential verified
// at one host to another; it is refused.
func TestPutRefusesRebindingAProfileToAnotherEndpoint(t *testing.T) {
	file := creds.NewFile()
	if err := file.Put("default", apiURL, creds.Credential{Token: sandboxKey}); err != nil {
		t.Fatal(err)
	}
	err := file.Put("default", "https://other.example", creds.Credential{Token: pat})
	if !errors.Is(err, creds.ErrAPIURLMismatch) {
		t.Fatalf("err = %v, want ErrAPIURLMismatch", err)
	}
	if file.Profiles["default"].PAT != nil {
		t.Fatal("the PAT was stored despite the refusal")
	}
}

func TestPrefixAndDisplayNeverCarryTheWholeToken(t *testing.T) {
	if got, want := creds.Prefix(sandboxKey), "ferry_sk_sandbox_grpQ0HNk"; got != want {
		t.Fatalf("Prefix = %q, want %q", got, want)
	}
	if got, want := creds.Prefix(pat), "ferry_pat_grpQ0HNk"; got != want {
		t.Fatalf("Prefix = %q, want %q", got, want)
	}
	c := creds.Credential{Token: sandboxKey, TokenPrefix: creds.Prefix(sandboxKey)}
	display := c.Display()
	if strings.Contains(sandboxKey, display) {
		t.Fatalf("Display %q is a literal substring of the token", display)
	}
	if !strings.HasSuffix(display, sandboxKey[len(sandboxKey)-4:]) {
		t.Fatalf("Display %q does not end in the last four characters", display)
	}
}

func TestRemove(t *testing.T) {
	file := creds.NewFile()
	if err := file.Put("default", apiURL, creds.Credential{Token: sandboxKey}); err != nil {
		t.Fatal(err)
	}
	if err := file.Put("default", apiURL, creds.Credential{Token: pat}); err != nil {
		t.Fatal(err)
	}

	class := creds.ClassPAT
	if err := file.Remove("default", &class); err != nil {
		t.Fatal(err)
	}
	if file.Profiles["default"].PAT != nil {
		t.Fatal("the PAT slot survived a targeted removal")
	}
	if file.Profiles["default"].APIKey == nil {
		t.Fatal("the api_key slot was removed too")
	}

	if err := file.Remove("default", nil); err != nil {
		t.Fatal(err)
	}
	if _, ok := file.Profiles["default"]; ok {
		t.Fatal("the profile survived a full removal")
	}
	if err := file.Remove("default", nil); !errors.Is(err, creds.ErrNoProfile) {
		t.Fatalf("err = %v, want ErrNoProfile", err)
	}
}

func TestLoadRefusesAFutureSchema(t *testing.T) {
	store, path := newStore(t)
	if err := os.WriteFile(path, []byte(`{"schema":99,"profiles":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(); !errors.Is(err, creds.ErrSchemaUnsupported) {
		t.Fatalf("err = %v, want ErrSchemaUnsupported", err)
	}
}

func TestForOnAnUnknownProfile(t *testing.T) {
	file := creds.NewFile()
	if _, err := file.For("nope", creds.RequireAPIKey, ""); !errors.Is(err, creds.ErrNoProfile) {
		t.Fatalf("err = %v, want ErrNoProfile", err)
	}
}
