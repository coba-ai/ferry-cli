package runs_test

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kurenn/ferry-cli/internal/cli"
	"github.com/kurenn/ferry-cli/internal/fsx"
	"github.com/kurenn/ferry-cli/internal/harness"
	"github.com/kurenn/ferry-cli/internal/noun"
	"github.com/kurenn/ferry-cli/internal/runs"
)

// AC57's other two claims — the ones about removing a record and refusing a
// decline. Both defend C16, and both are the same question asked from
// opposite ends: **can a plan nobody has agreed to be disposed of, or
// agreed to, by something other than a human at a prompt?**
//
// `prune` is the disposal side. A record still `awaiting_confirmation` is a
// decision nobody has made; removing it makes it a decision nobody *can*
// make, and the run stops appearing in `runs list`, so the person who would
// have made it never learns it was there.
//
// `decline` is the agreement side inverted. It writes "no", and only while
// "no" is still true: once `Begin` has run, a request under that key may be
// at FERRY, and turning an unknown into a "no" is the one lie this CLI must
// never tell.

const fixedNowRFC = "2026-02-01T00:00:00Z"

func fixedNow(t *testing.T) time.Time {
	t.Helper()

	parsed, err := time.Parse(time.RFC3339, fixedNowRFC)
	if err != nil {
		t.Fatalf("parse the fixed clock: %v", err)
	}

	return parsed.UTC()
}

// record is one run to arrange on disk.
type record struct {
	id    string
	state runs.State
	age   time.Duration
	token string
}

// homeWith writes a credentials file and arranges the runs a case needs.
//
// The runs go in through `runs.Create` and then have their step state set,
// rather than being written as JSON: the ledger validates a new run, and a
// record this test hand-wrote could be one the ledger would never produce
// — which would make every assertion below about a file that cannot exist.
func homeWith(t *testing.T, records ...record) string {
	t.Helper()

	home := t.TempDir()

	creds := `{"schema_version":1,"profiles":{"default":{"api_url":"https://api.example",` +
		`"credentials":[{"kind":"api_key","value":"ferry_sk_test_` + strings.Repeat("a", 32) +
		`","environment":"sandbox"}]}}}`

	if err := os.WriteFile(filepath.Join(home, "credentials.json"), []byte(creds), 0o600); err != nil {
		t.Fatalf("write credentials: %v", err)
	}

	ledger := ledgerAt(t, home)

	for _, r := range records {
		run := &runs.Run{
			SchemaVersion: runs.Schema,
			RunID:         r.id,
			Profile:       "default",
			APIURL:        "https://api.example",
			Environment:   "sandbox",
			PrincipalID:   "key_1",
			CreatedAt:     fixedNow(t).Add(-r.age),
			Argv:          []string{"ferry", "transfers", "execute"},
			Steps: []*runs.Step{{
				Name:           runs.StepExecute,
				Operation:      "transfers_execute",
				Method:         "POST",
				Path:           "/v1/transfers",
				IdempotencyKey: r.id + "-execute",
				State:          runs.StateNotStarted,
			}},
		}

		handle, err := ledger.Create(run)
		if err != nil {
			t.Fatalf("create run %s: %v", r.id, err)
		}

		// A step that may have sent reaches that state through `Begin`,
		// which is the only thing that pins a body and its digest — and
		// the ledger refuses a record with one and not the other. Going
		// through the real entry point is also the point: a state this
		// arrangement could reach and the CLI could not would make every
		// assertion below about a record that cannot exist.
		if r.state.MayHaveSent() {
			if err := handle.Begin(runs.StepExecute, []byte(`{"plan_token":"x"}`)); err != nil {
				t.Fatalf("begin run %s: %v", r.id, err)
			}
		}

		if r.state == runs.StateAwaitingConfirmation {
			if err := handle.AwaitConfirmation(runs.StepExecute); err != nil {
				t.Fatalf("await confirmation on %s: %v", r.id, err)
			}
		}

		step := handle.Run().Steps[0]
		step.State = r.state

		if r.token != "" {
			token := r.token
			step.PlanToken = &token
		}

		if err := handle.Save(); err != nil {
			t.Fatalf("save run %s: %v", r.id, err)
		}

		if err := handle.Close(); err != nil {
			t.Fatalf("close run %s: %v", r.id, err)
		}
	}

	return home
}

func ledgerAt(t *testing.T, home string) *runs.Ledger {
	t.Helper()

	return runs.New(fsx.OS(), filepath.Join(home, "runs"), func() time.Time { return fixedNow(t) })
}

func runCLI(t *testing.T, home string, args ...string) (stdout, stderr string, exit int) {
	t.Helper()

	cmd := cli.New(cli.Options{
		Noun: noun.Deps{HTTP: &http.Client{}, Now: func() time.Time { return fixedNow(t) }},
		Args: args,
	})

	return harness.Run(t, cmd, args, "", map[string]string{"FERRY_HOME": home}, false)
}

func onDisk(t *testing.T, home, id string) bool {
	t.Helper()

	_, err := ledgerAt(t, home).Load(id)

	return err == nil
}

// AC57 — `runs prune --older-than` removes only terminal, scrubbed records.
//
// The table is the census: **every state `runs.AllStates()` declares** gets
// a row, both directions. The ledger holds `AllStates()` against its own
// AST precisely so a new state cannot be added quietly, and that is only
// worth doing if the consumers are held to the census too — otherwise a new
// state arrives and silently takes whichever branch it falls into here.
func TestPruneRemovesOnlySettledScrubbedRecords(t *testing.T) {
	want := map[runs.State]bool{
		runs.StateNotStarted:           false,
		runs.StateAwaitingConfirmation: false,
		runs.StateDeclined:             true,
		runs.StatePending:              false,
		runs.StateAnswered:             false,
		runs.StateTerminal:             true,
		runs.StateUnreachable:          true,
	}

	declared := runs.AllStates()

	if len(declared) == 0 {
		t.Fatal("runs.AllStates() is empty; this table would be vacuous")
	}

	for _, state := range declared {
		if _, named := want[state]; !named {
			t.Errorf("runs.AllStates() declares %q and this table does not name it. Say "+
				"whether a record in that state may be pruned.", state)
		}
	}

	for state := range want {
		if !containsState(declared, state) {
			t.Errorf("this table names %q, which runs.AllStates() no longer declares", state)
		}
	}

	if t.Failed() {
		return
	}

	// One home, every state in it, one prune. Running them together is
	// what makes the excluded rows mean something: a prune that removed
	// nothing at all would pass a test that only checked exclusions.
	var arranged []record

	ids := map[runs.State]string{}

	for i, state := range declared {
		id := runID(i)
		ids[state] = id

		arranged = append(arranged, record{id: id, state: state, age: 60 * 24 * time.Hour})
	}

	home := homeWith(t, arranged...)

	stdout, stderr, exit := runCLI(t, home, "runs", "prune", "--older-than", "30d")

	if exit != 0 {
		t.Fatalf("exit = %d, want 0\n%s\n%s", exit, stdout, stderr)
	}

	for state, shouldGo := range want {
		id := ids[state]

		switch {
		case shouldGo && onDisk(t, home, id):
			t.Errorf("a %s run survived the prune; it is terminal and holds no token, so "+
				"nothing is waiting on it", state)
		case !shouldGo && !onDisk(t, home, id):
			t.Errorf("a %s run was pruned. The record is the only place the run exists, "+
				"and a caller who needs to resume or decline it now cannot.", state)
		}
	}
}

// What `runs prune` does with a record that still holds a plan token.
//
// The answer is not the one this test first asserted, and the correction is
// worth writing down: **`prune` scrubs before it prunes.** So a *terminal*
// record holding a token does not survive — the token is removed first,
// which makes the record prunable, and it goes in the same invocation.
// `Prunable`'s "and holds no token" condition is therefore unreachable
// through this verb by way of a terminal step, because a terminal step's
// token is exactly what `Scrub` takes.
//
// That ordering is the right one and this test pins it, because the reverse
// would be a real defect: pruning first would delete records without ever
// looking at what was in them, and `Scrub`'s census of where tokens hide is
// the only thing that knows. What must not happen is a token surviving in a
// record that is then left on disk, or a record disappearing while a token
// of its own is still live — and those are the two things asserted here.
func TestPruneScrubsBeforeItPrunes(t *testing.T) {
	token := "ferry_plan_" + strings.Repeat("B", 32)

	settled := "01JQBN8Z5K00000000000000T1"
	unsettled := "01JQBN8Z5K00000000000000T2"

	home := homeWith(t,
		record{id: settled, state: runs.StateTerminal, age: 60 * 24 * time.Hour, token: token},
		record{id: unsettled, state: runs.StateAnswered, age: 60 * 24 * time.Hour, token: token},
	)

	stdout, stderr, exit := runCLI(t, home, "runs", "prune", "--older-than", "30d")

	if exit != 0 {
		t.Fatalf("exit = %d, want 0\n%s\n%s", exit, stdout, stderr)
	}

	// The terminal one: scrubbed, then pruned. It is gone, and the token
	// went with it.
	if onDisk(t, home, settled) {
		t.Error("a terminal record survived the prune. Its token is scrubbed first, which " +
			"is what makes it prunable.")
	}

	// The unresolved one: neither scrubbed nor pruned. Its step may have
	// sent and has not settled, so the token is still the thing a resume
	// would need.
	if !onDisk(t, home, unsettled) {
		t.Fatal("an unresolved record was pruned; a resume can no longer find it")
	}

	after, err := ledgerAt(t, home).Load(unsettled)
	if err != nil {
		t.Fatalf("load %s: %v", unsettled, err)
	}

	if !after.Steps[0].HasPlanToken() {
		t.Error("the unresolved record's plan token was scrubbed. The step has not settled, " +
			"so the token is what a resume would send.")
	}

	// And nothing that went is still on disk under another name — the
	// sweep, rather than a field check, because a token can hide in
	// `body`, `argv`, a response header or a field added later.
	for _, name := range filesIn(t, filepath.Join(home, "runs")) {
		raw, readErr := os.ReadFile(name)
		if readErr != nil {
			t.Fatalf("read %s: %v", name, readErr)
		}

		if strings.Contains(string(raw), token) && !strings.Contains(name, unsettled) {
			t.Errorf("%s still holds the plan token after a prune", name)
		}
	}
}

// filesIn lists the ledger directory.
func filesIn(t *testing.T, dir string) []string {
	t.Helper()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}

	var out []string

	for _, e := range entries {
		if !e.IsDir() {
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}

	return out
}

// `--older-than` is honoured, in both directions.
func TestPruneHonoursTheAge(t *testing.T) {
	young := "01JQBN8Z5K00000000000000Y1"
	old := "01JQBN8Z5K00000000000000Y2"

	home := homeWith(t,
		record{id: young, state: runs.StateTerminal, age: 24 * time.Hour},
		record{id: old, state: runs.StateTerminal, age: 60 * 24 * time.Hour},
	)

	stdout, stderr, exit := runCLI(t, home, "runs", "prune", "--older-than", "30d")

	if exit != 0 {
		t.Fatalf("exit = %d, want 0\n%s\n%s", exit, stdout, stderr)
	}

	if !onDisk(t, home, young) {
		t.Error("a one-day-old settled run was pruned by --older-than 30d")
	}

	if onDisk(t, home, old) {
		t.Error("a sixty-day-old settled run survived --older-than 30d")
	}

	// The report names what went, so a caller can see it rather than
	// diffing a directory.
	if !strings.Contains(stdout, old) {
		t.Errorf("the prune does not name the run it removed\n%s", stdout)
	}

	if strings.Contains(stdout, young) {
		t.Errorf("the prune names a run it did not remove\n%s", stdout)
	}
}

// AC57 — `runs decline <id>` writes `declined` for an execute step that has
// not run `Begin`, and refuses one that has, naming `runs resume`.
//
// This is C16 at its sharpest. `awaiting_confirmation` is written *before*
// the prompt and authorises nothing, so `decline` is how a headless machine
// records the "no" a killed prompt never collected. The refusal is the
// other half of the same rule: `MayHaveSent()` is the predicate, and where
// it is true the honest answer is "find out", not "no".
func TestDeclineWritesNoOnlyWhileNoIsStillTrue(t *testing.T) {
	const id = "01JQBN8Z5K00000000000000D1"

	token := "ferry_plan_" + strings.Repeat("C", 32)

	for _, tc := range []struct {
		name     string
		state    runs.State
		declined bool
		mentions []string
	}{
		{name: "awaiting confirmation", state: runs.StateAwaitingConfirmation, declined: true},
		{name: "not started", state: runs.StateNotStarted, declined: true},
		{name: "pending", state: runs.StatePending, mentions: []string{"runs resume"}},
		{name: "answered", state: runs.StateAnswered, mentions: []string{"runs resume"}},
		{name: "terminal", state: runs.StateTerminal, mentions: []string{"runs resume"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The claim and the ledger's own predicate must agree. If
			// they ever disagree this test is asserting a rule the
			// ledger does not hold, and the ledger is the authority.
			if tc.declined == tc.state.MayHaveSent() {
				t.Fatalf("this row says declinable=%v for %s, whose MayHaveSent() is %v",
					tc.declined, tc.state, tc.state.MayHaveSent())
			}

			home := homeWith(t, record{id: id, state: tc.state, age: time.Hour, token: token})

			stdout, stderr, exit := runCLI(t, home, "runs", "decline", id)

			after, err := ledgerAt(t, home).Load(id)
			if err != nil {
				t.Fatalf("the run is gone after a decline: %v", err)
			}

			got := after.Steps[0].State

			if !tc.declined {
				if exit == 0 {
					t.Fatalf("declining a %s step succeeded. A request under this key may "+
						"already be at FERRY, so \"no\" is not an available answer.\n%s",
						tc.state, stdout)
				}

				if got != tc.state {
					t.Errorf("the refused decline changed the step from %q to %q",
						tc.state, got)
				}

				report := stdout + stderr

				for _, want := range tc.mentions {
					if !strings.Contains(report, want) {
						t.Errorf("the refusal does not name %q, so the caller is told no "+
							"and not told what to do instead\n%s", want, report)
					}
				}

				return
			}

			if exit != 0 {
				t.Fatalf("exit = %d, want 0\n%s\n%s", exit, stdout, stderr)
			}

			if got != runs.StateDeclined {
				t.Errorf("the step is %q after a decline, want declined", got)
			}

			// And the token goes with the "no". A declined plan is one a
			// human refused, so the token is still live — the one case
			// where leaving it on disk leaves an executable transfer
			// somebody said no to.
			if after.Steps[0].HasPlanToken() {
				t.Error("a declined step still holds its plan token, which is live: the " +
					"human refused it, so nothing spent or expired it")
			}
		})
	}
}

func containsState(states []runs.State, want runs.State) bool {
	for _, s := range states {
		if s == want {
			return true
		}
	}

	return false
}

// runID is a valid ULID per index. `runs.Create` validates the shape, so a
// made-up string would fail arrangement rather than the assertion.
func runID(n int) string {
	id := "01JQBN8Z5K" + strings.Repeat("0", 15) + string(rune('A'+n))

	if len(id) != 26 {
		panic("runID is not 26 characters: " + id)
	}

	return id
}
