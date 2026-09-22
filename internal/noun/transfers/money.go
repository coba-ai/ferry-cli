package transfers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/coba-ai/ferry-cli/internal/api"
	"github.com/coba-ai/ferry-cli/internal/cli/flight"
	"github.com/coba-ai/ferry-cli/internal/consent"
	"github.com/coba-ai/ferry-cli/internal/creds"
	"github.com/coba-ai/ferry-cli/internal/fault"
	"github.com/coba-ai/ferry-cli/internal/noun"
	"github.com/coba-ai/ferry-cli/internal/outcome"
	"github.com/coba-ai/ferry-cli/internal/poll"
	"github.com/coba-ai/ferry-cli/internal/render"
	"github.com/coba-ai/ferry-cli/internal/runs"
	"github.com/coba-ai/ferry-cli/internal/version"
)

// DefaultTimeout is `--timeout`'s default (§5.7).
const DefaultTimeout = 120 * time.Second

// options are the money flags of PLAN §5.1, shared by every verb that can
// reach the wire.
type options struct {
	broadcast      bool
	yes            bool
	idempotencyKey string
	noWait         bool
	timeout        time.Duration
	allowPending   bool
}

// stepPlan is one HTTP call a run owns, named before the run exists.
type stepPlan struct {
	name string
	op   outcome.Operation
}

var (
	simulateStep = stepPlan{name: runs.StepSimulate, op: outcome.OpSimulateTransfer}
	executeStep  = stepPlan{name: runs.StepExecute, op: outcome.OpExecuteTransfer}
)

func planFor(name string) stepPlan {
	if name == runs.StepExecute {
		return executeStep
	}

	return simulateStep
}

// runner is one money invocation.
type runner struct {
	cmd  *cobra.Command
	deps Deps
	opts options

	local    *noun.Local
	sessions map[outcome.Operation]*noun.Session
	ledger   *runs.Ledger
	guard    *fault.Guard

	handle *runs.Handle

	// live is whether the credential is a live-environment key. It prefixes
	// every line of the text report (AC58).
	live bool

	// sim is the simulation this invocation received, for the confirmation
	// prompt and the text report. Nil for `execute --plan`, which holds a
	// token and no quote.
	sim *api.Simulation
}

// begin does every local thing PLAN §5.4 puts before the first durable
// effect: resolve the profile, pre-check the credential class for each
// operation the command may perform, assert `--env`, and open the ledger.
//
// Nothing here sends and nothing here writes, and that is structural rather
// than observed: the client is built by `noun.Session` and the ledger handle
// by [runner.create], both after every refusal below has had its chance.
//
// `get_command` is pre-checked alongside the money operation, because a
// command that may poll must not discover at the 202 that it holds no
// credential for the poll — the money request would already have left.
func begin(cmd *cobra.Command, deps Deps, opts options, ops ...outcome.Operation) (*runner, error) {
	deps = deps.resolve()

	local, err := noun.OpenLocal(deps.Noun)
	if err != nil {
		return nil, err
	}

	r := &runner{
		cmd:      cmd,
		deps:     deps,
		opts:     opts,
		local:    local,
		sessions: map[outcome.Operation]*noun.Session{},
	}

	for _, op := range append(ops, outcome.OpGetCommand) {
		if _, done := r.sessions[op]; done {
			continue
		}

		session, err := local.Session(cmd, op)
		if err != nil {
			return nil, err
		}

		r.sessions[op] = session
	}

	if err := r.assertEnvironment(ops[0]); err != nil {
		return nil, err
	}

	r.live = isLive(r.sessions[ops[0]].Match.Credential.Environment)
	r.guard = fault.NewGuard(deps.Noun.FS)
	r.ledger = runs.New(r.guard, local.Paths.RunsDir(), deps.now)

	deps.Flight.NoteProfile(local.Profile(), r.environment())

	return r, nil
}

// assertEnvironment is AC58: `--env` on a money command is an assertion about
// the stored credential, checked before anything is sent.
//
// A mismatch is a refusal and not a warning. The two environments are
// different organisations' money: a caller who typed `--env sandbox` and
// holds a live key has said what they believe, and sending anyway would be
// the CLI deciding it knows better about which environment a transfer
// belongs in.
func (r *runner) assertEnvironment(op outcome.Operation) error {
	asserted := r.local.Globals.Env
	if asserted == "" {
		return nil
	}

	want, err := noun.ParseEnvironment(asserted)
	if err != nil {
		return err
	}

	session := r.sessions[op]

	got := session.Match.Credential.Environment
	if got == nil {
		return render.Refused(
			"--env %s was asserted and the credential in profile %q belongs to no environment (it is a %s).\n"+
				"Nothing was sent. A money command needs an API key for the environment it names.",
			want, r.local.Profile(), session.Match.Class)
	}

	if *got != want {
		return render.Refused(
			"--env %s was asserted and profile %q holds a %s key (%s).\n"+
				"Nothing was sent. Use `--profile` for the other environment, or drop `--env`.",
			want, r.local.Profile(), *got, session.Match.Credential.Display())
	}

	return nil
}

func (r *runner) environment() string {
	for _, op := range []outcome.Operation{outcome.OpExecuteTransfer, outcome.OpSimulateTransfer, outcome.OpGetCommand} {
		session, ok := r.sessions[op]
		if !ok {
			continue
		}

		if env := session.Match.Credential.Environment; env != nil {
			return string(*env)
		}
	}

	return ""
}

func (r *runner) session(op outcome.Operation) *noun.Session { return r.sessions[op] }

// close releases the sidecar lock.
func (r *runner) close() {
	if r.handle != nil {
		_ = r.handle.Close()
	}
}

func (r *runner) runID() string {
	if r.handle == nil {
		return ""
	}

	return r.handle.Run().RunID
}

// route is PLAN §5.4 step 4: a caller-supplied `--idempotency-key` the ledger
// already holds.
//
// The digest decides. The same bytes under the same key is the caller asking
// to resume the run that holds it (AC94); different bytes under a key FERRY
// has already seen is `409 IDEMPOTENCY_KEY_REUSED`, refused here with nothing
// sent because the local record is enough to know it (AC54).
func (r *runner) route(key string, body []byte) (*runs.KeyMatch, error) {
	if key == "" {
		return nil, nil
	}

	if !runs.ValidKey(key) {
		return nil, render.Usage("--idempotency-key must be 1 to 255 bytes of printable ASCII")
	}

	match, err := r.ledger.FindByKey(key)
	if err != nil {
		return nil, render.Fault(err, "the run ledger could not be read: %v", err)
	}

	if match == nil {
		return nil, nil
	}

	digest := runs.Body(body).SHA256()

	if match.Step.BodySHA256 == nil || *match.Step.BodySHA256 != digest {
		return nil, render.Refused(
			"--idempotency-key %s is already held by run %s, against a different request.\n"+
				"Nothing was sent. FERRY answers a reused key carrying a different body `409 "+
				"IDEMPOTENCY_KEY_REUSED`; use a new key, or `ferry runs show %s` to see what that one holds.",
			key, match.Run.RunID, match.Run.RunID)
	}

	return match, nil
}

// orphanCheck is C18, PLAN §5.4 step 5.
//
// Fail closed. An unresolved run whose lock nobody holds is a request that
// may already have been sent under a key this process is about to replace
// with a new one, and `until ferry transfers create …; do sleep 5; done`
// turns that into a second transfer on iteration two. Refusing costs a caller
// one flag; proceeding costs them a transfer.
func (r *runner) orphanCheck(exempt ...string) error {
	if r.opts.allowPending {
		return nil
	}

	orphans, err := r.ledger.Orphans(r.local.Profile(), r.environment(), exempt...)
	if err != nil {
		return render.Fault(err, "the run ledger could not be read: %v", err)
	}

	if len(orphans) == 0 {
		return nil
	}

	var b strings.Builder

	fmt.Fprintf(&b, "%d unresolved run(s) exist for profile %q", len(orphans), r.local.Profile())

	if env := r.environment(); env != "" {
		fmt.Fprintf(&b, " in %s", env)
	}

	b.WriteString("; nothing was sent.\n")

	for _, o := range orphans {
		fmt.Fprintf(&b, "  %s  %s is %s\n", o.RunID, o.Step, o.State)
	}

	fmt.Fprintf(&b,
		"Resume one with `ferry runs resume %s`, `ferry runs decline %s` if it was never confirmed, "+
			"or pass `--allow-pending` to start a new run anyway.",
		orphans[0].RunID, orphans[0].RunID)

	// cli_fault, exit 1: nothing was sent by this invocation. AC81 names
	// the code, and the class is the one that means "a local problem, fix
	// it and run again" — which an unresolved run is.
	return &render.Error{Out: flight.OutcomeFor(outcome.ClassCLIFault, ""), Msg: b.String()}
}

// create mints the run: PLAN §5.4 steps 6 and 7.
//
// Every step's key is minted here, before the first send, because AC47 says
// so and because a key minted after the simulate answered is a key a crash in
// between can lose — and a lost key is a new key, which is the one thing C2
// forbids.
func (r *runner) create(plans []stepPlan, body []byte, userKey string) error {
	runID, err := r.deps.NewRunID()
	if err != nil {
		return render.Fault(err, "a run id could not be minted: %v", err)
	}

	session := r.session(plans[0].op)

	run := &runs.Run{
		RunID:           runID,
		CreatedAt:       r.deps.now(),
		CLIVersion:      version.Get().Version,
		Profile:         r.local.Profile(),
		APIURL:          session.Match.APIURL,
		Environment:     r.environment(),
		CredentialClass: string(session.Match.Class),
		TokenPrefix:     session.Match.Credential.TokenPrefix,
		PrincipalID:     session.Match.Credential.PrincipalID,
		Argv:            argv(r.cmd),
	}

	for _, p := range plans {
		route, ok := api.RouteFor(p.op)
		if !ok {
			return render.Fault(nil, "this CLI has no route for the operation %q", p.op)
		}

		key := runs.StepKey(runID, p.name)
		if userKey != "" && len(plans) == 1 {
			key = userKey
		}

		step := &runs.Step{
			Name:           p.name,
			Operation:      string(p.op),
			Method:         route.Method,
			Path:           route.Path,
			IdempotencyKey: key,
			State:          runs.StateNotStarted,
		}

		// Only the step that carries the caller's body gets it now. The
		// execute step of a --broadcast run has no body until the plan
		// token exists, and inventing one would put a `null` token on
		// disk under a digest that claims to be the request (M88).
		if p.name == plans[0].name && body != nil {
			step.Body = runs.Body(body)
		}

		run.Steps = append(run.Steps, step)
	}

	handle, err := r.ledger.Create(run)
	if err != nil {
		return render.Fault(err, "the run record could not be written, so nothing was sent: %v", err)
	}

	r.handle = handle
	r.deps.Flight.NoteRun(runID)

	return nil
}

// open takes an existing run as a holder, for a resume.
func (r *runner) open(runID string) error {
	handle, err := r.ledger.Open(runID)
	if err != nil {
		switch {
		case errors.Is(err, runs.ErrRunLocked):
			return render.Fault(err,
				"run %s is locked by another process; nothing was sent.\n"+
					"Wait for it to finish, or read it with `ferry runs show %s`.", runID, runID)

		case errors.Is(err, runs.ErrRunNotFound):
			return render.Usage("no run %s; `ferry runs list` shows what this profile holds", runID)

		default:
			return render.Fault(err, "run %s could not be read: %v", runID, err)
		}
	}

	r.handle = handle
	r.deps.Flight.NoteRun(runID)

	return nil
}

// checkCredential is C19 and AC70: the credential resuming a run must be the
// one that started it.
//
// Exit 7 and zero requests. A run resumed under another principal's key would
// send that principal's transfer, and the reply to the `409` FERRY would
// eventually answer is not readable by either of them.
func (r *runner) checkCredential() error {
	run := r.handle.Run()

	match := r.session(planFor(run.Steps[0].Name).op).Match

	switch {
	case run.TokenPrefix != "" && run.TokenPrefix != match.Credential.TokenPrefix:
		return &render.Error{
			Out: flight.OutcomeFor(outcome.ClassEscalate, fmt.Sprintf(
				"Re-login with the credential whose prefix is %s and resume run %s; nothing has been sent.",
				run.TokenPrefix, run.RunID)),
			Msg: fmt.Sprintf(
				"run %s was started with the credential %s and profile %q now holds %s",
				run.RunID, run.TokenPrefix, r.local.Profile(), match.Credential.TokenPrefix),
		}

	case run.PrincipalID != "" && match.Credential.PrincipalID != "" && run.PrincipalID != match.Credential.PrincipalID:
		return &render.Error{
			Out: flight.OutcomeFor(outcome.ClassEscalate, fmt.Sprintf(
				"Re-login with the credential whose prefix is %s and resume run %s; nothing has been sent.",
				run.TokenPrefix, run.RunID)),
			Msg: fmt.Sprintf(
				"run %s belongs to principal %s and profile %q now holds %s",
				run.RunID, run.PrincipalID, r.local.Profile(), match.Credential.PrincipalID),
		}
	}

	return nil
}

// answer is one step's outcome, and what the report needs to render it.
type answer struct {
	step *runs.Step

	// res is the answer the step's own request got.
	res api.Result

	// last is the answer the outcome was computed from: the poll's last
	// answer when the step was polled, and res otherwise.
	last api.Result

	body json.RawMessage
	out  outcome.Outcome

	sim  *api.Simulation
	txn  *api.Transaction
	poll *poll.Result

	// warnings are this CLI's own, added to the outcome's at report time.
	warnings []string
}

// perform is PLAN §5.4 steps 8 to 13 for one step.
//
// body is the bytes to send, or nil for a resend of a step that already holds
// its own. The nil is load-bearing: `runs.Begin` refuses a body that
// disagrees with the digest it holds, and passing nil is how a resume states
// that the bytes on disk are the bytes resent (C2, AC45).
func (r *runner) perform(ctx context.Context, p stepPlan, body []byte, commandIsConsent bool) (*answer, error) {
	step := r.handle.Run().Step(p.name)
	if step == nil {
		return nil, render.Fault(nil, "run %s has no %s step", r.runID(), p.name)
	}

	if p.name == runs.StepExecute {
		if err := r.obtainConsent(step, commandIsConsent); err != nil {
			return nil, err
		}
	}

	// C1. Nothing below this line may run before this has returned nil.
	// An interrupt that arrived before this point stops the invocation
	// here, before `Begin` and before the in-flight flag.
	//
	// The order is the whole claim. The context is already cancelled by
	// now, so the request would not leave the process — and a flag set for
	// a request that provably never left would make the CLI report
	// "whether money moved is not established" about a transfer it is
	// certain it did not send. That report sends a caller to `runs resume`
	// for nothing, and it is the answer this CLI must be able to *avoid*
	// giving, or exit 6 stops meaning anything.
	if interrupted(r.deps.Interrupted) {
		return nil, errInterrupted
	}

	if err := r.handle.Begin(p.name, body); err != nil {
		return nil, r.beginFailed(step, err)
	}

	fault.Die(fault.AfterRecordWritten)

	// C17. Set before the request leaves, never after it returns (M76),
	// and never cleared (M75).
	r.deps.Flight.MarkSent()

	res, err := r.session(p.op).Do(ctx, api.Request{
		Body:           step.Body,
		IdempotencyKey: step.IdempotencyKey,
	})
	if err != nil {
		return nil, err
	}

	fault.Die(fault.AfterSendBeforeRecord)

	if p.name == runs.StepSimulate {
		fault.Die(fault.AfterSimulateSendBeforeRecord)
	}

	// A signal that arrived while the request was on the wire ends the
	// invocation here, before the answer is recorded (AC69(a)). The step
	// stays `pending`, which is the true statement: the request left, and
	// what came back was not written down. Recording first and then
	// reporting the interrupt would claim more than this process knows —
	// the answer may have been the cancelled connection rather than
	// FERRY's.
	if interrupted(r.deps.Interrupted) {
		return nil, errInterrupted
	}

	a := r.classify(p, res)

	if id := commandID(res); id != "" {
		r.deps.Flight.NoteCommand(id)
	}

	// AC75, ordered: the execute step is closed at the moment the simulate
	// 202 is recorded, not after the poll. A crash between the two would
	// otherwise leave a run whose execute step still reads not_started,
	// which a resume would try to send with a token that was never issued.
	if err := r.record(p, a); err != nil {
		return nil, err
	}

	if r.opts.broadcast && p.name == runs.StepSimulate && res.Meta.Status == 202 {
		if err := r.handle.Unreachable(runs.StepExecute); err != nil {
			return nil, render.Fault(err, "the execute step of run %s could not be closed: %v", r.runID(), err)
		}
	}

	if err := r.followUp(ctx, p, a); err != nil {
		return nil, err
	}

	if a.sim != nil {
		r.sim = a.sim
	}

	return a, nil
}

// beginFailed turns a refusal to write the record into the refusal to send.
//
// ErrBodyMismatch is the one branch that is not a local fault: it means this
// invocation was asked to send different bytes under a key the ledger holds
// against others, which is the request FERRY answers `409
// IDEMPOTENCY_KEY_REUSED`, and it is refused here with nothing sent.
func (r *runner) beginFailed(step *runs.Step, err error) error {
	if errors.Is(err, runs.ErrBodyMismatch) {
		return render.Refused(
			"the %s step of run %s already holds different bytes under the key %s; nothing was sent.\n"+
				"Start a new run, or `ferry runs resume %s` to send what is recorded.",
			step.Name, r.runID(), step.IdempotencyKey, r.runID())
	}

	return render.Fault(err,
		"the run record could not be written, so nothing was sent: %v\nRun %s, step %s.",
		err, r.runID(), step.Name)
}

// classify reads the answer and asks the table what it means.
//
// `parsed` is C3's discriminant and is computed from the body this CLI could
// decode into the struct the operation answers with — not from "was it
// JSON". A 201 from execute whose body is a `StoredSimulation` is not a
// transaction with an empty status (A393).
func (r *runner) classify(p stepPlan, res api.Result) *answer {
	a := &answer{res: res, last: res, step: r.handle.Run().Step(p.name)}

	parsed := false
	status := ""

	if res.Transport == nil && res.Meta.Status >= 200 && res.Meta.Status < 300 {
		switch {
		case res.Meta.Status == 202:
			// A 202's body is a Command, not the operation's body. §5.7
			// takes it from here.
			if cmd, ok := poll.DecodeCommand(res.Body); ok {
				parsed = true
				a.poll = &poll.Result{Command: cmd}
			}

		case p.name == runs.StepSimulate:
			var sim api.Simulation
			if err := json.Unmarshal(res.Body, &sim); err == nil && sim.Object == "simulation" {
				parsed = true
				a.sim = &sim
			}

		case p.name == runs.StepExecute:
			var txn api.Transaction
			if err := json.Unmarshal(res.Body, &txn); err == nil && txn.Object == "transaction" {
				parsed = true
				a.txn = &txn
				status = txn.Status
			}
		}

		if parsed {
			a.body = append(json.RawMessage(nil), res.Body...)
		}
	}

	a.out = outcome.Classify(res.Input(parsed, status))

	return a
}

// record is PLAN §5.4 step 11.
//
// A failure here is the case C17 exists for: the request left, the answer
// came back, and the CLI cannot write down what it was. The error is local by
// its nature, and the root turns it into `pending`/6 because the flag is set
// — never exit 1, which would say "nothing was sent" about a request that
// was (AC69(b), M74).
func (r *runner) record(p stepPlan, a *answer) error {
	if fault.Armed(fault.RecordFailsAfterSend) {
		r.guard.Trip()
	}

	stored := &runs.Response{
		Status:     a.res.Meta.Status,
		Headers:    responseHeaders(a.res),
		Body:       a.res.Body,
		ReceivedAt: r.deps.now(),
	}

	if err := r.handle.Record(p.name, stored, ledgerOutcome(a.out)); err != nil {
		return render.Fault(err,
			"the answer to run %s's %s step could not be recorded: %v", r.runID(), p.name, err)
	}

	return nil
}

// followUp is PLAN §5.4 step 12: the three answers that are not the end.
//
// A `202`, a `503 UPSTREAM_BUSY` carrying a command id and a `409
// IDEMPOTENCY_KEY_IN_PROGRESS` all mean the same thing — FERRY holds a
// command and this CLI does not yet know how it ended. All three are followed
// by polling that command, and none of them by a second POST under a new key
// (AC52, AC53, M52, M68).
func (r *runner) followUp(ctx context.Context, p stepPlan, a *answer) error {
	commandID, delay, ok := pollTarget(a)
	if !ok {
		return nil
	}

	if r.opts.noWait {
		a.warnings = append(a.warnings,
			fmt.Sprintf("--no-wait: command %s was not followed to a terminal state", commandID))

		return nil
	}

	timeout := r.opts.timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}

	deps := poll.Deps{
		Client:      r.session(outcome.OpGetCommand).Client,
		Clock:       r.deps.Noun.Clock,
		Interrupted: r.deps.Interrupted,
	}

	if r.local.Mode == render.ModeText {
		deps.Progress = r.cmd.ErrOrStderr()
	}

	result, err := poll.Watch(ctx, deps, commandID, delay, poll.Now(deps.Clock).Add(timeout))
	if err != nil {
		return err
	}

	a.poll = &result
	a.out = result.Outcome

	if result.Unreadable {
		// The command's state is exactly what could not be read, so the
		// money question is open whatever the read's own class says (C3).
		a.out = pending(fmt.Sprintf(
			"The command could not be read while it was being followed, so its outcome is not established. "+
				"Follow it with `ferry commands watch %s`.", commandID))
	}

	if result.Body != nil {
		a.body = result.Body
	}

	if result.Last.Meta.Status != 0 || result.Last.Transport != nil {
		a.last = result.Last
	}

	if cmd := result.Command; cmd != nil && cmd.Result != nil {
		if cmd.Result.Body != nil {
			a.txn = cmd.Result.Body.Transaction
		}
	}

	// The state the loop stopped in is the run's answer, so the record must
	// carry it rather than the 202 that started the wait.
	return r.recordPoll(p, a)
}

// pollTarget decides whether an answer is followed, and after how long.
//
// The initial delay prefers the `Retry-After` header, because that is the
// server's instruction about this answer; the 202 body's
// `retry_after_seconds` is the fallback and 5 seconds the floor when neither
// said (§5.7 step 5).
func pollTarget(a *answer) (string, int, bool) {
	id := commandID(a.res)
	if id == "" {
		return "", 0, false
	}

	delay := 5
	if a.poll != nil && a.poll.Command != nil {
		delay = a.poll.Command.RetryDelaySeconds()
	}

	if a.res.Meta.RetryAfter != nil {
		delay = a.res.Meta.RetryDelaySeconds()
	}

	switch {
	case a.res.Meta.Status == 202:
		return id, delay, true

	case a.res.Envelope == nil:
		return "", 0, false

	case a.res.Envelope.Code == "UPSTREAM_BUSY", a.res.Envelope.Code == "IDEMPOTENCY_KEY_IN_PROGRESS":
		return id, delay, true
	}

	return "", 0, false
}

// recordPoll writes the state the loop stopped in over the answer that
// started it.
func (r *runner) recordPoll(p stepPlan, a *answer) error {
	if a.poll == nil || a.poll.Command == nil {
		return nil
	}

	stored := &runs.Response{
		Status:     a.last.Meta.Status,
		Headers:    responseHeaders(a.last),
		Body:       a.last.Body,
		ReceivedAt: r.deps.now(),
	}

	if err := r.handle.Record(p.name, stored, ledgerOutcome(a.out)); err != nil {
		return render.Fault(err,
			"the terminal state of run %s's %s step could not be recorded: %v", r.runID(), p.name, err)
	}

	return nil
}

// obtainConsent is C16. Every send of an execute request passes through here.
func (r *runner) obtainConsent(step *runs.Step, commandIsConsent bool) error {
	verdict, err := consent.Ask(consent.Request{
		Yes:              r.opts.yes,
		TTY:              r.deps.Noun.IsTTY(),
		CommandIsConsent: commandIsConsent,
		Out:              r.cmd.ErrOrStderr(),
		In:               r.deps.Noun.Stdin(),
		Interrupted:      r.deps.Interrupted,
		Question:         r.question(),
		Display:          func(w io.Writer) { r.displayPlan(w, step) },
		BeforePrompt: func() error {
			// Durable, before a byte of the prompt is written (AC72, V9,
			// M80). A process killed at the prompt leaves
			// awaiting_confirmation, which is evidence that a plan was
			// displayed and nobody answered — and which authorises
			// nothing.
			return r.handle.AwaitConfirmation(step.Name)
		},
		PromptShown: func() {
			if r.deps.PromptShown != nil {
				r.deps.PromptShown()
			}

			fault.Die(fault.AtPrompt)
		},
	})
	if err != nil {
		return render.Fault(err, "%v", err)
	}

	switch verdict {
	case consent.Granted:
		return nil

	case consent.Declined:
		if err := r.handle.Decline(step.Name); err != nil {
			return render.Fault(err, "the decline of run %s could not be recorded: %v", r.runID(), err)
		}

		return r.declined()

	default:
		return r.noConsent(step)
	}
}

// declined is what `n`, an EOF or a signal at the prompt reports.
//
// The class is `refused_fix`, exit 3: "refused locally, nothing sent,
// nothing spent". It is deliberately not `cli_fault`, exit 1 — a human
// answering `n` is the CLI working, and a wrapper that treats exit 1 as a
// bug to retry would retry a refusal.
func (r *runner) declined() error {
	return &render.Error{
		Out: flight.OutcomeFor(outcome.ClassRefusedFix, fmt.Sprintf(
			"Nothing was sent and nothing was spent. A plan is consumed only by an execute, "+
				"so simulate again when you are ready. `ferry runs show %s`.", r.runID())),
		Msg: fmt.Sprintf("declined: run %s was not executed and no execute request was sent", r.runID()),
	}
}

// noConsent is the headless refusal: nobody to ask, and the command did not
// carry consent.
//
// The exit code is the step's state and not a constant. An
// `awaiting_confirmation` step has sent nothing, so exit 1 — "nothing
// happened, fix it locally" — is true of it; a `pending` one may have sent,
// and saying "nothing happened" about that is what C17 forbids (AC73).
func (r *runner) noConsent(step *runs.Step) error {
	id := r.runID()

	if step.State.MayHaveSent() {
		return &render.Error{
			Out: pending(fmt.Sprintf(
				"Run %s's execute step is %s: a request under its key may already have reached FERRY. "+
					"Resend the recorded bytes with `ferry runs resume %s --yes` — the same key cannot "+
					"double-send, FERRY answers it with the outcome it already has.", id, step.State, id)),
			Msg: fmt.Sprintf(
				"run %s needs consent to resend its execute step and there is no terminal to ask at; nothing was sent", id),
		}
	}

	return &render.Error{
		Out: flight.OutcomeFor(outcome.ClassCLIFault, fmt.Sprintf(
			"Nothing was sent. Consent is given per invocation and is never stored, so this one has to carry it: "+
				"`ferry runs resume %s --yes` to execute, or `ferry runs decline %s` if it should not be.", id, id)),
		Msg: fmt.Sprintf("run %s is awaiting confirmation and there is no terminal to ask at", id),
	}
}

func (r *runner) question() string {
	if r.live {
		return "LIVE — execute? [y/N] "
	}

	return "Execute? [y/N] "
}

// ledgerOutcome converts a classification for the record.
func ledgerOutcome(oc outcome.Outcome) *runs.Outcome {
	return &runs.Outcome{
		Class:       string(oc.Class),
		ExitCode:    oc.Exit,
		Money:       string(oc.Money),
		SameKeySafe: oc.SameKeySafe,
		Next:        oc.Next,
	}
}

func pending(next string) outcome.Outcome {
	return flight.OutcomeFor(outcome.ClassPending, next)
}

// errInterrupted ends an invocation that a signal reached.
//
// It carries no outcome of its own. `internal/cli`'s wrapper classifies it
// against the in-flight flag, which is the only thing that knows whether
// "nothing was sent" is still true (C17) — and putting a class here would be
// a second place that decides it.
var errInterrupted = errors.New("interrupted")

func interrupted(ch <-chan struct{}) bool {
	if ch == nil {
		return false
	}

	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// commandID is the command an answer names: the header, or the detail when
// the header is absent (AC53).
func commandID(res api.Result) string {
	if res.Meta.CommandID != "" {
		return res.Meta.CommandID
	}

	if res.Envelope != nil {
		if id, ok := res.Envelope.Details["command_id"].(string); ok {
			return id
		}
	}

	return ""
}

// responseHeaders is the subset of an answer's headers the record keeps.
//
// The six `components.headers` of the contract and no others: a record that
// stored every header would store `Set-Cookie` and `Authorization` echoes
// from whatever proxy sits in front of FERRY.
func responseHeaders(res api.Result) map[string]string {
	out := map[string]string{}

	if res.Meta.CommandID != "" {
		out["Ferry-Command-Id"] = res.Meta.CommandID
	}

	if res.Meta.IdempotencyKey != "" {
		out["Idempotency-Key"] = res.Meta.IdempotencyKey
	}

	if res.Meta.Environment != "" {
		out["Ferry-Environment"] = res.Meta.Environment
	}

	if res.Meta.Replayed {
		out["Idempotency-Replayed"] = "true"
	}

	if res.Meta.RetryAfter != nil {
		out["Retry-After"] = fmt.Sprintf("%d", *res.Meta.RetryAfter)
	}

	return out
}

// argv records what was typed, for `runs list` and for a human reading a
// record weeks later.
//
// Flag values that look like a plan token are redacted. The record keeps the
// token in exactly two declared places (C6, V6) and `argv` is not one of
// them; `runs show` redacts those two on display, and a token that reached
// this field would slip past that.
func argv(cmd *cobra.Command) []string {
	if cmd == nil {
		return nil
	}

	out := strings.Fields(cmd.CommandPath())

	var flags []string

	cmd.Flags().Visit(func(f *pflag.Flag) {
		value := f.Value.String()
		if runs.ContainsPlanToken([]byte(value)) {
			value = runs.RedactedToken
		}

		flags = append(flags, "--"+f.Name+"="+value)
	})

	sort.Strings(flags)

	return append(append(out, cmd.Flags().Args()...), flags...)
}

func isLive(env *creds.Environment) bool { return env != nil && *env == creds.EnvLive }
