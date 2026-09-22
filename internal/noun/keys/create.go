package keys

import (
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/coba-ai/ferry-cli/internal/api"
	"github.com/coba-ai/ferry-cli/internal/creds"
	"github.com/coba-ai/ferry-cli/internal/noun"
	"github.com/coba-ai/ferry-cli/internal/outcome"
	"github.com/coba-ai/ferry-cli/internal/render"
)

func createCommand(deps noun.Deps) *cobra.Command {
	var (
		name      string
		scopes    string
		expiresAt string
		expiresIn string
		login     bool
	)

	cmd := &cobra.Command{
		Use:   "create",
		Short: "Mint an API key",
		Long: "Mint an API key and print its token once.\n\n" +
			"The token is printed on stdout and nowhere else: FERRY returns it on this\n" +
			"response only and has no endpoint that will return it again. This command\n" +
			"is never retried automatically — an answer that did not arrive may still\n" +
			"have minted a key, and a second attempt would mint a second one.",
		Args:          noun.NoArgs(),
		SilenceUsage:  true,
		SilenceErrors: false,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runCreate(cmd, deps, createFlags{
				name:      name,
				scopes:    scopes,
				expiresAt: expiresAt,
				expiresIn: expiresIn,
				login:     login,
				setAt:     cmd.Flags().Changed("expires-at"),
				setIn:     cmd.Flags().Changed("expires-in"),
			})
		},
	}

	cmd.Flags().StringVar(&name, "name", "", "the key's name (required)")
	cmd.Flags().StringVar(&scopes, "scopes", "", "comma-separated scopes (required)")
	cmd.Flags().StringVar(&expiresAt, "expires-at", "", "an RFC3339 instant the key expires at")
	cmd.Flags().StringVar(&expiresIn, "expires-in", "", "a relative lifetime such as 30d, 12h or 4w")
	cmd.Flags().BoolVar(&login, "login", false, "store the minted key in this profile")
	cmd.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return render.UsageFrom(err) })

	return cmd
}

type createFlags struct {
	name      string
	scopes    string
	expiresAt string
	expiresIn string
	login     bool
	setAt     bool
	setIn     bool
}

func runCreate(cmd *cobra.Command, deps noun.Deps, f createFlags) error {
	deps = deps.Resolve()

	session, err := noun.Open(cmd, deps, outcome.OpCreateAPIKey)
	if err != nil {
		return err
	}

	body, err := buildBody(session.Globals.Env, f, deps.Now())
	if err != nil {
		return err
	}

	raw, err := api.Marshal(body)
	if err != nil {
		return render.Fault(err, "the request body could not be encoded: %v", err)
	}

	res, err := session.Do(cmd.Context(), api.Request{Body: raw})
	if err != nil {
		return err
	}

	var key api.APIKey

	answer := noun.Classify(res, &key)

	// The 201 is the only answer that carries a token, and `--login` stores
	// it before anything is printed: a store that failed after the token
	// scrolled past would leave the operator holding a credential they
	// believe is saved.
	if answer.Outcome.Exit == 0 && f.login {
		if err := storeMinted(session, key); err != nil {
			return err
		}
	}

	answer.Text = func(w io.Writer) {
		render.APIKeyCreated(w, key)

		if f.login {
			fmt.Fprintf(w, "\nstored in profile %s for %s\n", session.Profile(), session.Match.APIURL)
		}
	}

	return session.Report(cmd, answer)
}

// buildBody is AC37: exactly the keys the caller supplied, and no others.
//
// `name`, `environment` and `scopes` are the endpoint's required trio
// (`api_keys_controller.rb:29`) and are always present because the flags are
// required. `expires_at` is present only when the caller named a lifetime.
// That is not cosmetic: `POST /v1/api_keys` refuses an unknown key outright
// (`base_controller.rb:530-543`) and an explicit `"expires_at": null` is a
// different request from one that omits it — the first asserts "no expiry",
// the second asks the server for its default.
func buildBody(env string, f createFlags, now time.Time) (api.CreateAPIKeyRequest, error) {
	if strings.TrimSpace(f.name) == "" {
		return api.CreateAPIKeyRequest{}, render.Usage("--name is required")
	}

	if env == "" {
		return api.CreateAPIKeyRequest{}, render.Usage("--env is required: sandbox or live")
	}

	environment, err := noun.ParseEnvironment(env)
	if err != nil {
		return api.CreateAPIKeyRequest{}, err
	}

	scopes, err := parseScopes(f.scopes)
	if err != nil {
		return api.CreateAPIKeyRequest{}, err
	}

	body := api.CreateAPIKeyRequest{
		Name:        f.name,
		Environment: string(environment),
		Scopes:      scopes,
	}

	switch {
	case f.setAt && f.setIn:
		return api.CreateAPIKeyRequest{}, render.Usage("--expires-at and --expires-in are two ways to say the same thing; pass one")

	case f.setAt:
		if _, err := time.Parse(time.RFC3339, f.expiresAt); err != nil {
			return api.CreateAPIKeyRequest{}, render.Usage("--expires-at %q is not an RFC3339 instant: %v", f.expiresAt, err)
		}

		body.ExpiresAt = f.expiresAt

	case f.setIn:
		d, err := parseLifetime(f.expiresIn)
		if err != nil {
			return api.CreateAPIKeyRequest{}, err
		}

		body.ExpiresAt = now.Add(d).UTC().Format(time.RFC3339)
	}

	return body, nil
}

// parseScopes validates against the contract's enum (`CreateApiKeyRequest.scopes`).
//
// Refusing a scope the document does not declare is refusing a request FERRY
// would refuse anyway — but locally, with the list in the message, rather than
// as a `400` whose `details.fields` a human has to read. `keys:manage` is
// deliberately not in the enum: it is control-plane only and cannot be granted
// to an API key.
func parseScopes(raw string) ([]string, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, render.Usage("--scopes is required: %s", strings.Join(api.Scopes, ", "))
	}

	var (
		out  []string
		seen = map[string]bool{}
		bad  []string
	)

	for _, part := range strings.Split(raw, ",") {
		scope := strings.TrimSpace(part)
		if scope == "" {
			continue
		}

		if !knownScope(scope) {
			bad = append(bad, scope)

			continue
		}

		if seen[scope] {
			continue
		}

		seen[scope] = true

		out = append(out, scope)
	}

	if len(bad) > 0 {
		sort.Strings(bad)

		return nil, render.Usage("--scopes names %s, which the contract does not declare; the scopes are %s",
			strings.Join(bad, ", "), strings.Join(api.Scopes, ", "))
	}

	if len(out) == 0 {
		return nil, render.Usage("--scopes is required: %s", strings.Join(api.Scopes, ", "))
	}

	return out, nil
}

func knownScope(scope string) bool {
	for _, s := range api.Scopes {
		if s == scope {
			return true
		}
	}

	return false
}

// parseLifetime reads `30d`, `12h`, `4w`.
//
// `time.ParseDuration` has no day or week, and a CLI whose longest expressible
// key lifetime is `720h` is one whose users type the wrong number of hours.
// Days are 24 hours and weeks are seven days: neither crosses a DST boundary
// in a way this CLI could resolve, and the value is sent as an absolute
// instant, so the arithmetic happens once, here, where it can be read.
func parseLifetime(raw string) (time.Duration, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return 0, render.Usage("--expires-in is empty")
	}

	unit := trimmed[len(trimmed)-1]

	var scale time.Duration

	switch unit {
	case 'd':
		scale = 24 * time.Hour
	case 'w':
		scale = 7 * 24 * time.Hour
	default:
		d, err := time.ParseDuration(trimmed)
		if err != nil {
			return 0, render.Usage("--expires-in %q is not a lifetime; use 30d, 4w, 12h or 90m", raw)
		}

		if d <= 0 {
			return 0, render.Usage("--expires-in %q is not in the future", raw)
		}

		return d, nil
	}

	n, err := strconv.Atoi(trimmed[:len(trimmed)-1])
	if err != nil {
		return 0, render.Usage("--expires-in %q is not a lifetime; use 30d, 4w, 12h or 90m", raw)
	}

	if n <= 0 {
		return 0, render.Usage("--expires-in %q is not in the future", raw)
	}

	return time.Duration(n) * scale, nil
}

// storeMinted is `--login`.
//
// The key is bound to the endpoint the PAT was already bound to, which
// `creds.Put` enforces: a profile holds one API URL, and a key minted through
// one deployment is not a credential for another.
func storeMinted(session *noun.Session, key api.APIKey) error {
	if key.Token == nil {
		return mintedButUnstored(key, nil, "the 201 carried no token")
	}

	cred := creds.Credential{
		Token:       *key.Token,
		PrincipalID: key.ID,
		StoredAt:    session.Deps.Now(),
	}

	if err := session.File.Put(session.Profile(), session.Match.APIURL, cred); err != nil {
		return mintedButUnstored(key, err, "the credential store refused it")
	}

	if err := session.Paths.EnsureAll(session.Deps.FS); err != nil {
		return mintedButUnstored(key, err, "the credential directory could not be created")
	}

	if err := session.Store.Save(session.File); err != nil {
		return mintedButUnstored(key, err, "the credentials file could not be written")
	}

	return nil
}

// mintedButUnstored is the one sentence every `--login` failure has to say.
//
// The key exists. `POST /v1/api_keys` already returned `201`, so whatever went
// wrong here went wrong after FERRY minted a credential, and an error that
// only says "could not store" leaves an operator believing the command did
// nothing. It names the id so the key can be revoked, and it does not print
// the token: a failure path is not one of AC42's two declared sites, and a
// token echoed into an error message is a token in whatever collects them.
func mintedButUnstored(key api.APIKey, cause error, why string) error {
	return render.Fault(cause,
		"the key was minted and --login could not store it: %s.\n"+
			"The key exists — `ferry keys list` shows it and `ferry keys revoke %s` removes it.\n"+
			"Its token is not printed here and is not recoverable; revoke this one and mint another.",
		why, key.ID)
}
