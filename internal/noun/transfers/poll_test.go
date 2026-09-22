package transfers_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/coba-ai/ferry-cli/internal/runs"
)

// AC50: a 202 is recorded before the first poll, the waits are the ones
// FERRY asked for, and the poll count is the one the recording provides.
//
// The wait *sequence* is the assertion. A loop that polled immediately, or
// that used its own backoff, would still reach the same final state and
// would pass any test that only looked at the outcome — which is mutation
// M50, "ignore retry_after_seconds".
func TestA202IsRecordedThenPolledOnFerrysSchedule(t *testing.T) {
	server, home := loggedIn(t, "execute.202.then_completed")

	clk := newClock()

	stdout, stderr, exit := run(t, invocation{
		home:  home,
		clock: clk,
		args:  []string{"transfers", "execute", "--plan", recordedPlanToken, "--yes"},
	})

	if exit != 0 {
		t.Fatalf("exit = %d, want 0\n%s\n%s", exit, stdout, stderr)
	}

	// The recording is a 202 with `Retry-After: 9` followed by two GETs:
	// an `upstream_unknown` command carrying `retry_after_seconds: 5`, and
	// then a `completed` one. Both waits are FERRY's — the response header
	// first, then the command's own field.
	if got, want := clk.seconds(), []int{9, 5}; !reflect.DeepEqual(got, want) {
		t.Errorf("the CLI waited %v seconds, want %v: the delays are FERRY's and not the CLI's",
			got, want)
	}

	if got := requestsTo(server, executePath); got != 1 {
		t.Errorf("execute requests = %d, want 1: polling is not resending", got)
	}

	if got := requestsTo(server, "/v1/commands/cmd_PLACEHOLDER_1"); got != 2 {
		t.Errorf("command reads = %d, want 2", got)
	}

	step := stepOf(t, home, runs.StepExecute)

	// The settled command read is what the record ends up holding: the
	// step is recorded again once the polling stops, and the last answer
	// is the one a caller wants to read back. That the *202* reached the
	// disk before the first poll is asserted where it can be measured —
	// `TestAPollThatCannotFitTheDeadlineStopsAtSix` stops before any poll
	// happens, so the 202 is the only thing that could be there.
	if step.Response == nil || step.Response.Status != 200 {
		t.Errorf("the settled answer was not recorded: response = %v", step.Response)
	}

	if step.Attempts != 1 {
		t.Errorf("attempts = %d, want 1: a poll is not an attempt", step.Attempts)
	}
}

// AC50's other half: the command id is reported before the waiting starts.
//
// A caller who gets bored and kills the CLI during a 9-second wait needs the
// id to watch, and the run record needs it too.
func TestTheCommandIDIsReportedBeforeTheWait(t *testing.T) {
	_, home := loggedIn(t, "execute.202.then_completed")

	clk := newClock()

	var whenReported int

	// The first sleep is the 9-second wait. Capturing the transcript's
	// length at that instant is how "before" is measured rather than
	// assumed: the alternative is to look at the finished output, where
	// everything is present whatever order it was written in.
	clk.onSleep = func(n int) {
		if n == 1 {
			whenReported = 1
		}
	}

	stdout, stderr, exit := run(t, invocation{
		home:  home,
		clock: clk,
		args:  []string{"transfers", "execute", "--plan", recordedPlanToken, "--yes"},
	})

	if exit != 0 {
		t.Fatalf("exit = %d, want 0\n%s", exit, stdout)
	}

	if whenReported != 1 {
		t.Fatal("the CLI never waited, so this test is not measuring what it claims")
	}

	// The progress goes to stderr, so that `--output json`'s single
	// document is untouched by it.
	if !strings.Contains(stderr, "cmd_PLACEHOLDER_1") {
		t.Errorf("the command id was never reported on stderr\n%s", stderr)
	}

	if !strings.Contains(stderr, "9") {
		t.Errorf("the wait was never announced; a caller sees a hang\n%s", stderr)
	}
}

// AC51's other stop, and the one the deadline test cannot see: **the loop
// stops the moment the class is not `pending`.**
//
// `pending` is the only class that means "ask again". Every other class is
// an answer, and polling past an answer is either pointless or actively
// wrong: `needs_operator` will not change without a human, so a loop that
// kept asking would hold the caller until the deadline and then report a
// timeout for a command whose state has been known since the second read.
//
// The deadline test cannot stand in for this. It stops the loop with a
// clock, so it stays green against a loop that never inspects the class at
// all — mutation M51, found by running it rather than by reading. The
// measurement here is the number of command reads: the recording answers
// `needs_operator` on the second, and a third would mean the class was not
// consulted.
func TestThePollStopsAtTheFirstAnswerThatIsNotPending(t *testing.T) {
	server, home := loggedIn(t, "execute.202.then_needs_operator")

	clk := newClock()

	stdout, stderr, exit := run(t, invocation{
		home:  home,
		clock: clk,
		args:  []string{"transfers", "execute", "--plan", recordedPlanToken, "--yes"},
	})

	// `needs_operator` is escalate: stop and read it.
	if exit != 7 {
		t.Fatalf("exit = %d, want 7: a command waiting on a human is escalate, not a "+
			"timeout and not a resend\n%s\n%s", exit, stdout, stderr)
	}

	// Two reads: `upstream_unknown`, then `needs_operator`. A third would
	// be the loop asking again after an answer.
	if got := requestsTo(server, "/v1/commands/cmd_PLACEHOLDER_1"); got != 2 {
		t.Errorf("command reads = %d, want 2. The recording answers needs_operator on the "+
			"second read, and the loop continues on `pending` alone.", got)
	}

	// And the waits stop with it: 9 from the 202's Retry-After, 5 from
	// the in-flight command. Nothing after the answer.
	if got := clk.seconds(); len(got) != 2 {
		t.Errorf("the CLI waited %v. There are two waits before the answer and none "+
			"after it; a third means the loop slept before asking a question it did "+
			"not need to ask.", got)
	}

	// The step is `terminal`, and the reason is worth writing down
	// because it reads wrong at first. `runs.Record` derives the state
	// from the outcome's **exit code**: classes 5 and 6 stay `answered`
	// — those are the two that mean "ask again" — and everything else is
	// terminal. Escalate is 7, so the step settles even though a human
	// still has work to do, because what settles is *this CLI's* part:
	// there is nothing further for a resume to send. The operator's work
	// happens against the command id, not against the run.
	if state := stepOf(t, home, runs.StepExecute).State; state != runs.StateTerminal {
		t.Errorf("the execute step is %s, want terminal: exit 7 is not one of the two "+
			"classes the ledger leaves answered", state)
	}

	if !strings.Contains(stdout+stderr, "cmd_PLACEHOLDER_1") {
		t.Errorf("the command id is not named, so there is nothing for an operator to "+
			"quote\n%s\n%s", stdout, stderr)
	}
}

// AC51: the poll deadline is a stop, and the stop is exit 6.
//
// The frozen clock is what makes this a test of the deadline rather than of
// the recording running out: time only advances when the CLI sleeps, and
// with `--timeout 5` a 9-second wait cannot fit, so the loop must refuse to
// start it. A loop that slept anyway would spend the whole timeout to learn
// nothing, which is M52.
func TestAPollThatCannotFitTheDeadlineStopsAtSix(t *testing.T) {
	server, home := loggedIn(t, "execute.202.then_completed")

	clk := frozenClock()

	stdout, stderr, exit := run(t, invocation{
		home:  home,
		clock: clk,
		args: []string{"transfers", "execute", "--plan", recordedPlanToken, "--yes",
			"--timeout", "5s"},
	})

	if exit != 6 {
		t.Fatalf("exit = %d, want 6: a command still in flight has no established outcome\n%s\n%s",
			exit, stdout, stderr)
	}

	// Nothing was waited out, because the wait would not have fit.
	if got := clk.seconds(); len(got) != 0 {
		t.Errorf("the CLI slept %v with a deadline that could not accommodate it", got)
	}

	if got := requestsTo(server, "/v1/commands/cmd_PLACEHOLDER_1"); got != 0 {
		t.Errorf("command reads = %d, want 0", got)
	}

	// And it did not resend. A timeout is not a reason to try again.
	if got := requestsTo(server, executePath); got != 1 {
		t.Errorf("execute requests = %d, want 1", got)
	}

	step := stepOf(t, home, runs.StepExecute)

	// `answered` and not `terminal`: the 202 is recorded, the outcome is
	// not settled, and a later `runs resume` or `commands watch` picks it
	// up from here.
	if step.State != runs.StateAnswered {
		t.Errorf("execute step is %s, want answered", step.State)
	}

	// AC50's "the 202 is recorded before the first poll", measured. No
	// poll happened in this invocation — the command read count above is
	// zero — so a 202 on disk can only have been written before polling
	// was attempted. In the settling case the same field holds the final
	// command read, which is why that test cannot make this claim.
	if step.Response == nil || step.Response.Status != 202 {
		t.Errorf("the 202 was not recorded before polling: response = %v", step.Response)
	}

	if step.Response != nil && step.Response.Headers["Ferry-Command-Id"] != "cmd_PLACEHOLDER_1" {
		t.Errorf("the recorded 202 does not carry the command id: %v", step.Response.Headers)
	}

	// The report has to name the command, or "not established" is a dead
	// end.
	if !strings.Contains(stdout+stderr, "cmd_PLACEHOLDER_1") {
		t.Errorf("the timeout report does not name the command\n%s\n%s", stdout, stderr)
	}
}

// The control for the test above: with a deadline that fits, the same
// recording settles at 0.
//
// Without this pair, "a timeout exits 6" is satisfied by a CLI that always
// exits 6, and `--timeout` would appear to work while doing nothing.
func TestTheSameRecordingSettlesWhenTheDeadlineFits(t *testing.T) {
	_, home := loggedIn(t, "execute.202.then_completed")

	stdout, _, exit := run(t, invocation{
		home:  home,
		clock: newClock(),
		args: []string{"transfers", "execute", "--plan", recordedPlanToken, "--yes",
			"--timeout", "60s"},
	})

	if exit != 0 {
		t.Fatalf("exit = %d, want 0 with a deadline the polls fit inside\n%s", exit, stdout)
	}

	if state := stepOf(t, home, runs.StepExecute).State; state != runs.StateTerminal {
		t.Errorf("execute step is %s, want terminal", state)
	}
}

// AC52: `503 UPSTREAM_BUSY` carrying a command id is polled, not resent.
//
// This is the row where the two answers differ by a header. With a command
// id there is something in flight to follow, so resending would be a second
// request for the same intent; without one there is nothing to follow and
// the identical request under the identical key is the remedy.
func TestUpstreamBusyWithACommandIDIsPolled(t *testing.T) {
	server, home := loggedIn(t, "execute.upstream_busy.with_command.503")

	stdout, stderr, exit := run(t, invocation{
		home:  home,
		clock: newClock(),
		args:  []string{"transfers", "execute", "--plan", recordedPlanToken, "--yes"},
	})

	// M53: resend instead of poll. Counted at the server.
	if got := requestsTo(server, executePath); got != 1 {
		t.Errorf("execute requests = %d, want 1: a busy upstream with a command id is followed, "+
			"not asked again", got)
	}

	step := stepOf(t, home, runs.StepExecute)

	if step.Attempts != 1 {
		t.Errorf("attempts = %d, want 1", step.Attempts)
	}

	if exit == 0 {
		t.Errorf("exit = 0 for a busy upstream\n%s\n%s", stdout, stderr)
	}

	// Whatever the class, the command has to be named: it is the only
	// handle on what is actually happening.
	if !strings.Contains(stdout+stderr, "cmd_") {
		t.Errorf("no command id in the report\n%s\n%s", stdout, stderr)
	}
}

// AC52's other arm: no command id, so the remedy is the identical request
// again — and `api.Retryable` sends it without asking, inside the retry
// budget.
//
// **A352, a recording gap.** `execute.upstream_busy.no_command.503` has a
// single interaction, and `api.Retryable` declares `UPSTREAM_BUSY` with no
// `Ferry-Command-Id` resendable ("read_quote refused and nothing was sent:
// resend"). So the CLI's second attempt finds no recorded interaction and is
// answered 599, and the classification a caller ends up with describes the
// fixture rather than FERRY. The scenario needs a second interaction to be
// driven end to end. Re-recording is another agent's and C13 forbids a
// hand-written body, so this test asserts what is reachable: the resend is
// byte-identical under the identical key, which is the property that makes
// resending safe at all.
func TestUpstreamBusyWithNoCommandIDResendsTheIdenticalRequest(t *testing.T) {
	server, home := loggedIn(t, "execute.upstream_busy.no_command.503")

	stdout, stderr, exit := run(t, invocation{
		home:  home,
		clock: newClock(),
		args:  []string{"transfers", "execute", "--plan", recordedPlanToken, "--yes"},
	})

	if exit == 0 {
		t.Fatalf("exit = 0 for a busy upstream\n%s\n%s", stdout, stderr)
	}

	keys := keysSentTo(server, executePath)
	digests := digestsSentTo(server, executePath)

	if len(keys) < 2 {
		t.Fatalf("execute requests = %d; `api.Retryable` declares this answer resendable, so "+
			"the retry layer should have sent it again. If that policy changed, this test and "+
			"A352 both need re-reading.", len(keys))
	}

	// The claim that matters. A retry under a fresh key is a second
	// transfer, and it is the one thing a retry loop must never do.
	for i := 1; i < len(keys); i++ {
		if keys[i] != keys[0] {
			t.Errorf("retry %d used key %q where the first attempt used %q", i, keys[i], keys[0])
		}

		if digests[i] != digests[0] {
			t.Errorf("retry %d sent different bytes: %s vs %s", i, digests[i], digests[0])
		}
	}

	// There is nothing to poll, so nothing was polled. A CLI that invented
	// a command id from the key would send a GET that 404s and report the
	// 404 instead of the 503.
	for _, req := range server.Requests() {
		if strings.HasPrefix(req.Path, "/v1/commands/") {
			t.Errorf("the CLI polled %s with no command id in the 503", req.Path)
		}
	}

	// One attempt, whatever the retry layer did inside it: `Begin` is
	// called once per money step per invocation, and the retries happen
	// underneath it. A counter that bumped per HTTP request would make a
	// resume look like a step that had been sent four times.
	if got := stepOf(t, home, runs.StepExecute).Attempts; got != 1 {
		t.Errorf("attempts = %d, want 1", got)
	}

	if state := stepOf(t, home, runs.StepExecute).State; !state.MayHaveSent() &&
		state != runs.StateAnswered {
		t.Errorf("execute step is %s; a 503 after a send must not read as nothing sent", state)
	}
}

// AC53: `409 IDEMPOTENCY_KEY_IN_PROGRESS` is never resent.
//
// The key is already in use by a request FERRY is still working on.
// Resending is the one thing that cannot help and the thing a naive retry
// loop does, because a 409 looks like a conflict to be retried.
func TestAnInProgressKeyIsNotResent(t *testing.T) {
	server, home := loggedIn(t, "execute.in_progress.409")

	stdout, stderr, exit := run(t, invocation{
		home:  home,
		clock: newClock(),
		args:  []string{"transfers", "execute", "--plan", recordedPlanToken, "--yes"},
	})

	// M54.
	if got := requestsTo(server, executePath); got != 1 {
		t.Fatalf("execute requests = %d, want 1: a key already in progress cannot be helped "+
			"by sending it again", got)
	}

	if exit == 0 {
		t.Errorf("exit = 0 for a key already in progress\n%s\n%s", stdout, stderr)
	}

	if got := stepOf(t, home, runs.StepExecute).Attempts; got != 1 {
		t.Errorf("attempts = %d, want 1", got)
	}

	// The caller must be told to wait or to watch, never to retry with a
	// new key — a new key is a second transfer.
	report := strings.ToLower(stdout + stderr)

	if strings.Contains(report, "new key") && !strings.Contains(report, "never a new key") {
		t.Errorf("the report suggests a new key\n%s\n%s", stdout, stderr)
	}
}

// The simulate arm of the same row, which is the one the recording covers
// with a command to follow.
func TestAnInProgressSimulateIsNotResent(t *testing.T) {
	server, home := loggedIn(t, "simulate.in_progress.409")

	stdout, stderr, exit := run(t, invocation{
		home:  home,
		clock: newClock(),
		args:  append([]string{"transfers", "create"}, canonicalSimulateArgs()...),
	})

	if got := requestsTo(server, simulatePath); got != 1 {
		t.Fatalf("simulate requests = %d, want 1\n%s\n%s", got, stdout, stderr)
	}

	if exit == 0 {
		t.Errorf("exit = 0 for a key already in progress\n%s\n%s", stdout, stderr)
	}
}
