package transfers_test

import (
	"strings"
	"testing"

	"github.com/coba-ai/ferry-cli/internal/render"
	"github.com/coba-ai/ferry-cli/internal/runs"
)

// AC46: `transfers create` without --broadcast sends only the simulate,
// prints the plan token once, and stores no token anywhere in the record.
//
// The three clauses are three different defects and are asserted separately:
// M44 executes without being asked, M45 stores the token it printed, and a
// third — printing the token twice, or not at all — is what the count is for.
func TestCreateWithoutBroadcastSimulatesOnly(t *testing.T) {
	server, home := loggedIn(t, "simulate.201")

	stdout, _, exit := run(t, invocation{
		home: home,
		args: append([]string{"transfers", "create"}, canonicalSimulateArgs()...),
	})

	if exit != 0 {
		t.Fatalf("exit = %d, want 0\n%s", exit, stdout)
	}

	// M44. Counted at the server: the CLI cannot be asked whether it sent
	// something, it can only be watched.
	if got := requestsTo(server, executePath); got != 0 {
		t.Errorf("execute requests = %d, want 0 without --broadcast", got)
	}

	if got := requestsTo(server, simulatePath); got != 1 {
		t.Errorf("simulate requests = %d, want 1", got)
	}

	if got := strings.Count(stdout, recordedPlanToken); got != 1 {
		t.Errorf("the plan token appears %d times on stdout, want exactly 1\n%s", got, stdout)
	}

	run := onlyRun(t, home)

	// M44 again, from the record's side: a run with no execute step cannot
	// grow one, whatever a later resume decides.
	if run.Step(runs.StepExecute) != nil {
		t.Error("a simulate-only run has an execute step; it must have exactly one step")
	}

	// M45. The token's only permitted resting place is a --broadcast run's
	// execute step, and this run has none — so any occurrence anywhere in
	// the record is a leak. Sweeping the whole file rather than the two
	// fields keeps this true of a field added later.
	record := mustReadFile(t, ledgerOf(t, home).RecordPath(run.RunID))
	if secrets := render.FindSecrets(record); len(secrets) > 0 {
		t.Errorf("the run record holds %d secret(s): %v\n%s", len(secrets), secrets, record)
	}

	step := stepOf(t, home, runs.StepSimulate)

	if step.State != runs.StateTerminal {
		t.Errorf("simulate step is %s, want terminal", step.State)
	}

	if step.Attempts != 1 {
		t.Errorf("attempts = %d, want 1", step.Attempts)
	}
}

// The floor for the sweep above: the detector must be able to find a token
// in a run record, or "no secrets found" would be true of any file at all.
//
// This is the positive control `docs/dev-loop-learnings.md` asks for, and it
// is not hypothetical — four shipped defects in this project were detectors
// compared against empty sets.
func TestTheRecordSweepCanFindAToken(t *testing.T) {
	server, home := loggedIn(t, "simulate.201")

	_, _, exit := run(t, invocation{
		home: home,
		args: append([]string{"transfers", "create", "--broadcast", "--yes"}, canonicalSimulateArgs()...),
	})

	_ = exit
	_ = server

	record := mustReadFile(t, ledgerOf(t, home).RecordPath(onlyRun(t, home).RunID))

	// A --broadcast run that got as far as storing the plan has the token
	// in the two declared places, so the same sweep that found nothing
	// above finds something here.
	if secrets := render.FindSecrets(record); len(secrets) == 0 {
		t.Errorf("the sweep found no secret in a record that stores a plan token; "+
			"it cannot be evidence of absence elsewhere\n%s", record)
	}
}

// AC49: the execute render says what actually happened and refuses the
// vocabulary that would overstate it (M49).
func TestExecuteRenderDoesNotClaimSuccess(t *testing.T) {
	_, home := loggedIn(t, "simulate.201", "execute.201.processing")

	stdout, _, exit := run(t, invocation{
		home: home,
		args: append([]string{"transfers", "create", "--broadcast", "--yes"}, canonicalSimulateArgs()...),
	})

	if exit != 0 {
		t.Fatalf("exit = %d, want 0\n%s", exit, stdout)
	}

	for _, want := range []string{"status", "subStatus", "not settled"} {
		if !strings.Contains(strings.ToLower(stdout), strings.ToLower(want)) {
			t.Errorf("the execute render does not mention %q\n%s", want, stdout)
		}
	}

	for _, forbidden := range []string{"success", "✓"} {
		if strings.Contains(strings.ToLower(stdout), forbidden) {
			t.Errorf("the execute render says %q; a transfer accepted upstream is not settled\n%s",
				forbidden, stdout)
		}
	}
}

// AC47's first two clauses: both keys exist before the first request, and
// the execute sends exactly the token the fresh 201 carried.
func TestBroadcastPreMintsBothKeysBeforeSending(t *testing.T) {
	server, home := loggedIn(t, "simulate.201", "execute.201.processing")

	stdout, _, exit := run(t, invocation{
		home: home,
		args: append([]string{"transfers", "create", "--broadcast", "--yes"}, canonicalSimulateArgs()...),
	})

	if exit != 0 {
		t.Fatalf("exit = %d, want 0\n%s", exit, stdout)
	}

	record := onlyRun(t, home)

	simulate := record.Step(runs.StepSimulate)
	execute := record.Step(runs.StepExecute)

	if simulate == nil || execute == nil {
		t.Fatalf("a --broadcast run must have both steps, got %d", len(record.Steps))
	}

	// M47 is "mint the execute key after the simulate 201". The key's
	// *shape* is what catches it: both keys are derived from the run id,
	// which exists before either request, so a key minted later would
	// either be a second ULID or the run id with a different suffix.
	wantSimulate := runs.StepKey(record.RunID, runs.StepSimulate)
	wantExecute := runs.StepKey(record.RunID, runs.StepExecute)

	if simulate.IdempotencyKey != wantSimulate {
		t.Errorf("simulate key = %q, want %q", simulate.IdempotencyKey, wantSimulate)
	}

	if execute.IdempotencyKey != wantExecute {
		t.Errorf("execute key = %q, want %q", execute.IdempotencyKey, wantExecute)
	}

	// And the keys that reached the wire are those keys, which is the half
	// the record alone cannot establish.
	if got := keysSentTo(server, simulatePath); len(got) != 1 || got[0] != wantSimulate {
		t.Errorf("simulate was sent under %v, want [%s]", got, wantSimulate)
	}

	if got := keysSentTo(server, executePath); len(got) != 1 || got[0] != wantExecute {
		t.Errorf("execute was sent under %v, want [%s]", got, wantExecute)
	}

	if execute.Body == nil || !strings.Contains(string(execute.Body), recordedPlanToken) {
		t.Errorf("the execute step's body is %q, want the token the simulate returned", execute.Body)
	}
}

// AC47's third clause: a refusal after the simulate leaves exactly one
// simulate on the wire. M46 is "re-simulate on PLAN_EXPIRED", which would
// make it two.
func TestBroadcastDoesNotResimulateOnARefusal(t *testing.T) {
	server, home := loggedIn(t, "simulate.201", "execute.plan_expired.410")

	stdout, _, exit := run(t, invocation{
		home: home,
		args: append([]string{"transfers", "create", "--broadcast", "--yes"}, canonicalSimulateArgs()...),
	})

	if exit != 4 {
		t.Fatalf("exit = %d, want 4 for a 410 PLAN_EXPIRED\n%s", exit, stdout)
	}

	if got := requestsTo(server, simulatePath); got != 1 {
		t.Errorf("simulate requests = %d, want exactly 1: the CLI must not price a second transfer", got)
	}

	if got := requestsTo(server, executePath); got != 1 {
		t.Errorf("execute requests = %d, want 1", got)
	}
}

// AC75: a simulate that answers 202 under --broadcast closes the execute
// step at the moment the 202 is recorded, polls, and exits 4 having sent no
// execute (M83).
func TestBroadcastSimulate202NeverExecutes(t *testing.T) {
	server, home := loggedIn(t, "simulate.202.then_completed")

	stdout, _, exit := run(t, invocation{
		home:  home,
		clock: newClock(),
		args:  append([]string{"transfers", "create", "--broadcast", "--yes"}, canonicalSimulateArgs()...),
	})

	if exit != 4 {
		t.Fatalf("exit = %d, want 4: a completed simulate issues no plan token\n%s", exit, stdout)
	}

	if got := requestsTo(server, executePath); got != 0 {
		t.Errorf("execute requests = %d, want 0: the plan token was never issued", got)
	}

	execute := stepOf(t, home, runs.StepExecute)

	if execute.State != runs.StateUnreachable {
		t.Errorf("execute step is %s, want unreachable", execute.State)
	}

	if execute.Plan != nil || execute.PlanToken != nil {
		t.Errorf("a step that can never run holds plan=%v token=%v; it must hold neither",
			execute.Plan, execute.PlanToken)
	}

	if execute.Body != nil {
		t.Errorf("the execute step holds a body %q; nothing was ever built for it", execute.Body)
	}
}

// AC47/§5.6: a replayed simulate 201 carries no token, so --broadcast cannot
// continue. The row exists because the answer is a 201 and a CLI reading only
// the status would send `{"plan_token": null}`.
func TestBroadcastReplayedSimulateNeverExecutes(t *testing.T) {
	server, home := loggedIn(t, "simulate.replay.201")

	stdout, _, exit := run(t, invocation{
		home: home,
		args: append([]string{"transfers", "create", "--broadcast", "--yes"}, canonicalSimulateArgs()...),
	})

	if exit != 4 {
		t.Fatalf("exit = %d, want 4 for a replayed simulate under --broadcast\n%s", exit, stdout)
	}

	if got := requestsTo(server, executePath); got != 0 {
		t.Errorf("execute requests = %d, want 0: a replayed simulate carries no plan token", got)
	}

	if state := stepOf(t, home, runs.StepExecute).State; state != runs.StateUnreachable {
		t.Errorf("execute step is %s, want unreachable", state)
	}
}

// AC87: `--idempotency-key` with `--broadcast` is exit 2 and zero requests
// (M96). It is refused before the profile is read, so "zero requests" does
// not depend on the credential being present either.
func TestBroadcastWithAnIdempotencyKeyIsAUsageError(t *testing.T) {
	server, home := loggedIn(t, "simulate.201")

	stdout, stderr, exit := run(t, invocation{
		home: home,
		args: append([]string{"transfers", "create", "--broadcast", "--idempotency-key", "k1"},
			canonicalSimulateArgs()...),
	})

	if exit != 2 {
		t.Fatalf("exit = %d, want 2\n%s\n%s", exit, stdout, stderr)
	}

	if got := len(server.Requests()); got != 0 {
		t.Errorf("the fixture saw %d request(s), want 0", got)
	}

	if _, err := ledgerOf(t, home).List(); err != nil {
		t.Errorf("list runs: %v", err)
	}

	if all, _ := ledgerOf(t, home).List(); len(all) != 0 {
		t.Errorf("%d run(s) were written for a usage error, want 0", len(all))
	}
}
