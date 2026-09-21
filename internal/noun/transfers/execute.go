package transfers

import (
	"time"

	"github.com/spf13/cobra"

	"github.com/kurenn/ferry-cli/internal/noun"
	"github.com/kurenn/ferry-cli/internal/outcome"
	"github.com/kurenn/ferry-cli/internal/render"
)

// moneyFlagSet says which of the money flags a verb declares. A simulate has
// no `--yes` because it asks nothing, and no `--broadcast` because it is the
// half that does not execute.
type moneyFlagSet struct {
	broadcast bool
	yes       bool

	// wait declares `--no-wait` and `--timeout`, which every verb that can
	// receive a 202 needs — simulate included: `simulate.202.then_completed`
	// is a recorded answer to `POST /v1/transfers/simulate` (A351).
	wait bool
}

func bindMoneyFlags(cmd *cobra.Command, o *options, set moneyFlagSet) {
	f := cmd.Flags()

	if set.broadcast {
		f.BoolVar(&o.broadcast, "broadcast", false, "execute the plan this invocation receives (the only way money leaves)")
	}

	if set.yes {
		f.BoolVar(&o.yes, "yes", false, "consent to this execute without a prompt")
	}

	f.StringVar(&o.idempotencyKey, "idempotency-key", "", "the Idempotency-Key to send, instead of a minted one")

	if set.wait {
		f.BoolVar(&o.noWait, "no-wait", false, "do not follow a 202 to a terminal state")
		f.DurationVar(&o.timeout, "timeout", DefaultTimeout, "how long to follow a command before reporting it unresolved")
	}

	f.BoolVar(&o.allowPending, "allow-pending", false, "start a new run even though an unresolved one exists")
}

func executeCommand(deps Deps) *cobra.Command {
	var (
		opts options
		plan string
	)

	cmd := &cobra.Command{
		Use:   "execute",
		Short: "Execute a plan a simulate priced",
		Long: "Execute the plan a `ferry transfers create` priced.\n\n" +
			"This moves money. On a terminal it shows what it can and asks; with no terminal\n" +
			"attached the command is itself the consent, which is what lets an agent run it.\n" +
			"Consent is established per invocation and is never stored, so a plan declined\n" +
			"once is never executed by a later `ferry runs resume` without being asked again.",
		Args:          noun.NoArgs(),
		SilenceUsage:  true,
		SilenceErrors: false,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runExecute(cmd, deps, opts, plan)
		},
	}

	cmd.Flags().StringVar(&plan, "plan", "", "the plan token to execute, or - to read it from stdin")
	bindMoneyFlags(cmd, &opts, moneyFlagSet{yes: true, wait: true})
	cmd.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return render.UsageFrom(err) })

	return cmd
}

// runExecute is C16's third arm (AC93): a money command that holds a token
// and no quote.
func runExecute(cmd *cobra.Command, deps Deps, opts options, plan string) error {
	token, err := planToken(cmd, plan)
	if err != nil {
		return err
	}

	r, err := begin(cmd, deps, opts, outcome.OpExecuteTransfer)
	if err != nil {
		return err
	}

	defer r.close()

	body, err := executeBody(token)
	if err != nil {
		return err
	}

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

	if err := r.create([]stepPlan{executeStep}, body, opts.idempotencyKey); err != nil {
		return err
	}

	answered, err := r.perform(cmd.Context(), executeStep, body, true)
	if err != nil {
		return err
	}

	return r.report(answered)
}

// parseTime reads one of the two instant spellings FERRY emits, and says so
// rather than substituting the zero time.
//
// The zero time is January 1st year 1, which renders as "expired 2025 years
// ago" — a sentence a caller would act on. A format this CLI cannot read is
// reported as unknown instead.
func parseTime(raw string) (time.Time, bool) {
	if raw == "" {
		return time.Time{}, false
	}

	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if at, err := time.Parse(layout, raw); err == nil {
			return at.UTC(), true
		}
	}

	return time.Time{}, false
}
