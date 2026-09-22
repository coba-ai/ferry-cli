// Package cli assembles `ferry` and owns what happens around every command:
// the global flags, the signal handler, the in-flight flag, the single JSON
// document and the exit code.
//
// # Why the wrapper exists
//
// PLAN §5.9 and AC41 require that `--output json` puts **exactly one**
// document on stdout on every exit path — a success, an API error, a usage
// error, a local fault, a refused pre-check, a panic in a renderer and a
// SIGINT — and that `outcome.exit_code` equals the code the process actually
// exits with. Neither property can be held by each verb agreeing to be
// careful. There are twenty verbs, four of them can exit in ways they do not
// choose, and the two properties are joint: a verb that wrote a document and
// then failed differently would satisfy "one document" and break the
// equality.
//
// So there is one place. [wrap] puts a recover, a stdout byte counter and
// the outcome classification around every leaf's RunE, and the document —
// when one is still owed — is written there from the same outcome the exit
// code is read from. A verb that has already answered is left alone, which
// is what makes the equality total rather than usually true: if the document
// on stdout came from the verb, the exit code comes from the same value.
//
// # Why the in-flight flag is here
//
// C17. A signal or a panic after a money request may have left must report
// `pending`/6 — "the outcome is not established" — and never `cli_fault`/1,
// which says nothing happened. The classification needs both the error and
// the flag, and the flag is per invocation, so both meet in one function.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/coba-ai/ferry-cli/internal/cli/flight"
	"github.com/coba-ai/ferry-cli/internal/fault"
	"github.com/coba-ai/ferry-cli/internal/noun"
	"github.com/coba-ai/ferry-cli/internal/noun/auth"
	"github.com/coba-ai/ferry-cli/internal/noun/commandq"
	"github.com/coba-ai/ferry-cli/internal/noun/corridors"
	"github.com/coba-ai/ferry-cli/internal/noun/keys"
	nounruns "github.com/coba-ai/ferry-cli/internal/noun/runs"
	"github.com/coba-ai/ferry-cli/internal/noun/transfers"
	"github.com/coba-ai/ferry-cli/internal/outcome"
	"github.com/coba-ai/ferry-cli/internal/render"
	"github.com/coba-ai/ferry-cli/internal/version"
)

// DefaultSignals are the three signals §5.3 V4 and AC69 name.
//
// SIGHUP is here for the same reason it declines a prompt: a closed terminal
// is the ordinary way an unattended `ferry` dies, and a CLI that ignored it
// would be killed mid-transfer with nothing written and nothing said.
var DefaultSignals = []os.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP}

// Options is what a caller may replace. Every field has a production
// default; `main.go` sets none of them but [Options.Signals].
type Options struct {
	// Noun is the dependency set every command shares.
	Noun noun.Deps

	// Signals are the signals this invocation listens for. Nil installs no
	// handler, which is what a test wanting a deterministic interrupt uses
	// together with [Options.Interrupted].
	Signals []os.Signal

	// Interrupted, when non-nil, is used instead of a signal handler. A
	// test closes it to deliver the interrupt at an instant it chose.
	Interrupted <-chan struct{}

	// PromptShown fires after a consent prompt has been written and before
	// the read begins. It is the seam AC72 and the signal-at-the-prompt
	// tests need; production passes nil.
	PromptShown func()

	// Args is the raw argument list this invocation was given —
	// `os.Args[1:]` in `main.go`.
	//
	// It is read for exactly one purpose: recovering `--output json` when
	// the flag parse failed before reaching it, so that a usage error
	// still answers a machine with a document (AC41). Nothing else reads
	// it, and cobra still does its own parsing.
	Args []string
}

// New builds `ferry`.
//
// One call is one invocation: the in-flight flag and the interrupt channel
// are made here and shared by every command in the tree, so a test that
// builds two roots gets two independent flags and cannot leak the first
// invocation's "a request may have left" into the second.
func New(opts Options) *cobra.Command {
	globals := &noun.Globals{}
	opts.Noun.Globals = globals

	flag := flight.New()

	interrupted := opts.Interrupted
	signals := opts.Signals

	own := make(chan struct{})
	if interrupted == nil {
		interrupted = own
	}

	money := transfers.Deps{
		Noun:        opts.Noun,
		Flight:      flag,
		Interrupted: interrupted,
		PromptShown: opts.PromptShown,
	}

	root := &cobra.Command{
		Use:   "ferry",
		Short: "Move money through FERRY from a terminal or a script",
		Long: "Move money through FERRY from a terminal or a script.\n\n" +
			"`ferry transfers create` prices a transfer and moves nothing. --broadcast is\n" +
			"the only way money leaves. Every money command writes an idempotency key to\n" +
			"disk before it sends, so a crash is resumed under the key it already used\n" +
			"rather than resent under a new one.\n\n" +
			"Exit codes are the interface: 0 done, 1 nothing was sent, 2 bad invocation,\n" +
			"3 refused, 4 re-simulate, 5 resend the same key, 6 outcome not established,\n" +
			"7 stop and read the command, 8 accepted upstream and then failed.",
		SilenceUsage:  true,
		SilenceErrors: false,
	}

	noun.BindGlobals(root, globals)

	root.AddCommand(
		auth.Command(opts.Noun),
		keys.Command(opts.Noun),
		corridors.Command(opts.Noun),
		transfers.Command(money),
		commandq.Command(commandq.Deps{Noun: opts.Noun, Interrupted: interrupted}),
		nounruns.Command(nounruns.Deps{Transfers: money}),
		versionCommand(),
		completionCommand(root),
	)

	w := &wrapper{
		globals:     globals,
		flight:      flag,
		signals:     signals,
		interrupted: interrupted,
		own:         own,
		ownSignals:  opts.Interrupted == nil,
		argv:        opts.Args,
	}

	wrap(root, w)

	return root
}

// wrap installs the exit plumbing on every command in the tree.
//
// Every leaf gets the recover, the counter and the classification. Every
// group — `ferry`, `ferry keys`, `ferry runs` — gets a RunE that writes its
// help to **stderr** and exits 2, because cobra's default is to print help
// on stdout and exit 0: in JSON mode that is a non-document on stdout, and
// in any mode it tells a script that `ferry transfers` succeeded.
func wrap(cmd *cobra.Command, w *wrapper) {
	for _, child := range cmd.Commands() {
		wrap(child, w)
	}

	if cmd.RunE == nil && cmd.Run == nil {
		cmd.RunE = func(c *cobra.Command, _ []string) error {
			_ = c.Help()

			return render.Usage("%s needs a subcommand", c.CommandPath())
		}

		cmd.SetHelpFunc(func(c *cobra.Command, _ []string) {
			_, _ = io.WriteString(c.ErrOrStderr(), c.UsageString())
		})
	}

	// A bad flag and a bad argument list are the two failures that never
	// reach a RunE: cobra returns both from `Command.execute()` before the
	// command body runs, so the wrapper's recover-and-classify never sees
	// them and nothing would write the document AC41 requires.
	//
	// Both hooks are installed per command rather than once on the root.
	// `Command.FlagErrorFunc` is inherited only when the command has not
	// set its own, and every noun leaf in this CLI sets its own in order to
	// return exit 2 — so a root-level hook is live for `ferry --bad` and
	// shadowed for `ferry transfers create --bad`, which is every case that
	// matters. The existing function is called and its verdict kept.
	flagError := cmd.FlagErrorFunc()
	cmd.SetFlagErrorFunc(func(c *cobra.Command, err error) error {
		return w.usage(c, asUsage(flagError(c, err)))
	})

	// Argument validation is the other pre-RunE failure: `ferry transfers
	// teleport` is rejected by the parent's Args validator, which returns
	// "unknown command" straight out of `Command.execute()`.
	//
	// The existing validator is kept and called. Replacing it would turn
	// every `ExactArgs(1)` in U4's nouns into "any number of arguments is
	// fine", which is a usage error becoming a request.
	validate := cmd.Args
	if validate == nil {
		validate = cobra.ArbitraryArgs
	}

	cmd.Args = func(c *cobra.Command, args []string) error {
		if err := validate(c, args); err != nil {
			return w.usage(c, asUsage(err))
		}

		return nil
	}

	inner := cmd.RunE
	if inner == nil {
		return
	}

	cmd.RunE = func(c *cobra.Command, args []string) error { return w.run(c, args, inner) }
	cmd.SilenceUsage = true
}

// wrapper is the one place an invocation ends.
type wrapper struct {
	globals *noun.Globals
	flight  *flight.Flag

	signals     []os.Signal
	interrupted <-chan struct{}
	own         chan struct{}
	ownSignals  bool

	// argv is the raw argument list, for recovering `--output` when the
	// flag parse itself failed.
	argv []string

	// seen is whether the interrupt arrived. It is separate from a read of
	// the channel so that the answer is stable: a classification that
	// selected on the channel twice could see it open and then closed, and
	// report a different class from the one it wrote into the document.
	seen atomic.Bool
}

func (w *wrapper) run(c *cobra.Command, args []string, inner func(*cobra.Command, []string) error) (err error) {
	tracker := &counter{w: c.OutOrStdout()}
	c.SetOut(tracker)

	ctx, cancel := context.WithCancel(c.Context())
	defer cancel()

	c.SetContext(ctx)

	stop := w.listen(cancel)
	defer stop()

	defer func() {
		v := recover()

		// A simulated process death is reported by nobody. It writes no
		// document and no message, and exits a code no outcome class
		// produces, which is the whole of how a test tells "the process
		// was killed here" from "the CLI reported this".
		if crash, ok := v.(fault.Crash); ok {
			// Cobra prints every error it is returned. Silencing it here
			// is the only way to return an exit code without also
			// announcing it, and announcing it is the one thing a killed
			// process cannot do.
			c.Root().SilenceErrors = true
			err = crashed{crash}

			return
		}

		err = w.finish(c, tracker, err, v)
	}()

	return inner(c, args)
}

// listen installs the signal handler for the duration of one command.
//
// It is scoped to the command rather than to the process because the test
// binary runs many invocations in one process: a handler left installed
// would turn a later test's signal into this invocation's cancel. When the
// caller supplied its own channel there is nothing to install — the test is
// delivering the interrupt itself.
func (w *wrapper) listen(cancel context.CancelFunc) func() {
	if !w.ownSignals || len(w.signals) == 0 {
		// Still watch the supplied channel, so a test's interrupt cancels
		// the context exactly as a signal would.
		done := make(chan struct{})

		go func() {
			select {
			case <-w.interrupted:
				w.seen.Store(true)
				cancel()
			case <-done:
			}
		}()

		return func() { close(done) }
	}

	ch := make(chan os.Signal, 1)
	signal.Notify(ch, w.signals...)

	done := make(chan struct{})

	go func() {
		select {
		case <-ch:
			w.seen.Store(true)

			// Closing the channel is what the consent prompt and the poll
			// loop are waiting on; cancelling the context is what an
			// in-flight HTTP request is waiting on. Both, in that order:
			// a prompt released by the close has an answer to give, while
			// a request killed by the cancel has none.
			close(w.own)
			cancel()

		case <-done:
		}
	}()

	return func() {
		signal.Stop(ch)
		close(done)
	}
}

// usage reports a failure that happened before the command body ran.
//
// It writes the document itself, because there is no `finish` on this path:
// cobra returns the error from `Command.execute()` and the wrapper's deferred
// recover is never installed. The error is returned unchanged so the exit
// code still comes from `outcome.Properties`.
//
// Nothing was sent on this path by construction — the flags were not even
// parsed — so there is no in-flight flag to consult.
func (w *wrapper) usage(c *cobra.Command, err *render.Error) error {
	if w.usageMode() != render.ModeJSON {
		return err
	}

	// Straight to the real stdout: the counter belongs to a RunE that will
	// not run. `c.OutOrStdout()` is what the harness swapped, so this is
	// the same destination a verb would have written to.
	if writeErr := render.WriteDocument(c.OutOrStdout(), w.document(err.Out)); writeErr != nil {
		fmt.Fprintf(c.ErrOrStderr(), "ferry: the outcome document could not be written: %v\n", writeErr)
	}

	return err
}

// asUsage keeps an already-classified failure and classifies anything else
// as exit 2.
//
// A noun leaf's own flag-error function already returns a `*render.Error`,
// and re-wrapping it would replace its message with the raw pflag text.
func asUsage(err error) *render.Error {
	var known *render.Error
	if errors.As(err, &known) {
		return known
	}

	return render.UsageFrom(err)
}

// finish is where an invocation's exit code and its document are decided
// together.
func (w *wrapper) finish(c *cobra.Command, tracker *counter, err error, panicked any) error {
	if panicked == nil && err == nil {
		// The command succeeded and has already said so. A signal that
		// arrived after that is too late to change what happened, and
		// reporting 6 here would say "the outcome is not established"
		// about an outcome that is written down.
		return nil
	}

	if tracker.wrote() && panicked == nil {
		// The verb answered for itself. Its document carries the exit
		// code, so the code must come from the same error it wrote from —
		// reclassifying here is what would make `outcome.exit_code`
		// disagree with `$?` (AC41).
		return err
	}

	out, msg := w.classify(err, panicked)

	if w.mode() == render.ModeJSON && !tracker.wrote() {
		if writeErr := render.WriteDocument(tracker, w.document(out)); writeErr != nil {
			// Nothing further can be written to stdout, so the exit code
			// is the only channel left. Keep the classified one: it is
			// what happened, and the write failing does not unhappen it.
			fmt.Fprintf(c.ErrOrStderr(), "ferry: the outcome document could not be written: %v\n", writeErr)
		}
	}

	// Built through `render.FromOutcome` so the outcome's `next` sentence is
	// appended to what a human reads. Constructing the `render.Error`
	// directly compiles and exits the right code, but drops the remediation
	// — and for the `pending` class the remediation is the run id and the
	// two commands that read and resend it, which is the entire value of
	// the report to someone whose money may have moved.
	report := render.FromOutcome(out, "%s", msg)
	report.Err = err

	return report
}

// classify is C17, written once.
//
// The flag decides between the two answers to "what happened", and it is the
// only thing that does. `cli_fault` says *nothing was sent*; once a money
// request may have left, that sentence is false whatever went wrong
// afterwards, so it becomes `pending` — "the outcome is not established,
// never a new key" (M73, M74, M75, M76, M101).
func (w *wrapper) classify(err error, panicked any) (outcome.Outcome, string) {
	sent := w.flight.MayHaveSent()

	switch {
	case panicked != nil:
		msg := fmt.Sprintf("the CLI panicked: %v", panicked)

		if sent {
			return w.pending("a panic"), msg
		}

		return flight.OutcomeFor(outcome.ClassCLIFault,
			"Nothing was sent. This is a bug in the CLI; the command can be run again."), msg

	case w.seen.Load():
		msg := "interrupted"

		if sent {
			return w.pending("an interrupt"), msg
		}

		return flight.OutcomeFor(outcome.ClassCLIFault,
			"Interrupted before anything was sent. Nothing happened; run the command again."), msg
	}

	var known *render.Error
	if errors.As(err, &known) {
		out := known.Out

		// The one reclassification of a classified error. Every other
		// class came from the decision table looking at a real answer,
		// and the table's answer is the truth about that answer; only
		// `cli_fault` makes a claim the flag can contradict.
		if sent && out.Class == outcome.ClassCLIFault {
			return w.pending("a local failure after the request left"), known.Msg
		}

		return out, known.Msg
	}

	if sent {
		return w.pending("an unclassified failure"), err.Error()
	}

	return flight.OutcomeFor(outcome.ClassCLIFault, "Nothing was sent."), err.Error()
}

// pending is the sentence a caller reads after an abnormal exit that may
// have moved money.
//
// It names the run and the command because "the outcome is not established"
// is useless without something to ask. The run id comes from the in-flight
// flag, which the money path set before it sent.
func (w *wrapper) pending(what string) outcome.Outcome {
	next := fmt.Sprintf(
		"This invocation ended on %s after a money request may have reached FERRY, so whether money "+
			"moved is not established. Do not resend under a new key.", what)

	if id := w.flight.RunID(); id != "" {
		next += fmt.Sprintf(" `ferry runs show %s` reads what was recorded, and `ferry runs resume %s` "+
			"resends the identical request under the identical key.", id, id)
	}

	if id := w.flight.CommandID(); id != "" {
		next += fmt.Sprintf(" FERRY's own record is `ferry commands watch %s`.", id)
	}

	return flight.OutcomeFor(outcome.ClassPending, next)
}

func (w *wrapper) document(out outcome.Outcome) render.Document {
	return render.Document{
		FerryCLI: render.CLIBlock{
			Version:     version.Get().Version,
			RunID:       render.Optional(w.flight.RunID()),
			Profile:     w.profile(),
			Environment: render.Optional(w.flight.Environment()),
		},
		Outcome: render.NewOutcomeBlock(out),

		// No `http` block. This document is written for an exit the CLI
		// decided on its own, and AC63 reads a null `http` as "no request
		// was made" — a block invented from a request whose answer nobody
		// read would be worse than none.
	}
}

func (w *wrapper) profile() string {
	if p := w.flight.Profile(); p != "" {
		return p
	}

	if w.globals.Profile != "" {
		return w.globals.Profile
	}

	return "default"
}

// mode reads `--output` without failing.
//
// An unparseable value has already been refused by `noun.OpenLocal`, which
// is where the usage error comes from; here it means "the caller asked for
// something we could not give", and text is the mode whose failure is a
// message rather than a malformed document.
func (w *wrapper) mode() render.Mode {
	mode, err := render.ParseMode(w.globals.Output)
	if err != nil {
		return render.ModeText
	}

	return mode
}

// usageMode is the mode for a failure that happened during flag parsing.
//
// It reads the argv first and the globals only as a fallback. The globals
// cannot be trusted on this path and cannot be *detected* as untrustworthy
// either: `noun.BindGlobals` registers `--output` with a default of "text",
// and pflag writes a flag's default into its variable at registration, so
// after a failed parse `globals.Output` reads "text" — a value indistinguish-
// able from a caller who asked for text. Checking it for emptiness looks like
// it works and never fires, which is how this cost an afternoon.
func (w *wrapper) usageMode() render.Mode {
	if raw := w.argvMode(); raw != "" {
		if mode, err := render.ParseMode(raw); err == nil {
			return mode
		}
	}

	return w.mode()
}

// argvMode finds `--output` in the raw arguments.
//
// It exists for one case: `ferry transfers create --no-such-flag --output
// json`, where pflag stops at the unknown flag and never reaches `--output`,
// so the globals are empty while the caller plainly asked for JSON. Reading
// the argv is the only way to honour that request, and answering a machine
// with a line of prose is the failure AC41 forbids.
//
// The scan is written out by hand rather than handed to pflag. pflag with
// `ParseErrorsWhitelist.UnknownFlags` is the obvious tool and it is the wrong
// one: skipping an unknown long flag consumes the argument after it, so in
// `--no-such-flag --output json` pflag eats `--output` as the unknown flag's
// value and reports no mode — which is the exact invocation this function
// exists for. `mode_test.go` pins all four spellings and that ordering.
//
// The last occurrence wins, matching pflag, and `--` ends the scan.
func (w *wrapper) argvMode() string {
	out := ""

	for i := 0; i < len(w.argv); i++ {
		arg := w.argv[i]

		if arg == "--" {
			break
		}

		switch {
		case arg == "--output", arg == "-o":
			if i+1 < len(w.argv) {
				out = w.argv[i+1]
				i++
			}

		case strings.HasPrefix(arg, "--output="):
			out = strings.TrimPrefix(arg, "--output=")

		case strings.HasPrefix(arg, "-o="):
			out = strings.TrimPrefix(arg, "-o=")

		case strings.HasPrefix(arg, "-o") && !strings.HasPrefix(arg, "--"):
			out = strings.TrimPrefix(arg, "-o")
		}
	}

	return out
}

// counter is stdout with a memory of whether anything reached it.
//
// It counts bytes rather than writes because the question is "has the caller
// already been given an answer", and a zero-byte write is not an answer.
type counter struct {
	w io.Writer
	n atomic.Int64
}

func (c *counter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n.Add(int64(n))

	return n, err
}

func (c *counter) wrote() bool { return c.n.Load() > 0 }

// crashed is a simulated process death. It carries an exit code no outcome
// class produces and no message.
type crashed struct{ fault.Crash }

func (c crashed) ExitCode() int { return fault.CrashExit }

// Crashed reports whether an error is a simulated process death, for
// `main.go`, which must exit without printing.
func Crashed(err error) bool {
	var c crashed

	return errors.As(err, &c)
}

func versionCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:           "version",
		Short:         "Print this CLI's build identity",
		Args:          noun.NoArgs(),
		SilenceUsage:  true,
		SilenceErrors: false,
		RunE: func(cmd *cobra.Command, _ []string) error {
			info := version.Get()

			fmt.Fprintf(cmd.OutOrStdout(), "ferry %s\ncommit %s\n%s %s/%s\ncontract %s\n",
				info.Version, info.Commit, info.GoVersion, info.OS, info.Arch, info.ContractSHA256)

			return nil
		},
	}

	return cmd
}

// completionCommand is cobra's generator, narrowed to the four shells and
// with its output forced to stdout.
func completionCommand(root *cobra.Command) *cobra.Command {
	return &cobra.Command{
		Use:                   "completion <bash|zsh|fish|powershell>",
		Short:                 "Print a shell completion script",
		Args:                  noun.ExactArgs(1),
		DisableFlagsInUseLine: true,
		SilenceUsage:          true,
		SilenceErrors:         false,
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()

			switch args[0] {
			case "bash":
				return root.GenBashCompletionV2(out, true)
			case "zsh":
				return root.GenZshCompletion(out)
			case "fish":
				return root.GenFishCompletion(out, true)
			case "powershell":
				return root.GenPowerShellCompletionWithDesc(out)
			default:
				return render.Usage("completion takes one of bash, zsh, fish, powershell")
			}
		},
	}
}
