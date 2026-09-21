package keys

import (
	"io"

	"github.com/spf13/cobra"

	"github.com/kurenn/ferry/cli/internal/api"
	"github.com/kurenn/ferry/cli/internal/noun"
	"github.com/kurenn/ferry/cli/internal/outcome"
	"github.com/kurenn/ferry/cli/internal/render"
)

func revokeCommand(deps noun.Deps) *cobra.Command {
	var reason string

	cmd := &cobra.Command{
		Use:   "revoke <id>",
		Short: "Revoke an API key",
		Long: "Revoke an API key.\n\n" +
			"Revocation is idempotent: revoking an already-revoked key answers 200 with\n" +
			"the key as it stands, so this command is safe to repeat.",
		Args:          noun.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: false,
		RunE: func(cmd *cobra.Command, args []string) error {
			session, err := noun.Open(cmd, deps, outcome.OpRevokeAPIKey)
			if err != nil {
				return err
			}

			// `reason` is optional and omitted when the caller gave none:
			// `RevokeApiKeyRequest` declares it optional, and sending
			// `{"reason": ""}` would record an empty reason as though one
			// had been given.
			raw, err := api.Marshal(api.RevokeAPIKeyRequest{Reason: reason})
			if err != nil {
				return render.Fault(err, "the request body could not be encoded: %v", err)
			}

			res, err := session.Do(cmd.Context(), api.Request{
				PathParams: map[string]string{"id": args[0]},
				Body:       raw,
			})
			if err != nil {
				return err
			}

			var key api.APIKey

			answer := noun.Classify(res, &key)
			answer.Text = func(w io.Writer) { render.APIKey(w, key) }

			return session.Report(cmd, answer)
		},
	}

	cmd.Flags().StringVar(&reason, "reason", "", "why the key is being revoked")
	cmd.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return render.UsageFrom(err) })

	return cmd
}
