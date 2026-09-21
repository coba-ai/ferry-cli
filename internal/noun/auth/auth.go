// Package auth is `ferry auth`: storing a credential an operator already
// minted, saying which ones are stored, and removing them.
//
// There is no endpoint that mints a personal access token (§1.2.15) and no
// login endpoint at all. `auth login` verifies a pasted token against `GET
// /v1/me` and stores it only if that answers 200 (AC35) — which is what makes
// the stored record's `api_url` mean something: the credential is bound to the
// endpoint it was proved to work against, and to no other (C7).
package auth

import (
	"github.com/spf13/cobra"

	"github.com/kurenn/ferry/cli/internal/noun"
)

// Command builds `ferry auth`.
func Command(deps noun.Deps) *cobra.Command {
	cmd := noun.NewCommand("auth", "Store, show and remove FERRY credentials",
		"Store, show and remove the credentials this CLI sends.\n\n"+
			"A token is never accepted on the command line: it would be in the shell\n"+
			"history and in the process table. Use --token-stdin or a terminal prompt.")

	cmd.AddCommand(loginCommand(deps), whoamiCommand(deps), logoutCommand(deps))

	return cmd
}
