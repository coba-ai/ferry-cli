package runs_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coba-ai/ferry-cli/internal/fsx"
	"github.com/coba-ai/ferry-cli/internal/runs"
	"github.com/coba-ai/ferry-cli/internal/ulid"
)

// This file attacks the one claim the package exists to make (C1): for every
// money request the CLI sends, the key and the exact body bytes are on disk,
// fsynced and renamed into place, before the first byte leaves.
//
// The attack is structured as a single question asked at every point the
// write can fail: after Begin returns, is the caller allowed to send, and is
// the key on disk? Exactly one pair is a defect —
//
//	Begin returned nil   and the key is not readable  -> money can be sent
//	                                                     under a key nothing
//	                                                     records. That is the
//	                                                     double-send.
//
// The other three are safe: an error means nothing is sent, and a key on disk
// with an error means a resume finds it.

// canSend is the rule every caller must follow: send only if Begin returned
// nil. It is written out here so the table below is about the ledger's
// guarantee and not about a caller's discipline.
func canSend(beginErr error) bool { return beginErr == nil }

// TestNoFaultInTheWritePathLosesTheKeyWhileTheRequestCouldBeSent walks the
// fault points of PLAN §6.1 in the durable-write sequence and checks the
// pairing above at each one.
func TestNoFaultInTheWritePathLosesTheKeyWhileTheRequestCouldBeSent(t *testing.T) {
	// Every operation WriteFileAtomic performs, with the failure a real
	// filesystem would produce there.
	faults := []struct {
		op  string
		nth int
		err error
		// mustRefuse is set for every fault at or before the rename. Those
		// are the ones where the record has not reached its final path, so
		// Begin has no business returning nil.
		//
		// This is the assertion that makes the table a control rather than an
		// observation. A fault-injecting filesystem cannot show that an
		// fsync's bytes reached the platter — an fsync whose error was
		// swallowed looks exactly like one that succeeded, because the data
		// is in the page cache either way and the test reads it straight
		// back. What it can show is that the error was *propagated*, and
		// that is what this field demands.
		mustRefuse bool
		what       string
	}{
		{fsx.OpCreate, 1, fsx.ENOSPC, true, "a full disk when the temp file is created"},
		{fsx.OpCreate, 1, fsx.EROFS, true, "a read-only filesystem"},
		{fsx.OpWrite, 1, fsx.ENOSPC, true, "a full disk mid-write"},
		{fsx.OpSync, 1, fsx.EIO, true, "an interrupted fsync of the record"},
		{fsx.OpClose, 1, fsx.EIO, true, "a failure surfaced at close"},
		{fsx.OpRename, 1, fsx.EIO, true, "a failed rename"},
		// After the rename the record *is* at its final path. Begin still
		// refuses, because durability is unconfirmed — but the record is not
		// absent, so this one is checked by the fail-closed test below rather
		// than demanded here.
		{"syncdir", 1, fsx.EIO, false, "a failed fsync of the directory, after the rename"},
	}

	for _, f := range faults {
		name := fmt.Sprintf("%s/%s", f.op, f.what)
		t.Run(name, func(t *testing.T) {
			ff := fsx.NewFault(fsx.OS())
			l, dir := newLedgerOn(t, ff)

			run := broadcastRun(t)
			h, err := l.Create(run)
			if err != nil {
				t.Fatalf("Create (before any fault): %v", err)
			}
			key := runs.StepKey(run.RunID, runs.StepSimulate)

			// The fault applies to Begin's write, not to Create's.
			ff.Reset()
			ff.FailAt(f.op, f.nth, f.err)

			beginErr := h.Begin(runs.StepSimulate, simulateBody)

			if f.mustRefuse && beginErr == nil {
				t.Fatalf("DEFECT: %s was injected before the rename, so the record never "+
					"reached its final path, yet Begin returned nil and the caller may "+
					"send. A write error that is not propagated is a send with no record.",
					f.what)
			}

			// Read the ledger the way a later invocation would: a fresh
			// ledger on the real filesystem, with no memory of this one.
			fresh := runs.New(fsx.OS(), dir, nil)
			match, findErr := fresh.FindByKey(key)
			keyOnDisk := findErr == nil && match != nil

			switch {
			case canSend(beginErr) && !keyOnDisk:
				t.Fatalf("DEFECT: Begin returned nil under %s, so the caller may send, "+
					"but the key is not on disk (FindByKey err %v). A crash here sends "+
					"money under a key nothing records.", f.what, findErr)
			case canSend(beginErr):
				// The stronger claim, and the one that discriminates: Create
				// has already put both keys on disk, so "the key is present"
				// is true even for a Begin that failed. What a successful
				// Begin must additionally guarantee is that the record on
				// disk says this step is pending and carries these exact
				// bytes — because that is what a resume reads to decide it
				// may resend rather than mint.
				step := match.Step
				if step.State != runs.StatePending {
					t.Fatalf("DEFECT: Begin returned nil under %s but the record on disk "+
						"says %s, not pending. A resume would not know the request may "+
						"have been sent.", f.what, step.State)
				}
				if string(step.Body) != string(simulateBody) {
					t.Fatalf("DEFECT: Begin returned nil under %s but the body on disk is "+
						"%q, not the bytes that would be sent", f.what, step.Body)
				}
				t.Logf("safe: %s did not prevent the write; the record is pending with the "+
					"exact body, so a send is permitted", f.what)
			case keyOnDisk:
				// Safe: nothing may be sent, and a resume can find the run.
				// The key is readable here because Create wrote it before
				// Begin was ever called — which is the design: the key
				// precedes the attempt, so a failed attempt still leaves a
				// run a later invocation must reckon with rather than walk
				// past.
				t.Logf("safe: %s -> Begin refused (%v); step on disk is %s and the key is "+
					"readable, so a resume finds the run rather than minting a new key",
					f.what, beginErr, match.Step.State)
			default:
				// Safe: nothing may be sent, and nothing was written.
				t.Logf("safe: %s -> Begin refused (%v) and no record landed", f.what, beginErr)
			}

			// Whatever happened, no temp file may be left where a sweep would
			// read it as a record.
			assertNoStrayRecords(t, dir, run.RunID)
			_ = h.Close()
		})
	}
}

// assertNoStrayRecords checks the directory holds only the record, the
// sidecar, and nothing a sweep would misread.
func assertNoStrayRecords(t *testing.T, dir, runID string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	for _, e := range entries {
		name := e.Name()
		switch {
		case name == runID+".json", name == runID+".lock":
		case strings.HasPrefix(name, ".") && strings.Contains(name, ".tmp"):
			// A temp file from an interrupted write. It must not be
			// mistakable for a record: sweeps read only <ulid>.json.
			if strings.HasSuffix(name, ".json") {
				t.Errorf("the leftover temp file %s ends in .json and would be read as a record", name)
			}
		default:
			t.Errorf("unexpected file in the ledger directory: %s", name)
		}
	}
}

// A record left behind by a rename that succeeded but whose directory fsync
// failed is *not* absent. The ledger must fail closed on it: a later money
// command has to see it, or it would mint a new key for a request that may
// already have been sent.
func TestARecordLeftByAFailedDirectoryFsyncIsFoundByTheNextInvocation(t *testing.T) {
	ff := fsx.NewFault(fsx.OS())
	l, dir := newLedgerOn(t, ff)

	run := broadcastRun(t)
	h, err := l.Create(run)
	if err != nil {
		t.Fatal(err)
	}
	ff.Reset()
	ff.FailAt("syncdir", 1, fsx.EIO)

	if err := h.Begin(runs.StepSimulate, simulateBody); err == nil {
		t.Fatal("Begin returned nil despite the directory fsync failing")
	}
	_ = h.Close()

	// A later invocation, with no memory of the first.
	fresh := runs.New(fsx.OS(), dir, nil)

	orphans, err := fresh.Orphans(run.Profile, run.Environment)
	if err != nil {
		t.Fatal(err)
	}
	if len(orphans) != 1 || orphans[0].RunID != run.RunID {
		t.Fatalf("the next invocation does not see the run as unresolved: %v. It would "+
			"proceed and mint a new key for a request that may have been sent.", orphans)
	}
	if orphans[0].State != runs.StatePending {
		t.Errorf("state = %s, want pending: the step reached Begin, so it may have sent", orphans[0].State)
	}

	match, err := fresh.FindByKey(runs.StepKey(run.RunID, runs.StepSimulate))
	if err != nil || match == nil {
		t.Fatalf("FindByKey does not find the key: %v / %v", match, err)
	}
	if string(match.Step.Body) != string(simulateBody) {
		t.Errorf("the body on disk is %q", match.Step.Body)
	}
}

// A record truncated by a crash mid-write must not be readable as a valid
// run. A sweep that skipped it would answer "this key is not in use" for a
// key that is, which is the direction that costs money.
func TestAPartiallyWrittenRecordIsRefusedRatherThanSkipped(t *testing.T) {
	l, dir := newLedger(t)
	run := broadcastRun(t)
	h := mustCreate(t, l, run)
	if err := h.Begin(runs.StepSimulate, simulateBody); err != nil {
		t.Fatal(err)
	}
	full := readRaw(t, l, run.RunID)
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	path := l.RecordPath(run.RunID)

	// Every prefix of the record: a crash could have stopped anywhere.
	for _, cut := range []int{0, 1, len(full) / 4, len(full) / 2, len(full) - 10, len(full) - 1} {
		if cut < 0 {
			continue
		}
		t.Run(fmt.Sprintf("truncated to %d of %d bytes", cut, len(full)), func(t *testing.T) {
			if err := os.WriteFile(path, full[:cut], 0o600); err != nil {
				t.Fatal(err)
			}
			fresh := runs.New(fsx.OS(), dir, nil)

			// The property is not "every truncation fails to parse" — the
			// record ends in a newline, and losing it changes nothing. It is
			// that a truncation never *silently changes what the record
			// says*: either loading is refused, or the run that loads carries
			// the same key and the same body bytes.
			got, err := fresh.Load(run.RunID)
			if err != nil {
				// Refused. Then the sweeps must refuse too, rather than
				// skipping the file and reporting the key unused.
				if _, ferr := fresh.FindByKey("anything"); ferr == nil {
					t.Errorf("Load refused the record but FindByKey read past it and "+
						"answered; a caller would believe the key at %d bytes is unused", cut)
				}
				if _, oerr := fresh.Orphans(run.Profile, run.Environment); oerr == nil {
					t.Errorf("Load refused the record but Orphans read past it; a money " +
						"command would proceed")
				}
				return
			}

			step := got.Step(runs.StepSimulate)
			if step == nil {
				t.Fatalf("a record truncated to %d bytes loaded with no simulate step", cut)
			}
			if step.IdempotencyKey != runs.StepKey(run.RunID, runs.StepSimulate) {
				t.Fatalf("a record truncated to %d bytes loaded with key %q, want %q",
					cut, step.IdempotencyKey, runs.StepKey(run.RunID, runs.StepSimulate))
			}
			if string(step.Body) != string(simulateBody) {
				t.Fatalf("a record truncated to %d bytes loaded with body %q, want %q",
					cut, step.Body, simulateBody)
			}
			t.Logf("truncating to %d of %d bytes left the record parseable and unchanged "+
				"(the lost bytes were trailing whitespace)", cut, len(full))
		})
	}
}

// A record whose body was rewritten but whose digest was not is the shape a
// torn write leaves. It must be refused: resending under a key whose recorded
// body no longer matches what would be sent is the double-send.
func TestATornBodyAndDigestIsRefused(t *testing.T) {
	l, dir := newLedger(t)
	run := broadcastRun(t)
	h := mustCreate(t, l, run)
	if err := h.Begin(runs.StepSimulate, simulateBody); err != nil {
		t.Fatal(err)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}

	var doc map[string]any
	if err := json.Unmarshal(readRaw(t, l, run.RunID), &doc); err != nil {
		t.Fatal(err)
	}
	steps := doc["steps"].([]any)
	step := steps[0].(map[string]any)
	step["body"] = `{"customer_id":"cus_SOMEBODY_ELSE","amount":{"value":"100.00","side":"source"}}`
	out, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(l.RecordPath(run.RunID), out, 0o600); err != nil {
		t.Fatal(err)
	}

	fresh := runs.New(fsx.OS(), dir, nil)
	if _, err := fresh.Open(run.RunID); !errors.Is(err, runs.ErrRecordCorrupt) {
		t.Fatalf("Open err = %v, want ErrRecordCorrupt. A resume would send a different "+
			"body under a key the record vouches for.", err)
	}
}

// --------------------------------------------------------- concurrency ----

// Two invocations racing to create runs in one ledger. Distinct run ids mean
// distinct records, so this must simply work — and every key must survive.
func TestManyConcurrentCreatesInOneLedgerLoseNoKey(t *testing.T) {
	l, dir := newLedger(t)

	const n = 24
	var wg sync.WaitGroup
	ids := make([]string, n)
	errs := make([]error, n)

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			run := broadcastRun(t)
			ids[i] = run.RunID
			h, err := l.Create(run)
			if err != nil {
				errs[i] = err
				return
			}
			if err := h.Begin(runs.StepSimulate, simulateBody); err != nil {
				errs[i] = err
				_ = h.Close()
				return
			}
			errs[i] = h.Close()
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
	}

	fresh := runs.New(fsx.OS(), dir, nil)
	for _, id := range ids {
		m, err := fresh.FindByKey(runs.StepKey(id, runs.StepSimulate))
		if err != nil {
			t.Fatalf("FindByKey for %s: %v", id, err)
		}
		if m == nil {
			t.Fatalf("the key for run %s is not on disk after %d concurrent creates", id, n)
		}
	}
}

// Two handles racing on one run. The lock must serialise them, and no
// interleaving may produce a record that fails its own digest check.
func TestConcurrentHandlesOnOneRunNeverProduceACorruptRecord(t *testing.T) {
	l, dir := newLedger(t)
	run := broadcastRun(t)
	h := mustCreate(t, l, run)
	if err := h.Begin(runs.StepSimulate, simulateBody); err != nil {
		t.Fatal(err)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}

	const n = 16
	var wg sync.WaitGroup
	locked := 0
	var mu sync.Mutex

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			hh, err := l.Open(run.RunID)
			if errors.Is(err, runs.ErrRunLocked) {
				mu.Lock()
				locked++
				mu.Unlock()
				return
			}
			if err != nil {
				t.Errorf("Open: %v", err)
				return
			}
			defer hh.Close()
			if err := hh.Record(runs.StepSimulate,
				&runs.Response{Status: 200, Body: []byte(fmt.Sprintf(`{"writer":%d}`, i))},
				&runs.Outcome{Class: "pending", ExitCode: 6}); err != nil {
				t.Errorf("Record: %v", err)
			}
		}(i)
	}
	wg.Wait()

	// Whatever the interleaving, the record must load and its digest hold.
	fresh := runs.New(fsx.OS(), dir, nil)
	got, err := fresh.Load(run.RunID)
	if err != nil {
		t.Fatalf("after %d racing writers the record does not load: %v", n, err)
	}
	if string(got.Step(runs.StepSimulate).Body) != string(simulateBody) {
		t.Fatalf("the body changed under concurrent writers: %q", got.Step(runs.StepSimulate).Body)
	}
	if got.Step(runs.StepSimulate).IdempotencyKey != runs.StepKey(run.RunID, runs.StepSimulate) {
		t.Fatal("the key changed under concurrent writers")
	}
	t.Logf("%d of %d writers were refused with ErrRunLocked", locked, n)
}

// A sweep running while another process writes must not produce a torn read.
// Writes land by rename, so a reader sees one version or the other.
func TestSweepsAreNeverTornByAConcurrentWrite(t *testing.T) {
	l, dir := newLedger(t)
	run := broadcastRun(t)
	h := mustCreate(t, l, run)
	if err := h.Begin(runs.StepSimulate, simulateBody); err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			if err := h.Record(runs.StepSimulate,
				&runs.Response{Status: 200, Body: []byte(fmt.Sprintf(`{"n":%d}`, i))},
				&runs.Outcome{Class: "pending", ExitCode: 6}); err != nil {
				t.Errorf("Record %d: %v", i, err)
				return
			}
		}
	}()

	reader := runs.New(fsx.OS(), dir, nil)
	reads := 0
	for {
		select {
		case <-done:
			if reads < 10 {
				t.Fatalf("only %d reads raced the writer; the test proves little", reads)
			}
			return
		default:
		}
		if _, err := reader.FindByKey(runs.StepKey(run.RunID, runs.StepSimulate)); err != nil {
			t.Fatalf("a read during a concurrent rewrite failed after %d reads: %v", reads, err)
		}
		reads++
	}
}

// ---------------------------------------------------------- the clock ----

// A clock that jumps backwards must not let the ledger lose a key or scrub a
// token that is still live in a way that matters.
func TestAClockMovingBackwardsDoesNotLoseAKey(t *testing.T) {
	// A now() that walks backwards on every call.
	t0 := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	var calls int
	backwards := func() time.Time {
		calls++
		return t0.Add(-time.Duration(calls) * time.Hour)
	}

	dir := filepath.Join(t.TempDir(), "runs")
	l := runs.New(fsx.OS(), dir, backwards)

	id, err := ulid.New()
	if err != nil {
		t.Fatal(err)
	}
	run := broadcastRun(t)
	run.RunID = id
	run.Steps[0].IdempotencyKey = runs.StepKey(id, runs.StepSimulate)
	run.Steps[1].IdempotencyKey = runs.StepKey(id, runs.StepExecute)

	h, err := l.Create(run)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Begin(runs.StepSimulate, simulateBody); err != nil {
		t.Fatalf("Begin under a backwards clock: %v", err)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}

	fresh := runs.New(fsx.OS(), dir, nil)
	m, err := fresh.FindByKey(runs.StepKey(id, runs.StepSimulate))
	if err != nil || m == nil {
		t.Fatalf("the key is not on disk after a write under a backwards clock: %v / %v", m, err)
	}
}

// Prune under a clock that has jumped forward by years must still refuse a
// run that is not terminal. Age is a permission to forget a settled run, not
// a reason to forget an unsettled one.
func TestPruneUnderAWildlyWrongClockStillRefusesAnUnsettledRun(t *testing.T) {
	l, _ := newLedger(t)
	run := broadcastRun(t)
	run.CreatedAt = time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	h, err := l.Create(run)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Begin(runs.StepExecute, executeBody()); err != nil {
		t.Fatal(err)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}

	pruned, err := l.Prune(time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if len(pruned) != 0 {
		t.Fatalf("a pending run was pruned because the clock said it was old: %v. The key "+
			"for a request that may have been sent is now gone.", pruned)
	}
}

// Scrub under a backwards clock must not remove a token the plan says is
// still live: the expiry test compares against now, and a clock behind the
// plan's expiry means the plan has not expired.
func TestScrubUnderABackwardsClockKeepsALiveToken(t *testing.T) {
	l, _ := newLedger(t)
	h := mustCreate(t, l, broadcastRun(t))
	runID := h.Run().RunID

	expires := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	if err := h.SetPlan(runs.StepExecute, &runs.Plan{ID: "p", ExpiresAt: expires}, planToken); err != nil {
		t.Fatal(err)
	}
	if err := h.AwaitConfirmation(runs.StepExecute); err != nil {
		t.Fatal(err)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}

	// A clock a year behind.
	scrubbed, err := l.Scrub(expires.Add(-365 * 24 * time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(scrubbed) != 0 {
		t.Fatalf("a live token was scrubbed under a backwards clock: %v", scrubbed)
	}
	run, err := l.Load(runID)
	if err != nil {
		t.Fatal(err)
	}
	if !run.Step(runs.StepExecute).HasPlanToken() {
		t.Fatal("the token is gone, so a resume would execute with a null token")
	}
}

// ---------------------------------------------------------- C16 again ----

// No stored state may stand in for consent (C16).
//
// This is not a property of one function, so it is checked over every state
// runs.AllStates() reports — and AllStates is held complete against the
// package's own syntax tree by TestAllStatesIsCompleteAgainstTheSource, in
// both directions. That pairing is what the claim rests on: iterating a list
// only ever examines the states the list happens to contain, so without the
// completeness check a StateConfirmed added later would be examined by
// nothing here and this test would stay green. What this test establishes is
// therefore narrower than "no state can ever mean consent" and more useful
// than a spot check: of the states the package declares, none is named or
// behaves as recorded consent, and a newly declared one cannot slip past
// without failing either the census or the checks below.
func TestNoStateMeansTheHumanAgreed(t *testing.T) {
	all := runs.AllStates()
	if len(all) == 0 {
		t.Fatal("runs.AllStates() is empty; every check below would pass vacuously")
	}

	// No state is named as though it recorded agreement. A future one would
	// be the defect the review found, so the check is over the names as well
	// as the behaviour.
	forbidden := []string{"confirm", "approv", "consent", "authoris", "authoriz",
		"agreed", "accepted_by", "granted", "permitted", "ok_to_send", "yes"}
	for _, s := range all {
		for _, word := range forbidden {
			if !strings.Contains(string(s), word) {
				continue
			}
			// awaiting_confirmation is the sole exemption, and it is earned
			// below rather than assumed: it is the state written *before* a
			// prompt, and the assertions that follow require it to authorise
			// nothing.
			if s == runs.StateAwaitingConfirmation && word == "confirm" {
				continue
			}
			t.Errorf("state %q contains %q and so reads as recorded consent; C16 says "+
				"no state may. U5 re-asks on every invocation, and a state that looks "+
				"like a stored answer invites a future reader to skip the prompt.", s, word)
		}
	}

	// And no state, whatever it is called, may be one from which a send is
	// permitted without asking. The ledger has no such notion, so the check
	// is that no state is simultaneously non-terminal and not one of the
	// three a resume must handle by asking, refusing, or resending under the
	// key already on disk.
	askable := map[runs.State]bool{
		runs.StateNotStarted:           true, // ask
		runs.StateAwaitingConfirmation: true, // ask again; the prompt died
		runs.StatePending:              true, // resend under the same key, after asking
		runs.StateAnswered:             true, // poll or resend, after asking
	}
	for _, s := range all {
		if !s.Terminal() && !askable[s] {
			t.Errorf("state %q is neither terminal nor one a resume must ask from. A "+
				"state a resume can act on without asking is consent by another name.", s)
		}
	}

	// awaiting_confirmation is the one state whose name mentions confirmation,
	// and it must be the opposite of consent: it is written *before* a prompt
	// and it permits nothing.
	if runs.StateAwaitingConfirmation.MayHaveSent() {
		t.Error("awaiting_confirmation reports that a request may have been sent; " +
			"it is written before the prompt, so nothing has been sent")
	}
	if runs.StateAwaitingConfirmation.Terminal() {
		t.Error("awaiting_confirmation is terminal, so an interrupted prompt could never be resolved")
	}

	// And it is declinable — the whole of V4: a prompt killed by a signal
	// leaves this state, and a headless caller must be able to resolve it.
	l, _ := newLedger(t)
	run := broadcastRun(t)
	h, err := l.Create(run)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.AwaitConfirmation(runs.StepExecute); err != nil {
		t.Fatal(err)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Decline(run.RunID); err != nil {
		t.Fatalf("an awaiting_confirmation run cannot be declined headlessly: %v", err)
	}
}
