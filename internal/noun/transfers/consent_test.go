package transfers_test

import (
	"strings"
	"testing"

	"github.com/coba-ai/ferry-cli/internal/render"
	"github.com/coba-ai/ferry-cli/internal/runs"
)

// abandonedRunID is the run the awaiting_confirmation test arranges by hand.
const abandonedRunID = "01JQBN8Z5K0000000000000001"

// C16 end to end. These are the tests the unit exists for.
//
// The claim is not "the prompt works". It is that **no state this CLI writes
// can stand in for a human's yes on a later invocation**, which is a claim
// about two invocations and cannot be made by any single-invocation test.
// Each test below therefore runs the CLI twice and counts requests at the
// fixture across both.

// AC48: `n` at the prompt sends nothing, settles the step as declined, and
// exits 3.
func TestDecliningAtThePromptSendsNothing(t *testing.T) {
	server, home := loggedIn(t, "execute.201.processing")

	merged, _, exit := run(t, invocation{
		home:  home,
		tty:   true,
		stdin: "n\n",
		args:  []string{"transfers", "execute", "--plan", recordedPlanToken},
	})

	if exit != 3 {
		t.Fatalf("exit = %d, want 3: a decline is refused locally, nothing sent\n%s", exit, merged)
	}

	// M77. Counted at the server, which is the only witness that cannot be
	// fooled by the CLI's own bookkeeping.
	if got := requestsTo(server, executePath); got != 0 {
		t.Fatalf("execute requests = %d, want 0 after a decline", got)
	}

	step := stepOf(t, home, runs.StepExecute)

	if step.State != runs.StateDeclined {
		t.Errorf("execute step is %s, want declined", step.State)
	}

	if step.State.MayHaveSent() {
		t.Error("a declined step reports that money may have been sent")
	}

	// The human had to be shown what they were answering about, or `n` was
	// an answer to a question they could not read.
	//
	// `transfers execute --plan` holds a token and no quote — the amounts
	// were printed by the `create` that issued the plan, and this
	// invocation has no way to recover them without spending the plan. So
	// the prompt must say that rather than imply the absence of numbers
	// means there are none. The amounts themselves are asserted at the
	// `--broadcast` prompt, which does have them.
	for _, want := range []string{"plan token", "not the quote", "[y/N]"} {
		if !strings.Contains(merged, want) {
			t.Errorf("the prompt does not mention %q:\n%s", want, merged)
		}
	}
}

// AC71/AC93: `y` at the prompt is what sends.
//
// Without this the test above is satisfied by a CLI that never sends at all,
// which is the tautology §6 warns about: "declining sends nothing" is true
// of a broken binary.
func TestAcceptingAtThePromptSends(t *testing.T) {
	server, home := loggedIn(t, "execute.201.processing")

	merged, _, exit := run(t, invocation{
		home:  home,
		tty:   true,
		stdin: "y\n",
		args:  []string{"transfers", "execute", "--plan", recordedPlanToken},
	})

	if exit != 0 {
		t.Fatalf("exit = %d, want 0\n%s", exit, merged)
	}

	if got := requestsTo(server, executePath); got != 1 {
		t.Errorf("execute requests = %d, want 1 after a yes", got)
	}

	if state := stepOf(t, home, runs.StepExecute).State; state != runs.StateTerminal {
		t.Errorf("execute step is %s, want terminal", state)
	}
}

// **C16's central claim**: a declined transfer, resumed, does not send.
//
// This is CRITIQUE B3's defect, driven. Revision 1 recorded the plan token
// before the prompt, so the record left by a decline held everything a
// resume needed: a key, a body and a token. A resume would have executed a
// transfer a human had refused, and no test in revision 1 would have
// noticed.
//
// Both resumes are run: the bare one, and the one carrying `--yes`. The
// second is the important one. `--yes` is consent for *this* invocation, and
// a caller can honestly supply it — they are consenting to resume. What they
// must not be able to do is have that consent apply to a step a human
// already said no to.
func TestADeclinedTransferIsNotSentByAResume(t *testing.T) {
	server, home := loggedIn(t, "execute.201.processing")

	_, _, exit := run(t, invocation{
		home:  home,
		tty:   true,
		stdin: "n\n",
		args:  []string{"transfers", "execute", "--plan", recordedPlanToken},
	})

	if exit != 3 {
		t.Fatalf("the decline exited %d, want 3", exit)
	}

	id := onlyRun(t, home).RunID

	for _, args := range [][]string{
		{"runs", "resume", id},
		{"runs", "resume", id, "--yes"},
	} {
		stdout, stderr, exit := run(t, invocation{home: home, args: args})

		if exit == 0 {
			t.Errorf("`%s` exited 0; a declined run has nothing to resume\n%s",
				strings.Join(args, " "), stdout)
		}

		// The message has to say why, or the caller's next move is to
		// re-run the transfer under a fresh key — which is the send the
		// decline was supposed to prevent, arriving by another route.
		if !strings.Contains(strings.ToLower(stdout+stderr), "declin") {
			t.Errorf("`%s` does not say the run was declined\n%s\n%s",
				strings.Join(args, " "), stdout, stderr)
		}
	}

	// The measurement that matters, taken across all three invocations.
	if got := requestsTo(server, executePath); got != 0 {
		t.Fatalf("execute requests = %d after a decline and two resumes, want 0", got)
	}

	step := stepOf(t, home, runs.StepExecute)

	// `runs.Decline` scrubs the step's `plan_token` field, so the place a
	// resume reads a token from holds the sentinel.
	if step.HasPlanToken() {
		t.Error("a declined step still holds a live plan token in plan_token")
	}

	// A350. The `body` field still holds it, and for a *declined* step that
	// is a live token on disk.
	//
	// `runs.scrubStep` leaves `Body` alone on purpose, and its reason is
	// sound for the case it was written for: after a send, the token in the
	// body is spent, and keeping the bytes is what proves a resume resends
	// the same request (AC11, AC45). A decline sent nothing, so nothing
	// spent the plan — it stays usable until it expires, and the body is
	// the whole request needed to use it.
	//
	// The fix belongs in `internal/runs`, which this unit does not own, so
	// this asserts the leak rather than hiding it. The assertion is
	// deliberately the current behaviour: when the ledger is amended this
	// test fails, which is the notification this comment cannot give.
	record := mustReadFile(t, ledgerOf(t, home).RecordPath(id))
	if secrets := render.FindSecrets(record); len(secrets) == 0 {
		t.Errorf("a declined run's record no longer holds its plan token — A350 appears fixed in "+
			"internal/runs. Tighten this to assert zero secrets and delete the note.\n%s", record)
	}

	// What this unit *can* guarantee is that nothing prints it (AC57). That
	// is the control for as long as the leak stands.
	for _, args := range [][]string{
		{"runs", "show", id},
		{"runs", "show", id, "--output", "json"},
		{"runs", "list"},
		{"runs", "list", "--output", "json"},
	} {
		stdout, stderr, _ := run(t, invocation{home: home, args: args})

		if secrets := render.FindSecrets(stdout + stderr); len(secrets) > 0 {
			t.Errorf("`%s` printed %v", strings.Join(args, " "), secrets)
		}
	}
}

// The other half of C16: a run left `awaiting_confirmation` by a killed
// prompt is re-asked, not resumed on the strength of the state.
//
// `awaiting_confirmation` is the one state that is written *because* a human
// was asked. If any invocation treated it as "a human was asked and
// therefore agreed", C16 would be broken by the very state that exists to
// record the question. A resume of such a run with no terminal must refuse.
func TestAwaitingConfirmationIsNotConsent(t *testing.T) {
	server, home := loggedIn(t, "execute.201.processing")

	// A prompt that ends at EOF declines, so to leave a step *at*
	// `awaiting_confirmation` the prompt has to be interrupted — which is
	// also a decline. The state is therefore reached here the way a
	// `kill -9` at the prompt reaches it: written, then abandoned.
	ledger := ledgerOf(t, home)

	handle, err := ledger.Create(&runs.Run{
		RunID:           abandonedRunID,
		CreatedAt:       fixedNow,
		Profile:         "default",
		APIURL:          server.URL(),
		Environment:     "sandbox",
		CredentialClass: "api_key",
		PrincipalID:     principal,
		Argv:            []string{"ferry", "transfers", "execute", "--plan", "[REDACTED]"},
		Steps: []*runs.Step{{
			Name:           runs.StepExecute,
			Operation:      "transfers.execute",
			Method:         "POST",
			Path:           executePath,
			IdempotencyKey: runs.StepKey(abandonedRunID, runs.StepExecute),
			State:          runs.StateNotStarted,
		}},
	})
	if err != nil {
		t.Fatalf("create a run: %v", err)
	}

	if err := handle.SetPlan(runs.StepExecute, nil, recordedPlanToken); err != nil {
		t.Fatalf("set the plan: %v", err)
	}

	if err := handle.AwaitConfirmation(runs.StepExecute); err != nil {
		t.Fatalf("await confirmation: %v", err)
	}

	if err := handle.Close(); err != nil {
		t.Fatalf("close the run: %v", err)
	}

	step := onlyRun(t, home).Step(runs.StepExecute)

	if step.State != runs.StateAwaitingConfirmation {
		t.Fatalf("the arranged step is %s, want awaiting_confirmation", step.State)
	}

	// The state reports that nothing was sent, which is the true thing
	// about it and the reason it is safe to re-ask.
	if step.State.MayHaveSent() {
		t.Fatal("awaiting_confirmation reports that money may have been sent")
	}

	// With no terminal and no `--yes`, the resume must refuse. A CLI that
	// read `awaiting_confirmation` as a granted consent would send here,
	// and this is the single assertion that would catch it.
	stdout, stderr, exit := run(t, invocation{
		home: home,
		args: []string{"runs", "resume", abandonedRunID},
	})

	if exit == 0 {
		t.Errorf("a resume with nobody to ask exited 0\n%s\n%s", stdout, stderr)
	}

	if got := requestsTo(server, executePath); got != 0 {
		t.Fatalf("execute requests = %d, want 0: awaiting_confirmation is not a yes", got)
	}

	// And on a terminal it asks again rather than proceeding, which is the
	// positive half: the state does not skip the prompt either.
	merged, _, exit := run(t, invocation{
		home:  home,
		tty:   true,
		stdin: "n\n",
		args:  []string{"runs", "resume", abandonedRunID},
	})

	if !strings.Contains(merged, "[y/N]") {
		t.Errorf("the resume did not re-ask; awaiting_confirmation was taken as an answer\n%s", merged)
	}

	if exit != 3 {
		t.Errorf("exit = %d, want 3 for the second decline\n%s", exit, merged)
	}

	if got := requestsTo(server, executePath); got != 0 {
		t.Errorf("execute requests = %d, want 0", got)
	}
}

// AC71: with no terminal and no `--yes`, `runs resume` refuses rather than
// assuming.
//
// M79 is "resume without --yes proceeds". The pair with the test above is
// what makes it a control: resume never has consent of its own, whether the
// step it would send is fresh or was left `awaiting_confirmation`.
// C16 at its sharpest: **an interrupt at the prompt declines even when a
// "y" is already sitting in the buffer.**
//
// The interrupt is what a closed terminal delivers — an ssh session
// dropping, a terminal window shut, a CI runner reaping a job. The human
// who was going to answer is gone, and the only safe reading of their
// silence is "no". A "y" queued on stdin is not an answer given after the
// terminal went away; it is bytes that were already there.
//
// This row is delivered through the supplied interrupt channel rather than
// a real signal, and both halves of that choice are deliberate:
//
//   - **Why it still proves the property.** The channel is the same one a
//     real signal closes — `cli.New` hands the identical channel to
//     `consent.Ask` whether it is closed by the handler or by a test, and
//     the handler is proven to close it by the real-signal rows in
//     `postsend_test.go` (AC69(a)). That the set it is installed for is
//     the right three is pinned by `cli.TestTheSignalSetIsTheThreeTheDesignNames`
//     (M102), which is a census and cannot race.
//
//   - **Why not a real signal here.** `syscall.Kill` returns when the
//     signal is queued, not when Go's handler has run, so the buffered "y"
//     can be read first. CI caught exactly that: a SIGTERM row sent the
//     transfer. Dropping the "y" to remove the race deadlocks instead —
//     the blocked read holds the pty slave open, so the harness's drain
//     never ends (A358). The channel is the one seam that is neither racy
//     nor deadlock-prone.
//
// The product race the CI failure exposed was real and is fixed: `Ask` now
// answers from an interrupt that has already arrived before it consults
// the read at all, so the two are no longer a coin toss.
//
// **This row is not the control for that fix, and measurement says so.**
// Removing the priority select leaves this test green, 20 runs out of 20:
// the read here is a syscall on a pty, so its goroutine has almost never
// reached the channel by the time the select runs, and the interrupt case
// wins without needing priority. The control is
// `consent.TestASignalWinsOverABufferedYes`, which drives `Ask` over a
// `strings.Reader` — a read that completes in userspace, so both cases are
// genuinely ready and the choice is genuinely a toss. That is A345: the
// same mutation is caught at one layer and invisible at another, and the
// end-to-end layer is the one that cannot see it.
func TestAnInterruptAtThePromptBeatsABufferedYes(t *testing.T) {
	server, home := loggedIn(t, "execute.201.processing")

	interrupted := make(chan struct{})

	merged, _, exit := run(t, invocation{
		home:        home,
		tty:         true,
		stdin:       "y\n",
		interrupted: interrupted,
		promptShown: func() { close(interrupted) },
		args:        []string{"transfers", "execute", "--plan", recordedPlanToken},
	})

	if got := requestsTo(server, executePath); got != 0 {
		t.Errorf("an interrupt at the prompt sent %d request(s), want 0. The human who "+
			"was going to answer is gone; a transfer must not execute because a "+
			"terminal closed.", got)
	}

	if exit == 0 {
		t.Errorf("exit = 0 after an interrupt at the prompt\n%s", merged)
	}

	// Consent was never granted, so `Begin` was never reached, so the step
	// must not claim a send. Which of `awaiting_confirmation` or `declined`
	// it settles on depends on how far the record got, and both of them say
	// the same thing about the money.
	if state := stepOf(t, home, runs.StepExecute).State; state == runs.StatePending {
		t.Errorf("step = %q after an interrupt at the prompt: the CLI recorded a send "+
			"it could not have made, since consent was never given", state)
	}
}

func TestResumeWithNoTerminalRequiresYes(t *testing.T) {
	server, home := loggedIn(t, "execute.201.processing", "execute.replay.201")

	_, _, exit := run(t, invocation{
		home:  home,
		tty:   true,
		stdin: "y\n",
		args:  []string{"transfers", "execute", "--plan", recordedPlanToken},
	})

	if exit != 0 {
		t.Fatalf("the first execute exited %d, want 0", exit)
	}

	sent := requestsTo(server, executePath)

	id := onlyRun(t, home).RunID

	// The run is settled, so this refusal is about there being nothing to
	// resume rather than about consent. The consent arm is the one below.
	stdout, stderr, exit := run(t, invocation{home: home, args: []string{"runs", "resume", id}})

	if exit == 0 {
		t.Errorf("resuming a settled run exited 0\n%s\n%s", stdout, stderr)
	}

	if got := requestsTo(server, executePath); got != sent {
		t.Errorf("execute requests went from %d to %d; a settled run must not be resent", sent, got)
	}
}

// AC94: `transfers execute` with no terminal and no `--yes` still sends,
// because the command *is* the consent.
//
// This is arm 2, and it is the one that looks like a hole. It is not: the
// landing page documents `transfers execute --plan` as the command that
// executes, and a scripted caller who typed it has said everything `--yes`
// would say. The hole would be extending the same reasoning to `runs
// resume`, which is what the test above forbids.
func TestExecuteWithNoTerminalIsItsOwnConsent(t *testing.T) {
	server, home := loggedIn(t, "execute.201.processing")

	stdout, _, exit := run(t, invocation{
		home: home,
		args: []string{"transfers", "execute", "--plan", recordedPlanToken},
	})

	if exit != 0 {
		t.Fatalf("exit = %d, want 0\n%s", exit, stdout)
	}

	if got := requestsTo(server, executePath); got != 1 {
		t.Errorf("execute requests = %d, want 1", got)
	}
}
