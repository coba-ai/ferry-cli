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

	"github.com/kurenn/ferry/cli/internal/api"
	"github.com/kurenn/ferry/cli/internal/noun"
)

// Command builds `ferry keys`.
func Command(deps noun.Deps) *cobra.Command {
	cmd := noun.NewCommand("keys", "List, read, mint and revoke API keys",
		"List, read, mint and revoke API keys.\n\n"+
			"These endpoints accept a personal access token only.")

	cmd.AddCommand(listCommand(deps), getCommand(deps), createCommand(deps), revokeCommand(deps))

	return cmd
}

// List is the `List` envelope of `GET /v1/api_keys`.
//
// Declared here rather than in `internal/api` because that package (U2)
// declares no list envelope: AC85 points `List.data.items` at `ApiKey` on the
// Rails side, and U2b wrote the five item schemas but not the wrapper. The
// item type is `api.APIKey`, so nothing about the key itself is re-declared.
type List struct {
	Object string       `json:"object"`
	Data   []api.APIKey `json:"data"`

	// HasMore and NextCursor are the two halves of the same fact and both
	// are read: a cursor is followed only when `has_more` says there is
	// another page, and it is never constructed (AC39).
	HasMore    bool    `json:"has_more"`
	NextCursor *string `json:"next_cursor"`
	Limit      int     `json:"limit"`
}

func encode(v any) (json.RawMessage, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}

	return raw, nil
}
