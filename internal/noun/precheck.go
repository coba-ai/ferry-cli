// Package noun is the plumbing every `ferry <noun> <verb>` shares: the global
// flags, the local state a command opens before it sends anything, and the
// pre-check that refuses a request the profile cannot make (AC43, C15).
//
// The pre-check is the part worth reading twice. FERRY decides the credential
// class per endpoint, before the action and again in the policy (§1.2.1), and
// answers `403 WRONG_TOKEN_CLASS` for the wrong one. The CLI knows the same
// table, so it refuses locally — exit 3, zero requests, and a sentence saying
// which credential to get. That is not an optimisation. A `keys create` sent
// with an API key is a request that reaches FERRY's authentication layer
// carrying a live bearer token, for an answer this process could have computed
// from the token's own shape without a socket.
package noun

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/kurenn/ferry-cli/internal/api"
	"github.com/kurenn/ferry-cli/internal/creds"
	"github.com/kurenn/ferry-cli/internal/fsx"
	"github.com/kurenn/ferry-cli/internal/outcome"
	"github.com/kurenn/ferry-cli/internal/render"
	"github.com/kurenn/ferry-cli/internal/version"
	"github.com/kurenn/ferry-cli/internal/xdg"
)

// Requirements is the credential class each operation needs (§1.2.1, §5.2).
//
// Keyed by operation rather than by noun so that it can be held to
// `outcome.Operations` in both directions (`precheck_test.go`): a new
// operation with no row here would otherwise default to whatever the call site
// happened to pass, and the call site is exactly where the mistake is made.
//
// `get_command` takes an API key: `CommandsController` declares
// `accepts_principals :api_key` (`commands_controller.rb:32`).
var Requirements = map[outcome.Operation]creds.Requirement{
	outcome.OpGetMe:            creds.RequireEither,
	outcome.OpListAPIKeys:      creds.RequirePAT,
	outcome.OpCreateAPIKey:     creds.RequirePAT,
	outcome.OpGetAPIKey:        creds.RequirePAT,
	outcome.OpRevokeAPIKey:     creds.RequirePAT,
	outcome.OpListCorridors:    creds.RequireEither,
	outcome.OpGetCorridor:      creds.RequireEither,
	outcome.OpSimulateTransfer: creds.RequireAPIKey,
	outcome.OpExecuteTransfer:  creds.RequireAPIKey,
	outcome.OpGetCommand:       creds.RequireAPIKey,
}

// RequirementFor reports the class an operation needs.
//
// The second return is false for an operation with no row, and every caller
// must refuse rather than guess: "which credential does this endpoint accept"
// has no safe default, and picking one means sending a token to an endpoint
// nobody decided it may go to.
func RequirementFor(op outcome.Operation) (creds.Requirement, bool) {
	r, ok := Requirements[op]

	return r, ok
}

// Globals are PLAN §5.1's global flags.
//
// The struct is filled by [BindGlobals], which U5's root calls on the root
// command and this unit's tests call on the noun command they drive. One
// binder, so the flags a test parses are the flags production defines.
type Globals struct {
	Profile string

	// APIURL is `--api`, falling back to FERRY_API_URL. Empty means "use
	// whatever the profile is bound to"; a value that disagrees with the
	// profile is refused before connecting (C7).
	APIURL string

	// Env is `--env`. It is read three ways, all of them declared: an
	// assertion about a pasted token at `auth login` (AC34), the
	// `environment` parameter of the key endpoints (§5.1), and an assertion
	// about the stored credential on a money command (AC58, U5's).
	Env string

	Output      string
	Debug       bool
	NoColor     bool
	RetryBudget time.Duration
}

// BindGlobals declares the global flags on cmd's persistent flag set.
//
// There is deliberately no `--token`. AC33: a token on a command line is in
// the shell history and in the process table of every other user on the
// machine, and neither is revocable. The only ways in are `--token-stdin` and
// a TTY prompt.
func BindGlobals(cmd *cobra.Command, g *Globals) {
	f := cmd.PersistentFlags()

	f.StringVar(&g.Profile, "profile", creds.DefaultProfile, "the credential profile to use")
	f.StringVar(&g.APIURL, "api", "", "the FERRY API endpoint (also FERRY_API_URL)")
	f.StringVar(&g.Env, "env", "", "sandbox or live")
	f.StringVar(&g.Output, "output", string(render.ModeText), "text or json")
	f.BoolVar(&g.Debug, "debug", false, "write a redacted request/response trace to stderr")
	f.BoolVar(&g.NoColor, "no-color", false, "never colour output (also NO_COLOR)")
	f.DurationVar(&g.RetryBudget, "retry-budget", api.DefaultRetryBudget, "total wall time retries may take")
}

// Deps is what a command needs from outside itself.
//
// Every field has a production default and a seam a test can replace. The two
// that matter are [Deps.ReadSecret] — the echo-off terminal read, which is the
// whole of AC33's second half — and [Deps.Lookup], which is how a test states
// the environment it means rather than inheriting the developer's shell.
type Deps struct {
	Globals *Globals

	// FS is the filesystem the credential store is read and written
	// through. Nil is the real one.
	FS fsx.FS

	// Lookup reads an environment variable. Nil is os.Getenv.
	Lookup xdg.Env

	// HTTP is the transport. Nil is http.DefaultClient.
	HTTP *http.Client

	// Clock is the retry loop's clock. Nil is the real one.
	Clock api.Clock

	// Now is the wall clock a stored credential is timestamped with.
	Now func() time.Time

	// ReadSecret reads a secret from a terminal with echo off. Nil is
	// term.ReadPassword, which is the only reason this CLI depends on
	// golang.org/x/term at all.
	ReadSecret func(fd int) ([]byte, error)

	// IsTerminal reports whether a descriptor is a terminal. Nil is
	// term.IsTerminal.
	IsTerminal func(fd int) bool
}

// DefaultReadSecret is the production echo-off read.
//
// Named rather than inlined so a test can assert by function identity that the
// default really is term.ReadPassword. The harness puts its pty in raw mode
// before the command runs, so echo is already off in every in-process test and
// an assertion that watched the transcript for the typed token would pass
// whatever this did — the identity check is the one that bites.
var DefaultReadSecret = term.ReadPassword

// DefaultIsTerminal is the production TTY test.
var DefaultIsTerminal = term.IsTerminal

func (d Deps) resolve() Deps {
	if d.Globals == nil {
		d.Globals = &Globals{Profile: creds.DefaultProfile, Output: string(render.ModeText)}
	}

	if d.FS == nil {
		d.FS = fsx.OS()
	}

	if d.Lookup == nil {
		d.Lookup = xdg.OSEnv
	}

	if d.HTTP == nil {
		d.HTTP = http.DefaultClient
	}

	if d.Now == nil {
		d.Now = func() time.Time { return time.Now().UTC() }
	}

	if d.ReadSecret == nil {
		d.ReadSecret = DefaultReadSecret
	}

	if d.IsTerminal == nil {
		d.IsTerminal = DefaultIsTerminal
	}

	return d
}

// Local is the CLI's own state: where it lives and what it holds. No socket is
// opened to build one.
type Local struct {
	Deps    Deps
	Globals *Globals
	Mode    render.Mode
	Paths   xdg.Paths
	Store   *creds.Store
	File    *creds.File

	// RequestedAPIURL is `--api` or FERRY_API_URL, normalised, or "" when
	// the caller named neither.
	RequestedAPIURL string
}

// OpenLocal resolves the paths, the output mode and the credentials file.
//
// It is separate from [Open] because `auth login` runs before there is a
// credential to open: it is the one command whose whole job is to create one.
func OpenLocal(d Deps) (*Local, error) {
	d = d.resolve()

	mode, err := render.ParseMode(d.Globals.Output)
	if err != nil {
		return nil, err
	}

	paths, err := xdg.Resolve(d.Lookup)
	if err != nil {
		return nil, render.Fault(err, "%v", err)
	}

	store := creds.NewStore(d.FS, paths.CredentialsFile())

	file, err := store.Load()
	if err != nil {
		if errors.Is(err, creds.ErrPermissionsTooWide) {
			return nil, render.Fault(err, "%v", err)
		}

		return nil, render.Fault(err, "the credentials file could not be read: %v", err)
	}

	requested := d.Globals.APIURL
	if requested == "" {
		requested = d.Lookup("FERRY_API_URL")
	}

	if requested != "" {
		normalised, err := creds.NormalizeAPIURL(requested)
		if err != nil {
			return nil, render.Usage("the API endpoint %q is not usable: %v", requested, err)
		}

		requested = normalised
	}

	return &Local{
		Deps:            d,
		Globals:         d.Globals,
		Mode:            mode,
		Paths:           paths,
		Store:           store,
		File:            file,
		RequestedAPIURL: requested,
	}, nil
}

// Profile is the profile this invocation names.
func (l *Local) Profile() string {
	if l.Globals.Profile == "" {
		return creds.DefaultProfile
	}

	return l.Globals.Profile
}

// Session is a Local with a credential chosen and a client built. Nothing has
// been sent: the first request leaves in [Session.Do].
type Session struct {
	*Local

	Op     outcome.Operation
	Match  creds.Match
	Client *api.Client
}

// Open is the pre-check (AC43, C15).
//
// The order is what makes "zero requests" true by construction rather than by
// discipline: every refusal below is computed from the operation, the token's
// shape and the file on disk, and the client that could send anything is built
// on the last line.
func Open(cmd *cobra.Command, d Deps, op outcome.Operation) (*Session, error) {
	local, err := OpenLocal(d)
	if err != nil {
		return nil, err
	}

	return local.Session(cmd, op)
}

// Session chooses the credential for an operation and builds the client.
func (l *Local) Session(cmd *cobra.Command, op outcome.Operation) (*Session, error) {
	req, ok := RequirementFor(op)
	if !ok {
		return nil, render.Fault(nil,
			"this CLI has no credential rule for the operation %q, so it will not send it. "+
				"Every operation needs a row in noun.Requirements (PLAN §5.2).", op)
	}

	file := l.File
	if override, err := l.tokenOverride(); err != nil {
		return nil, err
	} else if override != nil {
		file = override
	}

	match, err := file.For(l.Profile(), req, l.RequestedAPIURL)
	if err != nil {
		return nil, l.refuseCredential(req, err)
	}

	client := &api.Client{
		BaseURL:         match.APIURL,
		Token:           match.Credential.Token,
		UserAgentString: version.UserAgent(),
		Environment:     environmentOf(match),
		HTTP:            l.Deps.HTTP,
		Clock:           l.Deps.Clock,
		RetryBudget:     l.Globals.RetryBudget,
	}

	if l.Globals.Debug {
		client.Debug = cmd.ErrOrStderr()
	}

	return &Session{Local: l, Op: op, Match: match, Client: client}, nil
}

func environmentOf(m creds.Match) string {
	if m.Credential.Environment == nil {
		return ""
	}

	return string(*m.Credential.Environment)
}

// tokenOverride applies FERRY_TOKEN (§5.2): ephemeral, never written, and it
// replaces the slot of its own class rather than every slot — a PAT in the
// environment does not make a `transfers` command send a PAT.
func (l *Local) tokenOverride() (*creds.File, error) {
	raw := strings.TrimSpace(l.Deps.Lookup("FERRY_TOKEN"))
	if raw == "" {
		return nil, nil
	}

	if _, err := creds.Classify(raw); err != nil {
		return nil, render.Usage(
			"FERRY_TOKEN is set but is not a FERRY token: it matches neither `ferry_sk_<env>_…` nor `ferry_pat_…`")
	}

	apiURL := l.RequestedAPIURL
	if apiURL == "" {
		if p, ok := l.File.Profiles[l.Profile()]; ok {
			apiURL = p.APIURL
		}
	}

	if apiURL == "" {
		return nil, render.Usage(
			"FERRY_TOKEN is set but no API endpoint is configured; pass `--api` or set `FERRY_API_URL`")
	}

	// Copy rather than mutate: FERRY_TOKEN is ephemeral and must not reach
	// the file, and the cheapest way to guarantee that is for the document
	// the store would save never to have held it.
	clone := creds.NewFile()
	for name, p := range l.File.Profiles {
		clone.Profiles[name] = p
	}

	if err := clone.Put(l.Profile(), apiURL, creds.Credential{Token: raw, StoredAt: l.Deps.Now()}); err != nil {
		return nil, render.Usage("FERRY_TOKEN could not be used: %v", err)
	}

	return clone, nil
}

// refuseCredential turns a store error into the refusal a human can act on.
//
// Every branch is exit 3 or exit 1 and none of them has sent anything. The
// remediation names the credential class by the words `auth login` and `keys
// create` use, not by the internal spelling: "pat" is not a thing anybody
// types.
func (l *Local) refuseCredential(req creds.Requirement, err error) error {
	switch {
	case errors.Is(err, creds.ErrNoProfile):
		// `remediationFor` returns a complete sentence, which is what the
		// branch below needs and what this one used to wrap in "Run
		// `ferry auth login` with a … first." The result, for a command
		// that accepts either class, was: "Run `ferry auth login --api
		// <url>` with a Store either credential with `ferry auth login
		// --api <url> --token-stdin`. first." (A419)
		//
		// The wrapper is gone rather than the remediation reworded into a
		// noun phrase: every remediation already names the login command,
		// so the prefix was saying it twice even where it parsed.
		return render.Refused(
			"no credentials are stored for profile %q.\n%s",
			l.Profile(), remediationFor(req))

	case errors.Is(err, creds.ErrNoCredentialOfClass):
		return render.Refused(
			"this endpoint accepts %s and profile %q holds none.\n%s",
			req, l.Profile(), remediationFor(req))

	case errors.Is(err, creds.ErrAPIURLMismatch):
		return render.Refused("%v.\nRun `ferry auth login --api <url>` for that endpoint, or use a different --profile.", err)

	default:
		return render.Fault(err, "the credential for this command could not be read: %v", err)
	}
}

func remediationFor(req creds.Requirement) string {
	switch req {
	case creds.RequirePAT:
		return "Mint a personal access token with `bin/rails \"ferry:pat:issue[…]\"` and store it with " +
			"`ferry auth login --api <url> --token-stdin`; there is no endpoint that mints one."

	case creds.RequireAPIKey:
		return "Store an API key with `ferry keys create --env <kind> --name <name> --scopes <scopes> --login`, " +
			"or `ferry auth login --api <url> --token-stdin` with a `ferry_sk_` token."

	case creds.RequireEither:
		return "Store either credential with `ferry auth login --api <url> --token-stdin`."

	default:
		return "Store a credential with `ferry auth login --api <url> --token-stdin`."
	}
}

// Do sends the request.
//
// The operation comes from the session, so the request cannot be for an
// operation other than the one the pre-check cleared.
func (s *Session) Do(ctx context.Context, req api.Request) (api.Result, error) {
	req.Op = s.Op

	res, err := s.Client.Do(ctx, req)
	if err != nil {
		if errors.Is(err, api.ErrUsage) {
			return api.Result{}, render.Usage("%v", err)
		}

		return api.Result{}, render.Fault(err, "the request could not be sent: %v", err)
	}

	return res, nil
}

// Answer is one classified response and how to render it.
type Answer struct {
	Result  api.Result
	Outcome outcome.Outcome

	// Body is the decoded response, re-encoded for the JSON document. Nil
	// leaves `response` null.
	Body json.RawMessage

	// Text renders the success case in text mode. It is not called when the
	// outcome's exit code is non-zero: there is no body worth printing, and
	// the message on the returned error is what the caller needs.
	Text func(w io.Writer)
}

// Classify turns a result into an answer, decoding the 2xx body into out.
//
// `parsed` is what C3 turns on: a 2xx whose body this CLI cannot read is not a
// success. For a read that is exit 5 and another try; the money path, which is
// not this unit's, makes it exit 6.
func Classify[T any](res api.Result, out *T) Answer {
	parsed := false

	var body json.RawMessage

	if res.Transport == nil && res.Meta.Status >= 200 && res.Meta.Status < 300 {
		if err := json.Unmarshal(res.Body, out); err == nil {
			parsed = true
			body = append(json.RawMessage(nil), res.Body...)
		}
	}

	return Answer{
		Result:  res,
		Outcome: outcome.Classify(res.Input(parsed, "")),
		Body:    body,
	}
}

// Report writes the answer and returns the error carrying its exit code.
//
// One document in JSON mode, on every path, including the refusals — which is
// why the document is built here rather than in each verb.
func (s *Session) Report(cmd *cobra.Command, a Answer) error {
	return s.Local.ReportRemote(cmd, a)
}

// ReportRemote writes an answer for a command that did send a request but does
// not hold a [Session] — `auth login`, which builds its own client because it
// is the command that runs before there is a credential to open one with.
//
// The distinction from [Local.ReportLocal] is not cosmetic: it decides whether
// the `http` block appears in the JSON document and whether a failure is
// described as "FERRY answered 401 TOKEN_INVALID" or as "refused before it was
// sent". Reporting a real 401 as the latter would tell an operator their
// command line was wrong when their token was.
func (l *Local) ReportRemote(cmd *cobra.Command, a Answer) error {
	return l.report(cmd, &a.Result, a)
}

// ReportLocal writes an answer for a command that sent nothing.
func (l *Local) ReportLocal(cmd *cobra.Command, a Answer) error {
	return l.report(cmd, nil, a)
}

func (l *Local) report(cmd *cobra.Command, res *api.Result, a Answer) error {
	if l.Mode == render.ModeJSON {
		doc := render.Document{
			FerryCLI: render.CLIBlock{
				Version:     version.Get().Version,
				Profile:     l.Profile(),
				Environment: environmentBlock(l.Globals.Env),
			},
			Outcome:  render.NewOutcomeBlock(a.Outcome),
			Response: a.Body,
		}

		if res != nil && res.Transport == nil {
			doc.HTTP = render.NewHTTPBlock(*res)
		}

		if res != nil && res.Envelope != nil {
			doc.Error = errorBlock(res.Body)
		}

		if err := render.WriteDocument(cmd.OutOrStdout(), doc); err != nil {
			return err
		}
	} else if a.Outcome.Exit == 0 && a.Text != nil {
		a.Text(cmd.OutOrStdout())
	}

	if a.Outcome.Exit == 0 {
		for _, warning := range a.Outcome.Warnings {
			if l.Mode == render.ModeText {
				render.Errorf(cmd.ErrOrStderr(), "warning: %s", warning)
			}
		}

		return nil
	}

	return render.FromOutcome(a.Outcome, "%s", describe(res))
}

func environmentBlock(env string) *string {
	if env == "" {
		return nil
	}

	return &env
}

// errorBlock lifts the `error` object out of the envelope body verbatim.
//
// Verbatim because `details` is still `type: object` in the contract (A321):
// re-encoding it through a Go type would drop whatever this CLI's structs do
// not name, and `details` is where `reason`, `command_id`, `state` and the
// field list live.
func errorBlock(body []byte) json.RawMessage {
	var outer struct {
		Error json.RawMessage `json:"error"`
	}

	if err := json.Unmarshal(body, &outer); err != nil {
		return nil
	}

	return outer.Error
}

func describe(res *api.Result) string {
	if res == nil {
		return "the request was refused before it was sent"
	}

	if res.Transport != nil {
		return fmt.Sprintf("no answer arrived (%s): %v", res.Transport.Phase, res.Transport.Err)
	}

	if res.Envelope != nil {
		return fmt.Sprintf("FERRY answered %d %s: %s", res.Meta.Status, res.Envelope.Code, res.Envelope.Message)
	}

	if res.EnvelopeErr != nil {
		return fmt.Sprintf("FERRY answered %d with a body that is not the error envelope: %v",
			res.Meta.Status, res.EnvelopeErr)
	}

	return fmt.Sprintf("FERRY answered %d", res.Meta.Status)
}

// Stdin is the descriptor a prompt reads from.
//
// It is read through os.Stdin rather than cmd.InOrStdin() because the echo-off
// read needs a file descriptor, and the harness swaps the process streams. A
// command that prompted on cmd.InOrStdin() would be reading a pipe the harness
// never made a terminal.
func (d Deps) Stdin() *os.File { return os.Stdin }

// IsTTY reports whether stdin is a terminal.
func (d Deps) IsTTY() bool {
	d = d.resolve()

	return d.IsTerminal(int(d.Stdin().Fd()))
}

// ReadAllStdin reads the whole of a command's stdin.
func ReadAllStdin(cmd *cobra.Command) (string, error) {
	raw, err := io.ReadAll(cmd.InOrStdin())
	if err != nil {
		return "", render.Fault(err, "stdin could not be read: %v", err)
	}

	return string(raw), nil
}

// Resolve exposes the resolved dependencies to the noun packages.
func (d Deps) Resolve() Deps { return d.resolve() }

// NewCommand builds a noun command with this CLI's error conventions.
//
// The flag error function is the load-bearing part. Cobra's own flag errors
// carry no exit code, so an unknown flag — `--token`, say (AC33, M35) — would
// exit 1, the code that means "a local problem, fix it and run again". Exit 2
// is what a wrapper reads as "the command line was wrong", and the difference
// is the difference between retrying and stopping.
func NewCommand(use, short, long string) *cobra.Command {
	cmd := &cobra.Command{
		Use:           use,
		Short:         short,
		Long:          long,
		SilenceUsage:  true,
		SilenceErrors: false,
	}

	cmd.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return render.UsageFrom(err) })

	return cmd
}

// ExactArgs is cobra.ExactArgs with an exit code.
func ExactArgs(n int) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if len(args) != n {
			return render.Usage("%s takes exactly %d argument(s), got %d\n\nUsage:\n  %s",
				cmd.CommandPath(), n, len(args), cmd.UseLine())
		}

		return nil
	}
}

// NoArgs is cobra.NoArgs with an exit code.
func NoArgs() cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if len(args) > 0 {
			return render.Usage("%s takes no arguments, got %q\n\nUsage:\n  %s",
				cmd.CommandPath(), strings.Join(args, " "), cmd.UseLine())
		}

		return nil
	}
}

// Environments is the `--env` vocabulary.
var Environments = []creds.Environment{creds.EnvSandbox, creds.EnvLive}

// ParseEnvironment validates `--env`.
func ParseEnvironment(raw string) (creds.Environment, error) {
	for _, e := range Environments {
		if string(e) == raw {
			return e, nil
		}
	}

	names := make([]string, 0, len(Environments))
	for _, e := range Environments {
		names = append(names, string(e))
	}

	sort.Strings(names)

	return "", render.Usage("--env %q is not one of %s", raw, strings.Join(names, ", "))
}
