//go:build e2e

package e2e_test

import (
	"testing"
)

// AC62 — crash-resume at both fault points, against the real app.
//
// Defends C1 and C2. The property is the one the whole ledger exists for: a
// process killed between writing the record and getting an answer is resumed
// under **the key it already used**, so the resend is a repeat and never a
// second transfer.
//
// # Why the crash is on the execute step and not the simulate
//
// `fault.Die(fault.AfterRecordWritten)` fires on the first step that reaches it,
// and for `transfers create --broadcast` that is the simulate. AC62's assertion
// is about `<run>-execute`, so the run has to be one whose only step is an
// execute — `ferry transfers execute --plan <token>`, priced by a separate
// invocation. That is also the shape an operator hits: the plan is in hand, the
// execute is the risky half, and it is the half worth killing.
//
// # The two points assert different things, deliberately
//
// At `after_record_written` the request never left, so FERRY has never seen the
// key: the resend is the *first* request under it and `Idempotency-Replayed`
// must be **absent**. At `after_send_before_record` the request did leave and
// FERRY answered; the resend is a repeat and the header must be **present**.
// Asserting "it resumed" at both would pass for a CLI that could not tell them
// apart, which is mutation M69 in PLAN §6.2.

// crashedExecute prices a transfer, then kills an execute at `point`, and
// returns the run the dead process left behind.
func crashedExecute(t *testing.T, c *cli, point string) string {
	t.Helper()

	tr := newTransfer(t)

	priced, res := c.json(append([]string{"transfers", "create"}, tr.flags()...)...)
	if res.exitCode != 0 {
		t.Fatalf("pricing the transfer exited %d\nstdout:\n%s\nstderr:\n%s",
			res.exitCode, res.stdout, res.stderr)
	}

	var body struct {
		Plan struct {
			Token string `json:"token"`
		} `json:"plan"`
	}

	unmarshal(t, priced.Response, &body)

	if body.Plan.Token == "" {
		t.Fatalf("the simulate 201 carried no plan token, so there is nothing to execute")
	}

	before := c.pendingRuns()

	crash := c.runFault(point, "transfers", "execute",
		"--plan", body.Plan.Token, "--yes", "--output", "json")

	// 137 is 128+SIGKILL, the exit code `fault.Die` produces precisely so
	// that a simulated death cannot be confused with the CLI reporting an
	// outcome (no class produces it).
	if crash.exitCode != 137 {
		t.Fatalf("the armed execute exited %d, want 137 — the fault point %q did not fire, so "+
			"nothing below is about a crash.\nstdout:\n%s\nstderr:\n%s",
			crash.exitCode, point, crash.stdout, crash.stderr)
	}

	// A killed process reports nothing. Asserted because a crash that had
	// written a document would mean the process survived far enough to
	// classify itself, and the resume below would be resuming a run whose
	// outcome was already established.
	if crash.stdout != "" {
		t.Errorf("the crashed invocation wrote %d bytes to stdout; a killed process announces nothing\n%s",
			len(crash.stdout), crash.stdout)
	}

	after := c.pendingRuns()

	_, fresh := setDiff(before, after)
	if len(fresh) != 1 {
		t.Fatalf("the crash left %d new unresolved run(s) (%v), want exactly 1; before=%v after=%v",
			len(fresh), fresh, before, after)
	}

	return fresh[0]
}

func TestResumeAfterACrashBeforeTheRequestLeftSendsItForTheFirstTime(t *testing.T) {
	c := newCLI(t)
	c.login()
	c.mintSandboxKey()

	run := crashedExecute(t, c, "after_record_written")

	doc, res := c.json("runs", "resume", run, "--yes")
	if res.exitCode != 0 {
		t.Fatalf("runs resume exited %d\nstdout:\n%s\nstderr:\n%s", res.exitCode, res.stdout, res.stderr)
	}

	if doc.HTTP == nil {
		t.Fatalf("the resume document has http == null; it sent nothing")
	}

	if doc.HTTP.Status != 201 {
		t.Errorf("the resume answered %d, want 201", doc.HTTP.Status)
	}

	if doc.HTTP.Replayed {
		t.Errorf("the resume reports replayed == true. The crash was before the send, so FERRY " +
			"had never seen this key and the resend is its first use")
	}

	assertTerminal(t, c, run)
	assertKeyIs(t, doc, run+"-execute")
}

func TestResumeAfterACrashAfterTheRequestLeftReplaysTheSameKey(t *testing.T) {
	c := newCLI(t)
	c.login()
	c.mintSandboxKey()

	run := crashedExecute(t, c, "after_send_before_record")

	doc, res := c.json("runs", "resume", run, "--yes")
	if res.exitCode != 0 {
		t.Fatalf("runs resume exited %d\nstdout:\n%s\nstderr:\n%s", res.exitCode, res.stdout, res.stderr)
	}

	if doc.HTTP == nil {
		t.Fatalf("the resume document has http == null; it sent nothing")
	}

	if !doc.HTTP.Replayed {
		t.Errorf("the resume reports replayed == false. The crashed process had already sent this "+
			"key and FERRY had already answered it, so the resend must be a replay (status %d)",
			doc.HTTP.Status)
	}

	if doc.HTTP.CommandID == nil || *doc.HTTP.CommandID == "" {
		t.Errorf("the replayed answer carries no command_id; there is nothing to follow the " +
			"transfer with")
	}

	key := run + "-execute"
	assertKeyIs(t, doc, key)
	assertTerminal(t, c, run)

	// The claim that is not the same HTTP answer read twice: FERRY's own
	// ledger holds **one** command for this key. A count rather than an
	// existence check, because two rows under one key is the defect (see
	// commands_with_key.rb).
	if got := commandsWithKey(t, key); got != 1 {
		t.Errorf("FERRY holds %d commands rows with caller_idempotency_key %q, want exactly 1. "+
			"The crashed send and the resend are one command or the key bought nothing", got, key)
	}
}

// assertKeyIs holds the resend to the key the run already owned.
//
// This is the assertion mutation M3 breaks: a resume that minted a fresh key
// would send a second transfer under a key FERRY has never seen, and the
// `-execute` suffix is what names the step it belongs to.
func assertKeyIs(t *testing.T, doc document, want string) {
	t.Helper()

	if doc.HTTP == nil || doc.HTTP.IdempotencyKey == nil {
		t.Fatalf("the resume document names no idempotency key, so nothing here is about the key")
	}

	if got := *doc.HTTP.IdempotencyKey; got != want {
		t.Errorf("the resume sent Idempotency-Key %q, want %q — a resume must reuse the run's own key",
			got, want)
	}
}

// assertTerminal reads the run back and requires the step to have settled.
func assertTerminal(t *testing.T, c *cli, run string) {
	t.Helper()

	if pending := c.pendingRuns(); toSet(pending)[run] {
		t.Errorf("run %s is still unresolved after a successful resume (unresolved: %v)", run, pending)
	}
}
