package runs

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"
)

// ErrRunLocked reports that another live process holds the run.
var ErrRunLocked = errors.New("runs: run is locked by another process")

// HolderRetry is how long a holder retries a non-blocking acquire before
// giving up (PLAN §5.3, V5).
//
// The number exists because of what the other two idioms do. A probe
// (Orphans, Scrub) acquires and releases inside one call, so it holds the
// lock for microseconds; without a retry, a resume that happened to ask
// during that window would report the run locked and refuse a legitimate
// resume (V5, mutation M106). 250 ms is several orders of magnitude longer
// than a probe's hold and short enough that a genuinely held run is reported
// promptly.
const HolderRetry = 250 * time.Millisecond

// holderPoll is the interval between acquire attempts.
const holderPoll = 5 * time.Millisecond

// lock is an flock on a run's sidecar file.
//
// The sidecar exists because flock locks an inode and rename replaces a
// directory entry (PLAN §1.2.30): a lock taken on <run>.json stops meaning
// anything the moment Record rewrites the record by temp+rename, which for a
// --broadcast run is the whole execute step. <run>.lock is created once and
// is never renamed or replaced while the record exists; only prune deletes
// it, under the lock and after the record (AC92).
type lock struct {
	f    *os.File
	path string
}

// acquire takes LOCK_EX|LOCK_NB on path, retrying for at most retryFor.
//
// A zero retryFor is the probe idiom: one attempt, no waiting.
func acquire(path string, retryFor time.Duration) (*lock, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("runs: open lock %s: %w", path, err)
	}

	deadline := time.Now().Add(retryFor)
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return &lock{f: f, path: path}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			f.Close()
			return nil, fmt.Errorf("runs: flock %s: %w", path, err)
		}
		if !time.Now().Before(deadline) {
			f.Close()
			return nil, fmt.Errorf("%w: %s", ErrRunLocked, path)
		}
		time.Sleep(holderPoll)
	}
}

// release drops the lock. Closing the descriptor releases the flock; the
// explicit LOCK_UN is there so the release is a statement rather than a
// consequence of garbage collection.
func (l *lock) release() error {
	if l == nil || l.f == nil {
		return nil
	}
	unlockErr := syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	closeErr := l.f.Close()
	l.f = nil
	if unlockErr != nil {
		return unlockErr
	}
	return closeErr
}

// probe runs fn while holding the lock, and releases before returning.
//
// This is the second of the three idioms. Nothing may retain the lock past
// fn: an Orphans sweep that held every lock it tested until it finished would
// be indistinguishable, to a concurrent resume, from a live process holding
// the run (mutation M105).
//
// A lock held by somebody else is not an error here — it is the answer. held
// is false and fn is not called.
func probe(path string, fn func() error) (held bool, err error) {
	l, err := acquire(path, 0)
	if err != nil {
		if errors.Is(err, ErrRunLocked) {
			return true, nil
		}
		return false, err
	}
	defer func() {
		if rerr := l.release(); rerr != nil && err == nil {
			err = rerr
		}
	}()
	if fn != nil {
		err = fn()
	}
	return false, err
}
