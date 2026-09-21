// Package keys is `ferry keys`: the API-key control plane.
//
// Every endpoint here accepts a personal access token and refuses an API key
// (`api_keys_controller.rb:27`), which is what keeps revocation meaningful — a
// key cannot mint its successor or revoke its sibling. The pre-check knows
// that table and refuses locally (AC43).
//
// `keys create` is the one command in this CLI that prints a secret and the
// one that is never retried. Both follow from the same fact: FERRY hands the
// plaintext key out exactly once, on the `201`, and has no endpoint that will
// return it again. An answer that does not arrive may still have minted a key,
// so a second attempt is a second credential nobody asked for (AC38).
package keys

import (
	"encoding/json"

	"github.com/spf13/cobra"

	"github.com/kurenn/ferry-cli/internal/api"
	"github.com/kurenn/ferry-cli/internal/noun"
)

// Command builds `ferry keys`.
func Command(deps noun.Deps) *cobra.Command {
	cmd := noun.NewCommand("keys", "List, read, mint and revoke API keys",
		"List, read, mint and revoke API keys.\n\n"+
			"These endpoints accept a personal access token only.")

	cmd.AddCommand(listCommand(deps), getCommand(deps), createCommand(deps), revokeCommand(deps))

	return cmd
}

// List is the `List` envelope of `GET /v1/api_keys`, with `ApiKey` items.
//
// The envelope itself now lives in `internal/api` and is pinned to the
// `List` schema in both directions (A402); this alias is what the verbs
// here read, so nothing about the envelope or the key is declared twice.
type List = api.List[api.APIKey]

func encode(v any) (json.RawMessage, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}

	return raw, nil
}
