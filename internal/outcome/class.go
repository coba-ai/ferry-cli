// Package outcome is the decision table of PLAN §5.6: it turns one API answer
// into one action.
//
// The package is pure. It reads no file, opens no socket and consults no clock;
// everything it decides comes from the tuple C4 names. That is deliberate — the
// table is the artefact a wrong branch turns into a lost or duplicated
// transfer, so it is the one part of the CLI that can be exhaustively tested
// without a server, a fixture or a recording.
//
// The vocabulary is organised by *what the caller must do*, not by error
// family. Two codes that mean quite different things to FERRY share an exit
// code when the caller's next move is the same, and one code splits across two
// exit codes when it does not — which is why `details.reason` and the command
// body's `operation` are inputs and not prose.
package outcome

// Class is the outcome class of PLAN §5.6. Exit code, "did money move?" and
// "is the same key safe?" are all functions of it, computed once in
// properties below, so no caller can pair a class with an exit code the table
// does not agree with.
type Class string

const (
	// ClassDone — a read succeeded, or a simulate returned a plan.
	ClassDone Class = "done"
	// ClassAcceptedUpstream — an execute was accepted upstream. Not settled.
	ClassAcceptedUpstream Class = "accepted_upstream"
	// ClassCLIFault — the CLI stopped before any money request left. C17
	// forbids this class once the in-flight flag is set.
	ClassCLIFault Class = "cli_fault"
	// ClassUsage — flags or arguments were wrong.
	ClassUsage Class = "usage"
	// ClassRefusedFix — refused; nothing sent; nothing spent. Fix and use a
	// new key.
	ClassRefusedFix Class = "refused_fix"
	// ClassRefusedResimulate — refused or unusable; the plan is spent, rolled
	// back, or was never issued. Simulate again.
	ClassRefusedResimulate Class = "refused_resimulate"
	// ClassTransient — refused transiently; nothing sent. Resend the same key.
	ClassTransient Class = "transient"
	// ClassPending — the outcome is not established. Never a new key.
	ClassPending Class = "pending"
	// ClassEscalate — FERRY cannot or will not resolve this. Stop.
	ClassEscalate Class = "escalate"
	// ClassUpstreamFailed — accepted upstream, then status: failed.
	ClassUpstreamFailed Class = "upstream_failed"
)

// Money is what the outcome says about whether money moved.
type Money string

const (
	MoneyNo               Money = "no"
	MoneyUnknown          Money = "unknown"
	MoneyAcceptedUpstream Money = "accepted_upstream"
	MoneySeeUpstream      Money = "see_upstream"
)

type classProperties struct {
	exit        int
	money       Money
	sameKeySafe bool
}

// properties is PLAN §5.6's first table, as data.
//
// sameKeySafe answers exactly one question: may the caller resend the
// byte-identical request under the same Idempotency-Key? It is true for the
// classes where the server's replay is the right next step (done, transient,
// pending) and false for the classes whose remedy is a different request
// (refused_fix, refused_resimulate, upstream_failed) or no request at all
// (escalate). For the three classes that never reach the wire it is true
// because nothing was sent, so nothing is at stake in resending.
var properties = map[Class]classProperties{
	ClassDone:              {exit: 0, money: MoneyNo, sameKeySafe: true},
	ClassAcceptedUpstream:  {exit: 0, money: MoneyAcceptedUpstream, sameKeySafe: true},
	ClassCLIFault:          {exit: 1, money: MoneyNo, sameKeySafe: true},
	ClassUsage:             {exit: 2, money: MoneyNo, sameKeySafe: true},
	ClassRefusedFix:        {exit: 3, money: MoneyNo, sameKeySafe: false},
	ClassRefusedResimulate: {exit: 4, money: MoneyNo, sameKeySafe: false},
	ClassTransient:         {exit: 5, money: MoneyNo, sameKeySafe: true},
	ClassPending:           {exit: 6, money: MoneyUnknown, sameKeySafe: true},
	ClassEscalate:          {exit: 7, money: MoneyUnknown, sameKeySafe: false},
	ClassUpstreamFailed:    {exit: 8, money: MoneySeeUpstream, sameKeySafe: false},
}

// Classes is every class the table may produce, in exit-code order. Exported
// so a coverage test can hold the vocabulary to equality rather than sampling
// it.
var Classes = []Class{
	ClassDone,
	ClassAcceptedUpstream,
	ClassCLIFault,
	ClassUsage,
	ClassRefusedFix,
	ClassRefusedResimulate,
	ClassTransient,
	ClassPending,
	ClassEscalate,
	ClassUpstreamFailed,
}

// Properties reports the exit code, money answer and same-key safety declared
// for a class, with no `next`. It exists so a test can hold Classes and
// properties to each other as sets — the alternative is newOutcome's panic,
// which arrives at first use rather than at build time.
func Properties(class Class) (Outcome, bool) {
	p, ok := properties[class]
	if !ok {
		return Outcome{}, false
	}

	return Outcome{
		Class:       class,
		Exit:        p.exit,
		Money:       p.money,
		SameKeySafe: p.sameKeySafe,
	}, true
}

// Outcome is what one answer becomes.
type Outcome struct {
	Class       Class
	Exit        int
	Money       Money
	SameKeySafe bool
	Next        string
	Warnings    []string
}

// newOutcome is the only constructor. Exit, Money and SameKeySafe are never
// passed in: a caller that could choose them could contradict the table.
func newOutcome(class Class, next string, warnings ...string) Outcome {
	p, ok := properties[class]
	if !ok {
		// Unreachable while Classes and properties agree, which
		// TestClassVocabularyAgrees holds. Failing closed rather than
		// returning a zero value, because a zero Exit is 0 — success.
		panic("outcome: no properties declared for class " + string(class))
	}

	return Outcome{
		Class:       class,
		Exit:        p.exit,
		Money:       p.money,
		SameKeySafe: p.sameKeySafe,
		Next:        next,
		Warnings:    warnings,
	}
}
