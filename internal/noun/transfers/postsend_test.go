//go:build faultinject

package transfers_test

import (
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/kurenn/ferry-cli/internal/fault"
	"github.com/kurenn/ferry-cli/internal/runs"
)

// AC69, C17: an abnormal exit after a money request may have left is exit 6,
// never 1.
//
// Exit 1 is `cli_fault` and it says *nothing was sent*. Once a request may
// have reached FERRY that sentence is false, and a caller who believes it
// sends the transfer again under a new key. The four cases below are the
// four ways this CLI can end abnormally after a send, and the fifth test is
// the control that proves the flag is not simply always set.
//
// Every assertion is on the **observed exit code** of the whole CLI, taken
// from `harness.Run`, not on the class of an error inside the money path.
// A345: the wrapper in `internal/cli` is where the reclassification happens,
// so an assertion below that layer would survive M73, M75 and M76 green.

// AC69(a): SIGINT delivered while the fixture holds the execute request open.
func TestSignalWhileTheRequestIsOnTheWireIsPending(t *testing.T) {
	server, home := loggedIn(t, "execute.201.processing")

	arrived, release := server.Hold("execute.201.processing")

	go func() {
		// A330: the arrival channel is what makes this deterministic. A
		// signal sent before the request was on the wire would be testing
		// the pre-send path, which is the control below and the opposite
		// answer.
		<-arrived

		_ = syscall.Kill(os.Getpid(), syscall.SIGINT)

		// Long enough for the handler goroutine to observe the signal
		// while the request is still held. The release below is what lets
		// the server's handler finish; whether the CLI sees the answer or
		// the cancelled connection does not change the outcome, and both
		// orders are exercised by running this test repeatedly.
		time.Sleep(100 * time.Millisecond)
		release()
	}()

	stdout, stderr, exit := run(t, invocation{
		home:    home,
		signals: true,
		args:    []string{"transfers", "execute", "--plan", recordedPlanToken, "--yes"},
	})

	// M73.
	if exit != 6 {
		t.Fatalf("exit = %d, want 6: a signal after the request left cannot report that nothing was sent\n%s\n%s",
			exit, stdout, stderr)
	}

	if got := requestsTo(server, executePath); got != 1 {
		t.Errorf("execute requests = %d, want 1", got)
	}

	step := stepOf(t, home, runs.StepExecute)

	if step.State != runs.StatePending {
		t.Errorf("execute step is %s, want pending: the answer was never recorded", step.State)
	}

	if !strings.Contains(stderr, onlyRun(t, home).RunID) {
		t.Errorf("the run id is not named anywhere in the report; there is nothing to resume from\n%s", stderr)
	}
}

// AC69(a) again, and the row the test above cannot carry: **a send that
// left the process and then failed on the wire.**
//
// The test above releases the held request, so `Do` returns a `201` and the
// in-flight flag is set on the success path whatever the order of the two
// statements around it. That makes it green against M76 — "set the flag
// after `Do` returns instead of before" — which was found by running the
// mutation rather than by reading the code, and is exactly A345's lesson
// about the layer an assertion sits at.
//
// Here the request is held and **never released**. The signal cancels the
// context while the bytes are at the server, so `Do` returns a transport
// error, and this is the case the whole invariant exists for: the request
// left, the answer is unknown, and reporting "nothing was sent" would send
// a caller to re-send a transfer FERRY may already be executing.
func TestASendThatLeftAndThenFailedIsPending(t *testing.T) {
	server, home := loggedIn(t, "execute.201.processing")

	arrived, release := server.Hold("execute.201.processing")

	// Released only at the end, so the server's handler can return and
	// the test server can shut down. Nothing releases it while the CLI is
	// running, which is what makes `Do`'s error the deterministic one.
	t.Cleanup(release)

	go func() {
		<-arrived

		_ = syscall.Kill(os.Getpid(), syscall.SIGINT)
	}()

	stdout, stderr, exit := run(t, invocation{
		home:    home,
		signals: true,
		args:    []string{"transfers", "execute", "--plan", recordedPlanToken, "--yes"},
	})

	// The measurement that says the bytes left: the server received them.
	if got := requestsTo(server, executePath); got != 1 {
		t.Fatalf("the fixture saw %d execute requests, want 1. Without a request having "+
			"arrived this test is about the pre-send path and proves nothing.", got)
	}

	if exit != 6 {
		t.Fatalf("exit = %d, want 6. The request reached the server and the answer never "+
			"came back, so whether money moved is not established (C17).\n%s\n%s",
			exit, stdout, stderr)
	}

	if state := stepOf(t, home, runs.StepExecute).State; state != runs.StatePending {
		t.Errorf("execute step is %s, want pending", state)
	}
}

// AC69(b): the filesystem refuses the write `Record` makes.
//
// The request left, the answer came back, and the CLI cannot write down what
// it was. M74 is "Record error → exit 1".
func TestARecordFailureAfterTheSendIsPending(t *testing.T) {
	server, home := loggedIn(t, "execute.201.processing")

	stdout, stderr, exit := run(t, invocation{
		home: home,
		env:  map[string]string{fault.EnvVar: fault.RecordFailsAfterSend},
		args: []string{"transfers", "execute", "--plan", recordedPlanToken, "--yes"},
	})

	if exit != 6 {
		t.Fatalf("exit = %d, want 6\n%s\n%s", exit, stdout, stderr)
	}

	if got := requestsTo(server, executePath); got != 1 {
		t.Errorf("execute requests = %d, want 1", got)
	}

	// The record on disk is the one from before the failed write: the
	// rename never happened, so `pending` is what a later invocation
	// finds — and `pending` is exactly right, because the answer is not
	// written down.
	if state := stepOf(t, home, runs.StepExecute).State; state != runs.StatePending {
		t.Errorf("execute step is %s, want pending", state)
	}
}

// AC69(c): a panic in the renderer, after `Record` returned nil.
//
// M75 is "clear the in-flight flag when Record returns nil" — the step this
// CLI deliberately does not have. With the flag cleared this panic would be
// exit 1, because by then the answer is safely recorded and the CLI would
// believe itself to be back on solid ground. It is not: the transfer was
// accepted upstream, and a caller told "nothing was sent" would send it
// again.
func TestAPanicAfterTheAnswerWasRecordedIsPending(t *testing.T) {
	server, home := loggedIn(t, "execute.201.processing")

	stdout, stderr, exit := run(t, invocation{
		home: home,
		env:  map[string]string{fault.EnvVar: fault.RenderPanicsAfter201},
		args: []string{"transfers", "execute", "--plan", recordedPlanToken, "--yes"},
	})

	if exit != 6 {
		t.Fatalf("exit = %d, want 6\n%s\n%s", exit, stdout, stderr)
	}

	if got := requestsTo(server, executePath); got != 1 {
		t.Errorf("execute requests = %d, want 1", got)
	}

	step := stepOf(t, home, runs.StepExecute)

	// A351: AC69(c) says `answered` here. The ledger says otherwise and the
	// ledger is right — `runs.Record` makes a step `terminal` for a settled
	// class, and a 201 execute is `accepted_upstream`, class 0. What the AC
	// is really asserting is that the answer reached the disk before the
	// panic, which `Response` is the evidence for.
	if step.State != runs.StateTerminal {
		t.Errorf("execute step is %s, want terminal (see A351)", step.State)
	}

	if step.Response == nil || step.Response.Status != 201 {
		t.Errorf("the 201 was not recorded before the panic: response = %v", step.Response)
	}

	if step.Outcome == nil || step.Outcome.ExitCode != 0 {
		t.Errorf("the recorded outcome is %v; the answer itself was a success and the record must say so",
			step.Outcome)
	}
}

// AC69(d): a signal during `poll.Watch`, after a 202 was recorded.
func TestASignalWhilePollingIsPending(t *testing.T) {
	server, home := loggedIn(t, "execute.202.then_completed")

	interrupt := make(chan struct{})
	clk := newClock()

	// The 202 is recorded and the loop then waits. Closing the channel
	// from the clock's first sleep puts the interrupt inside the wait,
	// which is the window AC69(d) names and which no amount of wall-clock
	// sleeping from outside could hit reliably.
	clk.onSleep = func(n int) {
		if n == 1 {
			close(interrupt)
		}
	}

	stdout, stderr, exit := run(t, invocation{
		home:        home,
		clock:       clk,
		interrupted: interrupt,
		args:        []string{"transfers", "execute", "--plan", recordedPlanToken, "--yes"},
	})

	// M101.
	if exit != 6 {
		t.Fatalf("exit = %d, want 6\n%s\n%s", exit, stdout, stderr)
	}

	if got := requestsTo(server, executePath); got != 1 {
		t.Errorf("execute requests = %d, want 1", got)
	}

	step := stepOf(t, home, runs.StepExecute)

	if step.State != runs.StateAnswered {
		t.Errorf("execute step is %s, want answered: the 202 was recorded before the wait", step.State)
	}

	if step.Response == nil || step.Response.Status != 202 {
		t.Errorf("the 202 was not recorded: response = %v", step.Response)
	}

	if !strings.Contains(stderr, "cmd_PLACEHOLDER_1") {
		t.Errorf("the command id is not named; there is nothing to watch\n%s", stderr)
	}

	// The loop stopped *at* the interrupt, not after it.
	//
	// The exit code alone does not say this. `internal/cli`'s wrapper
	// classifies on the in-flight flag and reports 6 whether the loop
	// noticed the signal or ran to completion first, so an assertion on
	// the exit code survives the poll loop ignoring `Interrupted`
	// entirely — which is mutation M101b, found by running it. The
	// measurement that distinguishes them is the wait sequence: the
	// recording schedules 9 seconds and then 5, and a loop that noticed
	// the signal during the first wait never takes the second.
	if got := clk.seconds(); len(got) != 1 {
		t.Errorf("the CLI waited %v seconds. The signal arrived during the first wait, so "+
			"there should be no second: a loop that keeps polling after an interrupt "+
			"holds a caller for the whole remaining schedule before telling them "+
			"anything.", got)
	}
}

// The control for all four: the same faults in a process that has not yet
// called `Do` on a money step exit 1, not 6.
//
// Without this the four tests above would be satisfied by a CLI that
// reported 6 for everything — which would be a CLI that can never say
// "nothing was sent, run it again", and would send every caller to `runs
// resume` after a typo.
func TestTheSameFaultsBeforeAnySendExitOne(t *testing.T) {
	t.Run("a panic before the send", func(t *testing.T) {
		server, home := loggedIn(t, "simulate.201")

		// `render_panics_after_201` fires inside the report. A run
		// refused before it is created never reaches the report, so the
		// pre-send panic is provoked where one can actually occur: a
		// profile whose credentials file cannot be read.
		_, _, exit := run(t, invocation{
			home: home,
			args: append([]string{"transfers", "create", "--profile", "absent"}, canonicalSimulateArgs()...),
		})

		if exit == 6 {
			t.Fatalf("exit = 6 for a refusal before any send; 6 says the outcome is not established")
		}

		if got := len(server.Requests()); got != 0 {
			t.Errorf("the fixture saw %d request(s), want 0", got)
		}
	})

	t.Run("an interrupt before the send", func(t *testing.T) {
		server, home := loggedIn(t, "simulate.201")

		interrupt := make(chan struct{})
		close(interrupt)

		_, _, exit := run(t, invocation{
			home:        home,
			interrupted: interrupt,
			args:        append([]string{"transfers", "create"}, canonicalSimulateArgs()...),
		})

		// The flag was never set, so `cli_fault` is the true answer: this
		// invocation sent nothing and the caller may simply run it again.
		if exit != 1 {
			t.Fatalf("exit = %d, want 1 for an interrupt before anything was sent", exit)
		}

		if got := len(server.Requests()); got != 0 {
			t.Errorf("the fixture saw %d request(s), want 0", got)
		}
	})
}
