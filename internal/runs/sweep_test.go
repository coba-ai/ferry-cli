package runs_test

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kurenn/ferry/cli/internal/fsx"
	"github.com/kurenn/ferry/cli/internal/runs"
)

// ---------------------------------------------------------------- AC82 ----

// AC82: Record strips plan.token from a simulate response before writing.
//
// The fresh simulate 201 is the token's only carrier, so a Record that stored
// the body verbatim would put a live bearer authorisation on disk on every
// simulate — including a simulate-only run, which AC46 says holds no token
// anywhere. Mutation M91 stores the body verbatim.
func TestRecordStripsPlanTokenFromASimulateResponse(t *testing.T) {
	l, _ := newLedger(t)
	h := mustCreate(t, l, broadcastRun(t))
	runID := h.Run().RunID

	body := []byte(`{"object":"simulation","command_id":"cmd_1","quote":{"rate":"1.0"},` +
		`"plan":{"object":"plan","id":"plan_1","expires_at":"2026-09-21T10:05:00Z","token":"` + planToken + `"}}`)

	if err := h.Begin(runs.StepSimulate, simulateBody); err != nil {
		t.Fatal(err)
	}
	if err := h.Record(runs.StepSimulate, &runs.Response{Status: 201, Body: body}, &runs.Outcome{Class: "done", ExitCode: 0}); err != nil {
		t.Fatal(err)
	}

	stored := h.Run().Step(runs.StepSimulate).Response.Body
	if strings.Contains(string(stored), "token") {
		t.Fatalf("the stored simulate body still carries a token key: %s", stored)
	}
	if runs.ContainsPlanToken(stored) {
		t.Fatalf("the stored simulate body still carries a plan token: %s", stored)
	}
	// Everything else survives: the strip must not cost the diagnostics.
	for _, want := range []string{`"plan"`, `"plan_1"`, `"expires_at"`, `"command_id"`, `"quote"`} {
		if !strings.Contains(string(stored), want) {
			t.Errorf("the strip removed %s as well: %s", want, stored)
		}
	}
	if runs.ContainsPlanToken(readRaw(t, l, runID)) {
		t.Fatalf("a plan token reached the record file")
	}
}

// A body the CLI cannot parse must not be a way onto the disk. The structural
// strip cannot fire here, so the textual sweep is what holds C6.
func TestRecordRedactsATokenInAnUnparseableBody(t *testing.T) {
	l, _ := newLedger(t)
	h := mustCreate(t, l, broadcastRun(t))

	if err := h.Begin(runs.StepSimulate, simulateBody); err != nil {
		t.Fatal(err)
	}
	truncated := []byte(`{"object":"simulation","plan":{"token":"` + planToken)
	if err := h.Record(runs.StepSimulate, &runs.Response{Status: 201, Body: truncated}, &runs.Outcome{Class: "pending", ExitCode: 6}); err != nil {
		t.Fatal(err)
	}
	if runs.ContainsPlanToken(readRaw(t, l, h.Run().RunID)) {
		t.Fatal("an unparseable body put a plan token on disk")
	}
}

// AC83 names an HTML 502 as a body that must classify pending/6 — the class
// meaning the money may have moved. So the ledger must be able to write it:
// if Record could fail on the response bytes, the outcome a caller most needs
// on disk would be the one that could not be recorded.
func TestRecordStoresABodyThatIsNotJSON(t *testing.T) {
	bodies := []struct {
		name string
		body []byte
		// want differs from body only where the storage is deliberately
		// lossy. Nothing is digested over a response body, so readability
		// wins over fidelity — but the loss is stated here, not implied.
		want []byte
	}{
		{"an HTML 502 from a proxy", []byte("<html><head><title>502 Bad Gateway</title></head><body>502</body></html>"), nil},
		{"a truncated JSON body", []byte(`{"object":"transaction","id":"txn_`), nil},
		{"a JSON body with an HTML-escapable character", []byte(`{"note":"a<b & c>d"}`), nil},
		{"empty", []byte{}, nil},
		{"a bare JSON string", []byte(`"accepted"`), nil},
		// ToValidUTF8 collapses a run of invalid bytes into one rune.
		{"not UTF-8 at all", []byte{0xff, 0xfe, 0x41}, []byte("\uFFFDA")},
	}

	for _, tc := range bodies {
		body := tc.body
		want := tc.want
		if want == nil {
			want = body
		}
		t.Run(tc.name, func(t *testing.T) {
			l, _ := newLedger(t)
			h := mustCreate(t, l, broadcastRun(t))
			runID := h.Run().RunID

			if err := h.Begin(runs.StepExecute, executeBody()); err != nil {
				t.Fatal(err)
			}
			if err := h.Record(runs.StepExecute,
				&runs.Response{Status: 502, Body: body},
				&runs.Outcome{Class: "pending", ExitCode: 6}); err != nil {
				t.Fatalf("Record: %v", err)
			}
			if err := h.Close(); err != nil {
				t.Fatal(err)
			}

			// And it survives a reload, because a resume reads it back.
			run, err := l.Load(runID)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			step := run.Step(runs.StepExecute)
			if step.State != runs.StateAnswered {
				t.Fatalf("state = %s, want answered", step.State)
			}
			if got := step.Response.Body; !sameBody(got, want) {
				t.Fatalf("body round-tripped to %q, want %q", got, want)
			}
		})
	}
}

// sameBody compares two stored response bodies. A JSON body is stored as
// JSON in the record, so it comes back re-indented and with <, > and &
// escaped; the comparison is therefore semantic for JSON and byte-exact for
// everything else. A request body would not get this latitude — body_sha256
// is computed over its exact bytes — but no digest covers a response.
func sameBody(got, want []byte) bool {
	if json.Valid(got) && json.Valid(want) {
		var a, b any
		if json.Unmarshal(got, &a) != nil || json.Unmarshal(want, &b) != nil {
			return false
		}
		x, _ := json.Marshal(a)
		y, _ := json.Marshal(b)
		return string(x) == string(y)
	}
	return string(got) == string(want)
}

// A token in a field the API does not put one in today is still a token.
func TestRecordRedactsATokenOutsidePlanToken(t *testing.T) {
	l, _ := newLedger(t)
	h := mustCreate(t, l, broadcastRun(t))

	if err := h.Begin(runs.StepSimulate, simulateBody); err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"meta":{"remediation":"the token ` + planToken + ` was issued"},"plan":{"id":"p"}}`)
	if err := h.Record(runs.StepSimulate, &runs.Response{Status: 201, Body: body}, &runs.Outcome{ExitCode: 0}); err != nil {
		t.Fatal(err)
	}
	if runs.ContainsPlanToken(readRaw(t, l, h.Run().RunID)) {
		t.Fatal("a token outside plan.token reached the record")
	}
	if !strings.Contains(string(h.Run().Step(runs.StepSimulate).Response.Body), "remediation") {
		t.Fatal("the redaction removed the surrounding text")
	}
}

// AC82's sweep: over a --broadcast run at each state, the token appears in
// exactly the two fields C6 permits and nowhere else.
//
// Mutation M115 stores a token in plan_token on a declined run and is killed
// by the declined case.
func TestBroadcastTokenOccurrencesAtEachState(t *testing.T) {
	type expectation struct {
		inPlanToken bool
		inBody      bool
	}
	cases := []struct {
		name  string
		state runs.State
		drive func(t *testing.T, h *runs.Handle)
		want  expectation
	}{
		{
			// A step nothing has happened to. Included because the census
			// check demands it, and because "the token is nowhere before a
			// plan exists" is worth stating rather than assuming.
			name:  "not_started",
			state: runs.StateNotStarted,
			drive: func(t *testing.T, h *runs.Handle) {},
			want:  expectation{inPlanToken: false, inBody: false},
		},
		{
			// Sent and answered but not settled: a 503 on the execute step.
			// The token stays in both fields, because a resume must resend
			// the identical body under the identical key.
			name:  "answered",
			state: runs.StateAnswered,
			drive: func(t *testing.T, h *runs.Handle) {
				recordFreshSimulate(t, h)
				if err := h.AwaitConfirmation(runs.StepExecute); err != nil {
					t.Fatal(err)
				}
				if err := h.Begin(runs.StepExecute, executeBody()); err != nil {
					t.Fatal(err)
				}
				if err := h.Record(runs.StepExecute, &runs.Response{Status: 503},
					&runs.Outcome{Class: "pending", ExitCode: 6}); err != nil {
					t.Fatal(err)
				}
			},
			want: expectation{inPlanToken: true, inBody: true},
		},
		{
			name:  "awaiting_confirmation",
			state: runs.StateAwaitingConfirmation,
			drive: func(t *testing.T, h *runs.Handle) {
				recordFreshSimulate(t, h)
				if err := h.AwaitConfirmation(runs.StepExecute); err != nil {
					t.Fatal(err)
				}
			},
			// The token is stored and no body exists yet: this is the window
			// the token is on disk for (AC80).
			want: expectation{inPlanToken: true, inBody: false},
		},
		{
			name:  "declined",
			state: runs.StateDeclined,
			drive: func(t *testing.T, h *runs.Handle) {
				recordFreshSimulate(t, h)
				if err := h.AwaitConfirmation(runs.StepExecute); err != nil {
					t.Fatal(err)
				}
				if err := h.Decline(runs.StepExecute); err != nil {
					t.Fatal(err)
				}
			},
			// A declined step never had a body, so a declined token leaves
			// the disk entirely.
			want: expectation{inPlanToken: false, inBody: false},
		},
		{
			name:  "pending",
			state: runs.StatePending,
			drive: func(t *testing.T, h *runs.Handle) {
				recordFreshSimulate(t, h)
				if err := h.AwaitConfirmation(runs.StepExecute); err != nil {
					t.Fatal(err)
				}
				if err := h.Begin(runs.StepExecute, executeBody()); err != nil {
					t.Fatal(err)
				}
			},
			// Sent, or possibly sent: the token is in plan_token and in the
			// verbatim body body_sha256 vouches for.
			want: expectation{inPlanToken: true, inBody: true},
		},
		{
			name:  "terminal",
			state: runs.StateTerminal,
			drive: func(t *testing.T, h *runs.Handle) {
				recordFreshSimulate(t, h)
				if err := h.AwaitConfirmation(runs.StepExecute); err != nil {
					t.Fatal(err)
				}
				if err := h.Begin(runs.StepExecute, executeBody()); err != nil {
					t.Fatal(err)
				}
				if err := h.Record(runs.StepExecute,
					&runs.Response{Status: 201, Body: []byte(`{"id":"txn_1","status":"processing"}`)},
					&runs.Outcome{Class: "accepted_upstream", ExitCode: 0}); err != nil {
					t.Fatal(err)
				}
			},
			// plan_token is scrubbed on the terminal write; body is not,
			// because body_sha256 must keep holding (V6).
			want: expectation{inPlanToken: false, inBody: true},
		},
		{
			name:  "unreachable",
			state: runs.StateUnreachable,
			drive: func(t *testing.T, h *runs.Handle) {
				if err := h.Begin(runs.StepSimulate, simulateBody); err != nil {
					t.Fatal(err)
				}
				if err := h.Record(runs.StepSimulate, &runs.Response{Status: 202, Body: []byte(`{"object":"command"}`)},
					&runs.Outcome{Class: "pending", ExitCode: 6}); err != nil {
					t.Fatal(err)
				}
				if err := h.Unreachable(runs.StepExecute); err != nil {
					t.Fatal(err)
				}
			},
			want: expectation{inPlanToken: false, inBody: false},
		},
	}

	covered := map[runs.State]bool{}
	for _, tc := range cases {
		covered[tc.state] = true
	}
	assertCoversEveryState(t, covered)

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l, _ := newLedger(t)
			h := mustCreate(t, l, broadcastRun(t))
			tc.drive(t, h)

			exec := h.Run().Step(runs.StepExecute)
			if exec.State != tc.state {
				t.Fatalf("the case claims to drive the execute step to %s but it is %s; "+
					"the coverage check above would be satisfied by a label", tc.state, exec.State)
			}

			gotPlanToken := exec.PlanToken != nil && *exec.PlanToken == planToken
			if gotPlanToken != tc.want.inPlanToken {
				t.Errorf("plan_token holds the token = %v, want %v (value %v)", gotPlanToken, tc.want.inPlanToken, deref(exec.PlanToken))
			}
			gotBody := strings.Contains(string(exec.Body), planToken)
			if gotBody != tc.want.inBody {
				t.Errorf("body holds the token = %v, want %v", gotBody, tc.want.inBody)
			}

			// And nowhere else in the file: count the occurrences in the raw
			// record and check the number against the two fields above.
			raw := string(readRaw(t, l, h.Run().RunID))
			want := 0
			if tc.want.inPlanToken {
				want++
			}
			if tc.want.inBody {
				want++
			}
			if got := strings.Count(raw, planToken); got != want {
				t.Fatalf("the token appears %d times in the record, want %d:\n%s", got, want, raw)
			}

			// A step that was declined or never started must also not be able
			// to reach a state where the token is re-exposed.
			if exec.State == runs.StateDeclined && exec.HasPlanToken() {
				t.Fatal("a declined step still reports a live plan token")
			}
		})
	}
}

func deref(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}

// recordFreshSimulate drives the run to the state a --broadcast simulate 201
// leaves: the simulate terminal, and the plan and token on the execute step.
func recordFreshSimulate(t *testing.T, h *runs.Handle) {
	t.Helper()
	if err := h.Begin(runs.StepSimulate, simulateBody); err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"object":"simulation","plan":{"id":"plan_1","expires_at":"2026-09-21T10:05:00Z","token":"` + planToken + `"}}`)
	if err := h.Record(runs.StepSimulate, &runs.Response{Status: 201, Body: body}, &runs.Outcome{Class: "done", ExitCode: 0}); err != nil {
		t.Fatal(err)
	}
	if err := h.SetPlan(runs.StepExecute, &runs.Plan{ID: "plan_1", ExpiresAt: time.Now().Add(5 * time.Minute).UTC()}, planToken); err != nil {
		t.Fatal(err)
	}
}

// ---------------------------------------------------------------- AC13 ----

// AC13: Scrub replaces the execute step's plan_token when the step is
// terminal, declined or unreachable, or when the plan expired more than the
// grace ago; body_sha256 is unchanged.
//
// Mutation M6 never scrubs; mutation M7 scrubs body_sha256 too.
func TestScrubOnExpiry(t *testing.T) {
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name      string
		expiresAt time.Time
		want      bool
	}{
		{"expired well past the grace", now.Add(-time.Hour), true},
		{"expired exactly at the grace boundary", now.Add(-runs.ScrubGrace), false},
		{"expired within the grace", now.Add(-time.Minute), false},
		{"not yet expired", now.Add(4 * time.Minute), false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l, _ := newLedger(t)
			h := mustCreate(t, l, broadcastRun(t))
			runID := h.Run().RunID

			if err := h.Begin(runs.StepSimulate, simulateBody); err != nil {
				t.Fatal(err)
			}
			if err := h.SetPlan(runs.StepExecute, &runs.Plan{ID: "plan_1", ExpiresAt: tc.expiresAt}, planToken); err != nil {
				t.Fatal(err)
			}
			if err := h.AwaitConfirmation(runs.StepExecute); err != nil {
				t.Fatal(err)
			}
			digestBefore := *h.Run().Step(runs.StepSimulate).BodySHA256
			if err := h.Close(); err != nil {
				t.Fatal(err)
			}

			scrubbed, err := l.Scrub(now)
			if err != nil {
				t.Fatalf("Scrub: %v", err)
			}
			if got := len(scrubbed) == 1; got != tc.want {
				t.Fatalf("Scrub reported %v, want scrubbed=%v", scrubbed, tc.want)
			}

			run, err := l.Load(runID)
			if err != nil {
				t.Fatal(err)
			}
			exec := run.Step(runs.StepExecute)
			if tc.want {
				if exec.PlanToken == nil || *exec.PlanToken != runs.ScrubbedToken {
					t.Fatalf("plan_token = %v, want the scrub sentinel", deref(exec.PlanToken))
				}
			} else if exec.PlanToken == nil || *exec.PlanToken != planToken {
				t.Fatalf("plan_token = %v, want the live token", deref(exec.PlanToken))
			}
			if got := *run.Step(runs.StepSimulate).BodySHA256; got != digestBefore {
				t.Fatalf("body_sha256 changed from %s to %s; a resume can no longer prove it resends the same request", digestBefore, got)
			}
		})
	}
}

func TestScrubOnTerminalStates(t *testing.T) {
	for _, tc := range []struct {
		name  string
		drive func(t *testing.T, h *runs.Handle)
	}{
		{"terminal", func(t *testing.T, h *runs.Handle) {
			if err := h.Begin(runs.StepExecute, executeBody()); err != nil {
				t.Fatal(err)
			}
			if err := h.Record(runs.StepExecute, &runs.Response{Status: 201}, &runs.Outcome{ExitCode: 0}); err != nil {
				t.Fatal(err)
			}
		}},
		{"declined", func(t *testing.T, h *runs.Handle) {
			if err := h.Decline(runs.StepExecute); err != nil {
				t.Fatal(err)
			}
		}},
		{"unreachable", func(t *testing.T, h *runs.Handle) {
			if err := h.Unreachable(runs.StepExecute); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l, _ := newLedger(t)
			h := mustCreate(t, l, broadcastRun(t))
			// The plan is live: only the state may cause the scrub here.
			if err := h.SetPlan(runs.StepExecute, &runs.Plan{ID: "p", ExpiresAt: time.Now().Add(time.Hour).UTC()}, planToken); err != nil {
				t.Fatal(err)
			}
			tc.drive(t, h)

			exec := h.Run().Step(runs.StepExecute)
			if exec.HasPlanToken() {
				t.Fatalf("a %s step still holds a live token: %v", tc.name, deref(exec.PlanToken))
			}
			if exec.PlanToken == nil || *exec.PlanToken != runs.ScrubbedToken {
				t.Fatalf("plan_token = %v, want the scrub sentinel (null would lose the fact that there was one)", deref(exec.PlanToken))
			}
		})
	}
}

// AC13: a run whose lock another process holds is skipped. Rewriting a record
// under a live holder would lose whatever that holder is about to write.
func TestScrubSkipsALockedRun(t *testing.T) {
	l, dir := newLedger(t)
	run := broadcastRun(t)
	h := mustCreate(t, l, run)
	if err := h.SetPlan(runs.StepExecute, &runs.Plan{ID: "p", ExpiresAt: time.Now().Add(-time.Hour).UTC()}, planToken); err != nil {
		t.Fatal(err)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}

	stop := startHelper(t, "hold-open", dir, run.RunID)

	scrubbed, err := l.Scrub(time.Now())
	if err != nil {
		t.Fatalf("Scrub: %v", err)
	}
	if len(scrubbed) != 0 {
		t.Fatalf("Scrub touched a locked run: %v", scrubbed)
	}

	stop()

	scrubbed, err = l.Scrub(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(scrubbed) != 1 {
		t.Fatalf("Scrub skipped the run after the holder exited: %v", scrubbed)
	}
}

// ---------------------------------------------------------------- AC14 ----

// AC14: FindByKey returns the run and step that own a key by exact match.
//
// Mutation M8 matches by prefix, and the near misses below are what kill it:
// the run id itself is a prefix of both of that run's keys, so a prefix rule
// would route a caller into a run whose body is a different request.
func TestFindByKeyIsAnExactMatch(t *testing.T) {
	l, _ := newLedger(t)
	run := broadcastRun(t)
	h := mustCreate(t, l, run)
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	other := broadcastRun(t)
	h2 := mustCreate(t, l, other)
	if err := h2.Close(); err != nil {
		t.Fatal(err)
	}

	for _, step := range []string{runs.StepSimulate, runs.StepExecute} {
		key := runs.StepKey(run.RunID, step)
		m, err := l.FindByKey(key)
		if err != nil {
			t.Fatal(err)
		}
		if m == nil {
			t.Fatalf("FindByKey(%s) found nothing", key)
		}
		if m.Run.RunID != run.RunID || m.Step.Name != step {
			t.Fatalf("FindByKey(%s) found run %s step %s", key, m.Run.RunID, m.Step.Name)
		}
	}

	nearMisses := []string{
		run.RunID,          // a prefix of both keys
		run.RunID + "-",    // a longer prefix
		run.RunID + "-sim", // a prefix of one key
		runs.StepKey(run.RunID, runs.StepSimulate) + "x", // an extension
		strings.ToLower(runs.StepKey(run.RunID, runs.StepSimulate)),
		"",
		"totally-unrelated",
	}
	for _, key := range nearMisses {
		m, err := l.FindByKey(key)
		if err != nil {
			t.Fatalf("FindByKey(%q): %v", key, err)
		}
		if m != nil {
			t.Errorf("FindByKey(%q) matched run %s step %s; the match is not exact", key, m.Run.RunID, m.Step.Name)
		}
	}
}

func TestFindByKeyFindsACallerSuppliedKey(t *testing.T) {
	l, _ := newLedger(t)
	run := broadcastRun(t)
	run.Steps = run.Steps[:1]
	run.Steps[0].IdempotencyKey = "my-own-key"
	h := mustCreate(t, l, run)
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}

	m, err := l.FindByKey("my-own-key")
	if err != nil {
		t.Fatal(err)
	}
	if m == nil || m.Run.RunID != run.RunID {
		t.Fatalf("FindByKey did not find a caller-supplied key: %v", m)
	}
}

// A sweep that skipped an unreadable record would answer "this key is not in
// use" for a key that is, which is the direction that costs money.
func TestSweepsRefuseToReadPastACorruptRecord(t *testing.T) {
	l, dir := newLedger(t)
	run := broadcastRun(t)
	h := mustCreate(t, l, run)
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(l.RecordPath(run.RunID), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = dir

	if _, err := l.FindByKey("anything"); !errors.Is(err, runs.ErrRecordCorrupt) {
		t.Errorf("FindByKey err = %v, want ErrRecordCorrupt", err)
	}
	if _, err := l.Orphans("default", "sandbox"); !errors.Is(err, runs.ErrRecordCorrupt) {
		t.Errorf("Orphans err = %v, want ErrRecordCorrupt", err)
	}
	if _, err := l.List(); !errors.Is(err, runs.ErrRecordCorrupt) {
		t.Errorf("List err = %v, want ErrRecordCorrupt", err)
	}
}

// A temp file left by a crashed write must not be read as a record, or every
// sweep would report a corrupt ledger after any interrupted write.
func TestSweepsIgnoreTempFilesAndSidecars(t *testing.T) {
	l, dir := newLedger(t)
	run := broadcastRun(t)
	h := mustCreate(t, l, run)
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{
		"." + run.RunID + ".json.tmpdeadbeef",
		"notes.txt",
		"01NOTAULID.json",
	} {
		if err := os.WriteFile(dir+"/"+name, []byte("{not json"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	all, err := l.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(all) != 1 || all[0].RunID != run.RunID {
		t.Fatalf("List = %v", all)
	}
}

// ---------------------------------------------------------------- AC90 ----

// AC90: Orphans returns runs whose step is awaiting_confirmation, pending, or
// answered with class 5/6.
//
// Each case names the state it drives a step into, and those names are
// checked against runs.AllStates() at the end. Before that check the table
// was a one-directional subset: a state added to the enum later would have
// gone unexamined here, and the "Exactly" in this test's name would have
// meant "exactly the cases someone remembered to write".
func TestOrphansReportsExactlyTheUnresolvedStates(t *testing.T) {
	cases := []struct {
		name  string
		state runs.State
		drive func(t *testing.T, h *runs.Handle)
		want  bool
	}{
		{"not_started", runs.StateNotStarted, func(t *testing.T, h *runs.Handle) {}, false},
		{"awaiting_confirmation", runs.StateAwaitingConfirmation, func(t *testing.T, h *runs.Handle) {
			if err := h.AwaitConfirmation(runs.StepExecute); err != nil {
				t.Fatal(err)
			}
		}, true},
		{"pending", runs.StatePending, func(t *testing.T, h *runs.Handle) {
			if err := h.Begin(runs.StepSimulate, simulateBody); err != nil {
				t.Fatal(err)
			}
		}, true},
		{"answered class 5", runs.StateAnswered, func(t *testing.T, h *runs.Handle) {
			recordSimulateWithExit(t, h, 5)
		}, true},
		{"answered class 6", runs.StateAnswered, func(t *testing.T, h *runs.Handle) {
			recordSimulateWithExit(t, h, 6)
		}, true},
		{"terminal class 0", runs.StateTerminal, func(t *testing.T, h *runs.Handle) {
			recordSimulateWithExit(t, h, 0)
		}, false},
		{"terminal class 4", runs.StateTerminal, func(t *testing.T, h *runs.Handle) {
			recordSimulateWithExit(t, h, 4)
		}, false},
		{"terminal class 7", runs.StateTerminal, func(t *testing.T, h *runs.Handle) {
			recordSimulateWithExit(t, h, 7)
		}, false},
		{"declined", runs.StateDeclined, func(t *testing.T, h *runs.Handle) {
			if err := h.Decline(runs.StepExecute); err != nil {
				t.Fatal(err)
			}
		}, false},
		{"unreachable", runs.StateUnreachable, func(t *testing.T, h *runs.Handle) {
			if err := h.Unreachable(runs.StepExecute); err != nil {
				t.Fatal(err)
			}
		}, false},
	}

	covered := map[runs.State]bool{}
	for _, tc := range cases {
		covered[tc.state] = true
		t.Run(tc.name, func(t *testing.T) {
			l, _ := newLedger(t)
			h := mustCreate(t, l, broadcastRun(t))
			tc.drive(t, h)

			// The case must actually have reached the state it claims, or the
			// coverage check above would be satisfied by a label.
			if got := h.Run().Step(stepDriven(t, h, tc.state)).State; got != tc.state {
				t.Fatalf("the case claims to drive the step to %s but it is %s", tc.state, got)
			}
			if err := h.Close(); err != nil {
				t.Fatal(err)
			}

			got, err := l.Orphans("default", "sandbox")
			if err != nil {
				t.Fatal(err)
			}
			if (len(got) == 1) != tc.want {
				t.Fatalf("Orphans = %v, want reported=%v", got, tc.want)
			}
		})
	}
	assertCoversEveryState(t, covered)
}

// stepDriven names the step a case's drive function acted on: the execute
// step for the states reached by AwaitConfirmation, Decline and Unreachable,
// and the simulate step for the ones reached by Begin and Record.
func stepDriven(t *testing.T, h *runs.Handle, state runs.State) string {
	t.Helper()
	if h.Run().Step(runs.StepExecute).State == state {
		return runs.StepExecute
	}
	return runs.StepSimulate
}

func recordSimulateWithExit(t *testing.T, h *runs.Handle, exit int) {
	t.Helper()
	if err := h.Begin(runs.StepSimulate, simulateBody); err != nil {
		t.Fatal(err)
	}
	if err := h.Record(runs.StepSimulate, &runs.Response{Status: 500}, &runs.Outcome{ExitCode: exit}); err != nil {
		t.Fatal(err)
	}
}

func TestOrphansFiltersByProfileAndEnvironment(t *testing.T) {
	l, _ := newLedger(t)

	mk := func(profile, env string) string {
		run := broadcastRun(t)
		run.Profile, run.Environment = profile, env
		h := mustCreate(t, l, run)
		if err := h.Begin(runs.StepSimulate, simulateBody); err != nil {
			t.Fatal(err)
		}
		if err := h.Close(); err != nil {
			t.Fatal(err)
		}
		return run.RunID
	}
	mine := mk("default", "sandbox")
	mk("other", "sandbox")
	mk("default", "live")

	got, err := l.Orphans("default", "sandbox")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].RunID != mine {
		t.Fatalf("Orphans = %v, want only %s", got, mine)
	}
	if got[0].Step != runs.StepSimulate || got[0].State != runs.StatePending {
		t.Fatalf("orphan describes %+v", got[0])
	}
}

// AC90/AC94: an exempt run id is never reported. The exemption carries the
// run a same-body --idempotency-key has just matched: that run *is* the one
// being resumed, and refusing it would make a legitimate resume need
// --allow-pending (V3). Mutation M109 ignores exempt.
func TestOrphansNeverReportsAnExemptRun(t *testing.T) {
	l, _ := newLedger(t)

	mk := func() string {
		run := broadcastRun(t)
		h := mustCreate(t, l, run)
		if err := h.Begin(runs.StepSimulate, simulateBody); err != nil {
			t.Fatal(err)
		}
		if err := h.Close(); err != nil {
			t.Fatal(err)
		}
		return run.RunID
	}
	resuming, other := mk(), mk()

	got, err := l.Orphans("default", "sandbox", resuming)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].RunID != other {
		t.Fatalf("Orphans = %v, want only %s (the exempt run must not be reported)", got, other)
	}

	// Without the exemption both are orphans, so the case above is not
	// passing because there was nothing to report.
	got, err = l.Orphans("default", "sandbox")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("without an exemption Orphans = %v, want both", got)
	}
}

// AC90: a run whose lock another process holds is not an orphan. Without
// this, two legitimate parallel transfers block each other and every
// concurrent caller reaches for --allow-pending. Mutation M90 (U5's
// pending_test) treats a locked run as an orphan; this is the ledger-side
// control for the same property.
func TestOrphansSkipsARunALiveProcessHolds(t *testing.T) {
	l, dir := newLedger(t)
	run := broadcastRun(t)
	h := mustCreate(t, l, run)
	if err := h.Begin(runs.StepSimulate, simulateBody); err != nil {
		t.Fatal(err)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}

	stop := startHelper(t, "hold-open", dir, run.RunID)

	got, err := l.Orphans("default", "sandbox")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("a run held by a live process was reported as an orphan: %v", got)
	}

	stop()

	got, err = l.Orphans("default", "sandbox")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("after the holder exited, Orphans = %v, want one", got)
	}
}

// AC90: the probe releases each lock before returning.
//
// This is the whole of V5: a probe that kept the locks it acquired would be
// indistinguishable, to a concurrent resume, from a live holder. Mutation
// M105 holds the lock and the Open below fails.
func TestOrphansReleasesEveryLockItTakes(t *testing.T) {
	l, _ := newLedger(t)
	var ids []string
	for i := 0; i < 3; i++ {
		run := broadcastRun(t)
		h := mustCreate(t, l, run)
		if err := h.Begin(runs.StepSimulate, simulateBody); err != nil {
			t.Fatal(err)
		}
		if err := h.Close(); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, run.RunID)
	}

	if _, err := l.Orphans("default", "sandbox"); err != nil {
		t.Fatal(err)
	}

	for _, id := range ids {
		h, err := l.Open(id)
		if err != nil {
			t.Fatalf("Open(%s) after the probe: %v (the probe did not release)", id, err)
		}
		if err := h.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

// The same property across processes: a probe running in a loop elsewhere
// must not be able to refuse a resume here.
func TestConcurrentProbesDoNotRefuseAResume(t *testing.T) {
	l, dir := newLedger(t)
	run := broadcastRun(t)
	h := mustCreate(t, l, run)
	if err := h.Begin(runs.StepSimulate, simulateBody); err != nil {
		t.Fatal(err)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}

	startHelper(t, "probe-orphans", dir, run.RunID)

	for i := 0; i < 20; i++ {
		h, err := l.Open(run.RunID)
		if err != nil {
			t.Fatalf("Open %d while another process probes: %v", i, err)
		}
		if err := h.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

// ---------------------------------------------------------------- AC92 ----

// AC92: Prune deletes the record before the sidecar, holding the sidecar lock
// through both.
//
// Mutation M107 unlinks the sidecar first, which leaves a record whose next
// holder creates a fresh inode to lock — so two processes could hold "the
// lock" for one run at the same time.
// newFaultLedger is the same ledger the other tests use, with the operation
// recorder in front of it so an ordering claim can be checked rather than
// asserted.
func newFaultLedger(t *testing.T) (*runs.Ledger, *fsx.Fault) {
	t.Helper()
	f := fsx.NewFault(fsx.OS())
	l, _ := newLedgerOn(t, f)
	return l, f
}

// prunableRun leaves a run every step of which is terminal, scrubbed, and
// old enough for the cutoffs below.
func prunableRun(t *testing.T, l *runs.Ledger) *runs.Run {
	t.Helper()
	run := broadcastRun(t)
	run.CreatedAt = time.Now().Add(-48 * time.Hour).UTC()
	h, err := l.Create(run)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Decline(runs.StepExecute); err != nil {
		t.Fatal(err)
	}
	if err := h.Record(runs.StepSimulate, &runs.Response{Status: 400}, &runs.Outcome{ExitCode: 3}); err != nil {
		t.Fatal(err)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	return run
}

func TestPruneUnlinksTheRecordBeforeTheSidecar(t *testing.T) {
	l, f := newFaultLedger(t)
	run := prunableRun(t, l)
	f.Reset()
	pruned, err := l.Prune(time.Now().Add(-24 * time.Hour))
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if len(pruned) != 1 || pruned[0] != run.RunID {
		t.Fatalf("Prune = %v", pruned)
	}

	var removals []string
	for _, op := range f.Ops() {
		if op.Name == "remove" {
			removals = append(removals, op.Path)
		}
	}
	want := []string{l.RecordPath(run.RunID), l.LockPath(run.RunID)}
	if len(removals) != 2 || removals[0] != want[0] || removals[1] != want[1] {
		t.Fatalf("removals:\n got %v\nwant %v (record first, then the sidecar)", removals, want)
	}

	for _, p := range want {
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s survived the prune", p)
		}
	}
}

// If the second unlink fails, the record is still gone. A sidecar with no
// record is inert; a record with no sidecar is a run whose lock means
// nothing.
func TestPruneLeavesNoRecordWhenTheSidecarUnlinkFails(t *testing.T) {
	l, f := newFaultLedger(t)
	run := prunableRun(t, l)

	f.Reset()
	f.FailAt("remove", 2, errors.New("EPERM"))
	if _, err := l.Prune(time.Now()); err == nil {
		t.Fatal("expected the injected sidecar-unlink failure")
	}
	if _, err := os.Stat(l.RecordPath(run.RunID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the record survived, so a failed prune leaves a run whose lock is meaningless")
	}
}

// AC57/AC92: prune removes only terminal, scrubbed records.
func TestPruneRemovesOnlyTerminalScrubbedRecords(t *testing.T) {
	old := time.Now().Add(-48 * time.Hour).UTC()

	// states is every state the case leaves a step of the run in. A prune
	// decision is over the whole run, not one step, so a case names a set --
	// and the union across cases is checked against the census, so a state
	// added later cannot go untested for prunability.
	cases := []struct {
		name   string
		states []runs.State
		drive  func(t *testing.T, h *runs.Handle)
		want   bool
	}{
		{"all terminal", []runs.State{runs.StateTerminal, runs.StateDeclined}, func(t *testing.T, h *runs.Handle) {
			recordSimulateWithExit(t, h, 3)
			if err := h.Decline(runs.StepExecute); err != nil {
				t.Fatal(err)
			}
		}, true},
		{"declined and unreachable", []runs.State{runs.StateTerminal, runs.StateUnreachable}, func(t *testing.T, h *runs.Handle) {
			recordSimulateWithExit(t, h, 4)
			if err := h.Unreachable(runs.StepExecute); err != nil {
				t.Fatal(err)
			}
		}, true},
		{"awaiting_confirmation is not terminal", []runs.State{runs.StateAwaitingConfirmation}, func(t *testing.T, h *runs.Handle) {
			recordSimulateWithExit(t, h, 0)
			if err := h.AwaitConfirmation(runs.StepExecute); err != nil {
				t.Fatal(err)
			}
		}, false},
		{"pending is not terminal", []runs.State{runs.StatePending}, func(t *testing.T, h *runs.Handle) {
			if err := h.Begin(runs.StepSimulate, simulateBody); err != nil {
				t.Fatal(err)
			}
		}, false},
		{"answered class 6 is not terminal", []runs.State{runs.StateAnswered}, func(t *testing.T, h *runs.Handle) {
			recordSimulateWithExit(t, h, 6)
		}, false},
		{"not_started is not terminal", []runs.State{runs.StateNotStarted}, func(t *testing.T, h *runs.Handle) {}, false},
	}

	covered := map[runs.State]bool{}
	for _, tc := range cases {
		for _, s := range tc.states {
			covered[s] = true
		}
	}
	assertCoversEveryState(t, covered)

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l, _ := newLedger(t)
			run := broadcastRun(t)
			run.CreatedAt = old
			h, err := l.Create(run)
			if err != nil {
				t.Fatal(err)
			}
			tc.drive(t, h)

			// The case must have reached the states it names, or the coverage
			// check above is satisfied by a label rather than a behaviour.
			reached := map[runs.State]bool{}
			for _, s := range h.Run().Steps {
				reached[s.State] = true
			}
			for _, want := range tc.states {
				if !reached[want] {
					t.Fatalf("the case claims to reach %s but the steps are in %v", want, reached)
				}
			}
			if err := h.Close(); err != nil {
				t.Fatal(err)
			}

			pruned, err := l.Prune(time.Now())
			if err != nil {
				t.Fatal(err)
			}
			if got := len(pruned) == 1; got != tc.want {
				t.Fatalf("Prune = %v, want pruned=%v", pruned, tc.want)
			}
		})
	}
}

func TestPruneLeavesRecordsNewerThanTheCutoff(t *testing.T) {
	l, _ := newLedger(t)
	run := broadcastRun(t)
	run.CreatedAt = time.Now().UTC()
	h, err := l.Create(run)
	if err != nil {
		t.Fatal(err)
	}
	recordSimulateWithExit(t, h, 3)
	if err := h.Decline(runs.StepExecute); err != nil {
		t.Fatal(err)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}

	pruned, err := l.Prune(time.Now().Add(-24 * time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(pruned) != 0 {
		t.Fatalf("Prune removed a record newer than the cutoff: %v", pruned)
	}
}

// A terminal record still holding a live token is not prunable: pruning it
// would delete the evidence without the scrub ever having run.
func TestPruneRefusesATerminalRecordStillHoldingAToken(t *testing.T) {
	l, _ := newLedger(t)
	run := broadcastRun(t)
	run.CreatedAt = time.Now().Add(-48 * time.Hour).UTC()
	h, err := l.Create(run)
	if err != nil {
		t.Fatal(err)
	}
	recordSimulateWithExit(t, h, 3)
	if err := h.Decline(runs.StepExecute); err != nil {
		t.Fatal(err)
	}
	// Force a live token back onto the terminal step, which is what mutation
	// M115 does deliberately.
	exec := h.Run().Step(runs.StepExecute)
	tok := planToken
	exec.PlanToken = &tok
	if err := h.Save(); err != nil {
		t.Fatal(err)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}

	pruned, err := l.Prune(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(pruned) != 0 {
		t.Fatalf("a record holding a live token was pruned: %v", pruned)
	}
}

func TestPruneSkipsALockedRun(t *testing.T) {
	l, dir := newLedger(t)
	run := prunableRun(t, l)

	stop := startHelper(t, "hold-open", dir, run.RunID)
	pruned, err := l.Prune(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(pruned) != 0 {
		t.Fatalf("Prune removed a run a live process holds: %v", pruned)
	}
	stop()
}

// AC92: Decline writes declined and scrubs for a step in
// awaiting_confirmation or not_started, and refuses anything in pending or
// later with ErrStepMayHaveSent. Mutation M103 accepts a pending step.
func TestDeclineRefusesAStepThatMayHaveSent(t *testing.T) {
	cases := []struct {
		name  string
		state runs.State
		drive func(t *testing.T, h *runs.Handle)
		want  error
	}{
		{"not_started", runs.StateNotStarted, func(t *testing.T, h *runs.Handle) {}, nil},
		{"awaiting_confirmation", runs.StateAwaitingConfirmation, func(t *testing.T, h *runs.Handle) {
			if err := h.AwaitConfirmation(runs.StepExecute); err != nil {
				t.Fatal(err)
			}
		}, nil},
		{"pending", runs.StatePending, func(t *testing.T, h *runs.Handle) {
			if err := h.Begin(runs.StepExecute, executeBody()); err != nil {
				t.Fatal(err)
			}
		}, runs.ErrStepMayHaveSent},
		{"answered", runs.StateAnswered, func(t *testing.T, h *runs.Handle) {
			if err := h.Begin(runs.StepExecute, executeBody()); err != nil {
				t.Fatal(err)
			}
			if err := h.Record(runs.StepExecute, &runs.Response{Status: 503}, &runs.Outcome{ExitCode: 6}); err != nil {
				t.Fatal(err)
			}
		}, runs.ErrStepMayHaveSent},
		{"terminal", runs.StateTerminal, func(t *testing.T, h *runs.Handle) {
			if err := h.Begin(runs.StepExecute, executeBody()); err != nil {
				t.Fatal(err)
			}
			if err := h.Record(runs.StepExecute, &runs.Response{Status: 201}, &runs.Outcome{ExitCode: 0}); err != nil {
				t.Fatal(err)
			}
		}, runs.ErrStepMayHaveSent},
		// The two already-resolved states. Both are no-ops rather than
		// errors: declining a run twice must not fail, and a step that can
		// never legitimately start needs nothing declined. They were missing
		// until the census check demanded them.
		{"declined", runs.StateDeclined, func(t *testing.T, h *runs.Handle) {
			if err := h.Decline(runs.StepExecute); err != nil {
				t.Fatal(err)
			}
		}, nil},
		{"unreachable", runs.StateUnreachable, func(t *testing.T, h *runs.Handle) {
			if err := h.Unreachable(runs.StepExecute); err != nil {
				t.Fatal(err)
			}
		}, nil},
	}

	covered := map[runs.State]bool{}
	for _, tc := range cases {
		covered[tc.state] = true
	}
	assertCoversEveryState(t, covered)

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l, _ := newLedger(t)
			run := broadcastRun(t)
			h, err := l.Create(run)
			if err != nil {
				t.Fatal(err)
			}
			if err := h.SetPlan(runs.StepExecute, &runs.Plan{ID: "p", ExpiresAt: time.Now().Add(time.Hour).UTC()}, planToken); err != nil {
				t.Fatal(err)
			}
			tc.drive(t, h)
			if err := h.Close(); err != nil {
				t.Fatal(err)
			}

			got, err := l.Decline(run.RunID)
			if !errors.Is(err, tc.want) {
				t.Fatalf("Decline err = %v, want %v", err, tc.want)
			}
			if tc.want != nil {
				// And the refusal changed nothing.
				after, lerr := l.Load(run.RunID)
				if lerr != nil {
					t.Fatal(lerr)
				}
				if after.Step(runs.StepExecute).State == runs.StateDeclined {
					t.Fatal("a step that may have sent was declined anyway")
				}
				return
			}
			// Decline succeeded. The step must now be resolved without having
			// sent, and hold no live token.
			//
			// An already-resolved step keeps the state it had: declining an
			// unreachable step is a no-op, because a step that can never
			// legitimately start has nothing to decline. Asserting `declined`
			// for every success would have been asserting the mechanism
			// instead of the property.
			after := got.Step(runs.StepExecute)
			wantState := runs.StateDeclined
			if tc.state == runs.StateUnreachable {
				wantState = runs.StateUnreachable
			}
			if after.State != wantState {
				t.Fatalf("state = %s, want %s", after.State, wantState)
			}
			if !after.State.Terminal() {
				t.Fatalf("state %s is not terminal, so the run is still an orphan", after.State)
			}
			if after.State.MayHaveSent() {
				t.Fatalf("state %s reports the request may have been sent, which Decline "+
					"must never produce", after.State)
			}
			if after.HasPlanToken() {
				t.Fatal("decline did not scrub the token")
			}
		})
	}
}

// A headless caller must be able to resolve an awaiting_confirmation run
// without a terminal: this is V4's remedy for a prompt killed by SIGKILL.
func TestDeclineClearsAnAwaitingConfirmationOrphan(t *testing.T) {
	l, _ := newLedger(t)
	run := broadcastRun(t)
	h, err := l.Create(run)
	if err != nil {
		t.Fatal(err)
	}
	recordSimulateWithExit(t, h, 0)
	if err := h.SetPlan(runs.StepExecute, &runs.Plan{ID: "p", ExpiresAt: time.Now().Add(time.Hour).UTC()}, planToken); err != nil {
		t.Fatal(err)
	}
	if err := h.AwaitConfirmation(runs.StepExecute); err != nil {
		t.Fatal(err)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}

	orphans, err := l.Orphans("default", "sandbox")
	if err != nil {
		t.Fatal(err)
	}
	if len(orphans) != 1 {
		t.Fatalf("the run is not an orphan to begin with: %v", orphans)
	}

	if _, err := l.Decline(run.RunID); err != nil {
		t.Fatalf("Decline: %v", err)
	}

	orphans, err = l.Orphans("default", "sandbox")
	if err != nil {
		t.Fatal(err)
	}
	if len(orphans) != 0 {
		t.Fatalf("the run is still an orphan after being declined: %v", orphans)
	}
	if runs.ContainsPlanToken(readRaw(t, l, run.RunID)) {
		t.Fatal("the declined run still holds a plan token on disk")
	}
}

func TestDeclineIsIdempotent(t *testing.T) {
	l, _ := newLedger(t)
	run := broadcastRun(t)
	h, err := l.Create(run)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Decline(run.RunID); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Decline(run.RunID); err != nil {
		t.Fatalf("second Decline: %v", err)
	}
}

func TestDeclineOfALockedRunIsRefused(t *testing.T) {
	l, dir := newLedger(t)
	run := broadcastRun(t)
	h, err := l.Create(run)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}

	startHelper(t, "hold-open", dir, run.RunID)
	if _, err := l.Decline(run.RunID); !errors.Is(err, runs.ErrRunLocked) {
		t.Fatalf("err = %v, want ErrRunLocked", err)
	}
}
