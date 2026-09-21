package outcome

import (
	"fmt"
	"strings"
)

// TransportPhase says whether bytes may have left the host. It is measured,
// not inferred from an error string: `api.Do` installs an `httptrace`
// `WroteHeaders`/`WroteRequest` hook, and the phase is what that hook
// observed.
type TransportPhase string

const (
	// PhaseDial — the connection was never established, so nothing left.
	PhaseDial TransportPhase = "dial"
	// PhaseAfterWrite — some or all of the request was written to the socket.
	// Server-side, `Executor#send_and_record` may already have sent upstream.
	PhaseAfterWrite TransportPhase = "after_write"
)

// Transport is a failure with no HTTP answer.
type Transport struct {
	Phase TransportPhase
	Err   error
}

// Envelope is the part of the seven-key error envelope the table branches on.
// It is a plain struct and not `api.Envelope` so that this package imports
// nothing: the table is pure, and a pure table cannot be made to depend on a
// transport by accident.
type Envelope struct {
	Code    string
	Details map[string]any
}

// Success is the part of a 2xx body the table branches on.
type Success struct {
	// Parsed is false when the body was not JSON at all. C3: an unparseable
	// 2xx on the money path is `pending`, never `done`.
	Parsed bool
	// TransactionStatus is the execute 201 body's `status`
	// (`lib/transfers/projection.rb` allowlist). Empty when absent.
	TransactionStatus string
}

// Input is C4's tuple. Every field is something an answer carried; nothing is
// derived inside the table from anything else.
type Input struct {
	Op     Operation
	Status int

	// Transport, when set, means there was no HTTP answer at all. Status,
	// Envelope and Success are then ignored.
	Transport *Transport

	// Envelope is set when the body decoded as the seven-key envelope. A
	// non-2xx answer with a nil Envelope is AC83's row.
	Envelope *Envelope

	// Success is set for a 2xx answer.
	Success *Success

	// CommandID is the `Ferry-Command-Id` header, or `details.command_id`
	// when the header was absent. The caller resolves that precedence;
	// `command_request.rb:432-439` sets the header from the detail, so in
	// practice they agree.
	CommandID string

	// Replayed is the `Idempotency-Replayed` header.
	Replayed bool
}

// Code is the answer's error code, or "" when there was no envelope.
func (in Input) Code() string {
	if in.Envelope == nil {
		return ""
	}

	return in.Envelope.Code
}

// Reason is `error.details.reason` — C19's discriminant, and a table input
// rather than prose.
func (in Input) Reason() string { return in.detail("reason") }

// State is `error.details.state`, carried by `IDEMPOTENCY_KEY_REPLAY_EXPIRED`
// (`replay.rb:114-117`) and `IDEMPOTENCY_KEY_IN_PROGRESS`
// (`replay.rb:163`).
func (in Input) State() string { return in.detail("state") }

// DetailCommandID is `error.details.command_id`, merged in by
// `Executor#replay_of` (`executor.rb:220-224`).
func (in Input) DetailCommandID() string { return in.detail("command_id") }

func (in Input) detail(key string) string {
	if in.Envelope == nil || in.Envelope.Details == nil {
		return ""
	}

	s, _ := in.Envelope.Details[key].(string)

	return s
}

// Classify turns one answer into one action.
//
// The order of the branches is the order of what is known, from least to most:
// an operation we do not recognise, then a failure with no answer, then a 2xx,
// then an answer whose body is not an envelope, then a code we do not know,
// then the table. Each earlier branch is a thing that makes the later ones
// meaningless, so none of them may be reordered.
func Classify(in Input) Outcome {
	class, ok := ClassOf(in.Op)
	if !ok {
		// Fail closed. An operation this package does not declare is not a
		// read by default: `api.Routes` and `Operations` are pinned to each
		// other, so reaching here means a caller invented an operation, and
		// we cannot say whether it moves money.
		return newOutcome(ClassPending, fmt.Sprintf(
			"This CLI does not recognise the operation %q, so it cannot say what this answer means. Treat the outcome as unknown.",
			in.Op,
		))
	}

	if in.Transport != nil {
		return annotate(in, classifyTransport(in, class))
	}

	if in.Status >= 200 && in.Status < 300 {
		return annotate(in, classifySuccess(in, class))
	}

	// AC83. A body that is not the seven-key envelope, at any status — an
	// HTML 502 from a proxy, a 413, a 415 whose body a middlebox replaced.
	// FERRY answers every /v1 refusal with the envelope
	// (`base_controller.rb`), so a non-envelope answer did not come from
	// FERRY's error renderer and says nothing about what FERRY did.
	if in.Envelope == nil {
		if class.IsMoney() {
			return newOutcome(ClassPending, fmt.Sprintf(
				"FERRY answered %d with a body that is not the error envelope, so this answer says nothing about whether the request executed. %s",
				in.Status, nextResume,
			))
		}

		return newOutcome(ClassTransient, fmt.Sprintf(
			"The server answered %d with a body that is not the error envelope. %s", in.Status, nextReadRetry,
		))
	}

	r, known := rules[in.Code()]
	if !known {
		return annotate(in, classifyUnknownCode(in, class))
	}

	v := r.verdictFor(class)
	if r.refine != nil {
		v = r.refine(in, class, v)
	}

	return annotate(in, newOutcome(v.class, v.next))
}

// classifyTransport is AC23. The split is by whether bytes may have left the
// host, and nothing else — not by error type, not by whether a deadline was
// involved.
func classifyTransport(in Input, class OpClass) Outcome {
	if in.Transport.Phase == PhaseAfterWrite {
		if class == OpControlCreate {
			return newOutcome(ClassPending,
				"The request was written but no answer arrived, so a key may have been minted and this CLI does not hold its token. Run `ferry keys list` before minting again.")
		}

		if class.IsMoney() {
			return newOutcome(ClassPending, "The request was written to the socket but no answer arrived. FERRY sends upstream before it answers, so this request may have executed. "+nextResume)
		}

		return newOutcome(ClassTransient, "The request was written but no answer arrived. "+nextReadRetry)
	}

	// PhaseDial, and anything else: nothing left the host.
	if class == OpControlCreate {
		return newOutcome(ClassTransient, "The connection was never established, so no key was minted. Run the command again.")
	}

	if class.IsMoney() {
		return newOutcome(ClassTransient, "The connection was never established, so nothing left this host. "+nextSameKey)
	}

	return newOutcome(ClassTransient, "The connection was never established. "+nextReadRetry)
}

// classifySuccess is AC22 and C9.
func classifySuccess(in Input, class OpClass) Outcome {
	if in.Success == nil || !in.Success.Parsed {
		// C3 and §7.1 A14: a 201 whose body the CLI cannot parse is exit 6,
		// not exit 0. For a read there is no money question, so an
		// unreadable answer is worth another try.
		if class.IsMoney() {
			return newOutcome(ClassPending, fmt.Sprintf(
				"FERRY answered %d but the body could not be read, so this CLI cannot say what it contained. %s", in.Status, nextResume,
			))
		}

		return newOutcome(ClassTransient, fmt.Sprintf(
			"The server answered %d with a body that could not be read. %s", in.Status, nextReadRetry,
		))
	}

	if in.Status == 202 {
		// Both money operations. §5.7 takes it from here; the terminal
		// classification is ClassifyCommand's.
		return newOutcome(ClassPending,
			"FERRY could not establish the outcome during the request. Poll the command: `ferry commands watch <command_id>`. Money may or may not have moved — do not resend under a new key.")
	}

	switch class {
	case OpMoneyExecute:
		// C9: 201 is acceptance upstream, not settlement. The one status
		// that is its own class is `failed`
		// (`vendor/oms/openapi.yaml` TransactionStatus).
		if in.Success.TransactionStatus == "failed" {
			return newOutcome(ClassUpstreamFailed,
				"FERRY accepted this transfer upstream and the upstream then failed it. Reconcile against the upstream before simulating a replacement.")
		}

		next := "Accepted upstream, not settled. Read `status` and `subStatus`, and reconcile later."
		if in.Success.TransactionStatus == "" {
			return newOutcome(ClassAcceptedUpstream, next,
				"the transaction body carried no `status`, so acceptance could not be qualified")
		}

		return newOutcome(ClassAcceptedUpstream, next)

	case OpMoneySimulate:
		if in.Replayed {
			return newOutcome(ClassDone,
				"This quote was replayed from an earlier answer under the same key. `plan.token` is handed out once and is not in a replay — use the token from the first answer, or simulate under a new key.",
				"replayed: this answer carries no plan.token")
		}

		return newOutcome(ClassDone, "The plan token is printed once and never stored. Execute it within five minutes or simulate again.")

	default:
		return newOutcome(ClassDone, "")
	}
}

// classifyUnknownCode is AC24 and C3, and it is where the table fails closed.
//
// The money half is the important one: a code this CLI has never seen, on a
// request that may have reached `send_and_record`, is `pending`. It is
// emphatically not `refused_fix` — that class's whole meaning is "nothing was
// sent", which is precisely what an unrecognised answer cannot establish.
//
// The read half splits by status because a novel 5xx on a `GET` is transient
// until proven otherwise, and telling a human to escalate a `GET` is noise.
func classifyUnknownCode(in Input, class OpClass) Outcome {
	if class.IsMoney() {
		return newOutcome(ClassPending, fmt.Sprintf(
			"FERRY answered %d with the code %q, which this CLI does not know. It cannot say whether the request executed. %s",
			in.Status, in.Code(), nextResume,
		))
	}

	if in.Status >= 500 {
		return newOutcome(ClassTransient, fmt.Sprintf(
			"The server answered %d with the unrecognised code %q. %s", in.Status, in.Code(), nextReadRetry,
		))
	}

	return newOutcome(ClassRefusedFix, fmt.Sprintf(
		"The server answered %d with the unrecognised code %q. %s", in.Status, in.Code(), nextReadFix,
	))
}

// annotate appends the two facts the envelope carries that a caller acts on.
//
// It is one rule for every code rather than a special case for the three that
// carry them today, so a code that starts carrying `details.state` tomorrow
// renders it without an edit here — and so that AC20's "`details.state` in
// `next`" and AC71's "`next` naming `details.command_id`" are properties of the
// table rather than of two hand-written sentences.
func annotate(in Input, out Outcome) Outcome {
	var b strings.Builder
	b.WriteString(out.Next)

	if state := in.State(); state != "" {
		if b.Len() > 0 {
			b.WriteString(" ")
		}

		fmt.Fprintf(&b, "The command under this key is `%s`.", state)
	}

	id := in.CommandID
	if id == "" {
		id = in.DetailCommandID()
	}

	if id != "" {
		if b.Len() > 0 {
			b.WriteString(" ")
		}

		fmt.Fprintf(&b, "Command %s: `ferry commands get %s`.", id, id)
	}

	out.Next = b.String()

	return out
}
