//go:build faultinject

package transfers_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/kurenn/ferry-cli/internal/fault"
	"github.com/kurenn/ferry-cli/internal/render"
	"github.com/kurenn/ferry-cli/internal/runs"
)

// This file runs only under `-tags faultinject`. Without the tag the
// injector is not compiled in and every point is a no-op, which is the
// property `fault_test.go` asserts from the other side: a released binary
// cannot be made to die at a money request by an environment variable.

// AC44, C1: the run record is on disk before the request is sent.
//
// The control is the pair. A record that exists proves `Begin` ran; a
// request count of zero proves nothing was sent. Either alone is satisfied
// by the mutation M1 names — sending before `Begin` leaves a record too, by
// the time anything looks.
func TestTheRecordIsOnDiskBeforeTheRequestIsSent(t *testing.T) {
	server, home := loggedIn(t, "simulate.201")

	stdout, _, exit := run(t, invocation{
		home: home,
		env:  map[string]string{fault.EnvVar: fault.AfterRecordWritten},
		args: append([]string{"transfers", "create"}, canonicalSimulateArgs()...),
	})

	if exit != fault.CrashExit {
		t.Fatalf("exit = %d, want %d (a simulated process death)\n%s", exit, fault.CrashExit, stdout)
	}

	if got := len(server.Requests()); got != 0 {
		t.Errorf("the fixture saw %d request(s), want 0: the process died before Do", got)
	}

	step := stepOf(t, home, runs.StepSimulate)

	if step.State != runs.StatePending {
		t.Fatalf("simulate step is %s, want pending", step.State)
	}

	// `pending` with `attempts: 0` would mean `Begin` wrote the state
	// without counting the attempt, and a resume reading it could not tell
	// a step that was about to send from one that never had been.
	if step.Attempts != 1 {
		t.Errorf("attempts = %d, want 1", step.Attempts)
	}

	if step.Body == nil || step.BodySHA256 == nil {
		t.Errorf("the record holds body=%q digest=%v; a resume needs both", step.Body, step.BodySHA256)
	}

	// A killed process says nothing. Anything on stdout here would mean
	// the CLI reported an outcome for a death it did not survive.
	if stdout != "" {
		t.Errorf("a simulated process death wrote to stdout:\n%s", stdout)
	}
}

// AC45, C2: a resume sends the byte-identical body under the identical key.
//
// M3 is "mint a fresh key on resume". The assertion is at the server, over
// both requests: the key and the sha256 of the bytes must be equal across
// them. Asserting only that the record still holds the same key would be
// the vacuous version — the record is what a resume reads, so it agrees
// with itself by construction.
func TestResumeSendsTheIdenticalRequestUnderTheIdenticalKey(t *testing.T) {
	server, home := loggedIn(t, "execute.201.processing", "execute.replay.201")

	_, _, exit := run(t, invocation{
		home: home,
		env:  map[string]string{fault.EnvVar: fault.AfterSendBeforeRecord},
		args: []string{"transfers", "execute", "--plan", recordedPlanToken, "--yes"},
	})

	if exit != fault.CrashExit {
		t.Fatalf("exit = %d, want %d", exit, fault.CrashExit)
	}

	if got := requestsTo(server, executePath); got != 1 {
		t.Fatalf("execute requests before the resume = %d, want 1", got)
	}

	if state := stepOf(t, home, runs.StepExecute).State; state != runs.StatePending {
		t.Fatalf("execute step is %s after the crash, want pending", state)
	}

	id := onlyRun(t, home).RunID

	stdout, _, exit := run(t, invocation{
		home: home,
		args: []string{"runs", "resume", id, "--yes"},
	})

	if exit != 0 {
		t.Fatalf("resume exit = %d, want 0\n%s", exit, stdout)
	}

	keys := keysSentTo(server, executePath)
	if len(keys) != 2 {
		t.Fatalf("execute requests = %d, want 2", len(keys))
	}

	if keys[0] != keys[1] {
		t.Errorf("the resume sent key %q where the first attempt sent %q; one intent, one key",
			keys[1], keys[0])
	}

	if want := runs.StepKey(id, runs.StepExecute); keys[0] != want {
		t.Errorf("key = %q, want the run's own %q", keys[0], want)
	}

	digests := digestsSentTo(server, executePath)
	if digests[0] != digests[1] {
		t.Errorf("the resume sent different bytes: %s then %s", digests[0], digests[1])
	}

	if state := stepOf(t, home, runs.StepExecute).State; state != runs.StateTerminal {
		t.Errorf("execute step is %s after a settled resume, want terminal", state)
	}
}

// AC80's first window: a crash between the simulate's `Record` and the
// execute's `Begin`.
//
// M87 is "resume re-simulates". The detector is the simulate count at the
// server, which must stay at one across both invocations: re-simulating
// would price a second transfer and spend a plan the caller already has.
func TestResumeAfterTheSimulateWasRecordedExecutesTheStoredPlan(t *testing.T) {
	server, home := loggedIn(t, "simulate.201", "execute.201.processing")

	_, _, exit := run(t, invocation{
		home: home,
		env:  map[string]string{fault.EnvVar: fault.AfterSimulateRecordedBeforeExecuteBegin},
		args: append([]string{"transfers", "create", "--broadcast", "--yes"}, canonicalSimulateArgs()...),
	})

	if exit != fault.CrashExit {
		t.Fatalf("exit = %d, want %d", exit, fault.CrashExit)
	}

	if got := requestsTo(server, simulatePath); got != 1 {
		t.Fatalf("simulate requests = %d, want 1", got)
	}

	if got := requestsTo(server, executePath); got != 0 {
		t.Fatalf("execute requests = %d, want 0: the crash was before Begin", got)
	}

	crashed := onlyRun(t, home)

	if state := crashed.Step(runs.StepExecute).State; state != runs.StateNotStarted {
		t.Fatalf("execute step is %s after the crash, want not_started", state)
	}

	if !crashed.Step(runs.StepExecute).HasPlanToken() {
		t.Fatal("the execute step holds no plan token; the inter-step window has nothing to resume from")
	}

	stdout, _, exit := run(t, invocation{
		home: home,
		args: []string{"runs", "resume", crashed.RunID, "--yes"},
	})

	if exit != 0 {
		t.Fatalf("resume exit = %d, want 0\n%s", exit, stdout)
	}

	// M87.
	if got := requestsTo(server, simulatePath); got != 1 {
		t.Errorf("simulate requests after the resume = %d, want 1: a resume never re-simulates", got)
	}

	keys := keysSentTo(server, executePath)

	want := []string{runs.StepKey(crashed.RunID, runs.StepExecute)}
	if !reflect.DeepEqual(keys, want) {
		t.Errorf("execute keys = %v, want %v", keys, want)
	}

	sent := bodyOfRequestTo(t, server, executePath, 0)
	if !strings.Contains(sent, recordedPlanToken) {
		t.Errorf("the execute body was %q, want the stored plan token", sent)
	}
}

// AC80's second window: a crash between the simulate's send and its
// `Record`.
//
// M88 is "resume executes with a null token". The resume re-sends the
// simulate — the bytes are on disk and the key is the same — and FERRY
// answers the replay, which carries no token. There is then nothing to
// execute, and the execute count must stay at zero.
func TestResumeAfterAnUnrecordedSimulateCannotExecute(t *testing.T) {
	server, home := loggedIn(t, "simulate.201", "simulate.replay.201")

	_, _, exit := run(t, invocation{
		home: home,
		env:  map[string]string{fault.EnvVar: fault.AfterSimulateSendBeforeRecord},
		args: append([]string{"transfers", "create", "--broadcast", "--yes"}, canonicalSimulateArgs()...),
	})

	if exit != fault.CrashExit {
		t.Fatalf("exit = %d, want %d", exit, fault.CrashExit)
	}

	crashed := onlyRun(t, home)

	if state := crashed.Step(runs.StepSimulate).State; state != runs.StatePending {
		t.Fatalf("simulate step is %s after the crash, want pending", state)
	}

	stdout, _, exit := run(t, invocation{
		home: home,
		args: []string{"runs", "resume", crashed.RunID, "--yes"},
	})

	if exit != 4 {
		t.Fatalf("resume exit = %d, want 4: the replay carries no plan token\n%s", exit, stdout)
	}

	// M88.
	if got := requestsTo(server, executePath); got != 0 {
		t.Errorf("execute requests = %d, want 0: there is no token to execute", got)
	}

	if got := requestsTo(server, simulatePath); got != 2 {
		t.Errorf("simulate requests = %d, want 2 (the crashed send and the resend)", got)
	}

	keys := keysSentTo(server, simulatePath)
	if keys[0] != keys[1] {
		t.Errorf("the resume re-simulated under %q where the first attempt used %q", keys[1], keys[0])
	}

	if state := stepOf(t, home, runs.StepExecute).State; state != runs.StateUnreachable {
		t.Errorf("execute step is %s, want unreachable", state)
	}

	record := mustReadFile(t, ledgerOf(t, home).RecordPath(crashed.RunID))
	if secrets := render.FindSecrets(record); len(secrets) > 0 {
		t.Errorf("a run whose execute is unreachable holds secrets %v", secrets)
	}
}
