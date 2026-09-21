package outcome_test

import (
	"strings"
	"testing"

	"github.com/kurenn/ferry-cli/internal/outcome"
)

// AC20. One example per row of PLAN §5.6, asserting the whole tuple —
// (class, exit, money, same_key_safe) — and a phrase of `next` that could only
// come from the intended row.
//
// The whole tuple, not the exit code alone, because `money` and
// `same_key_safe` are what a wrapper script and a human respectively act on,
// and a row that returns the right number while saying "the same key is safe"
// about a spent plan is wrong in the way that costs money.
//
// `nextHas` is a phrase and not the sentence: pinning the prose would make
// every wording change a test edit, and a test that is edited on every change
// stops being read. But it must be a phrase that distinguishes the row, so
// that swapping two rows' verdicts cannot leave it green.

type row struct {
	name        string
	in          outcome.Input
	class       outcome.Class
	exit        int
	money       outcome.Money
	sameKeySafe bool
	nextHas     string
}

func execErr(status int, code string) outcome.Input {
	return outcome.Input{
		Op:       outcome.OpExecuteTransfer,
		Status:   status,
		Envelope: &outcome.Envelope{Code: code},
	}
}

func simErr(status int, code string) outcome.Input {
	return outcome.Input{
		Op:       outcome.OpSimulateTransfer,
		Status:   status,
		Envelope: &outcome.Envelope{Code: code},
	}
}

func readErr(status int, code string) outcome.Input {
	return outcome.Input{
		Op:       outcome.OpGetCorridor,
		Status:   status,
		Envelope: &outcome.Envelope{Code: code},
	}
}

func withDetails(in outcome.Input, kv map[string]any) outcome.Input {
	in.Envelope = &outcome.Envelope{Code: in.Envelope.Code, Details: kv}

	return in
}

func TestDecisionTableRows(t *testing.T) {
	rows := []row{
		// --- the refusals that cost nothing, on both money operations ---
		{
			name:  "400 on execute is refused_fix: the plan is not looked up",
			in:    execErr(400, "VALIDATION_FAILED"),
			class: outcome.ClassRefusedFix, exit: 3, money: outcome.MoneyNo, sameKeySafe: false,
			nextHas: "new Idempotency-Key",
		},
		{
			name:  "401 on execute is refused_fix",
			in:    execErr(401, "TOKEN_EXPIRED"),
			class: outcome.ClassRefusedFix, exit: 3, money: outcome.MoneyNo, sameKeySafe: false,
			nextHas: "Nothing was sent and nothing was spent",
		},
		{
			name:  "403 INSUFFICIENT_SCOPE on execute is refused_fix",
			in:    execErr(403, "INSUFFICIENT_SCOPE"),
			class: outcome.ClassRefusedFix, exit: 3, money: outcome.MoneyNo, sameKeySafe: false,
			nextHas: "Nothing was sent and nothing was spent",
		},
		{
			name:  "404 NOT_FOUND on execute is refused_fix, not refused_resimulate",
			in:    execErr(404, "NOT_FOUND"),
			class: outcome.ClassRefusedFix, exit: 3, money: outcome.MoneyNo, sameKeySafe: false,
			nextHas: "Nothing was sent and nothing was spent",
		},

		// --- the spend policy: Denied commits with the plan consumed ---
		{
			name:  "POLICY_MAX_AMOUNT_PER_DAY on execute is refused_resimulate: the plan is spent",
			in:    execErr(403, "POLICY_MAX_AMOUNT_PER_DAY"),
			class: outcome.ClassRefusedResimulate, exit: 4, money: outcome.MoneyNo, sameKeySafe: false,
			nextHas: "The plan is spent",
		},
		{
			name:  "POLICY_CUSTOMER_NOT_ALLOWED on execute is refused_resimulate",
			in:    execErr(403, "POLICY_CUSTOMER_NOT_ALLOWED"),
			class: outcome.ClassRefusedResimulate, exit: 4, money: outcome.MoneyNo, sameKeySafe: false,
			nextHas: "The plan is spent",
		},
		{
			name:  "DESTINATION_TOO_NEW on execute is refused_resimulate",
			in:    execErr(403, "DESTINATION_TOO_NEW"),
			class: outcome.ClassRefusedResimulate, exit: 4, money: outcome.MoneyNo, sameKeySafe: false,
			nextHas: "the plan behind the refused request is spent",
		},
		{
			name:  "EXECUTION_SUSPENDED on execute is refused_resimulate",
			in:    execErr(403, "EXECUTION_SUSPENDED"),
			class: outcome.ClassRefusedResimulate, exit: 4, money: outcome.MoneyNo, sameKeySafe: false,
			nextHas: "The plan is spent",
		},
		{
			// The disjointness the operation input buys: one code, two
			// instructions, because on simulate there is no plan to spend.
			name:  "EXECUTION_SUSPENDED on simulate is refused_fix: no plan exists to spend",
			in:    simErr(403, "EXECUTION_SUSPENDED"),
			class: outcome.ClassRefusedFix, exit: 3, money: outcome.MoneyNo, sameKeySafe: false,
			nextHas: "Nothing was sent and nothing was spent",
		},
		{
			name:  "POLICY_MAX_AMOUNT_PER_DAY on simulate is refused_fix",
			in:    simErr(403, "POLICY_MAX_AMOUNT_PER_DAY"),
			class: outcome.ClassRefusedFix, exit: 3, money: outcome.MoneyNo, sameKeySafe: false,
			nextHas: "Nothing was sent and nothing was spent",
		},

		// --- the plan ---
		{
			name:  "PLAN_NOT_FOUND on execute is refused_resimulate",
			in:    execErr(404, "PLAN_NOT_FOUND"),
			class: outcome.ClassRefusedResimulate, exit: 4, money: outcome.MoneyNo, sameKeySafe: false,
			nextHas: "Simulate again",
		},
		{
			name:  "PLAN_EXPIRED on execute is refused_resimulate",
			in:    execErr(410, "PLAN_EXPIRED"),
			class: outcome.ClassRefusedResimulate, exit: 4, money: outcome.MoneyNo, sameKeySafe: false,
			nextHas: "plans last five minutes",
		},
		{
			name:  "PLAN_ALREADY_USED on execute is refused_resimulate",
			in:    execErr(409, "PLAN_ALREADY_USED"),
			class: outcome.ClassRefusedResimulate, exit: 4, money: outcome.MoneyNo, sameKeySafe: false,
			nextHas: "single-use",
		},
		{
			name:  "PLAN_CHANGED on execute is refused_resimulate",
			in:    execErr(409, "PLAN_CHANGED"),
			class: outcome.ClassRefusedResimulate, exit: 4, money: outcome.MoneyNo, sameKeySafe: false,
			nextHas: "re-priced",
		},
		{
			// Rolled back rather than spent (begin.rb:47-51), and the
			// sentence says so; the class is still 4 because the remedy is
			// a new plan either way.
			name:  "PLAN_KEY_MISMATCH on execute is refused_resimulate and says it was rolled back",
			in:    execErr(403, "PLAN_KEY_MISMATCH"),
			class: outcome.ClassRefusedResimulate, exit: 4, money: outcome.MoneyNo, sameKeySafe: false,
			nextHas: "rolled back rather than spent",
		},

		// --- the idempotency key ---
		{
			name: "IDEMPOTENCY_KEY_IN_PROGRESS is pending and names the command",
			in: withDetails(execErr(409, "IDEMPOTENCY_KEY_IN_PROGRESS"), map[string]any{
				"command_id": "cmd_01HZ",
			}),
			class: outcome.ClassPending, exit: 6, money: outcome.MoneyUnknown, sameKeySafe: true,
			nextHas: "ferry commands get cmd_01HZ",
		},
		{
			name: "IDEMPOTENCY_KEY_REPLAY_EXPIRED escalates and renders details.state",
			in: withDetails(execErr(409, "IDEMPOTENCY_KEY_REPLAY_EXPIRED"), map[string]any{
				"state": "completed",
			}),
			class: outcome.ClassEscalate, exit: 7, money: outcome.MoneyUnknown, sameKeySafe: false,
			nextHas: "The command under this key is `completed`.",
		},
		{
			name:  "COMMAND_UNRESOLVED escalates on execute",
			in:    execErr(409, "COMMAND_UNRESOLVED"),
			class: outcome.ClassEscalate, exit: 7, money: outcome.MoneyUnknown, sameKeySafe: false,
			nextHas: "A human must read this command",
		},
		{
			// AC51: `commands watch` is a read, and polling must stop here
			// rather than treat an operator hold as a fixable refusal.
			name:  "COMMAND_UNRESOLVED escalates on a read too",
			in:    readErr(409, "COMMAND_UNRESOLVED"),
			class: outcome.ClassEscalate, exit: 7, money: outcome.MoneyUnknown, sameKeySafe: false,
			nextHas: "A human must read this command",
		},

		// --- the upstream ---
		{
			name:  "UPSTREAM_NOT_CONFIGURED on execute is refused_fix: preflight refuses before the plan",
			in:    execErr(503, "UPSTREAM_NOT_CONFIGURED"),
			class: outcome.ClassRefusedFix, exit: 3, money: outcome.MoneyNo, sameKeySafe: false,
			nextHas: "no upstream credential",
		},
		{
			name:  "UPSTREAM_UNAVAILABLE is refused_resimulate: only ever replayed, and the plan is spent",
			in:    execErr(503, "UPSTREAM_UNAVAILABLE"),
			class: outcome.ClassRefusedResimulate, exit: 4, money: outcome.MoneyNo, sameKeySafe: false,
			nextHas: "the plan is spent",
		},
		{
			name:  "UPSTREAM_CREDENTIALS_INVALID is refused_fix",
			in:    execErr(503, "UPSTREAM_CREDENTIALS_INVALID"),
			class: outcome.ClassRefusedFix, exit: 3, money: outcome.MoneyNo, sameKeySafe: false,
			nextHas: "must re-attach it",
		},
		{
			name:  "UPSTREAM_CONTRACT_VIOLATION on execute is refused_resimulate",
			in:    execErr(502, "UPSTREAM_CONTRACT_VIOLATION"),
			class: outcome.ClassRefusedResimulate, exit: 4, money: outcome.MoneyNo, sameKeySafe: false,
			nextHas: "the plan is untouched",
		},
		{
			name:  "UPSTREAM_CONTRACT_VIOLATION on simulate is refused_fix",
			in:    simErr(502, "UPSTREAM_CONTRACT_VIOLATION"),
			class: outcome.ClassRefusedFix, exit: 3, money: outcome.MoneyNo, sameKeySafe: false,
			nextHas: "Nothing was sent and nothing was spent",
		},
		{
			name:  "SERVICE_UNAVAILABLE is transient on money",
			in:    execErr(503, "SERVICE_UNAVAILABLE"),
			class: outcome.ClassTransient, exit: 5, money: outcome.MoneyNo, sameKeySafe: true,
			nextHas: "same Idempotency-Key",
		},
		{
			name:  "UPSTREAM_AUTH_UNAVAILABLE is transient on money",
			in:    execErr(503, "UPSTREAM_AUTH_UNAVAILABLE"),
			class: outcome.ClassTransient, exit: 5, money: outcome.MoneyNo, sameKeySafe: true,
			nextHas: "same Idempotency-Key",
		},

		// --- rate limiting and FERRY's own faults ---
		{
			name:  "429 RATE_LIMITED is transient on money",
			in:    execErr(429, "RATE_LIMITED"),
			class: outcome.ClassTransient, exit: 5, money: outcome.MoneyNo, sameKeySafe: true,
			nextHas: "same Idempotency-Key",
		},
		{
			// send_and_record sends upstream before the caller sees a byte,
			// so a 500 on the money path cannot mean "nothing was sent".
			name:  "500 INTERNAL_ERROR on money is pending",
			in:    execErr(500, "INTERNAL_ERROR"),
			class: outcome.ClassPending, exit: 6, money: outcome.MoneyUnknown, sameKeySafe: true,
			nextHas: "The outcome is not established",
		},
		{
			name:  "500 INTERNAL_ERROR on a read is transient",
			in:    readErr(500, "INTERNAL_ERROR"),
			class: outcome.ClassTransient, exit: 5, money: outcome.MoneyNo, sameKeySafe: true,
			nextHas: "run the command again",
		},

		// --- the quote surface ---
		{
			name:  "422 QUOTE_REJECTED on simulate is refused_fix",
			in:    simErr(422, "QUOTE_REJECTED"),
			class: outcome.ClassRefusedFix, exit: 3, money: outcome.MoneyNo, sameKeySafe: false,
			nextHas: "change the transfer",
		},
		{
			name:  "422 CORRIDOR_UNSUPPORTED on simulate is refused_fix",
			in:    simErr(422, "CORRIDOR_UNSUPPORTED"),
			class: outcome.ClassRefusedFix, exit: 3, money: outcome.MoneyNo, sameKeySafe: false,
			nextHas: "does not quote transfers",
		},
	}

	runRows(t, rows)
}

// AC20 and the `Ferry-Command-Id` discriminant. Two `UPSTREAM_BUSY`s reach the
// caller under one code: `read_quote`'s command-less refusal, where nothing
// was sent, and `give_up`'s, where FERRY holds a `failed_retriable` command
// and has scheduled its own recovery. Telling the second caller "nothing was
// sent" would be false.
func TestUpstreamBusySplitsOnTheCommandHeader(t *testing.T) {
	runRows(t, []row{
		{
			name:  "no Ferry-Command-Id: nothing was sent, same key",
			in:    execErr(503, "UPSTREAM_BUSY"),
			class: outcome.ClassTransient, exit: 5, money: outcome.MoneyNo, sameKeySafe: true,
			nextHas: "Nothing was sent",
		},
		{
			name: "with Ferry-Command-Id: FERRY is retrying, poll",
			in: func() outcome.Input {
				in := execErr(503, "UPSTREAM_BUSY")
				in.CommandID = "cmd_busy"

				return in
			}(),
			class: outcome.ClassPending, exit: 6, money: outcome.MoneyUnknown, sameKeySafe: true,
			nextHas: "scheduled its own retry",
		},
		{
			// The read column does not split: a GET carries no key and
			// nothing of the caller's is in flight either way.
			name:  "on a read it is transient with or without the header",
			in:    readErr(503, "UPSTREAM_BUSY"),
			class: outcome.ClassTransient, exit: 5, money: outcome.MoneyNo, sameKeySafe: true,
			nextHas: "run the command again",
		},
	})
}

// AC71 and C19/B2. One code, two unrelated facts, distinguished only by
// `details.reason` (`replay.rb:103-142`).
//
// The asymmetry is the point. `different_request` means the digest did not
// match, so nothing of this caller's is under the key and a new key is safe
// advice. Every other answer — `different_credential`, a reason this CLI does
// not recognise, no reason at all — may be a `completed` command the caller
// cannot read, and "use a new key" there is an instruction to send the
// transfer twice.
func TestIdempotencyKeyReusedSplitsOnDetailsReason(t *testing.T) {
	reused := func(reason string) outcome.Input {
		in := execErr(409, "IDEMPOTENCY_KEY_REUSED")
		details := map[string]any{"command_id": "cmd_reused"}

		if reason != "" {
			details["reason"] = reason
		}

		return withDetails(in, details)
	}

	runRows(t, []row{
		{
			name:  "different_request is a client bug: fix and use a new key",
			in:    reused("different_request"),
			class: outcome.ClassRefusedFix, exit: 3, money: outcome.MoneyNo, sameKeySafe: false,
			nextHas: "already used for a *different* request body",
		},
		{
			name:  "different_credential escalates and names details.command_id",
			in:    reused("different_credential"),
			class: outcome.ClassEscalate, exit: 7, money: outcome.MoneyUnknown, sameKeySafe: false,
			nextHas: "ferry commands get cmd_reused",
		},
		{
			name:  "an absent reason escalates",
			in:    reused(""),
			class: outcome.ClassEscalate, exit: 7, money: outcome.MoneyUnknown, sameKeySafe: false,
			nextHas: "Do not send a new key",
		},
		{
			// The fail-closed direction. A reason the server grows later
			// lands on `escalate` without an edit here, because the table
			// declares `escalate` and only one string narrows it.
			name:  "a reason this CLI does not recognise escalates",
			in:    reused("some_future_reason"),
			class: outcome.ClassEscalate, exit: 7, money: outcome.MoneyUnknown, sameKeySafe: false,
			nextHas: "Do not send a new key",
		},
		{
			name:  "different_credential escalates on simulate too",
			in:    withDetails(simErr(409, "IDEMPOTENCY_KEY_REUSED"), map[string]any{"reason": "different_credential"}),
			class: outcome.ClassEscalate, exit: 7, money: outcome.MoneyUnknown, sameKeySafe: false,
			nextHas: "Do not send a new key",
		},
	})
}

// AC22 and C9. A 201 from execute is acceptance upstream, not settlement, and
// the one status that means the money is gone in the other direction is
// `failed`.
func TestExecuteSuccessClassifiesByTransactionStatus(t *testing.T) {
	exec201 := func(status string) outcome.Input {
		return outcome.Input{
			Op:      outcome.OpExecuteTransfer,
			Status:  201,
			Success: &outcome.Success{Parsed: true, TransactionStatus: status},
		}
	}

	runRows(t, []row{
		{
			name:  "status failed is upstream_failed",
			in:    exec201("failed"),
			class: outcome.ClassUpstreamFailed, exit: 8, money: outcome.MoneySeeUpstream, sameKeySafe: false,
			nextHas: "Reconcile against the upstream",
		},
		{
			name:  "status submitted is accepted upstream, not settled",
			in:    exec201("submitted"),
			class: outcome.ClassAcceptedUpstream, exit: 0, money: outcome.MoneyAcceptedUpstream, sameKeySafe: true,
			nextHas: "not settled",
		},
		{
			name:  "status completed is still only acceptance as far as this CLI is concerned",
			in:    exec201("completed"),
			class: outcome.ClassAcceptedUpstream, exit: 0, money: outcome.MoneyAcceptedUpstream, sameKeySafe: true,
			nextHas: "not settled",
		},
		{
			// An absent status is not `failed`, so it is not 8; but it is
			// not a qualified acceptance either, and the warning says so.
			name:  "an absent status is accepted upstream with a warning",
			in:    exec201(""),
			class: outcome.ClassAcceptedUpstream, exit: 0, money: outcome.MoneyAcceptedUpstream, sameKeySafe: true,
			nextHas: "not settled",
		},
	})

	if w := outcome.Classify(exec201("")).Warnings; len(w) != 1 {
		t.Errorf("a 201 with no `status` should warn that acceptance could not be qualified; got %v", w)
	}

	if w := outcome.Classify(exec201("submitted")).Warnings; len(w) != 0 {
		t.Errorf("a 201 with a `status` should not warn; got %v", w)
	}
}

func TestSimulateSuccessIsDone(t *testing.T) {
	fresh := outcome.Input{
		Op:      outcome.OpSimulateTransfer,
		Status:  201,
		Success: &outcome.Success{Parsed: true},
	}

	replayed := fresh
	replayed.Replayed = true

	runRows(t, []row{
		{
			name:  "a fresh simulate is done and says the token is printed once",
			in:    fresh,
			class: outcome.ClassDone, exit: 0, money: outcome.MoneyNo, sameKeySafe: true,
			nextHas: "printed once",
		},
		{
			// The replay carries no `plan.token`; exit 0 is still right,
			// but a caller told only "0" would look for a token that is
			// not there.
			name:  "a replayed simulate is done and says the token is not in the replay",
			in:    replayed,
			class: outcome.ClassDone, exit: 0, money: outcome.MoneyNo, sameKeySafe: true,
			nextHas: "is not in a replay",
		},
	})

	if w := outcome.Classify(replayed).Warnings; len(w) != 1 {
		t.Errorf("a replayed simulate should warn that the answer carries no plan.token; got %v", w)
	}
}

// A 202 is the 'not established' answer by construction, on both money
// operations. It is never 0, and never a new key.
func TestAcceptedIsPending(t *testing.T) {
	for _, op := range []outcome.Operation{outcome.OpSimulateTransfer, outcome.OpExecuteTransfer} {
		got := outcome.Classify(outcome.Input{
			Op:      op,
			Status:  202,
			Success: &outcome.Success{Parsed: true},
		})

		if got.Class != outcome.ClassPending || got.Exit != 6 {
			t.Errorf("202 on %s: got %s/%d, want pending/6", op, got.Class, got.Exit)
		}

		if got.Money != outcome.MoneyUnknown {
			t.Errorf("202 on %s: money is %q, want unknown", op, got.Money)
		}
	}
}

// AC24 and C3, both halves.
//
// The money half is the one that matters: `refused_fix` asserts "nothing was
// sent", and an answer this CLI cannot read is exactly the case where that
// cannot be asserted. The read half is B1/V1's: never exit 1, which means
// "the CLI stopped before any request left" and would be a lie about a
// request the server answered.
func TestUnknownCodeFailsClosed(t *testing.T) {
	runRows(t, []row{
		{
			name:  "an unknown code on execute is pending, not refused",
			in:    execErr(418, "SOMETHING_NEW"),
			class: outcome.ClassPending, exit: 6, money: outcome.MoneyUnknown, sameKeySafe: true,
			nextHas: `"SOMETHING_NEW"`,
		},
		{
			name:  "an unknown 5xx code on execute is pending, not transient",
			in:    execErr(503, "SOMETHING_NEW"),
			class: outcome.ClassPending, exit: 6, money: outcome.MoneyUnknown, sameKeySafe: true,
			nextHas: "cannot say whether the request executed",
		},
		{
			name:  "an unknown code on simulate is pending",
			in:    simErr(418, "SOMETHING_NEW"),
			class: outcome.ClassPending, exit: 6, money: outcome.MoneyUnknown, sameKeySafe: true,
			nextHas: "cannot say whether the request executed",
		},
		{
			name:  "an unknown 4xx code on a read is refused_fix",
			in:    readErr(418, "SOMETHING_NEW"),
			class: outcome.ClassRefusedFix, exit: 3, money: outcome.MoneyNo, sameKeySafe: false,
			nextHas: "unrecognised code",
		},
		{
			name:  "an unknown 5xx code on a read is transient",
			in:    readErr(500, "SOMETHING_NEW"),
			class: outcome.ClassTransient, exit: 5, money: outcome.MoneyNo, sameKeySafe: true,
			nextHas: "unrecognised code",
		},
	})

	// V1's explicit floor, stated once as its own claim: no unknown read
	// code, at any status the CLI could see, may be exit 1. Exit 1 means the
	// CLI stopped before any request left the host, and by the time a code
	// is being classified that is false.
	for _, status := range []int{400, 401, 403, 404, 409, 410, 413, 415, 418, 422, 429, 500, 502, 503, 504, 599} {
		got := outcome.Classify(readErr(status, "SOMETHING_NEW"))
		if got.Exit == 1 {
			t.Errorf("unknown read code at %d classified exit 1; C17 reserves 1 for a stop before the wire", status)
		}
	}
}

// AC83 and G4. An answer whose body is not the seven-key envelope did not come
// from FERRY's error renderer, so it says nothing about what FERRY did — which
// on a money path is `pending`, not a refusal and not a retry.
func TestNonEnvelopeBodies(t *testing.T) {
	nonEnvelope := func(op outcome.Operation, status int) outcome.Input {
		return outcome.Input{Op: op, Status: status}
	}

	runRows(t, []row{
		{
			name:  "an HTML 502 on execute is pending",
			in:    nonEnvelope(outcome.OpExecuteTransfer, 502),
			class: outcome.ClassPending, exit: 6, money: outcome.MoneyUnknown, sameKeySafe: true,
			nextHas: "not the error envelope",
		},
		{
			name:  "a 413 on execute is pending",
			in:    nonEnvelope(outcome.OpExecuteTransfer, 413),
			class: outcome.ClassPending, exit: 6, money: outcome.MoneyUnknown, sameKeySafe: true,
			nextHas: "not the error envelope",
		},
		{
			name:  "a 415 on simulate is pending",
			in:    nonEnvelope(outcome.OpSimulateTransfer, 415),
			class: outcome.ClassPending, exit: 6, money: outcome.MoneyUnknown, sameKeySafe: true,
			nextHas: "not the error envelope",
		},
		{
			name:  "an HTML 502 on a read is transient",
			in:    nonEnvelope(outcome.OpGetCorridor, 502),
			class: outcome.ClassTransient, exit: 5, money: outcome.MoneyNo, sameKeySafe: true,
			nextHas: "not the error envelope",
		},
		{
			// C3 and §7.1 A14: a 201 whose body will not parse is not a
			// success. The CLI cannot print what it cannot read, and on the
			// money path "I could not read the answer" is `pending`.
			name:  "an unparseable 201 on execute is pending, not done",
			in:    outcome.Input{Op: outcome.OpExecuteTransfer, Status: 201, Success: &outcome.Success{Parsed: false}},
			class: outcome.ClassPending, exit: 6, money: outcome.MoneyUnknown, sameKeySafe: true,
			nextHas: "could not be read",
		},
		{
			name:  "an unparseable 200 on a read is transient",
			in:    outcome.Input{Op: outcome.OpGetCorridor, Status: 200, Success: &outcome.Success{Parsed: false}},
			class: outcome.ClassTransient, exit: 5, money: outcome.MoneyNo, sameKeySafe: true,
			nextHas: "could not be read",
		},
	})
}

// AC23's classification half. The split is by whether bytes may have left the
// host — measured by the httptrace hook in `api.Do`, not guessed from an error
// string.
func TestTransportErrorsSplitByPhase(t *testing.T) {
	fault := func(op outcome.Operation, phase outcome.TransportPhase) outcome.Input {
		return outcome.Input{Op: op, Transport: &outcome.Transport{Phase: phase}}
	}

	runRows(t, []row{
		{
			name:  "a dial failure on execute is transient: nothing left the host",
			in:    fault(outcome.OpExecuteTransfer, outcome.PhaseDial),
			class: outcome.ClassTransient, exit: 5, money: outcome.MoneyNo, sameKeySafe: true,
			nextHas: "never established",
		},
		{
			name:  "a failure after the request was written is pending",
			in:    fault(outcome.OpExecuteTransfer, outcome.PhaseAfterWrite),
			class: outcome.ClassPending, exit: 6, money: outcome.MoneyUnknown, sameKeySafe: true,
			nextHas: "may have executed",
		},
		{
			name:  "a dial failure on simulate is transient",
			in:    fault(outcome.OpSimulateTransfer, outcome.PhaseDial),
			class: outcome.ClassTransient, exit: 5, money: outcome.MoneyNo, sameKeySafe: true,
			nextHas: "never established",
		},
		{
			name:  "a failure after write on simulate is pending",
			in:    fault(outcome.OpSimulateTransfer, outcome.PhaseAfterWrite),
			class: outcome.ClassPending, exit: 6, money: outcome.MoneyUnknown, sameKeySafe: true,
			nextHas: "may have executed",
		},
		{
			name:  "a dial failure on a read is transient",
			in:    fault(outcome.OpGetCorridor, outcome.PhaseDial),
			class: outcome.ClassTransient, exit: 5, money: outcome.MoneyNo, sameKeySafe: true,
			nextHas: "never established",
		},
		{
			// A read cannot have moved money, so after-write is still only
			// worth another try.
			name:  "a failure after write on a read is transient",
			in:    fault(outcome.OpGetCorridor, outcome.PhaseAfterWrite),
			class: outcome.ClassTransient, exit: 5, money: outcome.MoneyNo, sameKeySafe: true,
			nextHas: "no answer arrived",
		},
		{
			// A key may have been minted whose token this CLI does not
			// hold — the one non-money case where an unanswered write is
			// not safely retryable.
			name:  "a failure after write on keys create is pending and says to list",
			in:    fault(outcome.OpCreateAPIKey, outcome.PhaseAfterWrite),
			class: outcome.ClassPending, exit: 6, money: outcome.MoneyUnknown, sameKeySafe: true,
			nextHas: "ferry keys list",
		},
		{
			name:  "a dial failure on keys create is transient",
			in:    fault(outcome.OpCreateAPIKey, outcome.PhaseDial),
			class: outcome.ClassTransient, exit: 5, money: outcome.MoneyNo, sameKeySafe: true,
			nextHas: "no key was minted",
		},
	})
}

// An operation the package does not declare is not a read by default. Reaching
// here means a caller invented an operation, and this table cannot say whether
// it moves money — so it says so, at exit 6.
func TestUnknownOperationFailsClosed(t *testing.T) {
	got := outcome.Classify(outcome.Input{Op: outcome.Operation("wallets_transfer"), Status: 200})

	if got.Class != outcome.ClassPending || got.Exit != 6 {
		t.Errorf("an undeclared operation gave %s/%d, want pending/6", got.Class, got.Exit)
	}
}

func runRows(t *testing.T, rows []row) {
	t.Helper()

	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			got := outcome.Classify(r.in)

			if got.Class != r.class {
				t.Errorf("class: got %q, want %q", got.Class, r.class)
			}

			if got.Exit != r.exit {
				t.Errorf("exit: got %d, want %d", got.Exit, r.exit)
			}

			if got.Money != r.money {
				t.Errorf("money: got %q, want %q", got.Money, r.money)
			}

			if got.SameKeySafe != r.sameKeySafe {
				t.Errorf("same_key_safe: got %v, want %v", got.SameKeySafe, r.sameKeySafe)
			}

			if !strings.Contains(got.Next, r.nextHas) {
				t.Errorf("next does not contain %q:\n  %s", r.nextHas, got.Next)
			}

			if got.Next == "" {
				t.Error("next is empty; every outcome must say what the caller does")
			}
		})
	}
}
