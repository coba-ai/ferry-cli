package keys

import (
	"io"
	"net/url"
	"sort"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/kurenn/ferry-cli/internal/api"
	"github.com/kurenn/ferry-cli/internal/noun"
	"github.com/kurenn/ferry-cli/internal/outcome"
	"github.com/kurenn/ferry-cli/internal/render"
)

// Statuses is the `status` parameter's vocabulary (§1.2.14).
var Statuses = []string{"active", "all"}

// MaxPages bounds `--all`.
//
// A server that answered `has_more: true` with the same cursor forever would
// otherwise loop until the process was killed, holding a credential open
// against an endpoint that is misbehaving. The bound is generous — 20 pages at
// the 100-row ceiling is 2000 keys — and the message says what happened, which
// an infinite loop does not.
const MaxPages = 20

func listCommand(deps noun.Deps) *cobra.Command {
	var (
		status string
		limit  int
		cursor string
		all    bool
	)

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List API keys",
		Long: "List API keys.\n\n" +
			"--env, --status, --limit and --cursor are passed to the API unchanged and\n" +
			"only when given. The next page is followed only under --all; otherwise the\n" +
			"cursor the answer carried is printed for you to pass back.",
		Args:          noun.NoArgs(),
		SilenceUsage:  true,
		SilenceErrors: false,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runList(cmd, deps, listFlags{
				status:    status,
				limit:     limit,
				cursor:    cursor,
				all:       all,
				setStatus: cmd.Flags().Changed("status"),
				setLimit:  cmd.Flags().Changed("limit"),
				setCursor: cmd.Flags().Changed("cursor"),
			})
		},
	}

	cmd.Flags().StringVar(&status, "status", "", "active or all")
	cmd.Flags().IntVar(&limit, "limit", 0, "how many keys per page (the API defaults to 20 and clamps to 1..100)")
	cmd.Flags().StringVar(&cursor, "cursor", "", "a cursor from a previous answer's next_cursor")
	cmd.Flags().BoolVar(&all, "all", false, "follow next_cursor until the last page")
	cmd.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return render.UsageFrom(err) })

	return cmd
}

type listFlags struct {
	status    string
	limit     int
	cursor    string
	all       bool
	setStatus bool
	setLimit  bool
	setCursor bool
}

// queryFor is AC39's pass-through, and it is a function of the flags alone.
//
// Exactly the parameters the caller set, with the values they typed. Nothing
// is defaulted in: `limit` has a server-side default of 20 (`pagination.rb:36-38`)
// and sending `limit=20` because the CLI knows that would make the CLI a
// second authority on a number the server owns — and a stale one the day the
// default changes.
//
// The test derives the expected query from the same flags it passed and
// compares against what the fixture logged, so the two sides of AC39's
// equality have independent authorities.
func queryFor(env string, f listFlags) (url.Values, error) {
	q := url.Values{}

	if env != "" {
		environment, err := noun.ParseEnvironment(env)
		if err != nil {
			return nil, err
		}

		q.Set("environment", string(environment))
	}

	if f.setStatus {
		if !knownStatus(f.status) {
			return nil, render.Usage("--status %q is not one of %v", f.status, Statuses)
		}

		q.Set("status", f.status)
	}

	if f.setLimit {
		if f.limit < 1 {
			return nil, render.Usage("--limit %d is not a positive count", f.limit)
		}

		q.Set("limit", strconv.Itoa(f.limit))
	}

	if f.setCursor {
		if f.cursor == "" {
			return nil, render.Usage("--cursor was given with no value")
		}

		q.Set("cursor", f.cursor)
	}

	return q, nil
}

func knownStatus(s string) bool {
	for _, known := range Statuses {
		if known == s {
			return true
		}
	}

	return false
}

func runList(cmd *cobra.Command, deps noun.Deps, f listFlags) error {
	session, err := noun.Open(cmd, deps, outcome.OpListAPIKeys)
	if err != nil {
		return err
	}

	query, err := queryFor(session.Globals.Env, f)
	if err != nil {
		return err
	}

	var (
		collected []api.APIKey
		page      List
		answer    noun.Answer
		pages     int
	)

	for {
		res, err := session.Do(cmd.Context(), api.Request{Query: query})
		if err != nil {
			return err
		}

		page = List{}
		answer = noun.Classify(res, &page)
		pages++

		if answer.Outcome.Exit != 0 {
			return session.Report(cmd, answer)
		}

		collected = append(collected, page.Data...)

		// AC39: the next page is followed only under `--all`, and only to a
		// cursor the answer itself carried. There is no arithmetic on a
		// cursor anywhere in this CLI — an opaque token that a client can
		// compute is a client that will compute a wrong one.
		if !f.all || !page.HasMore || page.NextCursor == nil || *page.NextCursor == "" {
			break
		}

		if pages >= MaxPages {
			return render.Fault(nil,
				"--all followed %d pages and the answer still says there are more; stopping rather than "+
					"looping. Narrow the query with --env or --status, or page by hand with --cursor %s.",
				pages, *page.NextCursor)
		}

		next := cloneQuery(query)
		next.Set("cursor", *page.NextCursor)
		query = next
	}

	body, err := encode(List{
		Object:     page.Object,
		Data:       collected,
		HasMore:    page.HasMore,
		NextCursor: page.NextCursor,
		Limit:      page.Limit,
	})
	if err != nil {
		return render.Fault(err, "the answer could not be re-encoded: %v", err)
	}

	answer.Body = body

	summary := render.Page{
		Returned:   len(collected),
		Limit:      page.Limit,
		HasMore:    page.HasMore,
		NextCursor: page.NextCursor,
		Followed:   f.all,
	}

	answer.Text = func(w io.Writer) {
		render.APIKeyList(w, collected)
		render.Pagination(w, summary)
	}

	return session.Report(cmd, answer)
}

func cloneQuery(q url.Values) url.Values {
	out := url.Values{}

	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}

	sort.Strings(keys)

	for _, k := range keys {
		for _, v := range q[k] {
			out.Add(k, v)
		}
	}

	return out
}
