package auth

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/kurenn/ferry/cli/internal/creds"
	"github.com/kurenn/ferry/cli/internal/noun"
	"github.com/kurenn/ferry/cli/internal/outcome"
	"github.com/kurenn/ferry/cli/internal/render"
)

// Classes is the `--class` vocabulary, in `creds`'s spelling.
//
// "pat" and not "personal_access_token": the recordings use the long form
// because FERRY does, and the store uses the short one. Two vocabularies for
// one idea is a thing to name rather than to paper over — this flag is the
// store's, and `fixture` translates at its own boundary.
var Classes = []creds.Class{creds.ClassAPIKey, creds.ClassPAT}

// logoutCommand is AC36's second half: remove the named slot, or both.
func logoutCommand(deps noun.Deps) *cobra.Command {
	var class string

	cmd := &cobra.Command{
		Use:   "logout",
		Short: "Remove stored credentials",
		Long: "Remove one credential slot from a profile, or the whole profile when no\n" +
			"--class is given.\n\nSends nothing. FERRY is not told: a token removed here is still\n" +
			"valid until it is revoked, which for an API key is `ferry keys revoke`.",
		Args:          noun.NoArgs(),
		SilenceUsage:  true,
		SilenceErrors: false,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runLogout(cmd, deps, class)
		},
	}

	cmd.Flags().StringVar(&class, "class", "", "api_key or pat; omitted, both are removed")
	cmd.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return render.UsageFrom(err) })

	return cmd
}

func runLogout(cmd *cobra.Command, deps noun.Deps, class string) error {
	local, err := noun.OpenLocal(deps)
	if err != nil {
		return err
	}

	var (
		selected *creds.Class
		removed  []string
	)

	if class != "" {
		parsed, err := parseClass(class)
		if err != nil {
			return err
		}

		selected = &parsed
	}

	profile, ok := local.File.Profiles[local.Profile()]
	if !ok {
		return render.Refused("profile %q holds no credentials; nothing to remove", local.Profile())
	}

	switch {
	case selected == nil:
		if profile.APIKey != nil {
			removed = append(removed, string(creds.ClassAPIKey))
		}

		if profile.PAT != nil {
			removed = append(removed, string(creds.ClassPAT))
		}

	case *selected == creds.ClassAPIKey && profile.APIKey != nil:
		removed = append(removed, string(creds.ClassAPIKey))

	case *selected == creds.ClassPAT && profile.PAT != nil:
		removed = append(removed, string(creds.ClassPAT))
	}

	if err := local.File.Remove(local.Profile(), selected); err != nil {
		return render.Fault(err, "%v", err)
	}

	if err := local.Store.Save(local.File); err != nil {
		return render.Fault(err, "%v", err)
	}

	done, ok := outcome.Properties(outcome.ClassDone)
	if !ok {
		return render.Fault(nil, "the outcome table declares no properties for `done`")
	}

	if removed == nil {
		removed = []string{}
	}

	body, err := json.Marshal(struct {
		Profile string   `json:"profile"`
		Removed []string `json:"removed"`
	}{Profile: local.Profile(), Removed: removed})
	if err != nil {
		return render.Fault(err, "the result could not be encoded: %v", err)
	}

	return local.ReportLocal(cmd, noun.Answer{
		Outcome: done,
		Body:    body,
		Text: func(w io.Writer) {
			if len(removed) == 0 {
				fmt.Fprintf(w, "profile %s held no matching credential; nothing removed\n", local.Profile())

				return
			}

			fmt.Fprintf(w, "removed %s from profile %s\n", strings.Join(removed, " and "), local.Profile())
		},
	})
}

func parseClass(raw string) (creds.Class, error) {
	for _, c := range Classes {
		if string(c) == raw {
			return c, nil
		}
	}

	names := make([]string, 0, len(Classes))
	for _, c := range Classes {
		names = append(names, string(c))
	}

	sort.Strings(names)

	return "", render.Usage("--class %q is not one of %s", raw, strings.Join(names, ", "))
}
