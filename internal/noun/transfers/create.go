package transfers

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/kurenn/ferry-cli/internal/api"
	"github.com/kurenn/ferry-cli/internal/cli/flight"
	"github.com/kurenn/ferry-cli/internal/fault"
	"github.com/kurenn/ferry-cli/internal/noun"
	"github.com/kurenn/ferry-cli/internal/outcome"
	"github.com/kurenn/ferry-cli/internal/render"
	"github.com/kurenn/ferry-cli/internal/runs"
)

// Command builds `ferry transfers`.
func Command(deps Deps) *cobra.Command {
	cmd := noun.NewCommand("transfers", "Price and execute transfers",
		"Price a transfer and, with --broadcast, execute the plan this invocation received.\n\n"+
			"`ferry transfers create` is a dry run: it prices, prints the plan token once, and\n"+
			"moves no money. --broadcast is the only way money leaves.")

	cmd.AddCommand(createCommand(deps), simulateCommand(deps), executeCommand(deps))

	return cmd
}

func createCommand(deps Deps) *cobra.Command {
	var (
		fields bodyFlags
		opts   options
	)

	cmd := &cobra.Command{
		Use:   "create",
		Short: "Price a transfer, and with --broadcast execute it",
		Long: "Price a transfer and print the plan it returns.\n\n" +
			"Without --broadcast nothing is executed and no money moves: the plan token is\n" +
			"printed once and can be executed later with `ferry transfers execute --plan`.\n\n" +
			"With --broadcast this invocation simulates and then executes the plan it just\n" +
			"received, under two idempotency keys minted before the first request. On a\n" +
			"terminal it shows the quote and asks; with no terminal attached, --broadcast is\n" +
			"itself the consent.",
		Args:          noun.NoArgs(),
		SilenceUsage:  true,
		SilenceErrors: false,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runCreate(cmd, deps, opts, fields)
		},
	}

	bindBodyFlags(cmd, &fields)
	bindMoneyFlags(cmd, &opts, moneyFlagSet{broadcast: true, yes: true, wait: true})
	cmd.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return render.UsageFrom(err) })

	return cmd
}

func simulateCommand(deps Deps) *cobra.Command {
	var (
		fields bodyFlags
		opts   options
	)

	cmd := &cobra.Command{
		Use:   "simulate",
		Short: "Price a transfer",
		Long: "Price a transfer and print the plan it returns.\n\n" +
			"This is `ferry transfers create` without --broadcast: it can never execute.",
		Args:          noun.NoArgs(),
		SilenceUsage:  true,
		SilenceErrors: false,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runCreate(cmd, deps, opts, fields)
		},
	}

	bindBodyFlags(cmd, &fields)
	bindMoneyFlags(cmd, &opts, moneyFlagSet{wait: true})
	cmd.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return render.UsageFrom(err) })

	return cmd
}

// runCreate is PLAN §5.4 for a simulate, and §5.5 when --broadcast doubles it.
func runCreate(cmd *cobra.Command, deps Deps, opts options, fields bodyFlags) error {
	// AC87. Two steps need two keys, and a caller who wants to name them
	// runs two invocations. Refused before the profile is even read, so
	// "zero requests" needs no argument beyond the ordering.
	if opts.broadcast && opts.idempotencyKey != "" {
		return render.Usage(
			"--idempotency-key cannot be combined with --broadcast: a broadcast run sends two requests " +
				"and needs two keys.\nRun `ferry transfers create` and `ferry transfers execute --plan …` " +
				"as two invocations to name them yourself.")
	}

	ops := []outcome.Operation{outcome.OpSimulateTransfer}
	if opts.broadcast {
		ops = append(ops, outcome.OpExecuteTransfer)
	}

	r, err := begin(cmd, deps, opts, ops...)
	if err != nil {
		return err
	}

	defer r.close()

	body, err := buildBody(cmd, fields)
	if err != nil {
		return err
	}

	// §5.4 step 4 before step 5 (V3, AC94, M108): a key the ledger already
	// holds against these exact bytes is a resume, and the run it names
	// must not then be reported as an orphan blocking itself.
	match, err := r.route(opts.idempotencyKey, body)
	if err != nil {
		return err
	}

	if match != nil {
		return r.resumeMatched(cmd, match)
	}

	if err := r.orphanCheck(); err != nil {
		return err
	}

	plans := []stepPlan{simulateStep}
	if opts.broadcast {
		plans = append(plans, executeStep)
	}

	if err := r.create(plans, body, opts.idempotencyKey); err != nil {
		return err
	}

	simulated, err := r.perform(cmd.Context(), simulateStep, body, true)
	if err != nil {
		return err
	}

	if !opts.broadcast {
		return r.report(simulated)
	}

	return r.broadcast(cmd, simulated)
}

// broadcast is PLAN §5.4 step 13: the second half of a --broadcast run.
//
// The only path to an execute is a *fresh* simulate 201 carrying
// `plan.token`. Every other way a simulate can end — a refusal, a 202, a
// replay — leaves the execute step unreachable, because the token is minted
// with the plan and is never stored, replayed or polled for (A393, §1.2.9).
// The CLI does not re-simulate on the caller's behalf: that would price a
// second transfer nobody asked for (C8).
func (r *runner) broadcast(cmd *cobra.Command, simulated *answer) error {
	if simulated.out.Exit != 0 {
		r.closeExecute()

		return r.report(simulated)
	}

	token := freshPlanToken(simulated)
	if token == "" {
		r.closeExecute()

		simulated.out = flight.OutcomeFor(outcome.ClassRefusedResimulate, fmt.Sprintf(
			"The plan token was never handed to this invocation, so there is nothing to execute and "+
				"nothing was sent. Simulate again under a new key: `ferry transfers create --broadcast …`. "+
				"Run %s records what happened.", r.runID()))
		simulated.warnings = append(simulated.warnings, simulateGaveNoToken(simulated))

		return r.report(simulated)
	}

	if err := r.handle.SetPlan(runs.StepExecute, planRecord(simulated.sim), token); err != nil {
		return render.Fault(err,
			"the plan could not be recorded on run %s, so the execute was not sent: %v", r.runID(), err)
	}

	fault.Die(fault.AfterSimulateRecordedBeforeExecuteBegin)

	body, err := executeBody(token)
	if err != nil {
		return err
	}

	// --broadcast is C16's second arm: the command carries the consent on a
	// non-TTY, and a terminal is still asked.
	executed, err := r.perform(cmd.Context(), executeStep, body, true)
	if err != nil {
		return err
	}

	executed.warnings = append(executed.warnings, fmt.Sprintf(
		"the plan %s was priced by this invocation's simulate and executed under key %s",
		planID(simulated.sim), r.handle.Run().Step(runs.StepExecute).IdempotencyKey))

	return r.report(executed)
}

// closeExecute marks the execute step unreachable, if it can be.
//
// Best effort by necessity and not by choice: the step is already
// `unreachable` when the simulate answered 202 (AC75 writes it at the
// moment of recording), and `Unreachable` refuses a step that may have sent.
// Both refusals are correct and neither should displace the outcome the
// caller is about to read, so the error is dropped rather than reported.
func (r *runner) closeExecute() {
	step := r.handle.Run().Step(runs.StepExecute)
	if step == nil || step.State.Terminal() || step.State.MayHaveSent() {
		return
	}

	_ = r.handle.Unreachable(runs.StepExecute)
}

// freshPlanToken is the token this invocation may execute, or "".
//
// `Idempotency-Replayed: true` is the case worth naming: the answer is a 201
// and it is the *stored* simulation, which carries no token because the
// token is not stored (C6, A393). Reading `plan.token` without checking the
// header would find `nil` and send `{"plan_token": null}`; reading the
// header without checking the field would trust a shape the contract makes
// optional.
func freshPlanToken(a *answer) string {
	if a.sim == nil || a.res.Meta.Replayed {
		return ""
	}

	if a.sim.Plan.Token == nil || *a.sim.Plan.Token == "" {
		return ""
	}

	return *a.sim.Plan.Token
}

func simulateGaveNoToken(a *answer) string {
	switch {
	case a.res.Meta.Replayed:
		return "the simulate was answered from the idempotency store, and a stored simulation carries no plan token"
	case a.poll != nil:
		return "the simulate was completed asynchronously, and a command's stored result carries no plan token"
	default:
		return "the simulate answered without a plan token"
	}
}

func planRecord(sim *api.Simulation) *runs.Plan {
	if sim == nil {
		return nil
	}

	plan := &runs.Plan{ID: sim.Plan.ID}

	if at, ok := parseTime(sim.Plan.ExpiresAt); ok {
		plan.ExpiresAt = at
	}

	return plan
}

func planID(sim *api.Simulation) string {
	if sim == nil {
		return "(unknown)"
	}

	return sim.Plan.ID
}

// executeBody is AC55: exactly `{"plan_token": "…"}` and nothing else.
//
// `POST /v1/transfers` refuses an unrecognised top-level key outright, so a
// field this CLI adds is not a harmless extra — it is a request FERRY will
// not accept, discovered at the wire (M54).
func executeBody(token string) ([]byte, error) {
	raw, err := api.Marshal(api.ExecuteRequest{PlanToken: token})
	if err != nil {
		return nil, render.Fault(err, "the execute body could not be encoded: %v", err)
	}

	return raw, nil
}
