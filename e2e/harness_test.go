//go:build e2e

// Package e2e_test drives the built `ferry` binary against a running FERRY
// with FERRY_OMS_BACKEND=fixture (PLAN §5.14, AC61–AC63).
//
// # Why these tests are behind a build tag
//
// They need three things `go test ./...` cannot make: a Postgres, a Rails app
// and a credential. A file that compiled into the default suite would have to
// decide what to do when they are absent, and every answer to that is worse
// than not compiling — a skip reads as a pass in the summary, and a failure
// makes the default suite unrunnable on a laptop.
//
// So the tag is the gate, and inside it there is **no skip**. Every variable
// below is required, and a missing one is `t.Fatal` with the name in the
// message. A suite that ran with no credential and quietly passed nothing is
// the failure mode this file exists to prevent, and it is the same failure the
// e2e CI job is shaped around (A411).
//
// # Running it
//
// `e2e/run.sh` does all of it — bootstrap, server, binaries, this suite — and
// is what CI runs too, so the two cannot drift. See e2e/README.md.
package e2e_test

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The environment this suite is given. PLAN §5.14 names the first four;
// railsDirVar is A412's addition, because AC62's `commands` row count needs a
// `bin/rails runner` and the app is no longer in this repository.
const (
	apiURLVar   = "FERRY_API_URL"
	patVar      = "FERRY_E2E_PAT"
	binVar      = "FERRY_E2E_BIN"
	faultBinVar = "FERRY_E2E_FAULT_BIN"
	railsDirVar = "FERRY_E2E_RAILS_DIR"
)

// commandTimeout bounds one `ferry` invocation.
//
// Generous, and bounded anyway: a money command that hangs would otherwise
// hold the runner until the job's own timeout, and the diagnosis "the job
// timed out" does not say which command did it.
const commandTimeout = 90 * time.Second

// env reads a required variable, or fails naming it.
//
// Never a skip. See the package comment.
func env(t *testing.T, name string) string {
	t.Helper()

	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		t.Fatalf("%s is unset or empty. This suite drives a real FERRY and cannot invent one; "+
			"run it through e2e/run.sh, which sets every variable (PLAN §5.14).", name)
	}

	return v
}

// cli is one profile's worth of CLI: a binary, an endpoint and a FERRY_HOME.
type cli struct {
	t *testing.T

	bin      string
	faultBin string
	apiURL   string
	home     string
	pat      string
}

// newCLI gives the test a profile of its own.
//
// A fresh FERRY_HOME per test is not tidiness. The orphan check (AC81) refuses
// to start a new run while an unresolved one exists in the same profile, so
// two tests sharing a home would have the second refused by the first's
// wreckage — and a crash-resume test deliberately leaves wreckage.
func newCLI(t *testing.T) *cli {
	t.Helper()

	return &cli{
		t:        t,
		bin:      env(t, binVar),
		faultBin: env(t, faultBinVar),
		apiURL:   env(t, apiURLVar),
		pat:      env(t, patVar),
		home:     t.TempDir(),
	}
}

// result is one invocation's outcome.
type result struct {
	stdout   string
	stderr   string
	exitCode int
}

// document is the single JSON document `--output json` writes (PLAN §5.9).
//
// Only the members these tests read are declared. `HTTP` is a pointer because
// AC63's first refusal is exactly `http == null`, and a value type cannot tell
// "absent" from "a zero-valued block".
type document struct {
	FerryCLI struct {
		Version     string  `json:"version"`
		RunID       *string `json:"run_id"`
		Profile     string  `json:"profile"`
		Environment *string `json:"environment"`
	} `json:"ferry_cli"`

	Outcome struct {
		Class    string `json:"class"`
		ExitCode int    `json:"exit_code"`
		Money    string `json:"money"`
		Next     string `json:"next"`
	} `json:"outcome"`

	HTTP *struct {
		Status         int     `json:"status"`
		CommandID      *string `json:"command_id"`
		IdempotencyKey *string `json:"idempotency_key"`
		Replayed       bool    `json:"replayed"`
	} `json:"http"`

	Response json.RawMessage `json:"response"`
	Error    *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// run invokes the release-shaped binary.
func (c *cli) run(args ...string) result {
	c.t.Helper()

	return c.exec(c.bin, nil, "", args...)
}

// runFault invokes the `faultinject` binary with one fault point armed.
func (c *cli) runFault(point string, args ...string) result {
	c.t.Helper()

	return c.exec(c.faultBin, []string{"FERRY_CLI_FAULT=" + point}, "", args...)
}

func (c *cli) exec(bin string, extraEnv []string, stdin string, args ...string) result {
	c.t.Helper()

	cmd := exec.Command(bin, args...)
	cmd.Env = append(append(os.Environ(),
		"FERRY_HOME="+c.home,
		"FERRY_API_URL="+c.apiURL,
		// The CLI colours by TTY detection, and there is no TTY here; set
		// anyway so a failure diff is never about escape codes.
		"NO_COLOR=1",
	), extraEnv...)

	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}

	var out, errOut strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errOut

	done := make(chan error, 1)

	if err := cmd.Start(); err != nil {
		c.t.Fatalf("start %s %v: %v", bin, args, err)
	}

	go func() { done <- cmd.Wait() }()

	var waitErr error

	select {
	case waitErr = <-done:
	case <-time.After(commandTimeout):
		_ = cmd.Process.Kill()
		<-done
		c.t.Fatalf("`ferry %s` did not return within %s.\nstdout:\n%s\nstderr:\n%s",
			strings.Join(args, " "), commandTimeout, out.String(), errOut.String())
	}

	code := 0

	var exit *exec.ExitError
	if errors.As(waitErr, &exit) {
		code = exit.ExitCode()
	} else if waitErr != nil {
		c.t.Fatalf("`ferry %s`: %v", strings.Join(args, " "), waitErr)
	}

	return result{stdout: out.String(), stderr: errOut.String(), exitCode: code}
}

// json runs a command in JSON mode and parses the one document it owes.
func (c *cli) json(args ...string) (document, result) {
	c.t.Helper()

	res := c.run(append(args, "--output", "json")...)

	return c.parse(res, args), res
}

func (c *cli) parse(res result, args []string) document {
	c.t.Helper()

	var doc document
	if err := json.Unmarshal([]byte(res.stdout), &doc); err != nil {
		c.t.Fatalf("`ferry %s --output json` did not write one parseable document (%v).\n"+
			"stdout:\n%s\nstderr:\n%s", strings.Join(args, " "), err, res.stdout, res.stderr)
	}

	// AC41's equality, asserted on every document this suite reads rather
	// than once: the exit code and the document are decided together, and a
	// path where they disagree is a path where neither can be trusted.
	if doc.Outcome.ExitCode != res.exitCode {
		c.t.Errorf("`ferry %s`: outcome.exit_code is %d and the process exited %d; "+
			"AC41 requires them equal", strings.Join(args, " "), doc.Outcome.ExitCode, res.exitCode)
	}

	return doc
}

// login stores the bootstrap's PAT in this profile, through the only route
// that carries a token into the process (AC33).
func (c *cli) login() {
	c.t.Helper()

	res := c.exec(c.bin, nil, c.pat+"\n", "auth", "login", "--token-stdin", "--api", c.apiURL, "--output", "json")
	if res.exitCode != 0 {
		c.t.Fatalf("auth login exited %d\nstdout:\n%s\nstderr:\n%s", res.exitCode, res.stdout, res.stderr)
	}
}

// mintSandboxKey is AC61's third step, and the precondition of every money
// command: a PAT cannot simulate or execute, so the money scopes arrive on an
// API key the PAT mints.
func (c *cli) mintSandboxKey() document {
	c.t.Helper()

	doc, res := c.json("keys", "create",
		"--env", "sandbox",
		"--name", "cli-e2e",
		"--scopes", "read,money:simulate,money:execute",
		"--login")

	if res.exitCode != 0 {
		c.t.Fatalf("keys create exited %d\nstdout:\n%s\nstderr:\n%s", res.exitCode, res.stdout, res.stderr)
	}

	return doc
}

// transfer is one well-formed transfer's worth of flags.
//
// walletCrypto -> bankUs, which is the corridor the API's own fixture spec
// uses (`spec/support/corridors/simulate_requests.rb`) and the one its OMS
// document's `getQuote` example describes, so the fixture has a body of the
// right shape to answer with.
type transfer struct {
	customer    string
	source      string
	destination string
	amount      string
}

// newTransfer mints identifiers no other transfer has used.
//
// The 26-hexadecimal-character suffix is load-bearing and was measured, not
// guessed. An identifier the OMS contract cannot echo — `cst_e2e1234` was the
// one tried — is *not* refused as a 422. The fixture answers, FERRY's
// `Commands::Correlation` declines the answer, the command parks in
// `upstream_unknown`, and the CLI correctly reports `pending`/6 after its poll
// timeout. So a malformed id turns AC61's happy path into a two-minute exit 6
// with no indication that the id was the problem, which is why the shape is
// stated here with this comment rather than left to a helper.
func newTransfer(t *testing.T) transfer {
	t.Helper()

	return transfer{
		customer:    omsID(t, "cst"),
		source:      omsID(t, "wlt"),
		destination: omsID(t, "ext"),
		amount:      "100.00",
	}
}

func omsID(t *testing.T, prefix string) string {
	t.Helper()

	b := make([]byte, 13)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("read random bytes for an OMS id: %v", err)
	}

	return prefix + "_" + hex.EncodeToString(b)
}

// flags is the transfer as `ferry transfers create` takes it.
func (tr transfer) flags() []string {
	return []string{
		"--customer", tr.customer,
		"--amount", tr.amount,
		"--amount-side", "source",
		"--from-type", "walletCrypto",
		"--from-id", tr.source,
		"--from-asset", "usdc",
		"--from-network", "polygon",
		"--to-type", "bankUs",
		"--to-id", tr.destination,
		"--to-asset", "usd",
		"--to-network", "ach",
		"--to-account-holder", "customer",
	}
}

// unmarshal parses a document's `response` into v, naming the raw bytes when it
// cannot. A bare `json.Unmarshal` error says "cannot unmarshal string into
// field X" and not which answer it came from.
func unmarshal(t *testing.T, raw json.RawMessage, v any) {
	t.Helper()

	if err := json.Unmarshal(raw, v); err != nil {
		t.Fatalf("parse the response body: %v\n%s", err, raw)
	}
}

// runID reads the run the document names, failing if it names none.
func runID(t *testing.T, doc document) string {
	t.Helper()

	if doc.FerryCLI.RunID == nil {
		t.Fatalf("the document carries no run_id; every money command mints one (PLAN §5.3)")
	}

	return *doc.FerryCLI.RunID
}

// pendingRuns lists the unresolved runs in this profile.
//
// Used to find the run a crashed invocation left behind: the process died
// before writing anything to stdout, so the ledger is the only place its id
// exists.
func (c *cli) pendingRuns() []string {
	c.t.Helper()

	doc, res := c.json("runs", "list", "--pending")
	if res.exitCode != 0 {
		c.t.Fatalf("runs list --pending exited %d\nstdout:\n%s\nstderr:\n%s",
			res.exitCode, res.stdout, res.stderr)
	}

	var body struct {
		Data []struct {
			RunID string `json:"run_id"`
		} `json:"data"`
	}

	if err := json.Unmarshal(doc.Response, &body); err != nil {
		c.t.Fatalf("parse runs list response: %v\n%s", err, doc.Response)
	}

	ids := make([]string, 0, len(body.Data))
	for _, r := range body.Data {
		ids = append(ids, r.RunID)
	}

	return ids
}

// commandsWithKey asks the API's database how many `commands` rows carry a
// caller idempotency key, through the API repository's own Rails.
//
// This is the one assertion in the suite that does not go through the CLI, and
// it is the point of AC62's second branch: "the resend replayed" is a claim
// about FERRY's ledger, and the only witness to it that is not the same HTTP
// answer the CLI already read is the row count.
func commandsWithKey(t *testing.T, key string) int {
	t.Helper()

	dir := env(t, railsDirVar)

	script, err := filepath.Abs("commands_with_key.rb")
	if err != nil {
		t.Fatalf("resolve commands_with_key.rb: %v", err)
	}

	cmd := exec.Command("bin/rails", "runner", script, key)
	cmd.Dir = dir
	cmd.Env = os.Environ()

	var out, errOut strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errOut

	if err := cmd.Run(); err != nil {
		t.Fatalf("bin/rails runner commands_with_key.rb in %s: %v\nstdout:\n%s\nstderr:\n%s",
			dir, err, out.String(), errOut.String())
	}

	var answer struct {
		Key   string `json:"key"`
		Count int    `json:"count"`
	}

	if err := json.Unmarshal([]byte(strings.TrimSpace(out.String())), &answer); err != nil {
		t.Fatalf("commands_with_key.rb wrote no parseable answer (%v):\n%s\nstderr:\n%s",
			err, out.String(), errOut.String())
	}

	if answer.Key != key {
		t.Fatalf("commands_with_key.rb answered for key %q, not %q", answer.Key, key)
	}

	return answer.Count
}
