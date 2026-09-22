package keys_test

import (
	"encoding/json"
	"net/url"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/coba-ai/ferry-cli/internal/api"
	"github.com/coba-ai/ferry-cli/internal/fixture"
	"github.com/coba-ai/ferry-cli/internal/noun/keys"
)

// ---------------------------------------------------------------------------
// AC39 — --env, --status, --limit and --cursor are passed through unchanged
// and only when given; --all follows next_cursor.
// ---------------------------------------------------------------------------

// The pass-through, asserted as an equality between the query the caller asked
// for and the query the server received.
//
// The expected side is built here from the same flags the invocation passed,
// rather than copied from the implementation, so the two have independent
// authorities. A334: the comparison is over `url.Values` and not the raw
// target, because `url.Values.Encode` sorts keys and the order a caller typed
// them in is not preserved.
//
// The mutation U4-4 defaults `limit=20` in when the caller gave none; the
// first row is what reddens. §6.2's M40 is the sibling defect — constructing a
// cursor rather than echoing one — and is caught by
// TestAllFollowsTheCursorTheAnswerCarried below.
func TestTheListQueryIsExactlyWhatTheCallerAskedFor(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want url.Values
	}{
		{
			name: "nothing given sends nothing",
			args: nil,
			want: url.Values{},
		},
		{
			name: "--limit alone",
			args: []string{"--limit", "2"},
			want: url.Values{"limit": {"2"}},
		},
		{
			name: "--env alone",
			args: []string{"--env", "sandbox"},
			want: url.Values{"environment": {"sandbox"}},
		},
		{
			name: "--status alone",
			args: []string{"--status", "all"},
			want: url.Values{"status": {"all"}},
		},
		{
			name: "--cursor alone",
			args: []string{"--cursor", "CURSOR_PLACEHOLDER_1"},
			want: url.Values{"cursor": {"CURSOR_PLACEHOLDER_1"}},
		},
		{
			name: "all four together",
			args: []string{"--env", "live", "--status", "active", "--limit", "7", "--cursor", "abc"},
			want: url.Values{
				"environment": {"live"},
				"status":      {"active"},
				"limit":       {"7"},
				"cursor":      {"abc"},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// page1 is loaded so the one matching row can be answered; the
			// rest are 599s, which is fine — the assertion is on the
			// request, which the fixture logs either way (AC31).
			server, home := loggedIn(t, "keys.list.page1.200")

			args := append([]string{"list", "--api", server.URL()}, tc.args...)

			run(t, invocation{home: home, args: args})

			if n := requestCount(server); n != 1 {
				t.Fatalf("the fixture saw %d requests, want 1", n)
			}

			got := server.Requests()[0].Query

			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("the query sent is %v and the caller asked for %v", got, tc.want)
			}
		})
	}
}

// The parameter vocabulary, held to equality so a fifth one cannot be added
// silently — and so a defaulted value would show up as an extra key.
func TestNoParameterIsSentThatTheCallerDidNotName(t *testing.T) {
	server, home := loggedIn(t, "keys.list.page1.200")

	run(t, invocation{home: home, args: []string{"list", "--api", server.URL(), "--limit", "2"}})

	query := server.Requests()[0].Query

	got := make([]string, 0, len(query))
	for k := range query {
		got = append(got, k)
	}

	sort.Strings(got)

	if !reflect.DeepEqual(got, []string{"limit"}) {
		t.Errorf("the query carries %v; only `limit` was named", got)
	}
}

// `--all` follows the cursor the answer carried, and only that one.
//
// The two recordings are a real sequence: page1 was recorded at `?limit=2` and
// page2 at `?limit=2&cursor=CURSOR_PLACEHOLDER_1`, so a CLI that invented a
// cursor, or that reused the first query, would be answered 599 by the second
// request rather than quietly getting the same page twice.
//
// Mutation M40 constructs the cursor instead of echoing the one the answer
// carried; the second request's query is what reddens.
func TestAllFollowsTheCursorTheAnswerCarried(t *testing.T) {
	server, home := loggedIn(t, "keys.list.page1.200", "keys.list.page2.200")

	stdout, stderr, exit := run(t, invocation{
		home: home,
		args: []string{"list", "--api", server.URL(), "--limit", "2", "--all", "--output", "json"},
	})

	if exit != 0 {
		t.Fatalf("exit %d, want 0\nstdout: %s\nstderr: %s", exit, stdout, stderr)
	}

	requests := server.Requests()
	if len(requests) != 2 {
		t.Fatalf("the fixture saw %d requests, want 2", len(requests))
	}

	if got := requests[0].Query; !reflect.DeepEqual(got, url.Values{"limit": {"2"}}) {
		t.Errorf("the first page asked for %v, want limit=2 alone", got)
	}

	want := url.Values{"limit": {"2"}, "cursor": {"CURSOR_PLACEHOLDER_1"}}
	if got := requests[1].Query; !reflect.DeepEqual(got, want) {
		t.Errorf("the second page asked for %v, want %v", got, want)
	}

	for i, req := range requests {
		if !req.Matched() {
			t.Errorf("request %d (%s %s) matched no recording", i, req.Method, req.Path)
		}
	}

	if remaining := server.Remaining(); len(remaining) > 0 {
		t.Errorf("recordings were left unplayed: %v", remaining)
	}

	// Three keys across two pages, in one document.
	var doc struct {
		Response struct {
			Data       []json.RawMessage `json:"data"`
			HasMore    bool              `json:"has_more"`
			NextCursor *string           `json:"next_cursor"`
		} `json:"response"`
	}

	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("parse document: %v\n%s", err, stdout)
	}

	if len(doc.Response.Data) != 3 {
		t.Errorf("the document carries %d keys, want 3 (two from page one, one from page two)",
			len(doc.Response.Data))
	}

	if doc.Response.HasMore {
		t.Error("has_more is true after --all followed to the end")
	}

	if doc.Response.NextCursor != nil {
		t.Errorf("next_cursor is %q after the last page", *doc.Response.NextCursor)
	}
}

// Without `--all` the cursor is printed and not followed: the control that
// stops the test above from being a statement about pagination in general.
func TestWithoutAllTheCursorIsReportedAndNotFollowed(t *testing.T) {
	server, home := loggedIn(t, "keys.list.page1.200", "keys.list.page2.200")

	stdout, _, exit := run(t, invocation{
		home: home,
		args: []string{"list", "--api", server.URL(), "--limit", "2"},
	})

	if exit != 0 {
		t.Fatalf("exit %d, want 0", exit)
	}

	if n := requestCount(server); n != 1 {
		t.Errorf("the fixture saw %d requests; without --all only the page asked for is fetched", n)
	}

	if !strings.Contains(stdout, "CURSOR_PLACEHOLDER_1") {
		t.Errorf("the next cursor was not printed, so there is no way to ask for the next page:\n%s", stdout)
	}

	if !strings.Contains(stdout, "--cursor") {
		t.Errorf("the output does not say how to use the cursor:\n%s", stdout)
	}
}

// The `--status` vocabulary, both directions against `keys.Statuses`.
func TestTheStatusVocabularyIsHeldToItsDeclaredSet(t *testing.T) {
	if len(keys.Statuses) == 0 {
		t.Fatal("keys.Statuses is empty; both loops below would assert nothing")
	}

	for _, status := range keys.Statuses {
		server, home := loggedIn(t, "keys.list.page1.200")

		_, stderr, exit := run(t, invocation{
			home: home,
			args: []string{"list", "--api", server.URL(), "--status", status},
		})

		if exit == 2 {
			t.Errorf("the declared status %q was refused locally: %q", status, stderr)
		}

		if got := server.Requests()[0].Query.Get("status"); got != status {
			t.Errorf("--status %s was sent as %q", status, got)
		}
	}

	for _, status := range []string{"revoked", "expired", "ALL", "inactive"} {
		server, home := loggedIn(t, "keys.list.page1.200")

		_, stderr, exit := run(t, invocation{
			home: home,
			args: []string{"list", "--api", server.URL(), "--status", status},
		})

		if exit != 2 {
			t.Errorf("--status %s exits %d, want 2", status, exit)
		}

		if n := requestCount(server); n != 0 {
			t.Errorf("--status %s produced %d requests", status, n)
		}

		if !strings.Contains(stderr, status) {
			t.Errorf("the refusal does not name the value: %q", stderr)
		}
	}
}

// `keys.list.bad_status.400` cannot be reached from this command, and that is
// the point.
//
// U0 recorded FERRY refusing `?status=retired` with a `400 VALIDATION_FAILED`.
// The CLI refuses the same value from its own vocabulary and sends nothing, so
// the recording stays unplayed — a local refusal is strictly better than the
// round trip, and this asserts the CLI is the tighter of the two rather than
// merely agreeing with the server after the fact.
func TestTheValueFERRYWouldRefuseIsRefusedBeforeItIsSent(t *testing.T) {
	server := fixture.New(t, "keys.list.bad_status.400")
	home := newHome(t)

	writeProfile(t, home, server.URL(), canaryPAT(t))

	_, stderr, exit := run(t, invocation{
		home: home,
		args: []string{"list", "--api", server.URL(), "--status", "retired"},
	})

	if exit != 2 {
		t.Errorf("exit %d, want 2", exit)
	}

	if n := requestCount(server); n != 0 {
		t.Errorf("the fixture saw %d requests", n)
	}

	if remaining := server.Remaining()["keys.list.bad_status.400"]; remaining != 1 {
		t.Errorf("the recording has %d interactions left unplayed, want 1; the CLI sent the request "+
			"FERRY would have refused", remaining)
	}

	if !strings.Contains(stderr, "retired") {
		t.Errorf("the refusal does not name the value: %q", stderr)
	}
}

// `--limit 0` is a usage error rather than a parameter the API has to refuse.
func TestANonPositiveLimitIsRefusedLocally(t *testing.T) {
	for _, limit := range []string{"0", "-1"} {
		server, home := loggedIn(t, "keys.list.page1.200")

		_, stderr, exit := run(t, invocation{
			home: home,
			args: []string{"list", "--api", server.URL(), "--limit", limit},
		})

		if exit != 2 {
			t.Errorf("--limit %s exits %d, want 2", limit, exit)
		}

		if n := requestCount(server); n != 0 {
			t.Errorf("--limit %s produced %d requests", limit, n)
		}

		if !strings.Contains(stderr, "--limit") {
			t.Errorf("the refusal does not name the flag: %q", stderr)
		}
	}
}

// A402, the other half of the binding asserted in the corridors package:
// `GET /v1/api_keys` is read through the paginated envelope.
//
// `keys list` re-encodes this struct to build the aggregated page (AC39), so
// the wrong envelope here is not only a decode that drops `has_more` — it is
// a JSON document this CLI prints with three keys missing, having followed
// no cursor to find them.
func TestTheKeyListEnvelopeIsThePaginatedOne(t *testing.T) {
	got := reflect.TypeOf(keys.List{})
	want := reflect.TypeOf(api.List[api.APIKey]{})

	if got != want {
		t.Fatalf("keys reads GET /v1/api_keys as %s, not the paginated %s", got, want)
	}
}
