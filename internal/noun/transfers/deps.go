// Package transfers is the money path: simulate, execute, and the run that
// holds the idempotency key across a crash.
//
// Read PLAN §5.4 alongside this package. The order of the steps there is the
// whole of C1 and C17, and `money.go` is that order written out once so that
// `transfers create --broadcast`, `transfers execute --plan` and `runs
// resume` cannot each get it slightly different.
//
// Three things this package must never be edited into doing:
//
//   - Sending an execute without asking *this* invocation (C16). Every arm
//     goes through `internal/consent`, and no state in `internal/runs` means
//     a human agreed.
//   - Clearing the in-flight flag (C17). It is set immediately before
//     `api.Do` and the only writer is `flight.MarkSent`.
//   - Minting a second key for a request that may already have been sent
//     (C2, C18). `runs.Begin` is the single "about to send" entry point and
//     a resend passes a nil body, so the bytes on disk are the bytes resent.
package transfers

import (
	"time"

	"github.com/kurenn/ferry-cli/internal/cli/flight"
	"github.com/kurenn/ferry-cli/internal/noun"
	"github.com/kurenn/ferry-cli/internal/ulid"
)

// Deps is what the money commands need beyond `noun.Deps`.
//
// It is a struct of this package's own rather than three more fields on
// `noun.Deps`, which is U4's file: the money path needs an in-flight flag and
// a signal channel, and a read command has no use for either.
type Deps struct {
	Noun noun.Deps

	// Flight is C17's flag. A nil flag reports "nothing sent", which is
	// wrong for this package, so [Deps.resolve] makes one rather than
	// letting a money command run without one.
	Flight *flight.Flag

	// Interrupted closes when SIGINT, SIGTERM or SIGHUP reaches the
	// process. The consent prompt and the poll loop both watch it.
	Interrupted <-chan struct{}

	// NewRunID mints a run id. Nil is `ulid.New`.
	NewRunID func() (string, error)

	// PromptShown fires after the consent prompt has been written and
	// before the read begins — the instant `at_prompt` fires at (V9). It is
	// nil in production; a test that has to deliver a signal while the
	// prompt is up sets it, because the prompt's arrival cannot otherwise
	// be witnessed from outside the process without polling for it, and a
	// polling observer cannot witness a window shorter than its poll.
	PromptShown func()
}

func (d Deps) resolve() Deps {
	d.Noun = d.Noun.Resolve()

	if d.Flight == nil {
		d.Flight = flight.New()
	}

	if d.NewRunID == nil {
		d.NewRunID = ulid.New
	}

	return d
}

func (d Deps) now() time.Time {
	if d.Noun.Now == nil {
		return time.Now().UTC()
	}

	return d.Noun.Now().UTC()
}
