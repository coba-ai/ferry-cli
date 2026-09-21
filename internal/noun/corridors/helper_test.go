package corridors_test

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kurenn/ferry/cli/internal/creds"
	"github.com/kurenn/ferry/cli/internal/fixture"
	"github.com/kurenn/ferry/cli/internal/fsx"
	"github.com/kurenn/ferry/cli/internal/harness"
	"github.com/kurenn/ferry/cli/internal/noun"
	"github.com/kurenn/ferry/cli/internal/noun/corridors"
	"github.com/kurenn/ferry/cli/internal/xdg"
)

// No test here calls t.Parallel(): `harness.Run` swaps the process streams and
// uses t.Setenv (A314).

// `GET /v1/corridors` accepts either credential class, and U0 recorded it with
// an API key. The fixture matches on class (A331), so the canary has to be one
// — a PAT here would be answered 599 and the test would be measuring the
// fixture rather than the CLI.
func canaryKey(t *testing.T) string {
	t.Helper()

	const tag = "CANARYCORRIDOR"

	return "ferry_sk_sandbox_" + tag + strings.Repeat("0", 43-len(tag))
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

// loggedIn is a fixture server and a profile holding the canary key bound to
// it.
func loggedIn(t *testing.T, scenarios ...string) (*fixture.Server, string) {
	t.Helper()

	server := fixture.New(t, scenarios...)
	home := newHome(t)

	store := creds.NewStore(fsx.OS(), filepath.Join(home, "credentials.json"))

	file, err := store.Load()
	if err != nil {
		t.Fatalf("load credentials: %v", err)
	}

	err = file.Put(creds.DefaultProfile, server.URL(), creds.Credential{
		Token:       canaryKey(t),
		PrincipalID: "key_PLACEHOLDER_1",
		StoredAt:    time.Unix(0, 0).UTC(),
	})
	if err != nil {
		t.Fatalf("put: %v", err)
	}

	if err := store.Save(file); err != nil {
		t.Fatalf("save credentials: %v", err)
	}

	return server, home
}

type invocation struct {
	home string
	args []string
}

func run(t *testing.T, inv invocation) (stdout, stderr string, exit int) {
	t.Helper()

	globals := &noun.Globals{}

	deps := noun.Deps{
		Globals: globals,
		HTTP:    &http.Client{},
		Now:     func() time.Time { return time.Unix(1_700_000_000, 0).UTC() },
	}

	cmd := corridors.Command(deps)
	noun.BindGlobals(cmd, globals)

	return harness.Run(t, cmd, inv.args, "", map[string]string{"FERRY_HOME": inv.home}, false)
}

func requestCount(s *fixture.Server) int { return len(s.Requests()) }

func response(t *testing.T, stdout string) json.RawMessage {
	t.Helper()

	var doc struct {
		Response json.RawMessage `json:"response"`
	}

	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("parse document: %v\n%s", err, stdout)
	}

	return doc.Response
}
