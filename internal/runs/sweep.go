package runs

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/coba-ai/ferry-cli/internal/fsx"
	"github.com/coba-ai/ferry-cli/internal/ulid"
)

// ScrubGrace is how long past a plan's expiry a token is kept (PLAN §5.3).
//
// Ten minutes is twice the plan's own lifetime, so a clock skewed by minutes
// cannot scrub a live plan. The decision is a local-clock one and it is in
// the safe direction only: a scrubbed token makes a resume exit 4, never
// send. Expiry as a *refusal* remains the server's (PLAN §10.2).
const ScrubGrace = 10 * time.Minute

// ids returns every run id with a record in the directory, sorted.
//
// It matches <ulid>.json and nothing else. Temp files are dotfiles ending in
// .tmp<suffix> and sidecars end in .lock, so neither can be mistaken for a
// record — which matters, because a sweep that read a half-written temp file
// would report a corrupt ledger on every concurrent write.
func (l *Ledger) ids() ([]string, error) {
	entries, err := l.fs.ReadDir(l.dir)
	if err != nil {
		if fsx.ErrNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("runs: read %s: %w", l.dir, err)
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".json") || strings.HasPrefix(name, ".") {
			continue
		}
		id := strings.TrimSuffix(name, ".json")
		if !ulid.Valid(id) {
			continue
		}
		out = append(out, id)
	}
	sort.Strings(out)
	return out, nil
}

// List returns every run in the directory, oldest first.
//
// A record that cannot be read is an error, not a skipped entry. Skipping is
// the direction that loses money: the orphan check and the caller-key lookup
// are both built on this, and a sweep that quietly omits an unreadable
// pending run reports "no unresolved runs" for a run that may have sent.
func (l *Ledger) List() ([]*Run, error) {
	ids, err := l.ids()
	if err != nil {
		return nil, err
	}
	out := make([]*Run, 0, len(ids))
	for _, id := range ids {
		run, err := l.load(id)
		if err != nil {
			if errors.Is(err, ErrRunNotFound) {
				continue // removed by a concurrent prune between readdir and read
			}
			return nil, err
		}
		out = append(out, run)
	}
	return out, nil
}

// Load reads a run without taking the lock. It is for a reader — `runs show`,
// `runs list` — that is not going to send anything.
func (l *Ledger) Load(runID string) (*Run, error) { return l.load(runID) }

// KeyMatch is what FindByKey found.
type KeyMatch struct {
	Run  *Run
	Step *Step
}

// FindByKey returns the run and step that own an idempotency key (AC14).
//
// The match is exact. A prefix match would be worse than no match: the keys
// of one run are <id>-simulate and <id>-execute, so a prefix rule would let a
// caller passing --idempotency-key <id> be routed into a run whose body is a
// different request (mutation M8).
func (l *Ledger) FindByKey(key string) (*KeyMatch, error) {
	if key == "" {
		return nil, nil
	}
	all, err := l.List()
	if err != nil {
		return nil, err
	}
	for _, run := range all {
		for _, s := range run.Steps {
			if s.IdempotencyKey == key {
				return &KeyMatch{Run: run, Step: s}, nil
			}
		}
	}
	return nil, nil
}

// Orphan is an unresolved run nobody is working on (C18, AC90).
type Orphan struct {
	RunID     string
	Step      string
	State     State
	ExitCode  int
	CreatedAt time.Time
	Argv      []string
}

// Orphans returns the runs a money command must refuse to walk past.
//
// A run qualifies when it is for this profile and environment, has a step
// that is awaiting_confirmation, pending, or answered with class 5 or 6, and
// its sidecar lock can be acquired. A lock another process holds means a
// concurrent invocation, not an orphan — without that, two legitimate
// parallel transfers would block each other and every concurrent caller
// would reach for --allow-pending, which is the opt-out this control depends
// on being rare (PLAN §9.2, CRITIQUE M9).
//
// This is the probe idiom: each lock is released before the next run is
// tested and before the call returns, so a concurrent resume's 250 ms retry
// outlasts the probe's hold (V5, mutations M105 and M106).
//
// exempt names run ids that are never reported. It carries the run a
// same-body --idempotency-key has just matched: that run *is* the one being
// resumed, and refusing it would make a legitimate resume need
// --allow-pending (V3, AC94, mutation M109).
func (l *Ledger) Orphans(profile, environment string, exempt ...string) ([]Orphan, error) {
	skip := make(map[string]bool, len(exempt))
	for _, id := range exempt {
		skip[id] = true
	}

	all, err := l.List()
	if err != nil {
		return nil, err
	}

	var out []Orphan
	for _, run := range all {
		if skip[run.RunID] {
			continue
		}
		if profile != "" && run.Profile != profile {
			continue
		}
		if environment != "" && run.Environment != environment {
			continue
		}
		unresolved := run.Unresolved()
		if len(unresolved) == 0 {
			continue
		}

		heldByAnother, err := probe(l.lockPath(run.RunID), nil)
		if err != nil {
			return nil, err
		}
		if heldByAnother {
			continue
		}

		s := unresolved[0]
		o := Orphan{
			RunID:     run.RunID,
			Step:      s.Name,
			State:     s.State,
			CreatedAt: run.CreatedAt,
			Argv:      run.Argv,
		}
		if s.Outcome != nil {
			o.ExitCode = s.Outcome.ExitCode
		}
		out = append(out, o)
	}
	return out, nil
}

// Scrub removes plan tokens that can no longer be used (AC13).
//
// A token goes when its step is terminal, declined or unreachable, or when
// the plan expired more than ScrubGrace ago by the local clock. A run whose
// lock another process holds is skipped: rewriting a record under a live
// holder would lose whatever that holder is about to write.
//
// body_sha256 is never touched (mutation M7).
func (l *Ledger) Scrub(now time.Time) ([]string, error) {
	ids, err := l.ids()
	if err != nil {
		return nil, err
	}

	var scrubbed []string
	for _, id := range ids {
		var did bool
		heldByAnother, err := probe(l.lockPath(id), func() error {
			run, lerr := l.load(id)
			if lerr != nil {
				if errors.Is(lerr, ErrRunNotFound) {
					return nil
				}
				return lerr
			}
			if !scrubRun(run, now) {
				return nil
			}
			h := &Handle{l: l, run: run}
			if serr := h.save(); serr != nil {
				return serr
			}
			did = true
			return nil
		})
		if err != nil {
			return scrubbed, err
		}
		if heldByAnother {
			continue
		}
		if did {
			scrubbed = append(scrubbed, id)
		}
	}
	return scrubbed, nil
}

// scrubRun reports whether anything was scrubbed.
func scrubRun(run *Run, now time.Time) bool {
	changed := false
	for _, s := range run.Steps {
		if !s.HasPlanToken() {
			continue
		}
		expired := s.Plan != nil && !s.Plan.ExpiresAt.IsZero() &&
			s.Plan.ExpiresAt.Add(ScrubGrace).Before(now)
		if s.State.Terminal() || expired {
			changed = scrubStep(s) || changed
		}
	}
	return changed
}

// Decline writes declined on a run's execute step and scrubs its token
// (AC92).
//
// This is the remedy a headless caller has for an awaiting_confirmation run:
// a prompt that died to SIGKILL leaves a state only a human at a terminal
// could otherwise clear, and C18 would then refuse every later money command
// on that FERRY_HOME (V4).
//
// It refuses a step that has run Begin. Unknown is not declinable.
func (l *Ledger) Decline(runID string) (*Run, error) {
	h, err := l.Open(runID)
	if err != nil {
		return nil, err
	}
	defer h.Close()

	name := StepExecute
	if h.run.Step(name) == nil {
		return nil, fmt.Errorf("%w: run %s has no execute step to decline", ErrNoSuchStep, runID)
	}
	if err := h.Decline(name); err != nil {
		return nil, err
	}
	return h.run, nil
}

// Prune deletes terminal, scrubbed records created before cutoff (AC92).
//
// Each run is removed under its own sidecar lock, and the record is unlinked
// *before* the sidecar. The order is the whole of the requirement: unlinking
// the sidecar first would leave a record whose next holder creates a fresh
// inode to lock, so two processes could hold "the lock" for one run at once
// (mutation M107).
//
// A run whose lock another process holds is left alone, and so is one that
// stopped being prunable between the read and the lock.
func (l *Ledger) Prune(cutoff time.Time) ([]string, error) {
	ids, err := l.ids()
	if err != nil {
		return nil, err
	}

	var pruned []string
	for _, id := range ids {
		var did bool
		heldByAnother, err := probe(l.lockPath(id), func() error {
			run, lerr := l.load(id)
			if lerr != nil {
				if errors.Is(lerr, ErrRunNotFound) {
					return nil
				}
				return lerr
			}
			if !run.Prunable() || !run.CreatedAt.Before(cutoff) {
				return nil
			}
			if rerr := l.fs.Remove(l.recordPath(id)); rerr != nil && !fsx.ErrNotFound(rerr) {
				return fmt.Errorf("runs: remove %s: %w", l.recordPath(id), rerr)
			}
			if rerr := l.fs.Remove(l.lockPath(id)); rerr != nil && !fsx.ErrNotFound(rerr) {
				return fmt.Errorf("runs: remove %s: %w", l.lockPath(id), rerr)
			}
			did = true
			return nil
		})
		if err != nil {
			return pruned, err
		}
		if heldByAnother {
			continue
		}
		if did {
			pruned = append(pruned, id)
		}
	}
	return pruned, nil
}

// RecordPath is where a run's record lives. Exported for a test or a render
// that needs to name the file.
func (l *Ledger) RecordPath(runID string) string { return l.recordPath(runID) }

// LockPath is where a run's sidecar lives.
func (l *Ledger) LockPath(runID string) string { return l.lockPath(runID) }
