package render

import (
	"fmt"
	"io"
	"strings"

	"github.com/coba-ai/ferry-cli/internal/api"
	"github.com/coba-ai/ferry-cli/internal/creds"
)

// The two `kind` values `GET /v1/me` answers with
// (`principal_serializer.rb`). They are the discriminant, and the only
// discriminant: an API key renders `user`, `role` as null and `environments`
// as `[]`; a personal access token renders `environment` as null and populates
// the other three. Branching on "which field happens to be set" would work
// today and would break the first time FERRY filled one of them in for the
// other class.
const (
	KindAPIKey = "api_key"
	KindPAT    = "personal_access_token"
)

// Principal writes what `GET /v1/me` answered.
func Principal(w io.Writer, p api.Principal) {
	var f fields

	f.add("kind", principalKind(p.Kind))
	f.add("organization", orgLine(p.Organization))

	switch p.Kind {
	case KindAPIKey:
		f.add("environment", environmentLine(p.Environment))
	case KindPAT:
		f.add("user", userLine(p.User))
		f.add("role", stringOr(p.Role, None))
		f.add("environments", environmentsLine(p.Environments))
	default:
		// An unrecognised kind is reported rather than guessed at. The
		// alternative is to pick one of the two branches and render the
		// fields it happens to have, which reads as though FERRY answered
		// something it did not.
		f.add("environment", environmentLine(p.Environment))
		f.add("user", userLine(p.User))
		f.add("role", stringOr(p.Role, None))
		f.add("environments", environmentsLine(p.Environments))
	}

	f.add("credential", credentialLine(p.Credential))
	f.add("scopes", sortedScopes(p.Credential.Scopes))
	f.add("expires at", orNone(p.Credential.ExpiresAt))
	f.add("last used", stringOr(p.Credential.LastUsedAt, "never"))

	f.write(w, "")
}

func principalKind(kind string) string {
	switch kind {
	case KindAPIKey:
		return "api_key (an API key: money endpoints, one environment)"
	case KindPAT:
		return "personal_access_token (a PAT: key management, all environments)"
	default:
		return fmt.Sprintf("%s (a kind this CLI does not recognise)", kind)
	}
}

func orgLine(o api.PrincipalOrganization) string {
	return fmt.Sprintf("%s (%s, %s, %s)", orNone(o.Name), orNone(o.Slug), orNone(o.ID), orNone(o.Status))
}

func environmentLine(e *api.PrincipalEnvironment) string {
	if e == nil {
		return None
	}

	return fmt.Sprintf("%s (%s, %s)", orNone(e.Kind), orNone(e.ID), orNone(e.Status))
}

func environmentsLine(envs []api.PrincipalEnvironment) string {
	// `[]` for an API key and populated for a PAT: the distinction C11
	// protects on the corridor body applies here too.
	if envs == nil {
		return NullList
	}

	if len(envs) == 0 {
		return EmptyList
	}

	parts := make([]string, 0, len(envs))
	for _, e := range envs {
		parts = append(parts, environmentLine(&e))
	}

	return strings.Join(parts, "; ")
}

func userLine(u *api.PrincipalUser) string {
	if u == nil {
		return None
	}

	return fmt.Sprintf("%s (%s)", orNone(u.Email), orNone(u.ID))
}

// credentialLine identifies the presented credential without printing it.
//
// `token_prefix` and `token_last4` are what FERRY itself renders
// (`principal_serializer.rb`); there is no `token` on any `GET /v1/me`
// response, so there is nothing here that could leak even by accident.
func credentialLine(c api.PrincipalCredential) string {
	return fmt.Sprintf("%s…%s (%s, %s)", orNone(c.TokenPrefix), orNone(c.TokenLast4), orNone(c.Name), orNone(c.ID))
}

func orNone(s string) string {
	if s == "" {
		return None
	}

	return s
}

// Slots writes both credential slots of a profile (AC36).
//
// Prefix and last four only. `creds.Credential.Display` is the only form of a
// stored token this CLI prints and it is built from those two fields; the
// token itself never reaches this function's output, which is half of what
// `secrets_test.go` measures.
func Slots(w io.Writer, profileName string, p creds.Profile) {
	fmt.Fprintf(w, "profile %s\n", profileName)

	var f fields

	f.add("api url", orNone(p.APIURL))
	f.write(w, "  ")

	slot(w, "api_key", p.APIKey)
	slot(w, "pat", p.PAT)
}

func slot(w io.Writer, name string, c *creds.Credential) {
	fmt.Fprintf(w, "  %s\n", name)

	if c == nil {
		fmt.Fprintf(w, "    %s\n", "not stored")

		return
	}

	var f fields

	f.add("credential", c.Display())
	f.add("environment", environmentOrNone(c.Environment))
	f.add("principal", orNone(c.PrincipalID))
	f.add("stored at", c.StoredAt.Format("2006-01-02T15:04:05Z07:00"))
	f.write(w, "    ")
}

func environmentOrNone(e *creds.Environment) string {
	// A PAT's environment is null and that is not "unknown": a personal
	// access token spans its organization's environments rather than being
	// bound to one (§5.2).
	if e == nil {
		return "(all — a personal access token is not bound to one)"
	}

	return string(*e)
}
