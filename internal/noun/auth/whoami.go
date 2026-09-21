package auth

import (
	"encoding/json"
	"io"

	"github.com/spf13/cobra"

	"github.com/kurenn/ferry/cli/internal/creds"
	"github.com/kurenn/ferry/cli/internal/noun"
	"github.com/kurenn/ferry/cli/internal/outcome"
	"github.com/kurenn/ferry/cli/internal/render"
)

// whoamiCommand is AC36's first half.
//
// It sends nothing. "Which credentials does this profile hold" is a question
// about this machine, and both slots are the answer — `GET /v1/me` could only
// speak for whichever one was presented, so a whoami that called it would
// answer a narrower question than the one asked. The pre-check still runs,
// because a profile holding neither credential has no answer to give and
// should say so in the vocabulary every other command uses (C15).
func whoamiCommand(deps noun.Deps) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "whoami",
		Short: "Show the credentials stored for this profile",
		Long: "Show both credential slots of a profile: the token prefix and last four\n" +
			"characters, the environment, and the principal each was verified as.\n\n" +
			"Sends nothing. The stored token itself is never printed.",
		Args:          noun.NoArgs(),
		SilenceUsage:  true,
		SilenceErrors: false,
		RunE: func(cmd *cobra.Command, _ []string) error {
			local, err := noun.OpenLocal(deps)
			if err != nil {
				return err
			}

			// RequireEither, through the same table every other command
			// uses, so "this profile holds nothing" is exit 3 with the
			// remediation rather than an empty render that looks like an
			// answer.
			if _, err := local.Session(cmd, outcome.OpGetMe); err != nil {
				return err
			}

			profile := local.File.Profiles[local.Profile()]

			done, ok := outcome.Properties(outcome.ClassDone)
			if !ok {
				return render.Fault(nil, "the outcome table declares no properties for `done`")
			}

			body, err := slotsDocument(local.Profile(), profile)
			if err != nil {
				return err
			}

			return local.ReportLocal(cmd, noun.Answer{
				Outcome: done,
				Body:    body,
				Text:    func(w io.Writer) { render.Slots(w, local.Profile(), profile) },
			})
		},
	}

	cmd.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return render.UsageFrom(err) })

	return cmd
}

// slotsJSON is what `--output json` says about a profile.
//
// A hand-built document rather than `creds.Profile` marshalled directly: that
// struct's `token` field is the token, and a renderer that marshalled it would
// write a bearer credential to stdout. The shape here carries the prefix and
// the last four and has nowhere to put the rest — which is a stronger
// guarantee than remembering to omit a field.
type slotsJSON struct {
	Profile string    `json:"profile"`
	APIURL  string    `json:"api_url"`
	APIKey  *slotJSON `json:"api_key"`
	PAT     *slotJSON `json:"pat"`

	// Stored and Missing are the two halves of the same census, both
	// rendered: a caller scripting against this should not have to infer an
	// absence from a null.
	Stored  []string `json:"stored"`
	Missing []string `json:"missing"`
}

type slotJSON struct {
	TokenPrefix string  `json:"token_prefix"`
	TokenLast4  string  `json:"token_last4"`
	Environment *string `json:"environment"`
	PrincipalID string  `json:"principal_id"`
	StoredAt    string  `json:"stored_at"`
}

func slotsDocument(name string, p creds.Profile) (json.RawMessage, error) {
	doc := slotsJSON{Profile: name, APIURL: p.APIURL, Stored: []string{}, Missing: []string{}}

	if p.APIKey != nil {
		doc.APIKey = slotOf(*p.APIKey)
		doc.Stored = append(doc.Stored, string(creds.ClassAPIKey))
	} else {
		doc.Missing = append(doc.Missing, string(creds.ClassAPIKey))
	}

	if p.PAT != nil {
		doc.PAT = slotOf(*p.PAT)
		doc.Stored = append(doc.Stored, string(creds.ClassPAT))
	} else {
		doc.Missing = append(doc.Missing, string(creds.ClassPAT))
	}

	raw, err := json.Marshal(doc)
	if err != nil {
		return nil, render.Fault(err, "the profile could not be encoded: %v", err)
	}

	return raw, nil
}

func slotOf(c creds.Credential) *slotJSON {
	var env *string

	if c.Environment != nil {
		s := string(*c.Environment)
		env = &s
	}

	return &slotJSON{
		TokenPrefix: c.TokenPrefix,
		TokenLast4:  c.Last4(),
		Environment: env,
		PrincipalID: c.PrincipalID,
		StoredAt:    c.StoredAt.Format("2006-01-02T15:04:05Z07:00"),
	}
}
