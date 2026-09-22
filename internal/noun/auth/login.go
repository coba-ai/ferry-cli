package auth

import (
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/coba-ai/ferry-cli/internal/api"
	"github.com/coba-ai/ferry-cli/internal/creds"
	"github.com/coba-ai/ferry-cli/internal/noun"
	"github.com/coba-ai/ferry-cli/internal/outcome"
	"github.com/coba-ai/ferry-cli/internal/render"
	"github.com/coba-ai/ferry-cli/internal/version"
)

// TokenStdinFlag is the only flag that carries a token into this process.
//
// AC33: there is no `--token <value>`, ever. A value on the command line is in
// `~/.bash_history` and in `/proc/<pid>/cmdline`, readable by every other
// process on the machine for as long as this one runs, and neither copy can be
// revoked. `login_test.go` asserts the whole flag set of this command in both
// directions, so a `--token` cannot be added without a test going red.
const TokenStdinFlag = "token-stdin"

func loginCommand(deps noun.Deps) *cobra.Command {
	var fromStdin bool

	cmd := &cobra.Command{
		Use:   "login",
		Short: "Verify a token against GET /v1/me and store it",
		Long: "Read a FERRY token from stdin or a terminal prompt, verify it against\n" +
			"GET /v1/me at the given endpoint, and store it only if that answers 200.\n\n" +
			"There is no --token flag: a token on the command line is in the shell\n" +
			"history and in the process table.",
		Args:          noun.NoArgs(),
		SilenceUsage:  true,
		SilenceErrors: false,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runLogin(cmd, deps, fromStdin)
		},
	}

	cmd.Flags().BoolVar(&fromStdin, TokenStdinFlag, false, "read the token from stdin instead of prompting")
	cmd.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return render.UsageFrom(err) })

	return cmd
}

// runLogin is AC33, AC34 and AC35, in the order that makes "zero requests"
// true for the two refusals that claim it.
//
// Nothing below sends until step 5. Steps 1–4 are the endpoint, the token, the
// token's shape and the `--env` assertion, all of them decidable from this
// process's own inputs — which is the point: a token of the wrong environment
// must not be presented to a server before being rejected, because presenting
// it is what puts it in that server's logs.
func runLogin(cmd *cobra.Command, deps noun.Deps, fromStdin bool) error {
	deps = deps.Resolve()

	local, err := noun.OpenLocal(deps)
	if err != nil {
		return err
	}

	// 1. The endpoint. AC35: neither `--api` nor FERRY_API_URL is exit 2
	// with this sentence and no request. No API URL is compiled into the
	// binary (§8 D-4), so there is nothing to fall back to and guessing one
	// would send a credential somewhere its owner never named.
	if local.RequestedAPIURL == "" {
		return render.Usage("no API endpoint configured; pass `--api` or set `FERRY_API_URL`")
	}

	// 2. The token.
	token, err := readToken(cmd, deps, fromStdin)
	if err != nil {
		return err
	}

	// 3. Its class and environment, from its shape alone (§1.2.2).
	class, err := creds.Classify(token)
	if err != nil {
		return render.Usage(
			"that token matches neither `ferry_sk_<sandbox|live>_…` nor `ferry_pat_…`, so this CLI will not send it")
	}

	tokenEnv, err := creds.EnvironmentOf(token)
	if err != nil {
		return render.Usage("that token's environment could not be read: %v", err)
	}

	// 4. The `--env` assertion (AC34).
	if err := assertEnvironment(local.Globals.Env, class, tokenEnv); err != nil {
		return err
	}

	// 5. The first and only request.
	client := &api.Client{
		BaseURL:         local.RequestedAPIURL,
		Token:           token,
		UserAgentString: version.UserAgent(),
		Environment:     environmentString(tokenEnv),
		HTTP:            deps.HTTP,
		Clock:           deps.Clock,
		RetryBudget:     local.Globals.RetryBudget,
	}

	if local.Globals.Debug {
		client.Debug = cmd.ErrOrStderr()
	}

	res, err := client.Do(cmd.Context(), api.Request{Op: outcome.OpGetMe})
	if err != nil {
		return render.Fault(err, "the request could not be sent: %v", err)
	}

	var principal api.Principal

	answer := noun.Classify(res, &principal)

	// 6. Store only on a 200 (AC35). `Classify` has already established that
	// the body decoded; an answer this CLI could not read is not a
	// verification, and storing on it would bind a credential to an endpoint
	// on the strength of a status line.
	if answer.Outcome.Exit == 0 && res.Meta.Status == 200 {
		if err := store(local, token, principal); err != nil {
			return err
		}

		answer.Text = func(w io.Writer) {
			fmt.Fprintf(w, "stored %s in profile %s for %s\n\n",
				creds.Prefix(token), local.Profile(), local.RequestedAPIURL)
			render.Principal(w, principal)
		}
	}

	return local.ReportRemote(cmd, answer)
}

// readToken is AC33's whole surface: stdin, or a terminal prompt with echo
// off. There is no third way in.
func readToken(cmd *cobra.Command, deps noun.Deps, fromStdin bool) (string, error) {
	if fromStdin {
		raw, err := noun.ReadAllStdin(cmd)
		if err != nil {
			return "", err
		}

		token := strings.TrimSpace(raw)
		if token == "" {
			return "", render.Usage("--%s was given and stdin was empty", TokenStdinFlag)
		}

		return token, nil
	}

	if !deps.IsTTY() {
		return "", render.Usage(
			"stdin is not a terminal, so there is nothing to prompt; pass --%s and pipe the token in", TokenStdinFlag)
	}

	fmt.Fprint(cmd.ErrOrStderr(), "FERRY token (input hidden): ")

	raw, err := deps.ReadSecret(int(deps.Stdin().Fd()))

	fmt.Fprintln(cmd.ErrOrStderr())

	if err != nil {
		return "", render.Fault(err, "the token could not be read from the terminal: %v", err)
	}

	token := strings.TrimSpace(string(raw))
	if token == "" {
		return "", render.Usage("no token was typed")
	}

	return token, nil
}

// assertEnvironment is AC34.
//
// Two refusals with two different exit codes, and the difference is real. A
// PAT with `--env` is a command line that cannot be satisfied — a personal
// access token spans its organization's environments and asserting one of them
// about it is meaningless, so it is exit 2, "you typed something that has no
// reading". An API key of the other environment is a well-formed command whose
// answer is no: exit 3, "nothing was sent, fix the credential".
func assertEnvironment(flag string, class creds.Class, tokenEnv *creds.Environment) error {
	if flag == "" {
		return nil
	}

	want, err := noun.ParseEnvironment(flag)
	if err != nil {
		return err
	}

	if class == creds.ClassPAT {
		return render.Usage(
			"--env %s was given with a personal access token, which belongs to no single environment; drop --env",
			want)
	}

	if tokenEnv == nil || *tokenEnv != want {
		return render.Refused(
			"--env %s was asserted and the token is a %s key; nothing was sent.\n"+
				"Paste the %s key, or drop --env.",
			want, environmentString(tokenEnv), want)
	}

	return nil
}

func environmentString(e *creds.Environment) string {
	if e == nil {
		return "(no environment)"
	}

	return string(*e)
}

// store writes the verified credential (AC35).
//
// The four facts the record carries are `api_url`, `token_prefix`,
// `environment` and `principal_id`. Three of them are derived by `creds.Put`
// from the token's own shape and the URL this invocation proved the token
// against; the fourth is `credential.id` from the answer, which is what a
// later `runs resume` compares to decide whether the credential that started a
// run is the one presenting now (C19).
func store(local *noun.Local, token string, principal api.Principal) error {
	cred := creds.Credential{
		Token:       token,
		PrincipalID: principal.Credential.ID,
		StoredAt:    local.Deps.Now(),
	}

	if err := local.File.Put(local.Profile(), local.RequestedAPIURL, cred); err != nil {
		return render.Fault(err, "the credential could not be stored: %v", err)
	}

	if err := local.Paths.EnsureAll(local.Deps.FS); err != nil {
		return render.Fault(err, "%v", err)
	}

	if err := local.Store.Save(local.File); err != nil {
		return render.Fault(err, "%v", err)
	}

	return nil
}
