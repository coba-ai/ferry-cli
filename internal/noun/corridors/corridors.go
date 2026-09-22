// Package corridors is `ferry corridors`: what FERRY will price, and between
// what.
//
// Two things here are load-bearing beyond the render. A corridor id is a
// composite — `walletCrypto->bankUs` (`corridors.rb:172`) — and the `>` has to
// survive into the request target as `%3E` (AC40); and `allowed_assets` and
// `allowed_networks` are `null` or `[]` and the two mean opposite things
// (C11). Both are places where a plausible-looking simplification changes what
// a caller believes they may send.
package corridors

import (
	"github.com/spf13/cobra"

	"github.com/coba-ai/ferry-cli/internal/api"
	"github.com/coba-ai/ferry-cli/internal/noun"
)

// Command builds `ferry corridors`.
func Command(deps noun.Deps) *cobra.Command {
	cmd := noun.NewCommand("corridors", "List and read corridors",
		"List the corridors FERRY knows, or read one by its composite id.\n\n"+
			"Either credential class is accepted.")

	cmd.AddCommand(listCommand(deps), getCommand(deps))

	return cmd
}

// List is the list envelope of `GET /v1/corridors`, with `Corridor` items.
//
// The corridors endpoint answers `{object, data}` with no pagination — every
// corridor FERRY knows fits in one answer — so this is `api.Collection` and
// not `api.List`, which is the paginated five-key envelope the key endpoints
// use. Both now live in `internal/api` and both are pinned to the shape the
// contract declares for them (A402).
type List = api.Collection[api.Corridor]
