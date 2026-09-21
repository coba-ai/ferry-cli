package runs

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/kurenn/ferry/cli/internal/fsx"
	"github.com/kurenn/ferry/cli/internal/ulid"
	"github.com/kurenn/ferry/cli/internal/xdg"
)

// FileMode is the mode a run record and its sidecar are written with.
const FileMode = 0o600

// Errors callers branch on.
var (
	// ErrRunNotFound is a run id with no record.
	ErrRunNotFound = errors.New("runs: no such run")
	// ErrRunExists is a run id that already has a record.
	ErrRunExists = errors.New("runs: run already exists")
	// ErrNoSuchStep is a step name the run does not have.
	ErrNoSuchStep = errors.New("runs: no such step")
	// ErrRecordCorrupt is AC11: a record whose stored body_sha256 disagrees
	// with its stored body. It is never repaired, because the two disagreeing
	// facts are the bytes that were sent and the digest of the bytes that
	// were sent, and the CLI cannot tell which one is right.
	ErrRecordCorrupt = errors.New("runs: run record is corrupt")
	// ErrStepMayHaveSent is AC92: a step past Begin cannot be declined.
	// Unknown is not convertible to no.
	ErrStepMayHaveSent = errors.New("runs: step may already have been sent; resume it, do not decline it")
	// ErrBodyMismatch is a Begin whose body disagrees with the body already
	// recorded under this step's key. One intent, one key (C2).
	ErrBodyMismatch = errors.New("runs: step already holds a different body under this key")
	// ErrBadTransition is a state change the machine in PLAN §5.3 does not
	// have.
	ErrBadTransition = errors.New("runs: invalid step state transition")
	// ErrSchemaUnsupported is a record from a future version.
	ErrSchemaUnsupported = errors.New("runs: unsupported run record schema")
)

// Ledger is the run directory.
type Ledger struct {
	fs  fsx.FS
	dir string
	now func() time.Time
}

// New returns a ledger over dir. A nil filesystem is the real one and a nil
// clock is time.Now.
func New(filesystem fsx.FS, dir string, now func() time.Time) *Ledger {
	if filesystem == nil {
		filesystem = fsx.OS()
	}
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &Ledger{fs: filesystem, dir: dir, now: now}
}

// Dir is the directory the ledger reads and writes.
func (l *Ledger) Dir() string { return l.dir }

func (l *Ledger) recordPath(runID string) string {
	return filepath.Join(l.dir, runID+".json")
}

func (l *Ledger) lockPath(runID string) string {
	return filepath.Join(l.dir, runID+".lock")
}

// Handle is a run held under its sidecar lock.
//
// A holder keeps the lock from Create or Open through its last write, which
// is what stops two invocations from rewriting one record and losing each
// other's updates. Close releases it.
type Handle struct {
	l      *Ledger
	run    *Run
	lock   *lock
	closed bool
}

// Run is the record. The caller may read it; every change goes through a
// method so that no change reaches memory without reaching the disk.
func (h *Handle) Run() *Run { return h.run }

// Close releases the lock.
func (h *Handle) Close() error {
	if h == nil || h.closed {
		return nil
	}
	h.closed = true
	return h.lock.release()
}

// Create mints a run: it takes the lock and writes the record with every step
// as the caller minted it.
//
// This is the first durable effect of a money command (PLAN §5.4 step 7).
// Every step key must already be set, because AC47 requires both keys of a
// --broadcast run to exist before the first send — a key minted later is a
// key a crash can lose.
func (l *Ledger) Create(run *Run) (*Handle, error) {
	if err := xdg.EnsureDir(l.fs, l.dir); err != nil {
		return nil, err
	}
	if err := validateNew(run); err != nil {
		return nil, err
	}
	if run.SchemaVersion == 0 {
		run.SchemaVersion = Schema
	}
	if run.CreatedAt.IsZero() {
		run.CreatedAt = l.now().UTC()
	}

	if _, err := l.fs.Stat(l.recordPath(run.RunID)); err == nil {
		return nil, fmt.Errorf("%w: %s", ErrRunExists, run.RunID)
	} else if !fsx.ErrNotFound(err) {
		return nil, fmt.Errorf("runs: stat %s: %w", l.recordPath(run.RunID), err)
	}

	lk, err := acquire(l.lockPath(run.RunID), HolderRetry)
	if err != nil {
		return nil, err
	}
	h := &Handle{l: l, run: run, lock: lk}
	if err := h.save(); err != nil {
		_ = lk.release()
		return nil, err
	}
	return h, nil
}

func validateNew(run *Run) error {
	if run == nil {
		return errors.New("runs: nil run")
	}
	if !ulid.Valid(run.RunID) {
		return fmt.Errorf("runs: run id %q is not a ULID", run.RunID)
	}
	if len(run.Steps) == 0 {
		return errors.New("runs: a run needs at least one step")
	}
	names := map[string]bool{}
	keys := map[string]bool{}
	for _, s := range run.Steps {
		if s == nil {
			return errors.New("runs: nil step")
		}
		if names[s.Name] {
			return fmt.Errorf("runs: duplicate step %q", s.Name)
		}
		names[s.Name] = true
		if !ValidKey(s.IdempotencyKey) {
			return fmt.Errorf("%w: step %q key %q", ErrInvalidKey, s.Name, s.IdempotencyKey)
		}
		if keys[s.IdempotencyKey] {
			// Two steps under one key would make the second request a replay
			// of the first: the server would answer the simulate's stored
			// result to an execute.
			return fmt.Errorf("runs: steps %q and another share the idempotency key %q", s.Name, s.IdempotencyKey)
		}
		keys[s.IdempotencyKey] = true
		if s.State == "" {
			s.State = StateNotStarted
		}
		if s.State != StateNotStarted {
			return fmt.Errorf("%w: a new run's step %q is %s, want not_started", ErrBadTransition, s.Name, s.State)
		}
		if s.Body != nil {
			if err := checkBody(s.Body); err != nil {
				return err
			}
			if s.BodySHA256 == nil {
				d := s.Body.SHA256()
				s.BodySHA256 = &d
			}
		}
	}
	return nil
}

// Open takes the run as a holder and returns it.
//
// The acquire retries for up to HolderRetry so that a probe's momentary hold
// cannot masquerade as a live holder (AC12, V5). A run another process really
// holds is ErrRunLocked.
func (l *Ledger) Open(runID string) (*Handle, error) {
	if !ulid.Valid(runID) {
		return nil, fmt.Errorf("%w: %q is not a run id", ErrRunNotFound, runID)
	}
	// Check the record before creating a sidecar, so a typo does not litter
	// the directory with lock files for runs that do not exist.
	if _, err := l.fs.Stat(l.recordPath(runID)); err != nil {
		if fsx.ErrNotFound(err) {
			return nil, fmt.Errorf("%w: %s", ErrRunNotFound, runID)
		}
		return nil, fmt.Errorf("runs: stat %s: %w", l.recordPath(runID), err)
	}

	lk, err := acquire(l.lockPath(runID), HolderRetry)
	if err != nil {
		return nil, err
	}
	// Re-read under the lock: prune may have removed the record between the
	// stat and the acquire.
	run, err := l.load(runID)
	if err != nil {
		_ = lk.release()
		if errors.Is(err, ErrRunNotFound) {
			// Remove the sidecar this call created for a record that is gone.
			_ = l.fs.Remove(l.lockPath(runID))
		}
		return nil, err
	}
	return &Handle{l: l, run: run, lock: lk}, nil
}

// load reads and verifies a record without taking the lock.
func (l *Ledger) load(runID string) (*Run, error) {
	raw, err := l.fs.ReadFile(l.recordPath(runID))
	if err != nil {
		if fsx.ErrNotFound(err) {
			return nil, fmt.Errorf("%w: %s", ErrRunNotFound, runID)
		}
		return nil, fmt.Errorf("runs: read %s: %w", l.recordPath(runID), err)
	}
	return decode(runID, raw)
}

func decode(runID string, raw []byte) (*Run, error) {
	var run Run
	if err := json.Unmarshal(raw, &run); err != nil {
		return nil, fmt.Errorf("%w: %s: %v", ErrRecordCorrupt, runID, err)
	}
	if run.SchemaVersion > Schema {
		return nil, fmt.Errorf("%w: %s is schema %d, this binary understands %d",
			ErrSchemaUnsupported, runID, run.SchemaVersion, Schema)
	}
	if run.RunID != runID {
		return nil, fmt.Errorf("%w: %s holds run_id %q", ErrRecordCorrupt, runID, run.RunID)
	}
	if err := verifyDigests(&run); err != nil {
		return nil, err
	}
	return &run, nil
}

// verifyDigests is AC11: the stored body_sha256 is *compared* against the
// stored body, never recomputed from it.
//
// Recomputing would make the field a decoration: the record would always
// agree with itself, and a body altered in place — by an editor, by a
// truncated write, by a rewrite that re-serialised it — would be resent under
// a key the server has already seen with the original bytes, which is the one
// request FERRY answers 409 IDEMPOTENCY_KEY_REUSED. Comparing turns that into
// a refusal before anything is sent (mutation M4).
func verifyDigests(run *Run) error {
	for _, s := range run.Steps {
		switch {
		case s.Body == nil && s.BodySHA256 == nil:
			continue
		case s.Body == nil && s.BodySHA256 != nil:
			return fmt.Errorf("%w: run %s step %s has a digest and no body", ErrRecordCorrupt, run.RunID, s.Name)
		case s.Body != nil && s.BodySHA256 == nil:
			return fmt.Errorf("%w: run %s step %s has a body and no digest", ErrRecordCorrupt, run.RunID, s.Name)
		}
		if got := s.Body.SHA256(); got != *s.BodySHA256 {
			return fmt.Errorf("%w: run %s step %s body digest is %s, record says %s",
				ErrRecordCorrupt, run.RunID, s.Name, got, *s.BodySHA256)
		}
	}
	return nil
}

// save writes the record durably: temp, fsync, rename, fsync(dir).
func (h *Handle) save() error {
	if h.closed {
		return errors.New("runs: handle is closed")
	}
	data, err := json.MarshalIndent(h.run, "", "  ")
	if err != nil {
		return fmt.Errorf("runs: encode %s: %w", h.run.RunID, err)
	}
	data = append(data, '\n')
	return fsx.WriteFileAtomic(h.l.fs, h.l.recordPath(h.run.RunID), data, FileMode)
}

// Save writes the current record durably. It exists for a caller that has
// changed a field no method covers; every method below already saves.
func (h *Handle) Save() error { return h.save() }

func (h *Handle) step(name string) (*Step, error) {
	s := h.run.Step(name)
	if s == nil {
		return nil, fmt.Errorf("%w: %s", ErrNoSuchStep, name)
	}
	return s, nil
}

// Begin is the write that matters (C1, AC10).
//
// It records the body and its digest, marks the step pending, increments
// attempts, and returns only once the record is fsynced and renamed into
// place. The caller sends nothing until it returns nil; if it returns an
// error, no record exists at the final path and nothing has been sent, which
// is what lets PLAN §5.4 step 9 answer exit 1.
//
// body may be nil for a resend of a step that already holds its bytes. A
// non-nil body that disagrees with the bytes already under this key is
// ErrBodyMismatch: one intent, one key (C2).
//
// Begin is called before every send, including a resend, so that attempts is
// durable and a pending step never reads attempts: 0.
func (h *Handle) Begin(stepName string, body []byte) error {
	s, err := h.step(stepName)
	if err != nil {
		return err
	}
	switch s.State {
	case StateDeclined, StateUnreachable:
		return fmt.Errorf("%w: step %s is %s and cannot be sent", ErrBadTransition, stepName, s.State)
	case StateTerminal:
		return fmt.Errorf("%w: step %s is terminal", ErrBadTransition, stepName)
	}

	if body != nil {
		if err := checkBody(body); err != nil {
			return err
		}
		digest := Body(body).SHA256()
		if s.BodySHA256 != nil && *s.BodySHA256 != digest {
			return fmt.Errorf("%w: step %s holds %s, this call offers %s",
				ErrBodyMismatch, stepName, *s.BodySHA256, digest)
		}
		s.Body = Body(body)
		s.BodySHA256 = &digest
	}
	if s.Body == nil {
		return fmt.Errorf("runs: step %s has no body to send", stepName)
	}

	prev, prevState, prevAttempts := s.State, s.State, s.Attempts
	s.State = StatePending
	s.Attempts++
	if err := h.save(); err != nil {
		// Roll memory back to what is on disk. A caller that ignores the
		// error and sends anyway would at least not be working from a record
		// that claims a durability it does not have.
		s.State, s.Attempts = prevState, prevAttempts
		_ = prev
		return err
	}
	return nil
}

// AwaitConfirmation marks a step awaiting_confirmation, durably, *before* the
// prompt is written (C16, AC72).
//
// The state is not consent and never becomes consent. It exists so that a
// process killed at the prompt leaves evidence that a plan was displayed and
// nobody answered — which C18's orphan check then refuses to walk past, and
// which `runs decline` can clear without a terminal.
func (h *Handle) AwaitConfirmation(stepName string) error {
	s, err := h.step(stepName)
	if err != nil {
		return err
	}
	if s.State != StateNotStarted && s.State != StateAwaitingConfirmation {
		return fmt.Errorf("%w: step %s is %s, want not_started", ErrBadTransition, stepName, s.State)
	}
	s.State = StateAwaitingConfirmation
	return h.save()
}

// SetPlan records the plan and its token on a step.
//
// This is the only writer of Step.PlanToken. It is durable because the token
// is the only link between the two HTTP calls of a --broadcast run: a crash
// in the window between the simulate Record and the execute Begin strands the
// plan if the token is not on disk (AC80).
func (h *Handle) SetPlan(stepName string, plan *Plan, token string) error {
	s, err := h.step(stepName)
	if err != nil {
		return err
	}
	if s.State.Terminal() {
		return fmt.Errorf("%w: step %s is %s", ErrBadTransition, stepName, s.State)
	}
	s.Plan = plan
	if token == "" {
		s.PlanToken = nil
	} else {
		t := token
		s.PlanToken = &t
	}
	return h.save()
}

// Record stores a response and its classification (PLAN §5.4 step 11).
//
// The step becomes terminal when the outcome is settled — classes 0, 3, 4, 7
// and 8 — and answered when it is class 5 or 6, which are the two a resume
// may pick up. Reaching terminal scrubs the execute step's plan token.
//
// Any simulate response has plan.token removed before it is written (AC82),
// and every stored response body is swept for a token shape afterwards, so
// that a field the server adds later cannot put one on disk.
func (h *Handle) Record(stepName string, resp *Response, oc *Outcome) error {
	s, err := h.step(stepName)
	if err != nil {
		return err
	}
	if resp != nil {
		stored := *resp
		stored.Body = stripPlanToken(stored.Body)
		if stored.ReceivedAt.IsZero() {
			stored.ReceivedAt = h.l.now().UTC()
		}
		s.Response = &stored
	}
	s.Outcome = oc

	switch {
	case oc == nil:
		s.State = StateAnswered
	case oc.Resumable():
		s.State = StateAnswered
	default:
		s.State = StateTerminal
	}
	if s.State == StateTerminal {
		scrubStep(s)
	}
	return h.save()
}

// Decline writes declined and scrubs the token (AC92, V4).
//
// It refuses a step in pending or later with ErrStepMayHaveSent: a request
// that may have been sent cannot be declined, only resumed, so the ledger can
// never turn "unknown" into "no" (mutation M103).
func (h *Handle) Decline(stepName string) error {
	s, err := h.step(stepName)
	if err != nil {
		return err
	}
	switch s.State {
	case StateDeclined, StateUnreachable:
		return nil // already resolved without sending
	case StateNotStarted, StateAwaitingConfirmation:
		s.State = StateDeclined
		scrubStep(s)
		return h.save()
	default:
		return fmt.Errorf("%w: step %s is %s", ErrStepMayHaveSent, stepName, s.State)
	}
}

// Unreachable marks a step that can never legitimately start (AC75, C8).
func (h *Handle) Unreachable(stepName string) error {
	s, err := h.step(stepName)
	if err != nil {
		return err
	}
	if s.State.MayHaveSent() {
		return fmt.Errorf("%w: step %s is %s", ErrStepMayHaveSent, stepName, s.State)
	}
	s.State = StateUnreachable
	scrubStep(s)
	return h.save()
}

// scrubStep replaces a live plan token with the sentinel.
//
// BodySHA256 and Body are untouched. Scrubbing either would destroy the proof
// that a resume resends the same request (AC11, AC45) — the token in Body is
// spent or expired by the time a step is terminal, and `runs show` redacts it
// on display (V6, mutation M7).
func scrubStep(s *Step) bool {
	if !s.HasPlanToken() {
		return false
	}
	scrubbed := ScrubbedToken
	s.PlanToken = &scrubbed
	return true
}
