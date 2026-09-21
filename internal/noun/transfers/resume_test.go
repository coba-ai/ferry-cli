//go:build faultinject

package transfers_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/kurenn/ferry-cli/internal/creds"
	"github.com/kurenn/ferry-cli/internal/fault"
	"github.com/kurenn/ferry-cli/internal/fsx"
	"github.com/kurenn/ferry-cli/internal/runs"
)

// `runs resume` on a step that may already have been sent. Every test here
// arranges the `pending` state with a real crash rather than by writing a
// record, so the state under test is one the CLI can actually produce.

// pendingRun crashes an execute after the send and returns the run id.
func pendingRun(t *testing.T, home string) string {
	t.Helper()

	_, _, exit := run(t, invocation{
		home: home,
		env:  map[string]string{fault.EnvVar: fault.AfterSendBeforeRecord},
		args: []string{"transfers", "execute", "--plan", recordedPlanToken, "--yes"},
	})

	if exit != fault.CrashExit {
		t.Fatalf("the arranging crash exited %d, want %d", exit, fault.CrashExit)
	}

	id := onlyRun(t, home).RunID

	if state := stepOf(t, home, runs.StepExecute).State; state != runs.StatePending {
		t.Fatalf("the arranged step is %s, want pending", state)
	}

	return id
}

// AC73: a `pending` step with nobody to ask is exit **6**, not 1.
//
// This is the sharpest case in the unit. The CLI is refusing — it has no
// consent — and a refusal ordinarily means "nothing happened". Here
// something may already have happened, and `cli_fault`'s sentence is
// "nothing was sent". Exit 1 would send a caller away believing the
// transfer never left.
func TestResumingAPendingStepWithNoTerminalIsSix(t *testing.T) {
	server, home := loggedIn(t, "execute.201.processing", "execute.replay.201")

	id := pendingRun(t, home)

	before := requestsTo(server, executePath)

	stdout, stderr, exit := run(t, invocation{
		home: home,
		args: []string{"runs", "resume", id},
	})

	// M79/M102.
	if exit != 6 {
		t.Fatalf("exit = %d, want 6: the step may already have been sent, so \"nothing was "+
			"sent\" is false\n%s\n%s", exit, stdout, stderr)
	}

	// And it did not send. A refusal that sent would be the worst of both.
	if got := requestsTo(server, executePath); got != before {
		t.Errorf("execute requests went from %d to %d on a refusal", before, got)
	}

	if state := stepOf(t, home, runs.StepExecute).State; state != runs.StatePending {
		t.Errorf("execute step is %s, want pending", state)
	}

	report := stdout + stderr

	// The remedy is `--yes`, under the same key, and the report has to say
	// so — and must not suggest a new key.
	if !strings.Contains(report, "--yes") {
		t.Errorf("the refusal does not name --yes\n%s", report)
	}

	if !strings.Contains(report, id) {
		t.Errorf("the refusal does not name the run\n%s", report)
	}
}

// The contrasting case: an `awaiting_confirmation` step with nobody to ask
// is exit 1, because nothing was sent.
//
// The pair is the control for the test above. Without it, "resume refuses
// with 6" is satisfied by a CLI that reports 6 for every headless resume —
// which would tell a caller whose prompt was killed before it sent that
// their money might have moved.
func TestResumingAnUnsentStepWithNoTerminalIsOne(t *testing.T) {
	server, home := loggedIn(t, "execute.201.processing")

	// A crash at the prompt, which writes `awaiting_confirmation` and then
	// dies before the read.
	_, _, exit := run(t, invocation{
		home:  home,
		tty:   true,
		stdin: "y\n",
		env:   map[string]string{fault.EnvVar: fault.AtPrompt},
		args:  []string{"transfers", "execute", "--plan", recordedPlanToken},
	})

	if exit != fault.CrashExit {
		t.Fatalf("the arranging crash exited %d, want %d", exit, fault.CrashExit)
	}

	if got := requestsTo(server, executePath); got != 0 {
		t.Fatalf("the arranging crash sent %d request(s), want 0", got)
	}

	step := stepOf(t, home, runs.StepExecute)

	if step.State != runs.StateAwaitingConfirmation {
		t.Fatalf("the arranged step is %s, want awaiting_confirmation", step.State)
	}

	id := onlyRun(t, home).RunID

	stdout, stderr, exit := run(t, invocation{home: home, args: []string{"runs", "resume", id}})

	if exit != 1 {
		t.Fatalf("exit = %d, want 1: nothing was sent, so the caller may simply run it again\n%s\n%s",
			exit, stdout, stderr)
	}

	if got := requestsTo(server, executePath); got != 0 {
		t.Errorf("execute requests = %d, want 0", got)
	}
}

// AC70: a resume whose profile now holds a different credential is refused.
//
// The recorded request was authorised by one principal. Resending it under
// another is a different caller's transfer under the first caller's key, and
// FERRY answers that `409 IDEMPOTENCY_KEY_REUSED` with a different
// credential — so the CLI can and should catch it first, with nothing sent.
func TestResumeRefusesAChangedCredential(t *testing.T) {
	server, home := loggedIn(t, "execute.201.processing", "execute.replay.201")

	id := pendingRun(t, home)

	before := requestsTo(server, executePath)

	// A different principal in the same profile, as `auth login` with
	// another key would leave it.
	store := creds.NewStore(fsx.OS(), filepath.Join(home, "credentials.json"))

	file, err := store.Load()
	if err != nil {
		t.Fatalf("load credentials: %v", err)
	}

	err = file.Put(creds.DefaultProfile, server.URL(), creds.Credential{
		Token:       canaryKey(t),
		PrincipalID: "key_SOMEONE_ELSE",
	})
	if err != nil {
		t.Fatalf("put the replacement credential: %v", err)
	}

	if err := store.Save(file); err != nil {
		t.Fatalf("save credentials: %v", err)
	}

	stdout, stderr, exit := run(t, invocation{
		home: home,
		args: []string{"runs", "resume", id, "--yes"},
	})

	// M104.
	if exit == 0 {
		t.Fatalf("the resume succeeded under a different principal\n%s\n%s", stdout, stderr)
	}

	if got := requestsTo(server, executePath); got != before {
		t.Errorf("execute requests went from %d to %d; nothing may be sent under a credential "+
			"that did not authorise the request", before, got)
	}

	report := stdout + stderr

	// Both principals, so the caller can see what changed. One alone
	// leaves them guessing which profile to switch back to.
	for _, want := range []string{principal, "key_SOMEONE_ELSE"} {
		if !strings.Contains(report, want) {
			t.Errorf("the refusal does not mention %q\n%s", want, report)
		}
	}

	// The step is untouched, so switching back and resuming still works.
	if state := stepOf(t, home, runs.StepExecute).State; state != runs.StatePending {
		t.Errorf("execute step is %s, want pending", state)
	}
}

// The control: the same resume with the original credential does send.
//
// Without it, the refusal above is satisfied by a resume that refuses
// always, and AC45's whole path would be dead.
func TestResumeProceedsWithTheOriginalCredential(t *testing.T) {
	server, home := loggedIn(t, "execute.201.processing", "execute.replay.201")

	id := pendingRun(t, home)

	before := requestsTo(server, executePath)

	stdout, stderr, exit := run(t, invocation{
		home: home,
		args: []string{"runs", "resume", id, "--yes"},
	})

	if exit != 0 {
		t.Fatalf("exit = %d, want 0\n%s\n%s", exit, stdout, stderr)
	}

	if got := requestsTo(server, executePath); got != before+1 {
		t.Errorf("execute requests = %d, want %d", got, before+1)
	}

	if state := stepOf(t, home, runs.StepExecute).State; state != runs.StateTerminal {
		t.Errorf("execute step is %s, want terminal", state)
	}
}

// A settled run has nothing to resume, and saying so is not exit 0.
//
// Reporting success for a resume that did nothing would make a retry wrapper
// believe it had resent, and a wrapper that loops until success would stop
// at the wrong moment.
func TestResumingASettledRunDoesNothing(t *testing.T) {
	server, home := loggedIn(t, "execute.201.processing")

	if _, _, exit := run(t, invocation{
		home: home,
		args: []string{"transfers", "execute", "--plan", recordedPlanToken, "--yes"},
	}); exit != 0 {
		t.Fatalf("the arranging run did not settle")
	}

	before := requestsTo(server, executePath)

	stdout, stderr, exit := run(t, invocation{
		home: home,
		args: []string{"runs", "resume", onlyRun(t, home).RunID, "--yes"},
	})

	if exit == 0 {
		t.Errorf("resuming a settled run exited 0\n%s\n%s", stdout, stderr)
	}

	if got := requestsTo(server, executePath); got != before {
		t.Errorf("execute requests went from %d to %d", before, got)
	}
}

// A run id that does not exist is refused without touching the ledger.
func TestResumingAnUnknownRunIsRefused(t *testing.T) {
	server, home := loggedIn(t, "execute.201.processing")

	stdout, stderr, exit := run(t, invocation{
		home: home,
		args: []string{"runs", "resume", "01JQBN8Z5K0000000000000099", "--yes"},
	})

	if exit == 0 {
		t.Fatalf("resuming an unknown run exited 0\n%s\n%s", stdout, stderr)
	}

	if got := len(server.Requests()); got != 0 {
		t.Errorf("the fixture saw %d request(s), want 0", got)
	}

	if all, _ := ledgerOf(t, home).List(); len(all) != 0 {
		t.Errorf("%d run(s) were written, want 0", len(all))
	}
}
