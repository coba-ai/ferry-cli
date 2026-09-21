package corridors

import (
	"io"

	"github.com/spf13/cobra"

	"github.com/kurenn/ferry-cli/internal/api"
	"github.com/kurenn/ferry-cli/internal/noun"
	"github.com/kurenn/ferry-cli/internal/outcome"
	"github.com/kurenn/ferry-cli/internal/render"
)

// getCommand is AC40.
//
// The id is passed to `api.Request.PathParams` exactly as the caller typed it,
// `->` and all, and `api.Expand` is what escapes it into the path. That
// division matters: the escaping belongs to the layer that owns the request
// target, and a noun that pre-escaped would double-encode the moment `Expand`
// did its job.
//
// A332: this cannot be asserted over HTTP. `net/url` escapes a raw `>` when it
// writes the request line, so `walletCrypto->bankUs` and `walletCrypto-%3EbankUs`
// put identical bytes on the wire and the fixture answers both. The assertion
// that AC40 rests on is on `api.Expand`'s return value, in `get_test.go`.
func getCommand(deps noun.Deps) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "get <id>",
		Short: "Read one corridor",
		Long: "Read one corridor by its composite id, such as walletCrypto->bankUs.\n\n" +
			"Quote the id in a shell: `>` is a redirection operator.",
		Args:          noun.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: false,
		RunE: func(cmd *cobra.Command, args []string) error {
			session, err := noun.Open(cmd, deps, outcome.OpGetCorridor)
			if err != nil {
				return err
			}

			res, err := session.Do(cmd.Context(), api.Request{PathParams: map[string]string{"id": args[0]}})
			if err != nil {
				return err
			}

			var corridor api.Corridor

			answer := noun.Classify(res, &corridor)
			answer.Text = func(w io.Writer) { render.Corridor(w, corridor) }

			return session.Report(cmd, answer)
		},
	}

	cmd.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return render.UsageFrom(err) })

	return cmd
}
