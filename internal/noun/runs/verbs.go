package runs

import (
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/coba-ai/ferry-cli/internal/cli/flight"
	"github.com/coba-ai/ferry-cli/internal/noun"
	"github.com/coba-ai/ferry-cli/internal/noun/transfers"
	"github.com/coba-ai/ferry-cli/internal/outcome"
	"github.com/coba-ai/ferry-cli/internal/render"
	"github.com/coba-ai/ferry-cli/internal/runs"
)

func listCommand(deps Deps) *cobra.Command {
	var pendingOnly bool

	cmd := &cobra.Command{
		Use:           "list",
		Short:         "List this machine's money runs",
		Args:          noun.NoArgs(),
		SilenceUsage:  true,
		SilenceErrors: false,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runList(cmd, deps, pendingOnly)
		},
	}

	cmd.Flags().BoolVar(&pendingOnly, "pending", false, "only runs that are unresolved")
	cmd.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return render.UsageFrom(err) })

	return cmd
}

func runList(cmd *cobra.Command, deps Deps, pendingOnly bool) error {
	local, ledger, err := open(deps)
	if err != nil {
		return err
	}

	all, err := load(deps, ledger)
	if err != nil {
		return err
	}

	var shown []*runs.Run

	for _, run := range all {
		if pendingOnly && len(run.Unresolved()) == 0 {
			continue
		}

		shown = append(shown, run)
	}

	views := make([]runView, 0, len(shown))
	for _, run := range shown {
		views = append(views, view(run))
	}

	body, err := encode(map[string]any{"object": "list", "data": views})
	if err != nil {
		return err
	}

	return local.ReportLocal(cmd, noun.Answer{
		Outcome: flight.OutcomeFor(outcome.ClassDone, ""),
		Body:    body,
		Text:    func(w io.Writer) { writeList(w, shown) },
	})
}

// writeList is one line per run, widest-first alignment.
//
// The unresolved state is the column that matters and it comes first after
// the id: a caller running `ferry runs list` is almost always looking for
// the run that is blocking their next transfer.
func writeList(w io.Writer, all []*runs.Run) {
	if len(all) == 0 {
		fmt.Fprintln(w, "no runs")

		return
	}

	rows := make([][4]string, 0, len(all))

	for _, run := range all {
		rows = append(rows, [4]string{
			run.RunID,
			summarise(run),
			run.Environment,
			run.CreatedAt.Format(time.RFC3339),
		})
	}

	widths := [4]int{}

	for _, row := range rows {
		for i, cell := range row {
			if len(cell) > widths[i] {
				widths[i] = len(cell)
			}
		}
	}

	for _, row := range rows {
		fmt.Fprintf(w, "%-*s  %-*s  %-*s  %s\n",
			widths[0], row[0], widths[1], row[1], widths[2], row[2], row[3])
	}
}

// summarise is the one phrase that says what a run is waiting for.
func summarise(run *runs.Run) string {
	if unresolved := run.Unresolved(); len(unresolved) > 0 {
		return fmt.Sprintf("%s %s", unresolved[0].Name, unresolved[0].State)
	}

	states := make([]string, 0, len(run.Steps))
	for _, step := range run.Steps {
		states = append(states, fmt.Sprintf("%s %s", step.Name, step.State))
	}

	return strings.Join(states, ", ")
}

func showCommand(deps Deps) *cobra.Command {
	cmd := &cobra.Command{
		Use:           "show <run_id>",
		Short:         "Read one run's record",
		Args:          noun.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: false,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runShow(cmd, deps, args[0])
		},
	}

	cmd.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return render.UsageFrom(err) })

	return cmd
}

func runShow(cmd *cobra.Command, deps Deps, runID string) error {
	local, ledger, err := open(deps)
	if err != nil {
		return err
	}

	if _, err := load(deps, ledger); err != nil {
		return err
	}

	run, err := ledger.Load(runID)
	if err != nil {
		return notFound(runID, err)
	}

	body, err := encode(view(run))
	if err != nil {
		return err
	}

	return local.ReportLocal(cmd, noun.Answer{
		Outcome: flight.OutcomeFor(outcome.ClassDone, ""),
		Body:    body,
		Text:    func(w io.Writer) { writeRun(w, run) },
	})
}

func writeRun(w io.Writer, run *runs.Run) {
	field := func(key, value string) {
		if value == "" {
			return
		}

		fmt.Fprintf(w, "%-18s %s\n", key+":", value)
	}

	// The text rendering reads the redacted view rather than the record, so
	// that there is exactly one place a field can be forgotten. Reaching
	// into `run` here for a field is how argv came to be redacted in this
	// renderer and not in the JSON one.
	r := view(run)

	field("run", r.RunID)
	field("created", r.CreatedAt.Format(time.RFC3339))
	field("cli", r.CLIVersion)
	field("profile", r.Profile)
	field("environment", r.Environment)
	field("api", r.APIURL)
	field("credential", r.TokenPrefix)
	field("principal", r.PrincipalID)

	if len(r.Argv) > 0 {
		field("argv", strings.Join(r.Argv, " "))
	}

	for _, v := range r.Steps {

		fmt.Fprintf(w, "\n%s\n", v.Name)
		field("  state", v.State)
		field("  operation", v.Operation)
		field("  request", v.Method+" "+v.Path)
		field("  idempotency key", v.IdempotencyKey)
		field("  attempts", strconv.Itoa(v.Attempts))

		if v.BodySHA256 != nil {
			field("  body sha256", *v.BodySHA256)
		}

		if len(v.Body) > 0 {
			field("  body", string(v.Body))
		}

		if v.Plan != nil {
			field("  plan", v.Plan.ID)

			if !v.Plan.ExpiresAt.IsZero() {
				field("  plan expires", v.Plan.ExpiresAt.Format(time.RFC3339))
			}
		}

		if v.PlanToken != nil {
			field("  plan token", *v.PlanToken)
		}

		if v.Response != nil {
			field("  response", strconv.Itoa(v.Response.Status))

			for _, name := range sortedKeys(v.Response.Headers) {
				field("    "+name, v.Response.Headers[name])
			}

			if len(v.Response.Body) > 0 {
				field("    body", string(v.Response.Body))
			}
		}

		if v.Outcome != nil {
			field("  outcome", fmt.Sprintf("%s (exit %d)", v.Outcome.Class, v.Outcome.ExitCode))
			field("  money", v.Outcome.Money)
			field("  next", v.Outcome.Next)
		}
	}
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for key := range m {
		out = append(out, key)
	}

	sort.Strings(out)

	return out
}

func resumeCommand(deps Deps) *cobra.Command {
	var ro transfers.ResumeOptions

	cmd := &cobra.Command{
		Use:   "resume <run_id>",
		Short: "Send a run's outstanding request again, under its own key",
		Long: "Send a run's outstanding request again, under the key it already used.\n\n" +
			"This cannot double-send: FERRY answers a repeated key with the outcome it\n" +
			"already has. It carries no consent of its own — an execute step is asked about\n" +
			"again on a terminal, and needs --yes without one, because consent is\n" +
			"established per invocation and is never stored.",
		Args:          noun.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: false,
		RunE: func(cmd *cobra.Command, args []string) error {
			return transfers.Resume(cmd, deps.Transfers, args[0], ro)
		},
	}

	f := cmd.Flags()
	f.BoolVar(&ro.Yes, "yes", false, "consent to this invocation's execute without a prompt")
	f.BoolVar(&ro.NoWait, "no-wait", false, "do not follow a 202 to a terminal state")
	f.DurationVar(&ro.Timeout, "timeout", transfers.DefaultTimeout, "how long to follow a command")
	f.BoolVar(&ro.AllowPending, "allow-pending", false, "resume even though other unresolved runs exist")
	cmd.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return render.UsageFrom(err) })

	return cmd
}

func declineCommand(deps Deps) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "decline <run_id>",
		Short: "Record that a run was never confirmed",
		Long: "Record that a run's execute was never confirmed, and scrub its plan token.\n\n" +
			"This is how a headless machine clears a run left awaiting confirmation — by a\n" +
			"killed process, or by a prompt nobody was there to answer. It sends nothing.\n\n" +
			"A step that may already have been sent cannot be declined, only resumed: the\n" +
			"ledger will not turn \"unknown\" into \"no\".",
		Args:          noun.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: false,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDecline(cmd, deps, args[0])
		},
	}

	cmd.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return render.UsageFrom(err) })

	return cmd
}

func runDecline(cmd *cobra.Command, deps Deps, runID string) error {
	local, ledger, err := open(deps)
	if err != nil {
		return err
	}

	run, err := ledger.Decline(runID)
	if err != nil {
		switch {
		case errors.Is(err, runs.ErrStepMayHaveSent):
			return &render.Error{
				Out: flight.OutcomeFor(outcome.ClassPending, fmt.Sprintf(
					"A request under this run's key may already have reached FERRY, so \"no\" is no longer "+
						"an available answer. Find out with `ferry runs resume %s --yes`, which resends the "+
						"recorded bytes under the recorded key and cannot double-send.", runID)),
				Msg: fmt.Sprintf("run %s cannot be declined: %v", runID, err),
				Err: err,
			}

		case errors.Is(err, runs.ErrRunLocked):
			return render.Fault(err, "run %s is held by another process; nothing was changed", runID)

		default:
			return notFound(runID, err)
		}
	}

	body, err := encode(view(run))
	if err != nil {
		return err
	}

	return local.ReportLocal(cmd, noun.Answer{
		Outcome: flight.OutcomeFor(outcome.ClassDone, ""),
		Body:    body,
		Text: func(w io.Writer) {
			fmt.Fprintf(w, "run %s declined; nothing was sent and any plan token was scrubbed\n", runID)
		},
	})
}

func pruneCommand(deps Deps) *cobra.Command {
	var olderThan string

	cmd := &cobra.Command{
		Use:   "prune",
		Short: "Remove settled run records",
		Long: "Remove run records that are settled and hold no plan token.\n\n" +
			"A run with a step still awaiting confirmation is never pruned: an unconfirmed\n" +
			"plan is a decision somebody has not made yet, and removing the record would\n" +
			"make it a decision nobody can make.",
		Args:          noun.NoArgs(),
		SilenceUsage:  true,
		SilenceErrors: false,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runPrune(cmd, deps, olderThan)
		},
	}

	cmd.Flags().StringVar(&olderThan, "older-than", "30d", "remove settled runs created before this much ago")
	cmd.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return render.UsageFrom(err) })

	return cmd
}

func runPrune(cmd *cobra.Command, deps Deps, olderThan string) error {
	local, ledger, err := open(deps)
	if err != nil {
		return err
	}

	age, err := parseAge(olderThan)
	if err != nil {
		return err
	}

	if _, err := load(deps, ledger); err != nil {
		return err
	}

	removed, err := ledger.Prune(deps.noun().Now().Add(-age))
	if err != nil {
		return render.Fault(err, "the run ledger could not be pruned: %v", err)
	}

	sort.Strings(removed)

	body, err := encode(map[string]any{"object": "list", "data": removed})
	if err != nil {
		return err
	}

	return local.ReportLocal(cmd, noun.Answer{
		Outcome: flight.OutcomeFor(outcome.ClassDone, ""),
		Body:    body,
		Text: func(w io.Writer) {
			if len(removed) == 0 {
				fmt.Fprintln(w, "no runs to prune")

				return
			}

			for _, id := range removed {
				fmt.Fprintln(w, id)
			}
		},
	})
}

// parseAge reads `--older-than`.
//
// It accepts Go's own duration spelling and the day and week suffixes a
// human reaches for, because "30d" is what the flag's default is and a flag
// whose default it could not parse would be a flag nobody could use.
func parseAge(raw string) (time.Duration, error) {
	if raw == "" {
		return 0, render.Usage("--older-than needs a duration, such as 30d or 12h")
	}

	multiplier := time.Duration(0)

	switch raw[len(raw)-1] {
	case 'd':
		multiplier = 24 * time.Hour
	case 'w':
		multiplier = 7 * 24 * time.Hour
	}

	if multiplier != 0 {
		n, err := strconv.Atoi(raw[:len(raw)-1])
		if err != nil || n < 0 {
			return 0, render.Usage("--older-than %q is not a duration, such as 30d or 12h", raw)
		}

		return time.Duration(n) * multiplier, nil
	}

	d, err := time.ParseDuration(raw)
	if err != nil || d < 0 {
		return 0, render.Usage("--older-than %q is not a duration, such as 30d or 12h", raw)
	}

	return d, nil
}

func notFound(runID string, err error) error {
	if errors.Is(err, runs.ErrRunNotFound) {
		return render.Usage("no run %s; `ferry runs list` shows what this profile holds", runID)
	}

	return render.Fault(err, "run %s could not be read: %v", runID, err)
}
