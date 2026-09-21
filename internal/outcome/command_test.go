package outcome_test

import (
	"strings"
	"testing"

	"github.com/kurenn/ferry/cli/internal/outcome"
)

// AC74 and CRITIQUE B4. The terminal rule is selected by the command body's
// `operation`, because that is what decides which body shape arrived.
//
// Revision 1 selected on the field instead: `completed` → read
// `result.body.status`, `failed` → 8, anything else → 0. A `transfers_simulate`
// command that completed asynchronously stores `{object, command_id, quote,
// plan}` and no top-level `status` (`complete_quote.rb:238-249`), so that rule
// read an absent field, saw "", and answered 0 — success — for a run whose
// plan token was handed to nobody.
func TestTerminalRuleIsSelectedByOperation(t *testing.T) {
	completed := func(op string, result *outcome.CommandResult) outcome.CommandInput {
		return outcome.CommandInput{
			Operation: op,
			State:     "completed",
			CommandID: "cmd_1",
			Result:    result,
		}
	}

	t.Run("a completed simulate is refused_resimulate, not done", func(t *testing.T) {
		// The body a completed simulate actually stores: a result, with no
		// top-level status. The field-selecting rule reads "" here and
		// answers 0; the operation-selecting rule never reads it.
		got := outcome.ClassifyCommand(completed("transfers_simulate", &outcome.CommandResult{Status: 201}))

		assertCommand(t, got, outcome.ClassRefusedResimulate, 4, outcome.MoneyNo, false,
			"the plan token was never issued")
	})

	t.Run("a completed execute with status failed is upstream_failed", func(t *testing.T) {
		got := outcome.ClassifyCommand(completed("transfers_execute",
			&outcome.CommandResult{Status: 201, TransactionStatus: "failed"}))

		assertCommand(t, got, outcome.ClassUpstreamFailed, 8, outcome.MoneySeeUpstream, false,
			"Reconcile against the upstream")
	})

	t.Run("a completed execute with any other status is accepted upstream", func(t *testing.T) {
		got := outcome.ClassifyCommand(completed("transfers_execute",
			&outcome.CommandResult{Status: 201, TransactionStatus: "submitted"}))

		assertCommand(t, got, outcome.ClassAcceptedUpstream, 0, outcome.MoneyAcceptedUpstream, true,
			"not settled")
	})

	t.Run("a completed execute whose result aged out is still 0", func(t *testing.T) {
		// The command row survives for 400 days so the unique key keeps
		// refusing re-execution, but `result` is nil once the replay window
		// passes (`command.rb:210-213`). What is gone is the body, not the
		// fact that it completed.
		in := completed("transfers_execute", nil)
		in.TransactionID = "txn_9"

		got := outcome.ClassifyCommand(in)

		assertCommand(t, got, outcome.ClassAcceptedUpstream, 0, outcome.MoneyAcceptedUpstream, true,
			"no longer replayable")

		if !strings.Contains(got.Next, "txn_9") {
			t.Errorf("next should name the transaction id so a human can reconcile:\n  %s", got.Next)
		}
	})

	t.Run("a completed command for an operation this CLI does not perform escalates", func(t *testing.T) {
		got := outcome.ClassifyCommand(completed("wallets_sweep", &outcome.CommandResult{Status: 201}))

		assertCommand(t, got, outcome.ClassEscalate, 7, outcome.MoneyUnknown, false,
			"which this CLI does not perform")
	})
}

// AC21 and C9. `contradiction` is FERRY saying it found a transaction it
// cannot account for, and it dominates every state — including
// `failed_terminal`, whose ordinary reading is "no money moved". That reading
// is exactly what a contradiction denies.
//
// This is asserted for every state in the contract's enum rather than for a
// sample, because the defect it guards against is an ordering one: a switch on
// `state` placed before the contradiction check answers 4 for
// `failed_terminal` and never reaches it.
func TestContradictionDominatesEveryState(t *testing.T) {
	if len(outcome.CommandStates) != 7 {
		t.Fatalf("openapi.yaml's Command.state enum has seven members; this package declares %d: %v",
			len(outcome.CommandStates), outcome.CommandStates)
	}

	for _, state := range outcome.CommandStates {
		for _, op := range []string{"transfers_simulate", "transfers_execute"} {
			got := outcome.ClassifyCommand(outcome.CommandInput{
				Operation:     op,
				State:         state,
				Contradiction: true,
				CommandID:     "cmd_c",
				// A result and a last error that would otherwise classify
				// 0 and 4 respectively, so a rule that reached them would
				// be visible here.
				Result:        &outcome.CommandResult{Status: 201, TransactionStatus: "submitted"},
				LastErrorCode: "PLAN_EXPIRED",
			})

			if got.Class != outcome.ClassEscalate || got.Exit != 7 {
				t.Errorf("%s/%s with a contradiction: got %s/%d, want escalate/7", op, state, got.Class, got.Exit)
			}

			if got.Money != outcome.MoneyUnknown {
				t.Errorf("%s/%s with a contradiction: money is %q, want unknown", op, state, got.Money)
			}
		}
	}
}

// AC51's two stopping states, and the non-terminal states, which are `pending`
// because that is what "the outcome is not established" means.
func TestNonTerminalAndOperatorStates(t *testing.T) {
	t.Run("needs_operator escalates", func(t *testing.T) {
		got := outcome.ClassifyCommand(outcome.CommandInput{
			Operation: "transfers_execute",
			State:     "needs_operator",
			CommandID: "cmd_op",
		})

		assertCommand(t, got, outcome.ClassEscalate, 7, outcome.MoneyUnknown, false,
			"a human must resolve it")
	})

	for _, state := range []string{"reserved", "inflight", "upstream_unknown", "failed_retriable"} {
		t.Run(state+" is pending", func(t *testing.T) {
			got := outcome.ClassifyCommand(outcome.CommandInput{
				Operation: "transfers_execute",
				State:     state,
				CommandID: "cmd_p",
			})

			assertCommand(t, got, outcome.ClassPending, 6, outcome.MoneyUnknown, true,
				"Keep polling")
		})
	}

	t.Run("a state outside the enum escalates rather than polling forever", func(t *testing.T) {
		// Not `pending`: §5.7's loop sleeps and retries on anything it does
		// not otherwise handle, so `pending` for an unrecognised state is
		// an instruction to poll until the deadline.
		got := outcome.ClassifyCommand(outcome.CommandInput{
			Operation: "transfers_execute",
			State:     "quantum_superposition",
			CommandID: "cmd_x",
		})

		assertCommand(t, got, outcome.ClassEscalate, 7, outcome.MoneyUnknown, false,
			"not in the contract's enum")
	})
}

// A `failed_terminal` command is classified by its `last_error.code` through
// the same table the live answer went through, under the same operation class
// — so a policy denial is 4 whether it arrived synchronously or was read back
// off a command an hour later.
func TestFailedTerminalReadsTheLastErrorThroughTheSameTable(t *testing.T) {
	failed := func(op, code string) outcome.CommandInput {
		return outcome.CommandInput{
			Operation:     op,
			State:         "failed_terminal",
			CommandID:     "cmd_f",
			LastErrorCode: code,
		}
	}

	t.Run("a policy denial on an execute command is 4", func(t *testing.T) {
		got := outcome.ClassifyCommand(failed("transfers_execute", "POLICY_MAX_AMOUNT_PER_DAY"))

		assertCommand(t, got, outcome.ClassRefusedResimulate, 4, outcome.MoneyNo, false, "The plan is spent")
	})

	t.Run("the same code on a simulate command is 3", func(t *testing.T) {
		// The operation class is carried through, so the one code that
		// means two things on the wire still means two things here.
		got := outcome.ClassifyCommand(failed("transfers_simulate", "POLICY_MAX_AMOUNT_PER_DAY"))

		assertCommand(t, got, outcome.ClassRefusedFix, 3, outcome.MoneyNo, false, "Nothing was sent")
	})

	t.Run("UPSTREAM_UNAVAILABLE is 4", func(t *testing.T) {
		got := outcome.ClassifyCommand(failed("transfers_execute", "UPSTREAM_UNAVAILABLE"))

		assertCommand(t, got, outcome.ClassRefusedResimulate, 4, outcome.MoneyNo, false, "the plan is spent")
	})

	t.Run("a last error code this CLI does not know escalates", func(t *testing.T) {
		// Not `pending`: a terminal command will not become a different
		// answer, so "come back and poll" would loop forever.
		got := outcome.ClassifyCommand(failed("transfers_execute", "SOMETHING_NEW"))

		assertCommand(t, got, outcome.ClassEscalate, 7, outcome.MoneyUnknown, false, "which this CLI does not know")
	})

	t.Run("a code whose wire remedy is to retry escalates too", func(t *testing.T) {
		// `RATE_LIMITED` is `transient`/5 on the wire — "nothing was
		// sent, resend the identical request under the same key". Said
		// about a `failed_terminal` command, that is advice to replay one
		// stored answer forever, and `money: no` is a claim about a
		// transfer whose failure reason we are reading second-hand.
		got := outcome.ClassifyCommand(failed("transfers_execute", "RATE_LIMITED"))

		assertCommand(t, got, outcome.ClassEscalate, 7, outcome.MoneyUnknown, false, `means "try again"`)
	})
}

// The whole vocabulary, in one sweep: no catalogued code may classify a
// terminal command into a class that tells the caller to come back.
//
// The sweep and not five examples, because `last_error.code` is written by the
// Rails side from a vocabulary that is not the wire catalogue — the recordings
// carry `UPSTREAM_UNKNOWN` there, which `errors.md` does not name — so which
// codes can arrive is not something this package gets to decide. Holding the
// property over every row is the only version of it that survives a new
// writer on the Ruby side.
func TestNoTerminalCommandIsToldToComeBack(t *testing.T) {
	forbidden := map[outcome.Class]bool{
		outcome.ClassTransient: true,
		outcome.ClassPending:   true,
	}

	codes := outcome.Codes()
	if len(codes) < 50 {
		t.Fatalf("the table has %d rows; this sweep expects the whole catalogue", len(codes))
	}

	checked := 0

	for _, code := range codes {
		for _, op := range []outcome.Operation{outcome.OpExecuteTransfer, outcome.OpSimulateTransfer} {
			checked++

			got := outcome.ClassifyCommand(outcome.CommandInput{
				Operation:     string(op),
				State:         "failed_terminal",
				LastErrorCode: code,
			})

			if forbidden[got.Class] {
				t.Errorf("a %s command that failed terminally with %s classifies %q/%d; a terminal command will not move again, so no class may tell the caller to wait or poll",
					op, code, got.Class, got.Exit)
			}

			if got.SameKeySafe && got.Class != outcome.ClassAcceptedUpstream {
				t.Errorf("a %s command that failed terminally with %s reports same_key_safe; resending the same key replays this terminal answer",
					op, code)
			}
		}
	}

	if checked < 100 {
		t.Fatalf("swept %d (code, operation) pairs; the sweep has stopped matching", checked)
	}
}

func assertCommand(t *testing.T, got outcome.Outcome, class outcome.Class, exit int, money outcome.Money, sameKeySafe bool, nextHas string) {
	t.Helper()

	if got.Class != class {
		t.Errorf("class: got %q, want %q", got.Class, class)
	}

	if got.Exit != exit {
		t.Errorf("exit: got %d, want %d", got.Exit, exit)
	}

	if got.Money != money {
		t.Errorf("money: got %q, want %q", got.Money, money)
	}

	if got.SameKeySafe != sameKeySafe {
		t.Errorf("same_key_safe: got %v, want %v", got.SameKeySafe, sameKeySafe)
	}

	if !strings.Contains(got.Next, nextHas) {
		t.Errorf("next does not contain %q:\n  %s", nextHas, got.Next)
	}
}
