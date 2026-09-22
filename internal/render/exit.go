package render

import (
	"fmt"

	"github.com/coba-ai/ferry-cli/internal/outcome"
)

// Error is a command failure that carries the process exit code.
//
// The exit code is never a literal: it comes from [outcome.Properties] for the
// class, so a message and an exit code cannot disagree. `harness.Run` and
// `internal/cli` both read it through the structural `ExitCode() int`
// interface, which is why nothing here has to import either of them.
type Error struct {
	// Out is the outcome this failure classifies as. Exit comes from it.
	Out outcome.Outcome

	// Msg is what a human reads. It carries its own remediation: the
	// outcome's Next is appended by the constructors below rather than left
	// for a caller to remember.
	Msg string

	// Err is the underlying cause, kept for errors.Is/As. It is never
	// printed on its own — Msg already says what happened in the CLI's
	// vocabulary.
	Err error
}

func (e *Error) Error() string { return e.Msg }

// ExitCode satisfies the structural interface `harness.Run` and the root use.
func (e *Error) ExitCode() int { return e.Out.Exit }

// Unwrap exposes the cause.
func (e *Error) Unwrap() error { return e.Err }

// Class is the outcome class this failure reports.
func (e *Error) Class() outcome.Class { return e.Out.Class }

// newError builds an Error for a class, refusing a class the table does not
// declare rather than defaulting its exit code to zero — a failure that exits
// 0 is the one bug in this file that nothing downstream could detect.
func newError(class outcome.Class, err error, format string, args ...any) *Error {
	out, ok := outcome.Properties(class)
	if !ok {
		panic("render: no properties declared for outcome class " + string(class))
	}

	return &Error{Out: out, Msg: fmt.Sprintf(format, args...), Err: err}
}

// Usage is exit 2: the flags or arguments were wrong. Nothing was sent.
func Usage(format string, args ...any) *Error {
	return newError(outcome.ClassUsage, nil, format, args...)
}

// UsageFrom wraps a cobra flag or argument error as exit 2.
//
// Cobra's own errors carry no exit code, so a command that let one escape
// would exit 1 — `cli_fault`, the class that means "the CLI stopped before
// anything was sent, fix the local problem". That is true here but it is the
// wrong sentence: the local problem is the command line, and exit 2 is the
// code a wrapper reads as "I typed this wrong".
func UsageFrom(err error) *Error {
	return newError(outcome.ClassUsage, err, "%s", err.Error())
}

// Fault is exit 1: the CLI stopped before any request left — a config, I/O or
// local-state problem.
//
// C17 forbids this class once a money request may have been sent. No command
// in this unit sends one, so every use here is a pre-wire failure by
// construction; a unit that sends money must consult the in-flight flag
// instead of calling this.
func Fault(err error, format string, args ...any) *Error {
	return newError(outcome.ClassCLIFault, err, format, args...)
}

// Refused is exit 3: refused locally, nothing sent, nothing spent. The
// pre-check (AC43) and the `--env` assertion (AC34) are the two refusals this
// unit makes without asking the server.
func Refused(format string, args ...any) *Error {
	return newError(outcome.ClassRefusedFix, nil, format, args...)
}

// FromOutcome reports an answer the decision table classified.
//
// The outcome is passed whole rather than re-derived, so the exit code a
// caller sees and the `next` sentence they read came from the same decision.
func FromOutcome(out outcome.Outcome, format string, args ...any) *Error {
	msg := fmt.Sprintf(format, args...)
	if out.Next != "" {
		msg += "\n" + out.Next
	}

	return &Error{Out: out, Msg: msg}
}
