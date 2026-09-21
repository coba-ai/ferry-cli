package keys

import (
	"io"

	"github.com/spf13/cobra"

	"github.com/kurenn/ferry-cli/internal/api"
	"github.com/kurenn/ferry-cli/internal/noun"
	"github.com/kurenn/ferry-cli/internal/outcome"
	"github.com/kurenn/ferry-cli/internal/render"
)

func getCommand(deps noun.Deps) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "get <id>",
		Short: "Read one API key",
		Long: "Read one API key.\n\n" +
			"The answer carries no token: FERRY returns the plaintext key on the create\n" +
			"201 only, and there is no endpoint that will return it again.",
		Args:          noun.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: false,
		RunE: func(cmd *cobra.Command, args []string) error {
			session, err := noun.Open(cmd, deps, outcome.OpGetAPIKey)
			if err != nil {
				return err
			}

			res, err := session.Do(cmd.Context(), api.Request{PathParams: map[string]string{"id": args[0]}})
			if err != nil {
				return err
			}

			var key api.APIKey

			answer := noun.Classify(res, &key)
			answer.Text = func(w io.Writer) { render.APIKey(w, key) }

			return session.Report(cmd, answer)
		},
	}

	cmd.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return render.UsageFrom(err) })

	return cmd
}
