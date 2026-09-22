//go:build faultinject

package transfers_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/coba-ai/ferry-cli/internal/creds"
	"github.com/coba-ai/ferry-cli/internal/fault"
	"github.com/coba-ai/ferry-cli/internal/fsx"
	"github.com/coba-ai/ferry-cli/internal/runs"
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

	// M77, and A370.
	//
	// The exit code is asserted, not merely required to be non-zero. AC70
	// and §7.1 A19 both say **7**, and 7 is not decoration: `escalate`
	// answers `money: unknown` and `same key safe: no`, while `cli_fault`
	// — exit 1, the code this refusal would otherwise fall to — answers
	// `money: no` and `same key safe: yes`. The run being resumed is
	// `pending`, so "money did not move" is exactly the claim nobody can
	// make about it. `if exit == 0` admitted 1, and the mutation that
	// produces 1 was green against the whole suite.
	if exit != 7 {
		t.Fatalf("exit = %d, want 7: a resume refused because the credential changed is an "+
			"escalation over a run whose outcome is unknown, not a local fault over one "+
			"that never left\n%s\n%s", exit, stdout, stderr)
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

// C19/AC70's **other arm**, which nothing asserted (A370).
//
// `checkCredential` compares two things, in order: the stored token's prefix,
// then the principal id. AC70 says "the profile's `token_prefix` **or**
// `principal_id` differs", and the test above drives the second arm only — it
// keeps the token and changes the principal. Deleting the token-prefix arm
// outright left `go test -tags faultinject ./...` entirely green, so half of
// the invariant that keeps one principal's transfer from being sent under
// another's key was carried by no example at all.
//
// The arm is not redundant with the one below it. The principal comparison is
// guarded by `match.Credential.PrincipalID != ""`, so a stored credential with
// no principal id — which `creds.Put` is perfectly willing to write, and which
// any caller that stores a token without reading `GET /v1/me` produces — skips
// it entirely. For those profiles the prefix comparison is the only guard
// there is.
//
// So this changes the token and holds the principal id fixed, which is the
// arrangement the sibling test cannot reach.
func TestResumeRefusesACredentialWhoseTokenPrefixChanged(t *testing.T) {
	server, home := loggedIn(t, "execute.201.processing", "execute.replay.201")

	id := pendingRun(t, home)

	before := requestsTo(server, executePath)

	// A second sandbox API key: same class, same environment, same
	// principal — so neither the pre-check, the `--env` assertion nor the
	// principal arm can account for the refusal. `creds.Put` recomputes
	// `token_prefix` from the token it is given, so changing the token is
	// what changes the prefix.
	replacement := "ferry_sk_sandbox_" + padded(t, "ROTATEDSK")

	if creds.Prefix(replacement) == creds.Prefix(canaryKey(t)) {
		t.Fatalf("the replacement token has the same prefix as the original (%s), so this "+
			"test cannot distinguish the arm it is about", creds.Prefix(replacement))
	}

	store := creds.NewStore(fsx.OS(), filepath.Join(home, "credentials.json"))

	file, err := store.Load()
	if err != nil {
		t.Fatalf("load credentials: %v", err)
	}

	err = file.Put(creds.DefaultProfile, server.URL(), creds.Credential{
		Token:       replacement,
		PrincipalID: principal,
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

	if exit != 7 {
		t.Fatalf("exit = %d, want 7\n%s\n%s", exit, stdout, stderr)
	}

	if got := requestsTo(server, executePath); got != before {
		t.Errorf("execute requests went from %d to %d; a rotated key must not resend a "+
			"transfer the key it replaced may already have sent", before, got)
	}

	report := stdout + stderr

	// Both prefixes, for the same reason the sibling names both principals:
	// the caller has to know which credential to put back.
	for _, want := range []string{creds.Prefix(canaryKey(t)), creds.Prefix(replacement)} {
		if !strings.Contains(report, want) {
			t.Errorf("the refusal does not mention %q\n%s", want, report)
		}
	}

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
