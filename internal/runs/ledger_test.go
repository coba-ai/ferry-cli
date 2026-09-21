package runs_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kurenn/ferry/cli/internal/fsx"
	"github.com/kurenn/ferry/cli/internal/runs"
	"github.com/kurenn/ferry/cli/internal/ulid"
)

const (
	planToken   = "ferry_plan_01JAAAAAAAAAAAAAAAAAAAAAAAxyzTOKEN"
	simulateURL = "/v1/transfers/simulate"
	executeURL  = "/v1/transfers"
)

var simulateBody = []byte(`{"customer_id":"cus_1","amount":{"value":"100.00","side":"source"}}`)

func executeBody() []byte {
	return []byte(`{"plan_token":"` + planToken + `"}`)
}

// The canary must be something the sweep would actually find; a fixture that
// does not look like a plan token would make every "no token on disk"
// assertion below pass for the wrong reason.
func init() {
	if !runs.ContainsPlanToken([]byte(planToken)) {
		panic("the test's plan token fixture is not token-shaped; the AC82 sweep would be vacuous")
	}
}

func newLedger(t *testing.T) (*runs.Ledger, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "runs")
	return runs.New(fsx.OS(), dir, nil), dir
}

func newLedgerOn(t *testing.T, filesystem fsx.FS) (*runs.Ledger, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "runs")
	return runs.New(filesystem, dir, nil), dir
}

// broadcastRun is the shape PLAN §5.3 draws: two steps, both keys minted
// before the first send.
func broadcastRun(t *testing.T) *runs.Run {
	t.Helper()
	id, err := ulid.New()
	if err != nil {
		t.Fatal(err)
	}
	return &runs.Run{
		RunID:           id,
		CLIVersion:      "0.1.0",
		Profile:         "default",
		APIURL:          "https://ferry.example",
		Environment:     "sandbox",
		CredentialClass: "api_key",
		TokenPrefix:     "ferry_sk_sandbox_grpQ0HNk",
		PrincipalID:     "key_01j",
		Argv:            []string{"transfers", "create", "--broadcast"},
		Steps: []*runs.Step{
			{
				Name: runs.StepSimulate, Operation: runs.OpSimulate,
				Method: "POST", Path: simulateURL,
				IdempotencyKey: runs.StepKey(id, runs.StepSimulate),
			},
			{
				Name: runs.StepExecute, Operation: runs.OpExecute,
				Method: "POST", Path: executeURL,
				IdempotencyKey: runs.StepKey(id, runs.StepExecute),
			},
		},
	}
}

func mustCreate(t *testing.T, l *runs.Ledger, run *runs.Run) *runs.Handle {
	t.Helper()
	h, err := l.Create(run)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = h.Close() })
	return h
}

func readRaw(t *testing.T, l *runs.Ledger, runID string) []byte {
	t.Helper()
	b, err := os.ReadFile(l.RecordPath(runID))
	if err != nil {
		t.Fatalf("read record: %v", err)
	}
	return b
}

// ---------------------------------------------------------------- AC10 ----

// AC10: Begin writes the record before returning, in the order write-temp ->
// fsync -> rename -> fsync(dir), mode 0600.
//
// Mutation M2 renames before the fsync and is killed by the order assertion.
func TestBeginWritesInTheOrderWriteFsyncRenameFsyncdir(t *testing.T) {
	f := fsx.NewFault(fsx.OS())
	l, _ := newLedgerOn(t, f)
	h := mustCreate(t, l, broadcastRun(t))

	f.Reset()
	if err := h.Begin(runs.StepSimulate, simulateBody); err != nil {
		t.Fatalf("Begin: %v", err)
	}

	want := []string{fsx.OpCreate, fsx.OpWrite, fsx.OpSync, fsx.OpClose, fsx.OpRename, fsx.OpSyncDir}
	if got := f.Names(); !slices.Equal(got, want) {
		t.Fatalf("Begin syscall order:\n got %v\nwant %v", got, want)
	}

	ops := f.Ops()
	if ops[0].Perm != 0o600 {
		t.Fatalf("record created with mode %#o, want 0600", ops[0].Perm)
	}
	if ops[4].Path != l.RecordPath(h.Run().RunID) {
		t.Fatalf("rename target %s, want the record path", ops[4].Path)
	}
	info, err := os.Stat(l.RecordPath(h.Run().RunID))
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("record mode %#o, want 0600", got)
	}
}

// AC10 / C1: when Begin returns, the key and the bytes are readable by a
// process that knows nothing about this one. This is the whole claim: the
// caller sends the first byte only after this function has returned.
func TestBeginIsDurableBeforeItReturns(t *testing.T) {
	l, _ := newLedger(t)
	h := mustCreate(t, l, broadcastRun(t))
	runID := h.Run().RunID

	if err := h.Begin(runs.StepSimulate, simulateBody); err != nil {
		t.Fatalf("Begin: %v", err)
	}

	// Read the bytes on disk directly, not through the handle's memory.
	var onDisk runs.Run
	if err := json.Unmarshal(readRaw(t, l, runID), &onDisk); err != nil {
		t.Fatalf("parse record: %v", err)
	}
	s := onDisk.Step(runs.StepSimulate)
	if s.State != runs.StatePending {
		t.Errorf("state on disk = %s, want pending", s.State)
	}
	if s.Attempts != 1 {
		t.Errorf("attempts on disk = %d, want 1; a pending step with attempts 0 is a record that lies", s.Attempts)
	}
	if string(s.Body) != string(simulateBody) {
		t.Errorf("body on disk = %q", s.Body)
	}
	if s.BodySHA256 == nil || *s.BodySHA256 != runs.Body(simulateBody).SHA256() {
		t.Errorf("digest on disk = %v", s.BodySHA256)
	}
	if s.IdempotencyKey != runs.StepKey(runID, runs.StepSimulate) {
		t.Errorf("key on disk = %q", s.IdempotencyKey)
	}
}

// AC10: a failure at any step of the write leaves no record at the final
// path. This is what lets PLAN §5.4 step 9 say "exit 1, nothing sent".
func TestCreateFailureLeavesNoRecord(t *testing.T) {
	for _, op := range []string{fsx.OpCreate, fsx.OpWrite, fsx.OpSync, fsx.OpClose, fsx.OpRename} {
		t.Run(op, func(t *testing.T) {
			f := fsx.NewFault(fsx.OS())
			l, _ := newLedgerOn(t, f)
			run := broadcastRun(t)
			f.FailAt(op, 1, fsx.ENOSPC)

			h, err := l.Create(run)
			if err == nil {
				_ = h.Close()
				t.Fatalf("Create succeeded with %s failing", op)
			}
			if _, statErr := os.Stat(l.RecordPath(run.RunID)); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("a record exists after a failed Create (%v)", statErr)
			}
		})
	}
}

// A Begin that fails must leave the *previous* record intact, not a partial
// one: the previous record is what a resume reads.
func TestBeginFailureLeavesThePreviousRecordIntact(t *testing.T) {
	f := fsx.NewFault(fsx.OS())
	l, _ := newLedgerOn(t, f)
	h := mustCreate(t, l, broadcastRun(t))
	runID := h.Run().RunID

	if err := h.Begin(runs.StepSimulate, simulateBody); err != nil {
		t.Fatal(err)
	}
	before := readRaw(t, l, runID)

	f.Reset()
	f.FailAt(fsx.OpSync, 1, fsx.EIO)
	err := h.Begin(runs.StepExecute, executeBody())
	if !errors.Is(err, fsx.EIO) {
		t.Fatalf("Begin err = %v, want the injected EIO", err)
	}
	if after := readRaw(t, l, runID); string(after) != string(before) {
		t.Fatalf("the record changed despite a failed write:\nbefore %s\nafter  %s", before, after)
	}

	// And the handle's memory agrees with the disk, so a caller that logs the
	// record after a failure does not report an attempt that never happened.
	if got := h.Run().Step(runs.StepExecute).Attempts; got != 0 {
		t.Fatalf("attempts in memory = %d after a failed Begin, want 0", got)
	}
	if got := h.Run().Step(runs.StepExecute).State; got != runs.StateNotStarted {
		t.Fatalf("state in memory = %s after a failed Begin, want not_started", got)
	}
}

// A read-only filesystem and a full disk are the same refusal: nothing sent.
func TestCreateOnAReadOnlyFilesystemRefusesBeforeAnythingIsSent(t *testing.T) {
	f := fsx.NewFault(fsx.OS())
	l, _ := newLedgerOn(t, f)
	f.FailAt(fsx.OpCreate, 1, fsx.EROFS)

	if _, err := l.Create(broadcastRun(t)); !errors.Is(err, fsx.EROFS) {
		t.Fatalf("err = %v, want EROFS", err)
	}
}

func TestBeginIncrementsAttemptsOnEveryResend(t *testing.T) {
	l, _ := newLedger(t)
	h := mustCreate(t, l, broadcastRun(t))

	if err := h.Begin(runs.StepSimulate, simulateBody); err != nil {
		t.Fatal(err)
	}
	if got := h.Run().Step(runs.StepSimulate).Attempts; got != 1 {
		t.Fatalf("attempts after the first Begin = %d, want 1", got)
	}

	// A resend passes no body: the bytes and the key are already on disk and
	// a resend that re-derived them could send something else.
	for want := 2; want <= 3; want++ {
		if err := h.Begin(runs.StepSimulate, nil); err != nil {
			t.Fatal(err)
		}
		if got := h.Run().Step(runs.StepSimulate).Attempts; got != want {
			t.Fatalf("attempts = %d, want %d", got, want)
		}
		if got := string(h.Run().Step(runs.StepSimulate).Body); got != string(simulateBody) {
			t.Fatalf("a resend changed the body to %q", got)
		}
	}
}

// C2: one intent, one key. A Begin offering different bytes under a key the
// ledger already holds is refused before anything is sent.
func TestBeginRefusesADifferentBodyUnderTheSameKey(t *testing.T) {
	l, _ := newLedger(t)
	h := mustCreate(t, l, broadcastRun(t))

	if err := h.Begin(runs.StepSimulate, simulateBody); err != nil {
		t.Fatal(err)
	}
	err := h.Begin(runs.StepSimulate, []byte(`{"customer_id":"cus_2"}`))
	if !errors.Is(err, runs.ErrBodyMismatch) {
		t.Fatalf("err = %v, want ErrBodyMismatch", err)
	}
	if string(h.Run().Step(runs.StepSimulate).Body) != string(simulateBody) {
		t.Fatal("the stored body was replaced by the refused one")
	}
}

func TestBeginRefusesATerminalStep(t *testing.T) {
	l, _ := newLedger(t)
	h := mustCreate(t, l, broadcastRun(t))

	if err := h.Decline(runs.StepExecute); err != nil {
		t.Fatal(err)
	}
	if err := h.Begin(runs.StepExecute, executeBody()); !errors.Is(err, runs.ErrBadTransition) {
		t.Fatalf("err = %v, want ErrBadTransition", err)
	}
}

// A body that is not valid UTF-8 cannot round-trip through the record, so it
// is refused at Begin rather than silently altered on disk. FERRY would
// refuse it too: a JSON document is UTF-8.
func TestBeginRefusesANonUTF8Body(t *testing.T) {
	l, _ := newLedger(t)
	h := mustCreate(t, l, broadcastRun(t))

	err := h.Begin(runs.StepSimulate, []byte{'{', '"', 'a', '"', ':', '"', 0xff, '"', '}'})
	if !errors.Is(err, runs.ErrBodyNotUTF8) {
		t.Fatalf("err = %v, want ErrBodyNotUTF8", err)
	}
}

// ---------------------------------------------------------------- AC11 ----

// AC11: Open yields exactly the stored body bytes and key.
//
// "Exactly" is the assertion, and the fixture is deliberately non-canonical:
// PLAN §5.3's record schema embeds the body as JSON, and encoding/json
// compacts and HTML-escapes anything it re-encodes, so a record written that
// way loses these bytes on its first rewrite (amendment A310). A canonical
// fixture is a fixed point of that transformation and could not tell the two
// encodings apart.
func TestOpenYieldsTheStoredBodyBytesVerbatimAcrossRewrites(t *testing.T) {
	nonCanonical := []byte("{\n  \"note\": \"a<b&c\",\n  \"customer_id\":  \"cus_1\"\n}")
	compacted, err := json.Marshal(json.RawMessage(nonCanonical))
	if err != nil {
		t.Fatal(err)
	}
	if string(compacted) == string(nonCanonical) {
		t.Fatal("the fixture is canonical, so this test cannot detect a re-serialisation")
	}

	l, _ := newLedger(t)
	h := mustCreate(t, l, broadcastRun(t))
	runID := h.Run().RunID

	if err := h.Begin(runs.StepSimulate, nonCanonical); err != nil {
		t.Fatal(err)
	}
	// Rewrite the record several times, which is what a real run does.
	if err := h.Record(runs.StepSimulate, &runs.Response{Status: 503}, &runs.Outcome{Class: "transient", ExitCode: 5}); err != nil {
		t.Fatal(err)
	}
	if err := h.Begin(runs.StepSimulate, nil); err != nil {
		t.Fatal(err)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := l.Open(runID)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer reopened.Close()

	s := reopened.Run().Step(runs.StepSimulate)
	if string(s.Body) != string(nonCanonical) {
		t.Fatalf("body after rewrites:\n got %q\nwant %q", s.Body, nonCanonical)
	}
	if *s.BodySHA256 != runs.Body(nonCanonical).SHA256() {
		t.Fatal("the digest no longer matches the bytes a resume would resend")
	}
	if s.IdempotencyKey != runs.StepKey(runID, runs.StepSimulate) {
		t.Fatalf("key = %q", s.IdempotencyKey)
	}
}

// AC11: a record whose stored body_sha256 disagrees with its stored body is
// ErrRecordCorrupt.
//
// Mutation M4 recomputes the digest instead of comparing it, which makes the
// field agree with itself always and this test go green on a record whose
// bytes were altered under a key the server has already seen.
func TestOpenRefusesADigestThatDisagreesWithTheBody(t *testing.T) {
	l, _ := newLedger(t)
	h := mustCreate(t, l, broadcastRun(t))
	runID := h.Run().RunID
	if err := h.Begin(runs.StepSimulate, simulateBody); err != nil {
		t.Fatal(err)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}

	// Alter the body on disk, leaving the digest as it was. This is what a
	// hand edit, a truncated write or a re-serialising rewrite looks like.
	raw := readRaw(t, l, runID)
	altered := strings.Replace(string(raw), `cus_1`, `cus_2`, 1)
	if altered == string(raw) {
		t.Fatal("the fixture edit did not apply")
	}
	if err := os.WriteFile(l.RecordPath(runID), []byte(altered), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := l.Open(runID)
	if !errors.Is(err, runs.ErrRecordCorrupt) {
		t.Fatalf("err = %v, want ErrRecordCorrupt", err)
	}
	if _, err := l.Load(runID); !errors.Is(err, runs.ErrRecordCorrupt) {
		t.Fatalf("Load err = %v, want ErrRecordCorrupt", err)
	}
}

func TestOpenRefusesAHalfRecordedBody(t *testing.T) {
	cases := map[string]string{
		"digest with no body": `"body": null`,
		"body with no digest": `"body_sha256": null`,
	}
	for name, blank := range cases {
		t.Run(name, func(t *testing.T) {
			l, _ := newLedger(t)
			h := mustCreate(t, l, broadcastRun(t))
			runID := h.Run().RunID
			if err := h.Begin(runs.StepSimulate, simulateBody); err != nil {
				t.Fatal(err)
			}
			if err := h.Close(); err != nil {
				t.Fatal(err)
			}

			raw := string(readRaw(t, l, runID))
			field := strings.SplitN(blank, ":", 2)[0]
			idx := strings.Index(raw, field)
			if idx < 0 {
				t.Fatalf("field %s not in the record", field)
			}
			end := strings.Index(raw[idx:], "\n")
			edited := raw[:idx] + blank + "," + raw[idx+end:]
			if err := os.WriteFile(l.RecordPath(runID), []byte(edited), 0o600); err != nil {
				t.Fatal(err)
			}

			if _, err := l.Open(runID); !errors.Is(err, runs.ErrRecordCorrupt) {
				t.Fatalf("err = %v, want ErrRecordCorrupt", err)
			}
		})
	}
}

// A record that embeds the body as JSON rather than as a string is refused
// loudly. Silently accepting it would mean accepting a body whose bytes the
// digest cannot vouch for.
func TestOpenRefusesABodyEmbeddedAsJSON(t *testing.T) {
	l, _ := newLedger(t)
	h := mustCreate(t, l, broadcastRun(t))
	runID := h.Run().RunID
	if err := h.Begin(runs.StepSimulate, simulateBody); err != nil {
		t.Fatal(err)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}

	raw := readRaw(t, l, runID)
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	steps := doc["steps"].([]any)
	steps[0].(map[string]any)["body"] = map[string]any{"customer_id": "cus_1"}
	edited, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(l.RecordPath(runID), edited, 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := l.Open(runID); !errors.Is(err, runs.ErrRecordCorrupt) {
		t.Fatalf("err = %v, want ErrRecordCorrupt", err)
	}
}

func TestOpenOfAMissingRunDoesNotCreateASidecar(t *testing.T) {
	l, dir := newLedger(t)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	id, err := ulid.New()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.Open(id); !errors.Is(err, runs.ErrRunNotFound) {
		t.Fatalf("err = %v, want ErrRunNotFound", err)
	}
	if _, err := os.Stat(l.LockPath(id)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a sidecar was created for a run that does not exist")
	}
}

func TestCreateRefusesADuplicateRun(t *testing.T) {
	l, _ := newLedger(t)
	run := broadcastRun(t)
	h := mustCreate(t, l, run)
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}

	again := broadcastRun(t)
	again.RunID = run.RunID
	if _, err := l.Create(again); !errors.Is(err, runs.ErrRunExists) {
		t.Fatalf("err = %v, want ErrRunExists", err)
	}
}

func TestCreateRefusesMalformedRuns(t *testing.T) {
	l, _ := newLedger(t)

	bad := broadcastRun(t)
	bad.RunID = "not-a-ulid"
	if _, err := l.Create(bad); err == nil {
		t.Error("a non-ULID run id was accepted")
	}

	bad = broadcastRun(t)
	bad.Steps[1].IdempotencyKey = bad.Steps[0].IdempotencyKey
	if _, err := l.Create(bad); err == nil {
		t.Error("two steps sharing one idempotency key were accepted")
	}

	bad = broadcastRun(t)
	bad.Steps[0].IdempotencyKey = ""
	if _, err := l.Create(bad); !errors.Is(err, runs.ErrInvalidKey) {
		t.Errorf("an empty key: err = %v, want ErrInvalidKey", err)
	}

	bad = broadcastRun(t)
	bad.Steps[0].IdempotencyKey = strings.Repeat("k", 256)
	if _, err := l.Create(bad); !errors.Is(err, runs.ErrInvalidKey) {
		t.Errorf("a 256-byte key: err = %v, want ErrInvalidKey", err)
	}

	bad = broadcastRun(t)
	bad.Steps[0].IdempotencyKey = "key\nwith-newline"
	if _, err := l.Create(bad); !errors.Is(err, runs.ErrInvalidKey) {
		t.Errorf("a key with a newline: err = %v, want ErrInvalidKey", err)
	}

	bad = broadcastRun(t)
	bad.Steps[0].State = runs.StatePending
	if _, err := l.Create(bad); !errors.Is(err, runs.ErrBadTransition) {
		t.Errorf("a new run with a pending step: err = %v", err)
	}
}

// ---------------------------------------------------------------- AC12 ----

// AC12: the run lock is the sidecar, and a second Open is refused *after* the
// first holder has rewritten the record at least once.
//
// The rewrite is the whole point. flock locks an inode and a rename replaces
// the directory entry, so a lock taken on <run>.json stops holding the moment
// Record renames a new file into place — which for a --broadcast run is the
// entire execute step (PLAN §1.2.30). Mutation M5 locks the record instead of
// the sidecar and process B's Open succeeds here.
func TestSecondOpenIsRefusedAfterTheHolderHasRewrittenTheRecord(t *testing.T) {
	l, dir := newLedger(t)
	run := broadcastRun(t)
	h := mustCreate(t, l, run)
	if err := h.Begin(runs.StepSimulate, simulateBody); err != nil {
		t.Fatal(err)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}

	inodeBefore := inodeOf(t, l.RecordPath(run.RunID))
	stop := startHelper(t, "hold-open", dir, run.RunID)

	if got := inodeOf(t, l.RecordPath(run.RunID)); got == inodeBefore {
		t.Fatalf("the helper did not rewrite the record (inode still %d); this test would not distinguish a sidecar lock from a record lock", got)
	}

	start := time.Now()
	if _, err := l.Open(run.RunID); !errors.Is(err, runs.ErrRunLocked) {
		t.Fatalf("Open while another process holds the run: err = %v, want ErrRunLocked", err)
	}
	if elapsed := time.Since(start); elapsed < runs.HolderRetry {
		t.Errorf("Open gave up after %s without exhausting the %s retry", elapsed, runs.HolderRetry)
	}

	stop()

	h2, err := l.Open(run.RunID)
	if err != nil {
		t.Fatalf("Open after the holder exited: %v", err)
	}
	defer h2.Close()
	if got := h2.Run().Step(runs.StepSimulate).State; got != runs.StateAnswered {
		t.Fatalf("the helper's Record did not survive: state = %s", got)
	}
}

// AC12, second half: a probe that acquires and releases while a holder is
// retrying does not make the holder fail.
//
// The helper holds for 100 ms with a bare flock — several orders of magnitude
// longer than the real probe's hold, so a retry that covers this covers that.
// Mutation M106 removes the retry and the Open below fails.
func TestHolderRetryOutlastsAProbesHold(t *testing.T) {
	l, dir := newLedger(t)
	run := broadcastRun(t)
	h := mustCreate(t, l, run)
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}

	startHelper(t, "hold-lock", dir, run.RunID, helperMSEnv+"=100")

	start := time.Now()
	h2, err := l.Open(run.RunID)
	if err != nil {
		t.Fatalf("Open during a 100ms probe-length hold: %v (a legitimate resume must not be refused by a probe's millisecond)", err)
	}
	defer h2.Close()
	if elapsed := time.Since(start); elapsed < 50*time.Millisecond {
		t.Fatalf("Open returned in %s, which is before the helper released; the test did not exercise the retry", elapsed)
	}
}

// Two holders in one process contend just as two processes do, because flock
// is per open file description.
func TestTwoHandlesInOneProcessContend(t *testing.T) {
	l, _ := newLedger(t)
	run := broadcastRun(t)
	h := mustCreate(t, l, run)

	if _, err := l.Open(run.RunID); !errors.Is(err, runs.ErrRunLocked) {
		t.Fatalf("err = %v, want ErrRunLocked", err)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	h2, err := l.Open(run.RunID)
	if err != nil {
		t.Fatalf("Open after Close: %v", err)
	}
	_ = h2.Close()
}

func TestSidecarIsNotRenamedByARewrite(t *testing.T) {
	l, _ := newLedger(t)
	run := broadcastRun(t)
	h := mustCreate(t, l, run)
	defer h.Close()

	before := inodeOf(t, l.LockPath(run.RunID))
	for i := 0; i < 3; i++ {
		if err := h.Save(); err != nil {
			t.Fatal(err)
		}
	}
	if after := inodeOf(t, l.LockPath(run.RunID)); after != before {
		t.Fatalf("the sidecar inode changed from %d to %d across record rewrites", before, after)
	}
}

func inodeOf(t *testing.T, path string) uint64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return inodeFromFileInfo(t, info)
}

func TestHandleCloseIsIdempotent(t *testing.T) {
	l, _ := newLedger(t)
	h := mustCreate(t, l, broadcastRun(t))
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	if err := h.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if err := h.Save(); err == nil {
		t.Fatal("a closed handle wrote to the record")
	}
}

func TestStepKeysAreTheDocumentedShape(t *testing.T) {
	run := broadcastRun(t)
	for _, s := range run.Steps {
		want := run.RunID + "-" + s.Name
		if s.IdempotencyKey != want {
			t.Errorf("%s key = %q, want %q", s.Name, s.IdempotencyKey, want)
		}
		if !runs.ValidKey(s.IdempotencyKey) {
			t.Errorf("%s key %q is not printable ASCII within 255 bytes", s.Name, s.IdempotencyKey)
		}
	}
}

func TestValidKey(t *testing.T) {
	ok := []string{"a", strings.Repeat("k", 255), "01JAAA-execute", "~ !@#$%^&*()"}
	bad := []string{"", strings.Repeat("k", 256), "with\nnewline", "with\ttab", "caf\u00e9", "nul\x00byte"}
	for _, k := range ok {
		if !runs.ValidKey(k) {
			t.Errorf("ValidKey(%q) = false", k)
		}
	}
	for _, k := range bad {
		if runs.ValidKey(k) {
			t.Errorf("ValidKey(%q) = true", k)
		}
	}
}

func TestRecordMovesTheStateMachineAsDocumented(t *testing.T) {
	cases := []struct {
		exit int
		want runs.State
	}{
		{0, runs.StateTerminal},
		{3, runs.StateTerminal},
		{4, runs.StateTerminal},
		{5, runs.StateAnswered},
		{6, runs.StateAnswered},
		{7, runs.StateTerminal},
		{8, runs.StateTerminal},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("exit %d", tc.exit), func(t *testing.T) {
			l, _ := newLedger(t)
			h := mustCreate(t, l, broadcastRun(t))
			if err := h.Begin(runs.StepSimulate, simulateBody); err != nil {
				t.Fatal(err)
			}
			if err := h.Record(runs.StepSimulate, &runs.Response{Status: 201}, &runs.Outcome{ExitCode: tc.exit}); err != nil {
				t.Fatal(err)
			}
			if got := h.Run().Step(runs.StepSimulate).State; got != tc.want {
				t.Fatalf("exit %d -> %s, want %s", tc.exit, got, tc.want)
			}
		})
	}
}
