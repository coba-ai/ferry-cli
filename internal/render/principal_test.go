package render_test

import (
	"strings"
	"testing"

	"github.com/coba-ai/ferry-cli/internal/api"
	"github.com/coba-ai/ferry-cli/internal/render"
)

// AC35's note: the API renders `user` and `role` as null and `environments` as
// `[]` for an API key, and the reverse for a PAT. The render branches on
// `kind` and on nothing else.
//
// The evidence is a principal with the *wrong* fields populated for its kind.
// A renderer that decided from "which field happens to be set" would show the
// PAT block for the first case below and the API-key block for the second; one
// that reads `kind` shows the block the kind names and ignores the rest. Both
// directions, so neither a missing block nor an extra one passes.
//
// §6.2 names no mutation for this; the one this unit ran (U4-2 in the PR body)
// branches on `p.User != nil` instead of on `kind`, and both rows redden.
func TestThePrincipalRenderBranchesOnKindAndNotOnWhichFieldIsSet(t *testing.T) {
	role := "owner"

	cases := []struct {
		name      string
		principal api.Principal
		want      []string
		notWant   []string
	}{
		{
			name: "an api key whose user and role are populated anyway",
			principal: api.Principal{
				Kind:         render.KindAPIKey,
				Organization: api.PrincipalOrganization{ID: "org_1", Name: "Org", Slug: "org", Status: "active"},
				Environment:  &api.PrincipalEnvironment{ID: "env_1", Kind: "sandbox", Status: "active"},
				User:         &api.PrincipalUser{ID: "usr_1", Email: "someone@example.test"},
				Role:         &role,
				Environments: []api.PrincipalEnvironment{{ID: "env_1", Kind: "sandbox"}},
				Credential: api.PrincipalCredential{
					ID: "key_1", Name: "k", TokenPrefix: "ferry_sk_sandbox_ABCDEFGH", TokenLast4: "WXYZ",
					Scopes: []string{"read"},
				},
			},
			want:    []string{"kind:", "api_key", "environment:", "sandbox"},
			notWant: []string{"someone@example.test", "role:", "environments:"},
		},
		{
			name: "a pat whose environment is populated anyway",
			principal: api.Principal{
				Kind:         render.KindPAT,
				Organization: api.PrincipalOrganization{ID: "org_1", Name: "Org", Slug: "org", Status: "active"},
				Environment:  &api.PrincipalEnvironment{ID: "env_1", Kind: "sandbox", Status: "active"},
				User:         &api.PrincipalUser{ID: "usr_1", Email: "someone@example.test"},
				Role:         &role,
				Environments: []api.PrincipalEnvironment{{ID: "env_1", Kind: "sandbox"}, {ID: "env_2", Kind: "live"}},
				Credential: api.PrincipalCredential{
					ID: "pat_1", Name: "t", TokenPrefix: "ferry_pat_ABCDEFGH", TokenLast4: "WXYZ",
					Scopes: []string{"read", "keys:manage"},
				},
			},
			want:    []string{"personal_access_token", "someone@example.test", "role:", "owner", "environments:"},
			notWant: []string{"environment: "},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out strings.Builder

			render.Principal(&out, tc.principal)

			got := out.String()

			if got == "" {
				t.Fatal("the render is empty, so every assertion below is vacuous")
			}

			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Errorf("the render omits %q:\n%s", want, got)
				}
			}

			for _, unwanted := range tc.notWant {
				if strings.Contains(got, unwanted) {
					t.Errorf("the render shows %q, which belongs to the other kind:\n%s", unwanted, got)
				}
			}

			if render.ContainsSecret(got) {
				t.Errorf("the principal render carries secret-shaped text:\n%s", got)
			}
		})
	}
}

// A kind neither constant names is reported as unrecognised rather than
// guessed at, because guessing renders fields FERRY did not answer with.
func TestAnUnrecognisedKindIsSaidToBeUnrecognised(t *testing.T) {
	var out strings.Builder

	render.Principal(&out, api.Principal{Kind: "service_account"})

	got := out.String()

	if !strings.Contains(got, "service_account") {
		t.Errorf("the render does not name the kind FERRY sent:\n%s", got)
	}

	if !strings.Contains(got, "does not recognise") {
		t.Errorf("the render presents an unknown kind as though it understood it:\n%s", got)
	}
}

// `[]` and a populated list are different answers here too: an API key's
// `environments` is `[]` and a PAT's is populated, and the words differ.
func TestAnEmptyEnvironmentListIsNotTheSameAsAnAbsentOne(t *testing.T) {
	var absent, empty strings.Builder

	render.Principal(&absent, api.Principal{Kind: render.KindPAT})
	render.Principal(&empty, api.Principal{Kind: render.KindPAT, Environments: []api.PrincipalEnvironment{}})

	if absent.String() == empty.String() {
		t.Error("a null `environments` and an empty one render identically; C11 says they are " +
			"different answers")
	}

	if !strings.Contains(empty.String(), render.EmptyList) {
		t.Errorf("an empty list does not render as %q:\n%s", render.EmptyList, empty.String())
	}

	if !strings.Contains(absent.String(), render.NullList) {
		t.Errorf("an absent list does not render as %q:\n%s", render.NullList, absent.String())
	}
}
