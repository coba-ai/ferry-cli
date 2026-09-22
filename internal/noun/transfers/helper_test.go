package transfers_test

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coba-ai/ferry-cli/internal/cli"
	"github.com/coba-ai/ferry-cli/internal/creds"
	"github.com/coba-ai/ferry-cli/internal/fixture"
	"github.com/coba-ai/ferry-cli/internal/fsx"
	"github.com/coba-ai/ferry-cli/internal/harness"
	"github.com/coba-ai/ferry-cli/internal/noun"
	"github.com/coba-ai/ferry-cli/internal/runs"
	"github.com/coba-ai/ferry-cli/internal/xdg"
)

// No test in this package calls t.Parallel(): `harness.Run` swaps the process
// streams and uses t.Setenv, so two of them in one process interleave (A314).

// Every test drives the **root command**, not `transfers.Command`.
//
// That is A345's lesson applied before it costs anything. The properties this
// package is responsible for — exit 6 after a send, exactly one JSON document,
// a panic that is classified rather than printed — are all decided in
// `internal/cli`'s wrapper. A test that built the noun command directly would
// be asserting the exit code of a `render.Error` rather than the exit code of
// the CLI, and every one of the mutations in §6.2 that lives in the wrapper
// would survive it green.

// canaryKey is the API key every test here logs in with.
//
// `POST /v1/transfers` and `POST /v1/transfers/simulate` accept an API key and
// refuse a PAT, and the fixture matches on credential class derived from the
// token's shape (A331), so a request carrying the wrong one is answered 599
// rather than the recorded body. The 43 characters after the prefix are what
// `creds.Classify` requires and what `render.FindSecrets` recognises.
func canaryKey(t *testing.T) string {
	t.Helper()

	return "ferry_sk_sandbox_" + padded(t, "CANARYSK")
}

func canaryLiveKey(t *testing.T) string {
	t.Helper()

	return "ferry_sk_live_" + padded(t, "CANARYLIVE")
}

func canaryPAT(t *testing.T) string {
	t.Helper()

	return "ferry_pat_" + padded(t, "CANARYPAT")
}

func padded(t *testing.T, tag string) string {
	t.Helper()

	if len(tag) > 43 {
		t.Fatalf("canary tag %q is too long", tag)
	}

	return tag + strings.Repeat("0", 43-len(tag))
}

// principal is the principal id every stored credential here carries, and
// the one the run records will hold. AC70's test changes it deliberately.
const principal = "key_PLACEHOLDER_1"

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
			PrincipalID: principal,
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

// loggedIn is the common arrangement: a fixture answering the named
// scenarios, and a profile holding the canary API key bound to it.
func loggedIn(t *testing.T, scenarios ...string) (*fixture.Server, string) {
	t.Helper()

	server := fixture.New(t, scenarios...)
	home := newHome(t)

	writeProfile(t, home, server.URL(), canaryKey(t))

	return server, home
}

// fixedNow is the instant every expiry render and every deadline is computed
// against.
var fixedNow = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

// clock is a fake that records what it was asked to wait for.
//
// The record is the whole point: AC50's claim is about the *sequence* of
// waits, and a test that only asserted "it eventually polled" would pass
// against `time.Sleep(0)` — which is M50. Time advances by exactly what is
// slept, so a deadline is reached by sleeping rather than by waiting.
type clock struct {
	mu     sync.Mutex
	now    time.Time
	slept  []time.Duration
	frozen bool

	// onSleep runs from inside the nth Sleep, before it returns. It is how
	// a test reaches a window that exists only while the CLI is waiting.
	onSleep func(n int)
}

func newClock() *clock { return &clock{now: fixedNow} }

// frozenClock never advances. It is for a test that wants the deadline never
// to arrive, so that what stops the loop is the command's state and not the
// timeout.
func frozenClock() *clock { return &clock{now: fixedNow, frozen: true} }

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.now
}

func (c *clock) Sleep(d time.Duration) {
	c.mu.Lock()

	c.slept = append(c.slept, d)

	if !c.frozen {
		c.now = c.now.Add(d)
	}

	n, hook := len(c.slept), c.onSleep

	c.mu.Unlock()

	if hook != nil {
		hook(n)
	}
}

func (c *clock) waits() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()

	return append([]time.Duration(nil), c.slept...)
}

// seconds is the wait sequence in whole seconds, which is how AC50 states it.
func (c *clock) seconds() []int {
	out := []int{}
	for _, d := range c.waits() {
		out = append(out, int(d/time.Second))
	}

	return out
}

// invocation is one `ferry …` run.
type invocation struct {
	home  string
	args  []string
	env   map[string]string
	stdin string
	tty   bool

	clock *clock

	// interrupted, when non-nil, replaces the signal handler: the test
	// closes it at the instant it chose. `signals` asks for the real
	// handler instead, which is what AC69(a) needs.
	interrupted <-chan struct{}
	signals     bool

	// promptShown fires after a consent prompt is written and before the
	// read begins (V9). It is how a test delivers a signal into that exact
	// window, which is too short to poll for.
	promptShown func()

	now func() time.Time
}

func run(t *testing.T, inv invocation) (stdout, stderr string, exit int) {
	t.Helper()

	now := inv.now
	if now == nil {
		now = func() time.Time { return fixedNow }
	}

	deps := noun.Deps{
		HTTP: &http.Client{},
		Now:  now,
	}

	if inv.clock != nil {
		deps.Clock = inv.clock
	}

	opts := cli.Options{
		Noun:        deps,
		Interrupted: inv.interrupted,
		PromptShown: inv.promptShown,
	}

	if inv.signals {
		opts.Signals = cli.DefaultSignals
	}

	cmd := cli.New(opts)

	env := map[string]string{"FERRY_HOME": inv.home}
	for k, v := range inv.env {
		env[k] = v
	}

	return harness.Run(t, cmd, inv.args, inv.stdin, env, inv.tty)
}

// ledgerOf reads the run ledger a test's home directory holds.
func ledgerOf(t *testing.T, home string) *runs.Ledger {
	t.Helper()

	return runs.New(fsx.OS(), filepath.Join(home, "runs"), func() time.Time { return fixedNow })
}

// onlyRun is the single run an invocation minted. It fails rather than
// returning nil, because every assertion that follows would otherwise
// dereference a nil and report a panic instead of the missing run.
func onlyRun(t *testing.T, home string) *runs.Run {
	t.Helper()

	all, err := ledgerOf(t, home).List()
	if err != nil {
		t.Fatalf("list runs: %v", err)
	}

	if len(all) != 1 {
		t.Fatalf("want exactly one run on disk, got %d", len(all))
	}

	return all[0]
}

func stepOf(t *testing.T, home, name string) *runs.Step {
	t.Helper()

	step := onlyRun(t, home).Step(name)
	if step == nil {
		t.Fatalf("run has no %s step", name)
	}

	return step
}

// requestsTo counts the requests the fixture received for one path.
//
// Counting at the server is what makes "zero requests" a measurement rather
// than a belief: the CLI's own bookkeeping cannot prove it did not send
// something.
func requestsTo(s *fixture.Server, path string) int {
	n := 0

	for _, req := range s.Requests() {
		if req.Path == path {
			n++
		}
	}

	return n
}

const (
	simulatePath = "/v1/transfers/simulate"
	executePath  = "/v1/transfers"
)

func keysSentTo(s *fixture.Server, path string) []string {
	var out []string

	for _, req := range s.Requests() {
		if req.Path == path {
			out = append(out, req.IdempotencyKey())
		}
	}

	return out
}

func digestsSentTo(s *fixture.Server, path string) []string {
	var out []string

	for _, req := range s.Requests() {
		if req.Path == path {
			out = append(out, req.SHA256)
		}
	}

	return out
}

// document is the `--output json` envelope, decoded loosely.
type document struct {
	FerryCLI struct {
		Version     string  `json:"version"`
		RunID       *string `json:"run_id"`
		Profile     string  `json:"profile"`
		Environment *string `json:"environment"`
	} `json:"ferry_cli"`
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
		t.Fatalf("parse document: %v\nstdout was:\n%s", err, stdout)
	}

	return doc
}

// canonicalSimulateArgs are the flags that reproduce `simulate.201`'s
// recorded request body exactly.
//
// The fixture compares the body structurally and answers 599 for anything
// else (AC30), so these flags are checked against the recording on every run
// of every test in this package rather than only in `body_test.go`.
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

// recordedPlanToken is the token `simulate.201` hands out and the token
// `execute.*` recordings expect in the request body.
const recordedPlanToken = "ferry_plan_PLACEHOLDER"

// bodyOfRequestTo is the nth request body the fixture received for a path.
func bodyOfRequestTo(t *testing.T, s *fixture.Server, path string, n int) string {
	t.Helper()

	seen := 0

	for _, req := range s.Requests() {
		if req.Path != path {
			continue
		}

		if seen == n {
			return string(req.Body)
		}

		seen++
	}

	t.Fatalf("the fixture saw no request %d to %s", n, path)

	return ""
}

func mustReadFile(t *testing.T, path string) string {
	t.Helper()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	return string(raw)
}
