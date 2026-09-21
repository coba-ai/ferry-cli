package outcome

import "fmt"

// CommandResult is `result` on the command body
// (`app/models/command.rb:210-213`). It is populated only for a `completed`
// row still inside its replay window, so a nil CommandResult on a `completed`
// command means "the answer aged out", not "it failed".
type CommandResult struct {
	Status int
	// TransactionStatus is `result.body.status`. It is absent for a
	// `transfers_simulate` command by construction: `CompleteQuote#stored_body`
	// (`complete_quote.rb:238-249`) stores `{object, command_id, quote, plan}`
	// and no top-level `status`.
	TransactionStatus string
}

// CommandInput is a command body, reduced to what the terminal rule reads.
type CommandInput struct {
	// Operation is the body's `operation` field. B4/AC74: the terminal rule
	// is selected by *this*, not by a field a quote body does not carry.
	Operation string
	State     string
	// Contradiction is true when the body's `contradiction` is non-null.
	Contradiction bool
	CommandID     string
	TransactionID string
	Result        *CommandResult
	LastErrorCode string
}

// ClassifyCommand is PLAN §5.6's terminal table (AC74), C9's contradiction
// rule (AC21) and AC51's two stopping states.
//
// == Why `operation` selects the rule
//
// CRITIQUE B4. Revision 1 applied the transaction rule to both operations:
// `completed` → classify `result.body.status` as a 201. A `transfers_simulate`
// command that reached `completed` has no `status` in its stored body, so that
// rule reads an absent field, gets "", and answers 0 — "your simulation
// succeeded" — for a command whose plan token was handed to nobody. Under
// `--broadcast` the same path then offers an execute step a plan with no
// token.
//
// The fix is not a guard on the field; it is selecting the rule by the thing
// that decides which body shape arrived.
func ClassifyCommand(in CommandInput) Outcome {
	// C9 and AC21: a contradiction dominates every state, `failed_terminal`
	// included, whose usual "no money moved" reading does not hold here
	// (`openapi.yaml`, Command.contradiction). This is checked first, and
	// the order is the control M22 mutates.
	if in.Contradiction {
		return commandOutcome(in, ClassEscalate,
			"FERRY found a transaction it cannot account for against this command. Stop and escalate whatever `state` says; on a live environment execution is already frozen.")
	}

	if !KnownState(in.State) {
		// Fail closed, and deliberately not `pending`: §5.7's poll loop
		// sleeps and loops on any state it does not otherwise handle, so
		// answering `pending` for a state outside the contract's enum is an
		// answer that polls forever.
		return commandOutcome(in, ClassEscalate, fmt.Sprintf(
			"This command is in state %q, which is not in the contract's enum. This CLI cannot say what it means.", in.State,
		))
	}

	if in.State == "needs_operator" {
		return commandOutcome(in, ClassEscalate,
			"This command could not be resolved automatically and is with an operator. Stop polling; a human must resolve it.")
	}

	switch in.State {
	case "completed":
		return classifyCompleted(in)
	case "failed_terminal":
		return classifyFailedTerminal(in)
	}

	// `reserved`, `inflight`, `upstream_unknown`, `failed_retriable`: in
	// progress. The outcome is not established, which is `pending` by
	// definition, and §5.7 keeps polling.
	return commandOutcome(in, ClassPending, fmt.Sprintf(
		"This command is %s: the outcome is not established. Keep polling; do not resend under a new key.", in.State,
	))
}

func classifyCompleted(in CommandInput) Outcome {
	switch in.Operation {
	case string(OpExecuteTransfer):
		if in.Result == nil {
			// The row survives for 400 days so the unique key keeps
			// refusing re-execution, but the stored answer aged out of the
			// replay window (`command.rb:210-213`). The command completed;
			// what is gone is the body, not the fact.
			return commandOutcome(in, ClassAcceptedUpstream,
				"This transfer completed, but its stored result is no longer replayable. Reconcile against the upstream using the transaction id.")
		}

		if in.Result.TransactionStatus == "failed" {
			return commandOutcome(in, ClassUpstreamFailed,
				"FERRY accepted this transfer upstream and the upstream then failed it. Reconcile against the upstream before simulating a replacement.")
		}

		if in.Result.TransactionStatus == "" {
			return commandOutcome(in, ClassAcceptedUpstream,
				"Accepted upstream, not settled. Read `status` and `subStatus`, and reconcile later.",
				"the stored transaction body carried no `status`, so acceptance could not be qualified")
		}

		return commandOutcome(in, ClassAcceptedUpstream,
			"Accepted upstream, not settled. Read `status` and `subStatus`, and reconcile later.")

	case string(OpSimulateTransfer):
		// B4. The plan exists and is unusable: `render_simulated`
		// (`command_request.rb:305-309`) is the only writer of `plan.token`
		// and it never runs for a command that completed asynchronously.
		return commandOutcome(in, ClassRefusedResimulate,
			"FERRY completed this simulation after the request ended; the plan token was never issued — simulate again under a new key.")
	}

	// A completed command for an operation this CLI does not perform. Fail
	// closed for the same reason the unknown-state branch does.
	return commandOutcome(in, ClassEscalate, fmt.Sprintf(
		"This command completed under the operation %q, which this CLI does not perform, so it cannot say what the result means.", in.Operation,
	))
}

// classifyFailedTerminal reads the row for `last_error.code`, under the
// operation class the command's own `operation` names — so a policy denial on
// an execute command is 4 here exactly as it was 4 on the wire.
func classifyFailedTerminal(in CommandInput) Outcome {
	class, ok := ClassOf(Operation(in.Operation))
	if !ok {
		return commandOutcome(in, ClassEscalate, fmt.Sprintf(
			"This command failed terminally under the operation %q, which this CLI does not perform.", in.Operation,
		))
	}

	r, known := rules[in.LastErrorCode]
	if !known {
		// Not `pending`: a terminal command cannot be resumed into a
		// different answer, so "come back and poll" would loop. A code we
		// cannot read on a terminal money command is a human's problem.
		return commandOutcome(in, ClassEscalate, fmt.Sprintf(
			"This command failed terminally with the code %q, which this CLI does not know.", in.LastErrorCode,
		))
	}

	v := r.verdictFor(class)

	return commandOutcome(in, v.class, v.next)
}

func commandOutcome(in CommandInput, class Class, next string, warnings ...string) Outcome {
	out := newOutcome(class, next, warnings...)

	if in.CommandID != "" {
		if out.Next != "" {
			out.Next += " "
		}

		out.Next += fmt.Sprintf("Command %s: `ferry commands get %s`.", in.CommandID, in.CommandID)
	}

	if in.TransactionID != "" {
		if out.Next != "" {
			out.Next += " "
		}

		out.Next += fmt.Sprintf("Transaction id %s.", in.TransactionID)
	}

	return out
}
