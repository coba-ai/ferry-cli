package transfers

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/kurenn/ferry-cli/internal/cli/flight"
	"github.com/kurenn/ferry-cli/internal/outcome"
	"github.com/kurenn/ferry-cli/internal/render"
	"github.com/kurenn/ferry-cli/internal/runs"
)

// ResumeOptions is what `ferry runs resume` may say.
//
// There is deliberately no field that could mean "this run was already
// approved". C16: consent is established per invocation, so the only way a
// resume can send an execute is `--yes` on *this* command line or a human
// answering *this* prompt. A run's own record cannot carry it, which is why
// `runs.AllStates()` is held to a census and why no state here is read as
// permission (A312).
type ResumeOptions struct {
	Yes          bool
	NoWait       bool
	Timeout      time.Duration
	AllowPending bool
}

// Resume performs `ferry runs resume <id>` (PLAN §5.3, AC73).
//
// It lives in this package rather than in `noun/runs` because it is the
// money path: it is the same [runner], the same `Begin`-then-send order and
// the same consent gate that `transfers create --broadcast` uses. A second
// implementation would be a second place for the order to be got wrong, and
// the order is C1.
func Resume(cmd *cobra.Command, deps Deps, runID string, ro ResumeOptions) error {
	opts := options{
		yes:          ro.Yes,
		noWait:       ro.NoWait,
		timeout:      ro.Timeout,
		allowPending: ro.AllowPending,
	}

	r, err := begin(cmd, deps, opts, outcome.OpSimulateTransfer, outcome.OpExecuteTransfer)
	if err != nil {
		return err
	}

	defer r.close()

	// §5.3 step 1. The lock is taken before the credential is compared so
	// that two resumes of one run cannot both reach step 2.
	if err := r.open(runID); err != nil {
		return err
	}

	// §5.3 step 2, C19, AC70. Nothing has been sent.
	if err := r.checkCredential(); err != nil {
		return err
	}

	return r.resume(cmd.Context())
}

// resumeMatched routes a same-body `--idempotency-key` into the run that
// holds it (AC94, V3).
//
// The caller has named a key the ledger already owns against these exact
// bytes, which is a request to continue that run and not to start one. It
// takes the lock the run's own resume would take, so two invocations racing
// on one key still serialise.
func (r *runner) resumeMatched(cmd *cobra.Command, match *runs.KeyMatch) error {
	if err := r.open(match.Run.RunID); err != nil {
		return err
	}

	if err := r.checkCredential(); err != nil {
		return err
	}

	// The matched run is exempt from the orphan check — it *is* the run
	// being resumed, and refusing it would make a legitimate resume need
	// `--allow-pending` (M109).
	if err := r.orphanCheck(match.Run.RunID); err != nil {
		return err
	}

	return r.resume(cmd.Context())
}

// resume is §5.3's resume order, steps 3 to 6.
func (r *runner) resume(ctx context.Context) error {
	run := r.handle.Run()

	step, why := resumable(run)
	if step == nil {
		return r.nothingToResume(why)
	}

	answered, err := r.resumeStep(ctx, step)
	if err != nil {
		return err
	}

	// §5.3 step 6. A resumed simulate of a two-step run continues into the
	// execute exactly as the original invocation would have, through the
	// same gate: a fresh 201 with a token, or nothing.
	if step.Name == runs.StepSimulate && run.Step(runs.StepExecute) != nil {
		r.opts.broadcast = true

		return r.broadcast(r.cmd, answered)
	}

	return r.report(answered)
}

// resumeStep sends one step's recorded request again.
//
// The body is nil on every path but one: the bytes on disk are the bytes
// resent, and `runs.Begin` pins them against the stored digest, so a resume
// cannot become a different request under a key FERRY has already seen (C2,
// AC45, M3). The exception is a `--broadcast` execute step that never
// started, whose body is built here from the token the simulate stored —
// the one place a resume constructs bytes, and it constructs them from the
// record rather than from a flag.
func (r *runner) resumeStep(ctx context.Context, step *runs.Step) (*answer, error) {
	plan := planFor(step.Name)

	if step.Name == runs.StepExecute && step.Body == nil {
		if !step.HasPlanToken() {
			return nil, r.strandedPlan(step)
		}

		body, err := executeBody(*step.PlanToken)
		if err != nil {
			return nil, err
		}

		// commandIsConsent is false: `runs resume` is not consent (AC73,
		// M79, M81). On a TTY this prompts; on a non-TTY without `--yes`
		// it refuses having sent nothing.
		return r.perform(ctx, plan, body, false)
	}

	return r.perform(ctx, plan, nil, false)
}

// strandedPlan is the `--broadcast` execute step that can never run: the
// simulate left via 202, or replayed, or its token was scrubbed.
//
// Exit 4 and re-simulate. This CLI will not simulate again on the caller's
// behalf — that would price a second transfer nobody asked for, and the
// price may have moved (C8, AC80).
func (r *runner) strandedPlan(step *runs.Step) error {
	reason := "the simulate that would have issued it never handed one to this CLI"
	if step.PlanToken != nil && *step.PlanToken == runs.ScrubbedToken {
		reason = "its plan token has been scrubbed, because the plan is spent or long expired"
	}

	return &render.Error{
		Out: flight.OutcomeFor(outcome.ClassRefusedResimulate, fmt.Sprintf(
			"Nothing was sent. Simulate again under a new key: `ferry transfers create --broadcast …`.")),
		Msg: fmt.Sprintf("run %s cannot be executed: %s", r.runID(), reason),
	}
}

// resumable is §5.3 step 3: the first step a resume may act on.
//
// The order is the steps' own, so a two-step run resumes its simulate before
// its execute. A `not_started` execute step qualifies only when the simulate
// before it is settled and a token is stored — which is the `--broadcast`
// inter-step window of AC80, and the only case where a step nothing has
// touched is worth sending.
func resumable(run *runs.Run) (*runs.Step, string) {
	for i, step := range run.Steps {
		if step.Unresolved() {
			return step, ""
		}

		if step.State == runs.StateNotStarted && i > 0 && step.HasPlanToken() {
			return step, ""
		}
	}

	for _, step := range run.Steps {
		switch step.State {
		case runs.StateDeclined:
			return nil, fmt.Sprintf("its %s step was declined, and a decline is not reopened by a resume", step.Name)
		case runs.StateUnreachable:
			return nil, fmt.Sprintf("its %s step can never run", step.Name)
		case runs.StateNotStarted:
			return nil, fmt.Sprintf("its %s step holds nothing to send", step.Name)
		}
	}

	return nil, "every step is settled"
}

// nothingToResume reports a run that has nothing left to do.
//
// Exit 4 rather than 0. A caller who typed `runs resume` believes there is
// something outstanding, and answering 0 would tell them it has now been
// done. Exit 4 says "not this run; simulate again", which is the true next
// move for a settled or declined one.
func (r *runner) nothingToResume(why string) error {
	return &render.Error{
		Out: flight.OutcomeFor(outcome.ClassRefusedResimulate, fmt.Sprintf(
			"Nothing was sent. `ferry runs show %s` reads the record; simulate again if you still want the transfer.",
			r.runID())),
		Msg: fmt.Sprintf("run %s has nothing to resume: %s", r.runID(), why),
	}
}
