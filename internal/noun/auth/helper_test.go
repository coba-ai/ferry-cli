package auth_test

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/kurenn/ferry/cli/internal/creds"
	"github.com/kurenn/ferry/cli/internal/fixture"
	"github.com/kurenn/ferry/cli/internal/fsx"
	"github.com/kurenn/ferry/cli/internal/harness"
	"github.com/kurenn/ferry/cli/internal/noun"
	"github.com/kurenn/ferry/cli/internal/noun/auth"
	"github.com/kurenn/ferry/cli/internal/xdg"
)

// No test in this package calls t.Parallel(): `harness.Run` swaps the process
// streams and uses t.Setenv, and neither is safe under it (A314).

// Canary credentials, 43 characters after the prefix as `lib/ferry/tokens.rb`
// requires. They are canaries and not placeholders: `secrets_test.go` and the
// assertions below search transcripts for them, so a token that reached
// stdout would be found by its own distinctive body.
func canaryBody(t *testing.T, tag string) string {
	t.Helper()

	body := strings.ToUpper("canary" + tag)
	if len(body) > 43 {
		t.Fatalf("canary tag %q is too long", tag)
	}

	body += strings.Repeat("0", 43-len(body))

	return body
}

func sandboxKey(t *testing.T) string { return "ferry_sk_sandbox_" + canaryBody(t, "sk") }
func liveKey(t *testing.T) string    { return "ferry_sk_live_" + canaryBody(t, "live") }
func patToken(t *testing.T) string   { return "ferry_pat_" + canaryBody(t, "pat") }

// newHome is an empty $FERRY_HOME.
func newHome(t *testing.T) string {
	t.Helper()

	home := t.TempDir()

	paths := xdg.Paths{ConfigDir: home, StateDir: home, Home: home}
	if err := paths.EnsureAll(fsx.OS()); err != nil {
		t.Fatalf("create %s: %v", home, err)
	}

	return home
}

// writeProfile stores the given tokens against apiURL.
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
			PrincipalID: "stored_" + token[:12],
			StoredAt:    time.Unix(0, 0).UTC(),
		})
		if err != nil {
			t.Fatalf("put %s: %v", token, err)
		}
	}

	if err := store.Save(file); err != nil {
		t.Fatalf("save credentials: %v", err)
	}
}

// readCredentials reads the raw credentials document, or nil when there is
// none. A missing file is a distinct answer from an empty one: AC35's "stores
// only on a 200" is the claim that the file was never created.
func readCredentials(t *testing.T, home string) []byte {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join(home, "credentials.json"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}

		t.Fatalf("read credentials: %v", err)
	}

	return raw
}

func storedProfile(t *testing.T, home string) creds.Profile {
	t.Helper()

	raw := readCredentials(t, home)
	if raw == nil {
		t.Fatal("no credentials file was written")
	}

	var file creds.File
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatalf("parse credentials: %v", err)
	}

	return file.Profiles[creds.DefaultProfile]
}

// invocation is one `ferry auth …` run.
type invocation struct {
	home   string
	args   []string
	stdin  string
	tty    bool
	env    map[string]string
	deps   func(*noun.Deps)
	server *fixture.Server
}

// run builds the command the way U5's root will and drives it through the
// harness.
//
// The command is rebuilt for every run: cobra keeps parsed flag values on the
// command, so a reused one would carry the previous invocation's state.
func run(t *testing.T, inv invocation) (stdout, stderr string, exit int) {
	t.Helper()

	globals := &noun.Globals{}

	deps := noun.Deps{
		Globals: globals,
		HTTP:    &http.Client{},
		Now:     func() time.Time { return time.Unix(1_700_000_000, 0).UTC() },
	}

	if inv.deps != nil {
		inv.deps(&deps)
	}

	cmd := auth.Command(deps)
	noun.BindGlobals(cmd, globals)

	env := map[string]string{"FERRY_HOME": inv.home}
	for k, v := range inv.env {
		env[k] = v
	}

	return harness.Run(t, cmd, inv.args, inv.stdin, env, inv.tty)
}

// requestCount is the fixture's own log, which is what "zero requests" has to
// be read off: the absence of an error says nothing about whether a socket was
// opened.
func requestCount(s *fixture.Server) int { return len(s.Requests()) }

// flagNames is every flag reachable from cmd, its own and the ones it
// inherits. AC33's "there is no --token" is a claim about both: a persistent
// flag on the root would be just as readable from the process table.
//
// pflag is imported directly here. It is `// indirect` in go.mod and stays
// that way: A313 and §4.3.4 freeze the file, and `go mod tidy` — which would
// reclassify it — is not run by any unit after U1.
func flagNames(cmd *cobra.Command) []string {
	seen := map[string]bool{}

	// Both sets, deduplicated: cobra merges a parent's persistent flags into
	// the child's own set at parse time, so the two overlap.
	add := func(f *pflag.Flag) { seen[f.Name] = true }

	cmd.Flags().VisitAll(add)
	cmd.InheritedFlags().VisitAll(add)

	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}

	return names
}
