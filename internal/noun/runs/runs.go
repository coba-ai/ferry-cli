// Package runs is `ferry runs`: read the ledger, resume what is unresolved,
// decline what was never confirmed, and remove what is settled.
//
// These verbs exist because the money path can leave state behind. A crash
// between `Begin` and the answer leaves a run whose outcome nobody knows;
// C18 then refuses the next money command until somebody says what should
// happen to it. Without `resume` and `decline` that refusal would be a
// deadlock on a headless machine, which is why AC72 requires `decline` to
// work with no terminal at all.
//
// Two rules hold across the whole package:
//
//   - **No render prints a plan token** (AC57, C6). `plan_token` and the
//     `plan_token` inside a stored `body` both come out `[REDACTED]`, at
//     every step state, and `redact_test.go` sweeps the rendered text for the
//     token shape rather than trusting the two call sites.
//   - **No verb here sends anything except `resume`,** which is the money
//     path itself and goes through `internal/noun/transfers`.
package runs

import (
	"github.com/spf13/cobra"

	"github.com/coba-ai/ferry-cli/internal/noun"
	"github.com/coba-ai/ferry-cli/internal/noun/transfers"
	"github.com/coba-ai/ferry-cli/internal/render"
	"github.com/coba-ai/ferry-cli/internal/runs"
)

// Deps is what these verbs need. The money dependencies come through
// [transfers.Deps] because `resume` is a money command.
type Deps struct {
	Transfers transfers.Deps
}

func (d Deps) noun() noun.Deps { return d.Transfers.Noun.Resolve() }

// Command builds `ferry runs`.
func Command(deps Deps) *cobra.Command {
	cmd := noun.NewCommand("runs", "Read, resume and retire money runs",
		"Read the record of every money command this machine has started.\n\n"+
			"A run holds the idempotency key, the exact request bytes and what FERRY\n"+
			"answered. It is what makes a crashed transfer resumable under the key it\n"+
			"already used, instead of resent under a new one.")

	cmd.AddCommand(
		listCommand(deps),
		showCommand(deps),
		resumeCommand(deps),
		declineCommand(deps),
		pruneCommand(deps),
	)

	return cmd
}

// open resolves the local state and the ledger without sending anything.
//
// Every verb but `resume` stops here: they read and write the ledger and
// never build a client, so "zero requests" is structural for them in the way
// PLAN §5.4 steps 1–7 make it structural for the money path.
func open(deps Deps) (*noun.Local, *runs.Ledger, error) {
	local, err := noun.OpenLocal(deps.noun())
	if err != nil {
		return nil, nil, err
	}

	ledger := runs.New(deps.noun().FS, local.Paths.RunsDir(), deps.noun().Now)

	return local, ledger, nil
}

// load reads and scrubs, then re-reads.
//
// `Scrub` is run on every load (AC13, C6): a token whose plan expired more
// than the grace ago is removed the next time anything looks at the ledger,
// so a machine that ran one transfer months ago does not keep a bearer
// authorisation next to the key that could re-mint it. The scrub is a
// local-clock decision and errs safe — a token scrubbed early makes a resume
// exit 4 and send nothing.
func load(deps Deps, ledger *runs.Ledger) ([]*runs.Run, error) {
	if _, err := ledger.Scrub(deps.noun().Now()); err != nil {
		return nil, render.Fault(err, "the run ledger could not be scrubbed: %v", err)
	}

	all, err := ledger.List()
	if err != nil {
		return nil, render.Fault(err, "the run ledger could not be read: %v", err)
	}

	return all, nil
}
