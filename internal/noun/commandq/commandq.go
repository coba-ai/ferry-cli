// Package commandq is `ferry commands`: read a command, or follow one to its
// end.
//
// The package is named `commandq` and not `commands` because `Command` is
// cobra's own type and every other noun package exposes a function of that
// name; `commands.Command` would read as a constructor for the noun rather
// than for the CLI command.
//
// Neither verb here moves money. They are how a caller picks up a transfer
// this CLI lost sight of — the `next` sentence of every `pending` outcome
// names `ferry commands watch <id>`, and that sentence has to lead somewhere.
package commandq

import (
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"

	"github.com/coba-ai/ferry-cli/internal/api"
	"github.com/coba-ai/ferry-cli/internal/noun"
	"github.com/coba-ai/ferry-cli/internal/outcome"
	"github.com/coba-ai/ferry-cli/internal/poll"
	"github.com/coba-ai/ferry-cli/internal/render"
)

// Deps is what these verbs need beyond `noun.Deps`.
type Deps struct {
	Noun noun.Deps

	// Interrupted closes when a signal reaches the process, so `watch`
	// stops waiting rather than holding the terminal until its timeout.
	Interrupted <-chan struct{}
}

// Command builds `ferry commands`.
func Command(deps Deps) *cobra.Command {
	cmd := noun.NewCommand("commands", "Read and follow FERRY commands",
		"Read a command, or follow one until it stops moving.\n\n"+
			"A command is FERRY's record of one money operation. When a transfer's outcome\n"+
			"is not established — a 202, a timeout, a crash — the command is what still\n"+
			"knows, and `ferry commands watch` is how to find out without sending anything.")

	cmd.AddCommand(getCommand(deps), watchCommand(deps))

	return cmd
}

func getCommand(deps Deps) *cobra.Command {
	cmd := &cobra.Command{
		Use:           "get <command_id>",
		Short:         "Read a command once",
		Args:          noun.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: false,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runGet(cmd, deps, args[0])
		},
	}

	cmd.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return render.UsageFrom(err) })

	return cmd
}

// runGet reads a command once and classifies it as a read.
//
// A single read is a read, whatever it is a read *of*: the answer says
// nothing about whether this invocation moved money, because this invocation
// sent no money request. The command's own state is rendered, and the
// terminal rule that would turn it into a money outcome belongs to the verb
// that was waiting for it.
// detailOnFailure writes the command's fields when the outcome is not a
// success.
//
// A354: `noun.report` calls `Answer.Text` only when `Exit == 0`, so on every
// failing path the detail block is dropped and the caller gets the outcome
// sentence alone. For most verbs that is a small loss. For a command
// carrying a contradiction it is the whole of what an operator needs — the
// escalation sentence says a transaction cannot be accounted for, and
// `kind`, `detected_by` and `prior_state` say *which* and *how*, and those
// are exactly the fields that go missing.
//
// It writes to **stderr**, so stdout still carries exactly one JSON document
// in JSON mode and nothing but the report in text mode (AC41), and it is
// skipped entirely in JSON mode where the whole command is in the document
// already.
func detailOnFailure(cmd *cobra.Command, mode render.Mode, a noun.Answer) {
	if mode != render.ModeText || a.Outcome.Exit == 0 || a.Text == nil {
		return
	}

	a.Text(cmd.ErrOrStderr())
}

// resultBody is `command.result.body` or an empty one.
//
// A completed command whose stored answer aged out carries `body: null`, so
// every reader has to cope with its absence rather than with an arm being
// nil.
func resultBody(c poll.Command) api.CommandResultBody {
	if c.Result == nil || c.Result.Body == nil {
		return api.CommandResultBody{}
	}

	return *c.Result.Body
}

func runGet(cmd *cobra.Command, deps Deps, id string) error {
	session, err := noun.Open(cmd, deps.Noun, outcome.OpGetCommand)
	if err != nil {
		return err
	}

	res, err := session.Do(cmd.Context(), api.Request{PathParams: map[string]string{"id": id}})
	if err != nil {
		return err
	}

	var command poll.Command

	answer := noun.Classify(res, &command)

	// `noun.Classify` classifies the **HTTP answer**, which for every one
	// of these reads is `200 done`, exit 0. The question `commands get`
	// asks is about the command, so the command's own classification
	// replaces it: a `failed_terminal` command read successfully over a
	// 200 is exit 8, not a success, and a caller scripting `ferry commands
	// get` on `$?` would otherwise be told every command in the queue had
	// finished happily.
	//
	// The override is conditional on the body having decoded. When it did
	// not, the HTTP classification is the only honest one available — there
	// is no command to classify — and `api`'s reading of an unreadable 200
	// is the right answer to report.
	if len(answer.Body) > 0 {
		answer.Outcome = outcome.ClassifyCommand(command.Input())
	}

	answer.Text = func(w io.Writer) { writeCommand(w, command) }

	detailOnFailure(cmd, session.Mode, answer)

	return session.Report(cmd, answer)
}

func watchCommand(deps Deps) *cobra.Command {
	var timeout time.Duration

	cmd := &cobra.Command{
		Use:   "watch <command_id>",
		Short: "Follow a command until it stops moving",
		Long: "Follow a command until it reaches a state it will not leave.\n\n" +
			"This sends no money request. It is the safe way to learn how a transfer ended\n" +
			"when the invocation that started it did not find out.",
		Args:          noun.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: false,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWatch(cmd, deps, args[0], timeout)
		},
	}

	cmd.Flags().DurationVar(&timeout, "timeout", 120*time.Second,
		"how long to follow the command before reporting it unresolved")
	cmd.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return render.UsageFrom(err) })

	return cmd
}

// runWatch is the same loop the money path uses (AC59).
//
// There is one `poll.Watch` in this CLI and `internal/poll/sweep_test.go`
// holds it to that with an AST sweep: a second loop would be a second set of
// rules about when to stop, and "when to stop polling a money command" is
// the decision that separates `pending` from `escalate`.
func runWatch(cmd *cobra.Command, deps Deps, id string, timeout time.Duration) error {
	session, err := noun.Open(cmd, deps.Noun, outcome.OpGetCommand)
	if err != nil {
		return err
	}

	pollDeps := poll.Deps{
		Client:      session.Client,
		Clock:       deps.Noun.Resolve().Clock,
		Interrupted: deps.Interrupted,
	}

	if session.Mode == render.ModeText {
		pollDeps.Progress = cmd.ErrOrStderr()
	}

	// The first poll is immediate. The caller has typed the command id
	// because they want to know now, and nothing here has just been sent —
	// the delay in the money path exists because a command one second old
	// has not moved yet.
	result, err := poll.Watch(cmd.Context(), pollDeps, id, 0, poll.Now(pollDeps.Clock).Add(timeout))
	if err != nil {
		return err
	}

	answer := noun.Answer{
		Result:  result.Last,
		Outcome: result.Outcome,
		Body:    result.Body,
	}

	if result.Command != nil {
		command := *result.Command
		answer.Text = func(w io.Writer) { writeCommand(w, command) }
	}

	detailOnFailure(cmd, session.Mode, answer)

	return session.ReportRemote(cmd, answer)
}

// writeCommand renders a command for a human.
//
// `last_error` is printed whole and unread. A392: its `code` is not the wire
// error vocabulary — the recordings carry `UPSTREAM_UNKNOWN`, which the
// catalogue does not name — so this CLI shows an operator what FERRY said
// and branches on none of it.
func writeCommand(w io.Writer, c poll.Command) {
	field := func(key, value string) {
		if value == "" {
			return
		}

		fmt.Fprintf(w, "%-18s %s\n", key+":", value)
	}

	field("command", c.ID)
	field("operation", c.Operation)
	field("state", c.State)
	field("attempts", fmt.Sprintf("%d", c.Attempts))

	if c.TransactionID != nil {
		field("transaction", *c.TransactionID)
	}

	if c.IdempotencyKey != nil {
		field("idempotency key", *c.IdempotencyKey)
	}

	if c.Result != nil {
		field("result status", fmt.Sprintf("%d", c.Result.Status))

		if txn := resultBody(c).Transaction; txn != nil {
			field("status", txn.Status)
			field("sub-status", txn.SubStatus)
		}

		if sim := resultBody(c).Simulation; sim != nil {
			// A393: the stored arm carries no `plan.token`, structurally.
			// Saying so is better than a caller reading "plan: pln_…" and
			// looking for a token that was never stored.
			field("plan", sim.Plan.ID)
			fmt.Fprintln(w, "This command's stored simulation carries no plan token; simulate again to obtain one.")
		}
	}

	if c.Contradiction != nil {
		field("contradiction", c.Contradiction.Kind)
		field("detected by", c.Contradiction.DetectedBy)
		field("prior state", c.Contradiction.PriorState)
	}

	if c.LastError != nil {
		field("last error", c.LastError.Code)

		for _, key := range c.LastError.Keys() {
			raw, _ := json.Marshal(c.LastError.Rest[key])
			field("  "+key, string(raw))
		}
	}
}
