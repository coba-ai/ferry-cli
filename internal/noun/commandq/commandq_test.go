package commandq_test

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kurenn/ferry-cli/internal/cli"
	"github.com/kurenn/ferry-cli/internal/creds"
	"github.com/kurenn/ferry-cli/internal/fixture"
	"github.com/kurenn/ferry-cli/internal/fsx"
	"github.com/kurenn/ferry-cli/internal/harness"
	"github.com/kurenn/ferry-cli/internal/noun"
	"github.com/kurenn/ferry-cli/internal/xdg"
)

// No test here calls t.Parallel(): harness.Run swaps the process streams
// (A314).

var fixedNow = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

func loggedIn(t *testing.T, scenarios ...string) (*fixture.Server, string) {
	t.Helper()

	server := fixture.New(t, scenarios...)
	home := t.TempDir()

	paths := xdg.Paths{ConfigDir: home, StateDir: home, Home: home}
	if err := paths.EnsureAll(fsx.OS()); err != nil {
		t.Fatalf("create %s: %v", home, err)
	}

	store := creds.NewStore(fsx.OS(), filepath.Join(home, "credentials.json"))

	file, err := store.Load()
	if err != nil {
		t.Fatalf("load credentials: %v", err)
	}

	err = file.Put(creds.DefaultProfile, server.URL(), creds.Credential{
		Token:       "ferry_sk_sandbox_" + strings.Repeat("C", 43),
		PrincipalID: "key_PLACEHOLDER_1",
		StoredAt:    fixedNow,
	})
	if err != nil {
		t.Fatalf("put the credential: %v", err)
	}

	if err := store.Save(file); err != nil {
		t.Fatalf("save credentials: %v", err)
	}

	return server, home
}

type clock struct {
	now   time.Time
	slept []time.Duration
}

func (c *clock) Now() time.Time { return c.now }

func (c *clock) Sleep(d time.Duration) {
	c.slept = append(c.slept, d)
	c.now = c.now.Add(d)
}

func (c *clock) seconds() []int {
	out := []int{}
	for _, d := range c.slept {
		out = append(out, int(d/time.Second))
	}

	return out
}

func run(t *testing.T, home string, clk *clock, args ...string) (stdout, stderr string, exit int) {
	t.Helper()

	deps := noun.Deps{HTTP: &http.Client{}, Now: func() time.Time { return fixedNow }}
	if clk != nil {
		deps.Clock = clk
	}

	root := cli.New(cli.Options{Noun: deps, Args: args})

	return harness.Run(t, root, args, "", map[string]string{"FERRY_HOME": home}, false)
}

const commandID = "cmd_PLACEHOLDER_1"

func commandPath() string { return "/v1/commands/" + commandID }

func requestsTo(s *fixture.Server, path string) int {
	n := 0

	for _, req := range s.Requests() {
		if req.Path == path {
			n++
		}
	}

	return n
}

// AC59: `commands get` reads once and does not poll.
//
// The distinction from `watch` is the whole of the two commands. A `get`
// that polled would block a script that asked one question, and a `watch`
// that read once would silently answer "still in flight" and exit.
func TestGetReadsOnce(t *testing.T) {
	for _, tc := range []struct {
		name     string
		scenario string
		wantExit int
	}{
		{
			// A command still working is exit 6: the outcome is not
			// established. `get` reports that and stops.
			name:     "in flight",
			scenario: "commands.get.inflight.200",
			wantExit: 6,
		},
		{
			name:     "completed",
			scenario: "commands.get.completed.200",
			wantExit: 0,
		},
		{
			// Exit 7, not 8. Exit 8 is `upstream_failed` — accepted
			// upstream and *then* failed, where money did move. A
			// `failed_terminal` command never reached the upstream, so it
			// is `escalate`: stop and read it.
			name:     "failed terminally",
			scenario: "commands.get.failed_terminal.200",
			wantExit: 7,
		},
		{
			name:     "needs an operator",
			scenario: "commands.get.needs_operator.200",
			wantExit: 7,
		},
		{
			name:     "not found",
			scenario: "commands.get.404",
			wantExit: 3,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server, home := loggedIn(t, tc.scenario)

			clk := &clock{now: fixedNow}

			stdout, stderr, exit := run(t, home, clk, "commands", "get", commandID)

			if exit != tc.wantExit {
				t.Errorf("exit = %d, want %d\n%s\n%s", exit, tc.wantExit, stdout, stderr)
			}

			// One read, whatever the state.
			if got := requestsTo(server, commandPath()); got != 1 {
				t.Errorf("command reads = %d, want 1: `get` reads once", got)
			}

			// And it did not wait. A `get` that slept would be a `watch`.
			if got := clk.seconds(); len(got) != 0 {
				t.Errorf("`get` slept %v", got)
			}
		})
	}
}

// AC59: `commands watch` follows the same command through to a stop, using
// the same loop the money path uses.
func TestWatchFollowsToAStop(t *testing.T) {
	server, home := loggedIn(t, "commands.get.inflight.200", "commands.get.completed.200")

	clk := &clock{now: fixedNow}

	stdout, stderr, exit := run(t, home, clk, "commands", "watch", commandID)

	if exit != 0 {
		t.Fatalf("exit = %d, want 0\n%s\n%s", exit, stdout, stderr)
	}

	if got := requestsTo(server, commandPath()); got != 2 {
		t.Errorf("command reads = %d, want 2", got)
	}

	// The first read is immediate — there is no 202 header to honour, so
	// there is nothing to wait for — and the second waits the in-flight
	// command's own `retry_after_seconds`, which the recording sets to 1.
	if got := clk.seconds(); len(got) != 1 || got[0] != 1 {
		t.Errorf("the waits were %v, want a single 1-second wait taken from the command's "+
			"retry_after_seconds", got)
	}
}

// A392: `last_error.code` is not the wire error vocabulary, and a
// `failed_terminal` command may not be classified as something to come back
// to.
//
// The recordings carry `UPSTREAM_UNKNOWN` in that field, a code the error
// catalogue does not name at all. Reading it through the wire table made
// `RATE_LIMITED`, `SERVICE_UNAVAILABLE` and `UPSTREAM_BUSY` classify
// `transient` — "resend the identical request" — and `INTERNAL_ERROR`
// classify `pending` — "keep polling". Both send a caller back to a command
// `openapi.yaml` says will never move again.
//
// This asserts it where a caller experiences it: the exit code and the
// number of reads.
func TestAFailedTerminalCommandIsNeverSomethingToComeBackTo(t *testing.T) {
	for _, scenario := range []string{
		"commands.get.failed_terminal.200",
		"commands.get.failed_terminal.contradiction.200",
	} {
		t.Run(scenario, func(t *testing.T) {
			server, home := loggedIn(t, scenario)

			clk := &clock{now: fixedNow}

			stdout, stderr, exit := run(t, home, clk, "commands", "watch", commandID)

			// Not 5 (`transient`, "resend the identical request") and not
			// 6 (`pending`, "the outcome is not established, keep
			// polling"). Both would be instructions to act on a command
			// that is finished.
			switch exit {
			case 5:
				t.Errorf("a failed_terminal command exited 5, which tells the caller to resend"+
					"\n%s\n%s", stdout, stderr)
			case 6:
				t.Errorf("a failed_terminal command exited 6, which tells the caller the "+
					"outcome is not established\n%s\n%s", stdout, stderr)
			case 0:
				t.Errorf("a failed_terminal command exited 0\n%s\n%s", stdout, stderr)
			}

			// And the loop stopped. A watch that kept polling a terminal
			// command would exhaust the recording and hang or 599.
			if got := requestsTo(server, commandPath()); got != 1 {
				t.Errorf("command reads = %d, want 1: a terminal state stops the loop", got)
			}

			if got := clk.seconds(); len(got) != 0 {
				t.Errorf("the watch waited %v after reaching a terminal state", got)
			}
		})
	}
}

// A392's other half, from the code's side: `UPSTREAM_UNKNOWN` appears in the
// recording and is never treated as a wire error code.
//
// The `upstream_unknown` *state* is a command still working, so it is the
// one case where `pending` is right — and it is right because of the state,
// not because of the code.
func TestAnUpstreamUnknownStateIsPendingBecauseOfTheState(t *testing.T) {
	server, home := loggedIn(t, "commands.get.upstream_unknown.200")

	clk := &clock{now: fixedNow}

	stdout, stderr, exit := run(t, home, clk, "commands", "get", commandID)

	if exit != 6 {
		t.Fatalf("exit = %d, want 6 for a command still working\n%s\n%s", exit, stdout, stderr)
	}

	if got := requestsTo(server, commandPath()); got != 1 {
		t.Errorf("command reads = %d, want 1", got)
	}

	// The report names the state, because that is what the decision was
	// made on.
	if !strings.Contains(stdout+stderr, "upstream_unknown") {
		t.Errorf("the report does not name the state\n%s\n%s", stdout, stderr)
	}
}

// AC59: a contradiction is a stop, and it says so.
//
// A contradiction is FERRY telling the caller that two of its own records
// disagree. Continuing to poll, or reporting the command's state as if it
// were reliable, would hide the one thing that needs a human.
func TestAContradictionStopsAndIsReported(t *testing.T) {
	server, home := loggedIn(t, "commands.get.completed.contradiction.200")

	clk := &clock{now: fixedNow}

	stdout, stderr, exit := run(t, home, clk, "commands", "watch", commandID)

	if exit != 7 {
		t.Errorf("exit = %d, want 7: a contradiction needs a human\n%s\n%s", exit, stdout, stderr)
	}

	if got := requestsTo(server, commandPath()); got != 1 {
		t.Errorf("command reads = %d, want 1", got)
	}

	report := stdout + stderr

	// The escalation sentence, and the contradiction's own fields. The
	// sentence alone tells an operator to stop; `kind` tells them what they
	// are looking at, and without it the next step is to go and read the
	// command by hand.
	if !strings.Contains(strings.ToLower(report), "contradiction") {
		t.Errorf("the report does not use the word\n%s", report)
	}

	for _, want := range []string{"second_transaction", "recovery", "txn_PLACEHOLDER_2"} {
		if !strings.Contains(report, want) {
			t.Errorf("the report does not carry %q, which is what an operator acts on\n%s",
				want, report)
		}
	}

	// And stdout is untouched, so a script piping it is not handed prose.
	if strings.TrimSpace(stdout) != "" && !strings.HasPrefix(strings.TrimSpace(stdout), "command") {
		t.Errorf("unexpected stdout:\n%s", stdout)
	}
}

// A393: polling a completed simulate never yields a usable plan token, and
// the CLI must not suggest that it could.
//
// `Command.result.body` is a `oneOf` of `Transaction` and
// `StoredSimulation`, and the stored-simulate arm has no `plan.token` and no
// `meta` — the token is minted with the plan and never stored. A CLI that
// told a caller to poll for their token would be sending them somewhere the
// answer structurally cannot be.
func TestNoRenderingSuggestsPollingForAPlanToken(t *testing.T) {
	for _, scenario := range []string{
		"commands.get.completed.200",
		"commands.get.inflight.200",
		"commands.get.reserved.200",
		"commands.get.failed_retriable.200",
	} {
		t.Run(scenario, func(t *testing.T) {
			_, home := loggedIn(t, scenario)

			clk := &clock{now: fixedNow}

			stdout, stderr, _ := run(t, home, clk, "commands", "get", commandID)

			report := strings.ToLower(stdout + stderr)

			// The pairing is what makes this a real check rather than a
			// keyword ban: "plan token" next to an instruction to watch
			// or poll is the sentence that cannot be true.
			if !strings.Contains(report, "plan token") && !strings.Contains(report, "plan_token") {
				return
			}

			for _, suggestion := range []string{"commands watch", "keep polling", "poll again"} {
				if strings.Contains(report, suggestion) {
					t.Errorf("the report mentions a plan token and suggests %q; a completed "+
						"simulate does not store one (A393)\n%s\n%s", suggestion, stdout, stderr)
				}
			}
		})
	}
}

// The JSON rendering carries the command through unchanged, so a caller can
// read the fields this CLI chose not to interpret.
func TestTheJSONRenderingCarriesTheCommand(t *testing.T) {
	// **A355, a recording gap.** Not one of the ten `commands.get.*`
	// recordings carries a `last_error` — every one has `last_error: null`
	// — so the field A392 is entirely about cannot be driven through `ferry
	// commands get` at all. The recordings that do carry it are the
	// money-path polling ones, and the fixture consumes a scenario's
	// interactions in order, so its GET cannot be reached without first
	// sending the POST that precedes it.
	//
	// The pass-through of `last_error` and its unbound keys is therefore
	// covered where it can be: `internal/poll`'s round-trip test, over the
	// bytes from that recording. This test asserts what an end-to-end read
	// can establish.
	_, home := loggedIn(t, "commands.get.upstream_unknown.200")

	clk := &clock{now: fixedNow}

	stdout, stderr, exit := run(t, home, clk, "commands", "get", commandID, "--output", "json")

	if exit == 0 {
		t.Fatalf("exit = 0 for a command still working\n%s", stdout)
	}

	var doc struct {
		Response json.RawMessage `json:"response"`
		Outcome  struct {
			ExitCode int `json:"exit_code"`
		} `json:"outcome"`
	}

	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("parse the document: %v\n%s\n%s", err, stdout, stderr)
	}

	if doc.Outcome.ExitCode != exit {
		t.Errorf("outcome.exit_code = %d but the process exited %d", doc.Outcome.ExitCode, exit)
	}

	var command struct {
		ID    string `json:"id"`
		State string `json:"state"`
	}

	if err := json.Unmarshal(doc.Response, &command); err != nil {
		t.Fatalf("the response block is not a command: %v\n%s", err, doc.Response)
	}

	if command.ID != commandID {
		t.Errorf("the document's command id is %q, want %q", command.ID, commandID)
	}

	if command.State != "upstream_unknown" {
		t.Errorf("the document's state is %q, want upstream_unknown", command.State)
	}

	// The response block is the bytes FERRY sent, not a re-encoding of this
	// CLI's structs. That is what lets a caller reach a field this CLI
	// chose not to name — `last_error`'s fifteen properties, of which only
	// `code` is bound (A392).
	if !strings.Contains(string(doc.Response), `"poll"`) {
		t.Errorf("the response block is not the command FERRY sent:\n%s", doc.Response)
	}
}
