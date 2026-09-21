package corridors

import (
	"io"

	"github.com/spf13/cobra"

	"github.com/kurenn/ferry/cli/internal/api"
	"github.com/kurenn/ferry/cli/internal/noun"
	"github.com/kurenn/ferry/cli/internal/outcome"
	"github.com/kurenn/ferry/cli/internal/render"
)

func listCommand(deps noun.Deps) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List every corridor",
		Long: "List every corridor FERRY knows, with what each side allows.\n\n" +
			"`(not enumerated)` and `(none allowed)` are different answers: the first is\n" +
			"a corridor that names no restriction, the second one that allows nothing.",
		Args:          noun.NoArgs(),
		SilenceUsage:  true,
		SilenceErrors: false,
		RunE: func(cmd *cobra.Command, _ []string) error {
			session, err := noun.Open(cmd, deps, outcome.OpListCorridors)
			if err != nil {
				return err
			}

			res, err := session.Do(cmd.Context(), api.Request{})
			if err != nil {
				return err
			}

			var list List

			answer := noun.Classify(res, &list)
			answer.Text = func(w io.Writer) { render.CorridorList(w, list.Data) }

			return session.Report(cmd, answer)
		},
	}

	cmd.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return render.UsageFrom(err) })

	return cmd
}
