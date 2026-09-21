package keys_test

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kurenn/ferry-cli/internal/creds"
	"github.com/kurenn/ferry-cli/internal/fixture"
	"github.com/kurenn/ferry-cli/internal/fsx"
	"github.com/kurenn/ferry-cli/internal/harness"
	"github.com/kurenn/ferry-cli/internal/noun"
	"github.com/kurenn/ferry-cli/internal/noun/keys"
	"github.com/kurenn/ferry-cli/internal/xdg"
)

// No test here calls t.Parallel(): `harness.Run` swaps the process streams and
// uses t.Setenv (A314).

// canaryPAT is the personal access token every test in this package logs in
// with. Every endpoint under `ferry keys` accepts a PAT and refuses an API key
// (`api_keys_controller.rb:27`), and the fixture matches on credential class
// (A331), so a request made with the wrong one would not be answered at all.
//
// It is 43 characters after the prefix, which is what `creds.Classify` and
// `render.FindSecrets` both require — a shorter stand-in would be refused by
// the first and invisible to the second, and AC42's scan needs a secret that
// is actually detectable to have anything to find.
func canaryPAT(t *testing.T) string {
	t.Helper()

	return "ferry_pat_" + padded(t, "CANARYPAT")
}

func canaryKey(t *testing.T) string {
	t.Helper()

	return "ferry_sk_sandbox_" + padded(t, "CANARYSK")
}

func padded(t *testing.T, tag string) string {
	t.Helper()

	if len(tag) > 43 {
		t.Fatalf("canary tag %q is too long", tag)
	}

	return tag + strings.Repeat("0", 43-len(tag))
}

func newHome(t *testing.T) string {
	t.Helper()

	home := t.TempDir()

	paths := xdg.Paths{ConfigDir: home, StateDir: home, Home: home}
	if err := paths.EnsureAll(fsx.OS()); err != nil {
		t.Fatalf("create %s: %v", home, err)
	}

	return home
}

func writeProfile(t *testing.T, home, apiURL string, tokens ...string) {
	t.Helper()

	store := creds.NewStore(fsx.OS(), filepath.Join(home, "credentials.json"))

	file, err := store.Load()
	if err != nil {
		t.Fatalf("load credentials: %v", err)
	}

	for _, token := range tokens {
		err := file.Put(creds.DefaultProfile, apiURL, creds.Credential{
			Token:       token,
			PrincipalID: "pat_PLACEHOLDER_1",
			StoredAt:    time.Unix(0, 0).UTC(),
		})
		if err != nil {
			t.Fatalf("put: %v", err)
		}
	}

	if err := store.Save(file); err != nil {
		t.Fatalf("save credentials: %v", err)
	}
}

// loggedIn is the common arrangement: a fixture server and a profile holding
// the canary PAT bound to it.
func loggedIn(t *testing.T, scenarios ...string) (*fixture.Server, string) {
	t.Helper()

	server := fixture.New(t, scenarios...)
	home := newHome(t)

	writeProfile(t, home, server.URL(), canaryPAT(t))

	return server, home
}

func storedProfile(t *testing.T, home string) creds.Profile {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join(home, "credentials.json"))
	if err != nil {
		if os.IsNotExist(err) {
			return creds.Profile{}
		}

		t.Fatalf("read credentials: %v", err)
	}

	var file creds.File
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatalf("parse credentials: %v", err)
	}

	return file.Profiles[creds.DefaultProfile]
}

type invocation struct {
	home string
	args []string
	env  map[string]string
	deps func(*noun.Deps)
}

// fixedNow is the clock every `--expires-in` assertion is computed against.
var fixedNow = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

func run(t *testing.T, inv invocation) (stdout, stderr string, exit int) {
	t.Helper()

	globals := &noun.Globals{}

	deps := noun.Deps{
		Globals: globals,
		HTTP:    &http.Client{},
		Now:     func() time.Time { return fixedNow },
	}

	if inv.deps != nil {
		inv.deps(&deps)
	}

	cmd := keys.Command(deps)
	noun.BindGlobals(cmd, globals)

	env := map[string]string{"FERRY_HOME": inv.home}
	for k, v := range inv.env {
		env[k] = v
	}

	return harness.Run(t, cmd, inv.args, "", env, false)
}

func requestCount(s *fixture.Server) int { return len(s.Requests()) }

// document is the `--output json` envelope, decoded loosely: `response` is
// kept raw so a test can say what it wants about the shape rather than being
// limited to what a struct here happened to name.
type document struct {
	Outcome struct {
		Class       string   `json:"class"`
		ExitCode    int      `json:"exit_code"`
		Money       string   `json:"money"`
		SameKeySafe bool     `json:"same_key_safe"`
		Next        string   `json:"next"`
		Warnings    []string `json:"warnings"`
	} `json:"outcome"`
	HTTP     json.RawMessage `json:"http"`
	Response json.RawMessage `json:"response"`
	Error    json.RawMessage `json:"error"`
}

func decode(t *testing.T, stdout string) document {
	t.Helper()

	var doc document
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("parse document: %v\n%s", err, stdout)
	}

	return doc
}
