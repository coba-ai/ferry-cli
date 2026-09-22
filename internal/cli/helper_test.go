package cli_test

import (
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coba-ai/ferry-cli/internal/cli"
	"github.com/coba-ai/ferry-cli/internal/creds"
	"github.com/coba-ai/ferry-cli/internal/fixture"
	"github.com/coba-ai/ferry-cli/internal/fsx"
	"github.com/coba-ai/ferry-cli/internal/harness"
	"github.com/coba-ai/ferry-cli/internal/noun"
	"github.com/coba-ai/ferry-cli/internal/xdg"
)

// No test in this package calls t.Parallel(): harness.Run swaps the process
// streams (A314).

var fixedNow = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

const principal = "key_PLACEHOLDER_1"

func apiKey() string { return "ferry_sk_sandbox_" + strings.Repeat("C", 43) }

// A331: credential class is a match axis, so a profile holding only an API
// key cannot drive `keys get` — that endpoint accepts a personal access
// token and the fixture answers 599 for the wrong class. Both are stored.
func pat() string { return "ferry_pat_" + strings.Repeat("P", 43) }

func newHome(t *testing.T) string {
	t.Helper()

	home := t.TempDir()

	paths := xdg.Paths{ConfigDir: home, StateDir: home, Home: home}
	if err := paths.EnsureAll(fsx.OS()); err != nil {
		t.Fatalf("create %s: %v", home, err)
	}

	return home
}

// loggedIn is a fixture plus a profile bound to it.
func loggedIn(t *testing.T, scenarios ...string) (*fixture.Server, string) {
	t.Helper()

	server := fixture.New(t, scenarios...)
	home := newHome(t)

	store := creds.NewStore(fsx.OS(), filepath.Join(home, "credentials.json"))

	file, err := store.Load()
	if err != nil {
		t.Fatalf("load credentials: %v", err)
	}

	for _, token := range []string{apiKey(), pat()} {
		err = file.Put(creds.DefaultProfile, server.URL(), creds.Credential{
			Token:       token,
			PrincipalID: principal,
			StoredAt:    fixedNow,
		})
		if err != nil {
			t.Fatalf("put %s: %v", token, err)
		}
	}

	if err := store.Save(file); err != nil {
		t.Fatalf("save credentials: %v", err)
	}

	return server, home
}

type clock struct {
	now time.Time
}

func (c *clock) Now() time.Time        { return c.now }
func (c *clock) Sleep(d time.Duration) { c.now = c.now.Add(d) }

type invocation struct {
	home  string
	args  []string
	env   map[string]string
	stdin string
	tty   bool

	interrupted <-chan struct{}
	signals     bool
	promptShown func()
}

func run(t *testing.T, inv invocation) (stdout, stderr string, exit int) {
	t.Helper()

	opts := cli.Options{
		Noun: noun.Deps{
			HTTP:  &http.Client{},
			Now:   func() time.Time { return fixedNow },
			Clock: &clock{now: fixedNow},
		},
		Interrupted: inv.interrupted,
		PromptShown: inv.promptShown,

		// The raw argv, exactly as main.go passes os.Args[1:]. It is how
		// `--output json` survives a flag parse that failed before
		// reaching it (AC41).
		Args: inv.args,
	}

	if inv.signals {
		opts.Signals = cli.DefaultSignals
	}

	env := map[string]string{"FERRY_HOME": inv.home}
	for k, v := range inv.env {
		env[k] = v
	}

	return harness.Run(t, cli.New(opts), inv.args, inv.stdin, env, inv.tty)
}

func canonicalSimulateArgs() []string {
	return []string{
		"--customer", "cst_PLACEHOLDER_1",
		"--from-type", "walletCrypto",
		"--from-id", "wlt_PLACEHOLDER_1",
		"--from-asset", "usdc",
		"--from-network", "polygon",
		"--to-type", "bankUs",
		"--to-id", "ext_PLACEHOLDER_1",
		"--to-asset", "usd",
		"--to-network", "ach",
		"--to-account-holder", "customer",
		"--amount", "100.00",
		"--amount-side", "source",
		"--metadata", "order_id=order_PLACEHOLDER_1",
	}
}

const recordedPlanToken = "ferry_plan_PLACEHOLDER"

const (
	simulatePath = "/v1/transfers/simulate"
	executePath  = "/v1/transfers"
)

// requestsTo counts requests at the fixture, which is the only witness the
// CLI cannot talk out of what it saw.
func requestsTo(s *fixture.Server, path string) int {
	n := 0

	for _, req := range s.Requests() {
		if req.Path == path {
			n++
		}
	}

	return n
}
