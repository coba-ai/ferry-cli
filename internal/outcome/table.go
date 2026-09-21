package outcome

// The decision table of PLAN §5.6, as data.
//
// == Why every code in the catalogue has a row, including the ones no
// operation of this CLI can provoke
//
// `docs/api/errors.md` is generated from `Ferry::Api::Errors::CATALOG` and
// pinned to it by `spec/docs/api_docs_spec.rb`, so it is the catalogue at one
// remove. AC19 holds this table to it *in both directions*. The direction that
// matters is the one a subset check cannot see: a code in the catalogue that
// this table does not handle. Asserting that every code this table knows is in
// the catalogue would catch a typo and nothing else.
//
// So the table is a census, not a selection. A code that this CLI's operations
// cannot reach still gets a row and a reason, because the alternative — a
// wildcard on status, which is how revision 1 spelled it — satisfies "every
// code mapped" with rows that never name a code, and the count then stops
// meaning anything.
//
// == The three flags, and what each is asserted against
//
//   - `unreachable` marks a code `errors.md` itself says is never emitted. The
//     coverage test parses that section and holds this flag to it both ways, so
//     a code leaving or joining the list reddens here.
//   - `offSurface` marks a code emitted only by `POST /oms/webhooks/{locator}`,
//     which this CLI never calls. The coverage test derives that set from
//     `openapi.yaml` — the codes named inside the webhook operation and nowhere
//     else — rather than from the `WEBHOOK_` prefix, which is a naming
//     convention and not an authority.
//   - `refine` is the second input. Two codes carry their discriminant in the
//     body or the headers rather than in the code, and for both the *declared*
//     verdict is the conservative reading: deleting a refinement makes the
//     table safer, never more dangerous.

// verdict is one cell.
type verdict struct {
	class Class
	next  string
}

// rule is one row: a verdict per operation class, plus the flags.
//
// There is no `create` column. PLAN §5.6 gives `control_create` the read
// class's refusals — "as reads for refusals; never retried" — so a fourth
// column would be a copy of the third, and a copy is a thing that drifts.
// `control_create` differs in retry (api.Retryable) and in what a transport
// failure after the write means (classifyTransport), and in nothing here.
type rule struct {
	simulate    verdict
	execute     verdict
	read        verdict
	unreachable bool
	offSurface  bool
	refine      func(in Input, class OpClass, v verdict) verdict
}

func (r rule) verdictFor(class OpClass) verdict {
	switch class {
	case OpMoneySimulate:
		return r.simulate
	case OpMoneyExecute:
		return r.execute
	case OpRead, OpControlCreate:
		return r.read
	}

	// Unreachable while OpClasses and this switch agree, which
	// TestEveryOpClassHasAColumn holds.
	return verdict{}
}

// The recurring next sentences. Named rather than repeated so that changing
// what the CLI tells a caller to do is one edit in one place, and so the table
// below reads as a table.
const (
	nextFix        = "Nothing was sent and nothing was spent. Fix the request or the credential, then send it under a new Idempotency-Key."
	nextResimulate = "The plan behind this request cannot be used. Simulate again, and execute the new plan under a new Idempotency-Key."
	nextSameKey    = "Nothing was sent. Wait retry_after_seconds and resend the identical request under the same Idempotency-Key."
	nextResume     = "The outcome is not established. `ferry runs resume <run_id>` or `ferry commands watch <command_id>`. Never send this request under a new Idempotency-Key."
	nextEscalate   = "Stop. A human must read this command before anything else is sent under this key."
	nextReadFix    = "Fix the request or the credential and run the command again."
	nextReadRetry  = "Wait retry_after_seconds and run the command again."
)

// fix is the row shape for a refusal that costs nothing on every class: the
// request was turned away before FERRY looked at a plan.
func fix() rule {
	return rule{
		simulate: verdict{ClassRefusedFix, nextFix},
		execute:  verdict{ClassRefusedFix, nextFix},
		read:     verdict{ClassRefusedFix, nextReadFix},
	}
}

// spentOnExecute is the row shape for the refusals that cost the plan when
// they arrive on execute and cost nothing when they arrive on simulate. This
// is the disjointness PLAN §5.6 buys by making the operation an input:
// `EXECUTION_SUSPENDED` is two different instructions under one code.
func spentOnExecute(executeNext string) rule {
	return rule{
		simulate: verdict{ClassRefusedFix, nextFix},
		execute:  verdict{ClassRefusedResimulate, executeNext},
		read:     verdict{ClassRefusedFix, nextReadFix},
	}
}

// planUnusable is the row shape for the plan refusals, which mean the same
// thing on both money operations.
func planUnusable(next string) rule {
	return rule{
		simulate: verdict{ClassRefusedResimulate, next},
		execute:  verdict{ClassRefusedResimulate, next},
		read:     verdict{ClassRefusedFix, nextReadFix},
	}
}

// transient is the row shape for "nothing left the host, come back with the
// same key".
func transient() rule {
	return rule{
		simulate: verdict{ClassTransient, nextSameKey},
		execute:  verdict{ClassTransient, nextSameKey},
		read:     verdict{ClassTransient, nextReadRetry},
	}
}

// offSurfaceRule is the row shape for a code only FERRY's webhook ingress
// emits. On a money operation it is classified exactly as an unrecognised code
// is, and for the same reason: an answer from a surface this request cannot
// reach is not an answer we understand, and an answer we do not understand on
// the money path is `pending`, never `refused`.
func offSurfaceRule(readClass Class, readNext string) rule {
	const next = "FERRY answered a /v1 request with a code only its webhook ingress emits. Treat the outcome as unknown: `ferry runs resume <run_id>`, and report this — it is a bug in FERRY."

	return rule{
		simulate:   verdict{ClassPending, next},
		execute:    verdict{ClassPending, next},
		read:       verdict{readClass, readNext},
		offSurface: true,
	}
}

// rules is the census. One entry per code in `docs/api/errors.md`, asserted
// equal to it in both directions by TestEveryCatalogueCodeHasARow and
// TestEveryRowNamesACatalogueCode.
var rules = map[string]rule{
	// --- authentication: the credential is wrong, and no plan was reached ---
	"TOKEN_MISSING":          fix(),
	"TOKEN_INVALID":          fix(),
	"TOKEN_EXPIRED":          fix(),
	"TOKEN_REVOKED":          fix(),
	"ORGANIZATION_SUSPENDED": fix(),
	"ENVIRONMENT_DISABLED":   fix(),

	// --- authorisation: authenticated, not permitted. `Executor#preflight`
	// refuses before the plan is looked up (`executor.rb:193-201`), so
	// nothing is spent. ---
	"WRONG_TOKEN_CLASS":  fix(),
	"INSUFFICIENT_SCOPE": fix(),
	"ROLE_FORBIDDEN":     fix(),
	"STEP_UP_REQUIRED":   fix(),
	"FORBIDDEN":          fix(),

	// --- the request itself ---
	"NOT_FOUND":              fix(),
	"VALIDATION_FAILED":      fix(),
	"MALFORMED_JSON":         fix(),
	"CURSOR_INVALID":         fix(),
	"UNSUPPORTED_MEDIA_TYPE": fix(),

	// --- the idempotency key: shape ---
	"IDEMPOTENCY_KEY_REQUIRED": fix(),
	"IDEMPOTENCY_KEY_INVALID":  fix(),

	// --- the idempotency key: collision.
	//
	// C19/B2. `Ledger::Replay#refuse` (`replay.rb:103-142`) answers this one
	// code for two unrelated facts, and the difference is in
	// `details.reason`:
	//
	//   * `different_request` — the digest does not match. Nothing of the
	//     caller's is under this key that they did not already know about;
	//     it is a client bug, and a new key is the right advice.
	//   * `different_credential` — the presenting key is neither the
	//     command's key nor linked to it through `KeyChain`. The command
	//     under this key may be `completed`. Telling that caller to send a
	//     new key is telling them to send the transfer twice.
	//
	// The declared verdict is `escalate`, and the refinement narrows it.
	// That direction is the whole point: a refinement that is deleted,
	// mis-wired or never runs leaves the safe answer standing, and an absent
	// or unrecognised reason is `escalate` by construction rather than by a
	// branch somebody has to remember to write.
	"IDEMPOTENCY_KEY_REUSED": {
		simulate: verdict{ClassEscalate, nextReusedEscalate},
		execute:  verdict{ClassEscalate, nextReusedEscalate},
		read:     verdict{ClassRefusedFix, nextReadFix},
		refine:   refineKeyReused,
	},

	// Someone holds this key right now. `details.command_id` is merged in by
	// `Executor#replay_of` (`executor.rb:213-217`), so there is always
	// something to poll. Never a new key (AC53).
	"IDEMPOTENCY_KEY_IN_PROGRESS": {
		simulate: verdict{ClassPending, "This key names a command that is still running. Poll it: `ferry commands watch <command_id>`. Do not resend under a new key."},
		execute:  verdict{ClassPending, "This key names a command that is still running. Poll it: `ferry commands watch <command_id>`. Do not resend under a new key."},
		read:     verdict{ClassTransient, nextReadRetry},
	},

	// The stored answer aged out of the seven-day window. The server's own
	// remediation is "use a new key" (`command_request.rb:97-101`); this
	// table refuses to automate that for a money key, because
	// `replay_expires_at` is set at Begin (`begin.rb:147`) and the command
	// under the key may be in any state, `completed` included. `details.state`
	// is rendered into `next` by annotate.
	"IDEMPOTENCY_KEY_REPLAY_EXPIRED": {
		simulate: verdict{ClassEscalate, "The stored answer for this key has expired, and FERRY will never re-execute the key. Read the command before sending anything else."},
		execute:  verdict{ClassEscalate, "The stored answer for this key has expired, and FERRY will never re-execute the key. Read the command before sending anything else."},
		read:     verdict{ClassRefusedFix, nextReadFix},
	},

	// An operator holds this command. The read column is `escalate` and not
	// the read class's usual `refused_fix`, because `poll.Watch` is a read
	// and AC51 requires polling to stop at 7 here.
	"COMMAND_UNRESOLVED": {
		simulate: verdict{ClassEscalate, nextEscalate},
		execute:  verdict{ClassEscalate, nextEscalate},
		read:     verdict{ClassEscalate, nextEscalate},
	},

	// --- rate limiting and FERRY's own faults ---
	"RATE_LIMITED": transient(),

	// A 500 on a money request is the case C3 exists for: the request was
	// written, and `Executor#send_and_record` (`executor.rb:342-361`) sends
	// upstream before the caller sees a byte. On a read it is what
	// `errors.md` says it is — retriable.
	"INTERNAL_ERROR": {
		simulate: verdict{ClassPending, nextResume},
		execute:  verdict{ClassPending, nextResume},
		read:     verdict{ClassTransient, nextReadRetry},
	},
	// Emitted only by the rate-limit layer, and only from
	// `Ferry::Middleware::EarlyThrottle` and `RateLimit::Responder`
	// (`responder.rb:35`) — both of which run *before* the controller. So
	// "nothing was sent" is not an inference from the catalogue's
	// `retriable` flag here; it is where the code is raised from. It is a
	// 503 rather than a 429 deliberately: the counter was unavailable, so
	// nothing was counted and this is not the caller being throttled
	// (`early_throttle.rb:79`).
	"SERVICE_UNAVAILABLE": transient(),

	// --- the upstream ---
	//
	// Split by `Ferry-Command-Id`, which is the tell between the two
	// `UPSTREAM_BUSY`s: `read_quote` returns a command-less `Refused`
	// (`executor.rb:289-294`) — the quote read failed, T1 was never reached
	// and the plan is untouched — while `give_up` returns `Retriable` with
	// a `failed_retriable` command (`executor.rb:467-481`) whose plan was
	// consumed at T1 and which replays as 202. The declared verdict is the
	// command-less one and
	// the refinement escalates it to `pending` when the header is there,
	// which is again the direction where losing the refinement is safe in
	// the only way that matters: `transient` tells the caller to resend the
	// *same* key, which the server deduplicates.
	"UPSTREAM_BUSY": {
		simulate: verdict{ClassTransient, nextSameKey},
		execute:  verdict{ClassTransient, nextSameKey},
		read:     verdict{ClassTransient, nextReadRetry},
		refine:   refineUpstreamBusy,
	},

	// No upstream credential is attached to this deployment. Nothing to
	// retry until an operator changes that, and `Executor#preflight` refuses
	// before the plan is looked up (§1.2.4), so nothing is spent.
	"UPSTREAM_NOT_CONFIGURED": {
		simulate: verdict{ClassRefusedFix, "This deployment has no upstream credential attached, so this operation cannot run. There is nothing to retry until an operator attaches one."},
		execute:  verdict{ClassRefusedFix, "This deployment has no upstream credential attached, so this operation cannot run. There is nothing to retry until an operator attaches one. The plan was not looked up and is untouched."},
		read:     verdict{ClassRefusedFix, nextReadFix},
	},

	// Never emitted live: it arrives only replayed from a command recovery
	// gave up on, by which time the plan is spent.
	"UPSTREAM_UNAVAILABLE": planUnusable("The request never reached the upstream service and the command behind this key was given up on. Nothing executed, and the plan is spent — simulate again."),

	// Pre-send on execute: `#validate_quote` (`executor.rb:300-315`) answers
	// it without touching the plan. The plan is not spent, but the quote
	// behind it is unusable, so the remedy is the same as if it were.
	"UPSTREAM_CONTRACT_VIOLATION": {
		simulate: verdict{ClassRefusedFix, nextFix},
		execute:  verdict{ClassRefusedResimulate, "The quote behind this plan is one FERRY cannot act on. Nothing was sent and the plan is untouched, but it cannot be executed — simulate again."},
		read:     verdict{ClassRefusedFix, nextReadFix},
	},

	"UPSTREAM_CREDENTIALS_INVALID": {
		simulate:    verdict{ClassRefusedFix, "This deployment's upstream credential was refused. An operator must re-attach it; there is nothing to retry."},
		execute:     verdict{ClassRefusedFix, "This deployment's upstream credential was refused. An operator must re-attach it; there is nothing to retry."},
		read:        verdict{ClassRefusedFix, nextReadFix},
		unreachable: true,
	},
	"UPSTREAM_AUTH_UNAVAILABLE": {
		simulate:    verdict{ClassTransient, nextSameKey},
		execute:     verdict{ClassTransient, nextSameKey},
		read:        verdict{ClassTransient, nextReadRetry},
		unreachable: true,
	},

	// --- the plan ---
	//
	// `PLAN_KEY_MISMATCH` is `Refused`, which rolls the consumption back
	// (`begin.rb:47-51`), so the plan is not spent. The action is the same
	// either way — a new plan under the right credential — and the sentence
	// says which of the two happened rather than repeating the class's.
	"PLAN_NOT_FOUND":    planUnusable(nextResimulate),
	"PLAN_EXPIRED":      planUnusable("This plan has expired; plans last five minutes. Simulate again and execute the new plan under a new Idempotency-Key."),
	"PLAN_ALREADY_USED": planUnusable("This plan has already been used, and a plan is single-use. Simulate again."),
	"PLAN_CHANGED":      planUnusable("The quote behind this plan has been re-priced. Simulate again to see the new terms."),
	"PLAN_KEY_MISMATCH": planUnusable("This plan was issued to another credential. The plan was rolled back rather than spent, but it cannot be executed by this credential — simulate again under the credential you mean to use."),

	// --- the spend policy.
	//
	// On execute these are `Ledger::Begin::Denied`, which commits a
	// `failed_terminal` row with the plan consumed (`begin.rb:78-89`), so
	// the plan is genuinely gone and the byte-identical request replays the
	// denial forever. On simulate they cannot arrive at all — `Simulator`
	// never evaluates spend policy (§1.2.8) — and if one did, no plan would
	// exist to spend, so the honest answer is `refused_fix`. ---
	"POLICY_NOT_CONFIGURED":             spentOnExecute("This environment has no spend policy, so no money may move. The plan is spent. An operator must configure one; then simulate again."),
	"POLICY_INVALID":                    spentOnExecute("This credential's spend policy cannot be read, so no money may move. The plan is spent."),
	"POLICY_UNPRICED_ASSET":             spentOnExecute("This asset has no USD basis, so it cannot be counted against a spend policy. The plan is spent."),
	"POLICY_CUSTOMER_NOT_ALLOWED":       spentOnExecute("This credential may not move money for this customer. The plan is spent."),
	"POLICY_DESTINATION_NOT_ALLOWED":    spentOnExecute("This credential may not move money to this destination. The plan is spent."),
	"POLICY_MAX_AMOUNT_PER_TRANSACTION": spentOnExecute("This transfer is over the per-transaction limit. The plan is spent; simulate a smaller transfer."),
	"POLICY_MAX_AMOUNT_PER_DAY":         spentOnExecute("This transfer is over the daily amount limit. The plan is spent."),
	"POLICY_MAX_AMOUNT_PER_30D":         spentOnExecute("This transfer is over the 30-day amount limit. The plan is spent."),
	"POLICY_MAX_TRANSACTIONS_PER_DAY":   spentOnExecute("This transfer is over the daily transaction-count limit. The plan is spent."),

	// Reached through the same `Denied` path as the POLICY_* codes, so the
	// plan is spent, and `errors.md` says so. Unreachable today: no HTTP
	// caller supplies `destination_usable_after` (`begin.rb:206-207`).
	"DESTINATION_TOO_NEW": {
		simulate:    verdict{ClassRefusedFix, nextFix},
		execute:     verdict{ClassRefusedResimulate, "This destination is not usable yet. Wait for the delay this environment's policy requires, then simulate again — the plan behind the refused request is spent."},
		read:        verdict{ClassRefusedFix, nextReadFix},
		unreachable: true,
	},

	"EXECUTION_SUSPENDED": spentOnExecute("Execution is suspended for this environment. The plan is spent. Nothing will execute until it is lifted."),

	// --- the quote surface.
	//
	// These are simulate's 422s; `execute` validates nothing but the plan
	// token, so none of them can arrive there. The execute column is
	// `refused_resimulate` rather than `refused_fix` for the same reason
	// `UPSTREAM_CONTRACT_VIOLATION`'s is: on execute the question is not
	// "was the plan spent" but "is the plan still usable", and a pricing
	// refusal answered to an execute is an answer about the plan. ---
	"CORRIDOR_UNSUPPORTED":       quoteRefusal("This API does not quote transfers between these two instrument types. Fix the request and simulate again."),
	"CORRIDOR_NOT_EXECUTABLE":    quoteRefusal("This corridor is published but cannot be executed; see details.reason. Fix the request and simulate again."),
	"DESTINATION_NOT_EXECUTABLE": quoteRefusal("This destination cannot be executed against; see details.reason."),
	"INSTRUMENT_INVALID":         quoteRefusal("An instrument field is outside the vocabulary this arm admits; see details."),
	"AMOUNT_INVALID":             quoteRefusal("The amount block is not one this corridor can be quoted on; see details."),
	"QUOTE_REJECTED":             quoteRefusal("The upstream service refused to quote this transfer. The same body under a new key asks the same rejected question — change the transfer."),

	// --- the webhook ingress. This CLI never calls `POST
	// /oms/webhooks/{locator}`, so none of these can arrive at a /v1
	// request. They have rows because the table is a census (see the header
	// comment), and their money verdict is `pending` for the reason
	// offSurfaceRule states. ---
	"WEBHOOK_ENDPOINT_UNKNOWN":   offSurfaceRule(ClassRefusedFix, nextReadFix),
	"WEBHOOK_ENDPOINT_RETIRED":   offSurfaceRule(ClassRefusedFix, nextReadFix),
	"WEBHOOK_SIGNATURE_INVALID":  offSurfaceRule(ClassRefusedFix, nextReadFix),
	"WEBHOOK_MODE_MISMATCH":      offSurfaceRule(ClassRefusedFix, nextReadFix),
	"WEBHOOK_BODY_TOO_LARGE":     offSurfaceRule(ClassRefusedFix, nextReadFix),
	"WEBHOOK_INGRESS_UNVERIFIED": offSurfaceRule(ClassTransient, nextReadRetry),
}

const nextReusedEscalate = "This Idempotency-Key is held by a command this credential cannot read, so FERRY will not say what happened to it. Something of yours may be under this key. Do not send a new key: log back in as the credential that started the run and read the command."

func quoteRefusal(next string) rule {
	return rule{
		simulate: verdict{ClassRefusedFix, next},
		execute:  verdict{ClassRefusedResimulate, "FERRY answered an execute with a pricing refusal, so the plan behind it cannot be relied on. Simulate again."},
		read:     verdict{ClassRefusedFix, nextReadFix},
	}
}

// refineKeyReused is C19's discriminant. Only the one reason that means "your
// request differs and nothing of yours is under this key" narrows to
// refused_fix; `different_credential`, an unrecognised reason and an absent
// reason all keep the declared `escalate`.
func refineKeyReused(in Input, class OpClass, v verdict) verdict {
	if !class.IsMoney() {
		return v
	}

	if in.Reason() != "different_request" {
		return v
	}

	return verdict{
		ClassRefusedFix,
		"This Idempotency-Key was already used for a *different* request body. Nothing of this request was sent. Fix the body, or send this body under a new key.",
	}
}

// refineUpstreamBusy is the header discriminant. A `Ferry-Command-Id` means
// FERRY has a `failed_retriable` command of its own and has scheduled
// recovery; resending the same key answers 202 and the caller polls.
func refineUpstreamBusy(in Input, class OpClass, v verdict) verdict {
	if !class.IsMoney() {
		return v
	}

	if in.CommandID == "" {
		return v
	}

	return verdict{
		ClassPending,
		"FERRY could not reach the upstream service and has scheduled its own retry. Wait retry_after_seconds and resend the identical request under the same Idempotency-Key; it will answer 202 and you poll the command. Never a new key.",
	}
}

// Codes returns every code the table has a row for. Sorted by the caller;
// returned as a fresh slice so no test can mutate the table through it.
func Codes() []string {
	out := make([]string, 0, len(rules))
	for code := range rules {
		out = append(out, code)
	}

	return out
}

// Lookup reports the declared row for a code, before any refinement. It exists
// for the coverage test, which must be able to tell "handled" from "fell
// through to the unknown-code branch" — a distinction Classify's return value
// alone cannot make, because the fallback produces a perfectly ordinary
// Outcome.
func Lookup(code string) (class Class, next string, found bool) {
	r, ok := rules[code]
	if !ok {
		return "", "", false
	}

	v := r.execute

	return v.class, v.next, true
}

// VerdictFor reports the declared class for one code under one operation
// class, before refinement. For the coverage test.
func VerdictFor(code string, class OpClass) (Class, bool) {
	r, ok := rules[code]
	if !ok {
		return "", false
	}

	return r.verdictFor(class).class, true
}

// Unreachable reports the codes marked as never emitted. Held to
// `errors.md`'s own "Codes you will not see" section in both directions.
func Unreachable() []string { return flagged(func(r rule) bool { return r.unreachable }) }

// OffSurface reports the codes only FERRY's webhook ingress emits. Held to
// `openapi.yaml` in both directions.
func OffSurface() []string { return flagged(func(r rule) bool { return r.offSurface }) }

func flagged(pred func(rule) bool) []string {
	out := []string{}
	for code, r := range rules {
		if pred(r) {
			out = append(out, code)
		}
	}

	return out
}
