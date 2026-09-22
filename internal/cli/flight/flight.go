// Package flight holds C17's in-flight flag and the two identifiers an
// abnormal exit has to be able to name.
//
// # Why it is its own package
//
// PLAN §5.13 puts the in-flight flag in `internal/cli`, and that is where it
// belongs by responsibility. It cannot live in that package by compilation:
// `internal/cli` builds the command tree, so it imports the money noun
// packages, and the money noun packages are what set the flag. A single
// package would be an import cycle. This is a directory under
// `internal/cli/`, which is the prefix `cli/OWNERSHIP` gives U5, so the split
// is a package boundary and not a change of owner (amendment A350).
//
// # The flag is monotonic and this package offers no way to clear it
//
// [Flag.MarkSent] is the only writer of the flag and it only ever stores
// true. There is no Clear, no Reset and no setter taking a bool — not as a
// matter of discipline but as a matter of the API surface, because V1's
// finding was that revision 2 cleared the flag when `Record` returned nil and
// so reported "nothing was sent" for a process that had sent something and
// then panicked. `flight_test.go` walks this package's own syntax tree and
// fails if any assignment to the field stores anything but `true`.
//
// # Per invocation, not per process
//
// C17 says "process-wide" because a released binary runs one invocation.
// Here the flag is a value the root creates once and threads through the
// command tree, which is the same thing in a binary and is the only thing
// that is testable at all: `harness.Run` runs many commands in one process,
// and a package-level flag would carry the first test's send into the tenth.
// The property C17 needs — no code path clears it for the rest of the
// invocation — is unchanged, and it is the property the census test asserts.
package flight

import (
	"sync"
	"sync/atomic"

	"github.com/coba-ai/ferry-cli/internal/outcome"
)

// OutcomeFor builds an outcome for a class this CLI decided on its own —
// a decline, a refused orphan check, an abnormal exit — rather than from an
// API answer.
//
// It goes through `outcome.Properties` so the exit code, the money answer and
// the same-key answer come from the one table (C4). Nothing here may pass
// them in: a caller that could choose an exit code could pair `cli_fault`
// with 6, or `pending` with 0, and the second of those is a transfer that
// moved money reported as a success.
func OutcomeFor(class outcome.Class, next string, warnings ...string) outcome.Outcome {
	out, ok := outcome.Properties(class)
	if !ok {
		panic("flight: no properties declared for outcome class " + string(class))
	}

	out.Next = next
	out.Warnings = warnings

	return out
}

// Flag is one invocation's money state.
type Flag struct {
	// sent is C17. Written only by MarkSent, only ever to true.
	sent atomic.Bool

	mu        sync.Mutex
	runID     string
	commandID string
	profile   string
	env       string
}

// New returns a flag for one invocation, with nothing sent.
func New() *Flag { return &Flag{} }

// MarkSent records that a money request is about to leave, or has left.
//
// It is called immediately *before* `api.Do` on a money step and never after
// (mutation M76): a flag set once the answer is back cannot describe the
// window in which the answer never comes, which is the only window that
// matters.
func (f *Flag) MarkSent() {
	if f == nil {
		return
	}

	f.sent.Store(true)
}

// MayHaveSent reports whether a money request may have left this process.
//
// A nil Flag reports false, so a command built without one — every read
// command — answers "nothing was sent", which is true of it.
func (f *Flag) MayHaveSent() bool { return f != nil && f.sent.Load() }

// NoteRun records the run this invocation is working on, so that an abnormal
// exit can print `ferry runs show <id>` rather than "something went wrong".
func (f *Flag) NoteRun(id string) {
	if f == nil {
		return
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	f.runID = id
}

// NoteCommand records the FERRY command id, once one is known.
func (f *Flag) NoteCommand(id string) {
	if f == nil || id == "" {
		return
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	f.commandID = id
}

// NoteProfile records the profile and environment the invocation resolved to,
// for the `ferry_cli` block of a document the root has to write on its own.
func (f *Flag) NoteProfile(profile, environment string) {
	if f == nil {
		return
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	f.profile, f.env = profile, environment
}

// RunID is the run this invocation minted or resumed, or "".
func (f *Flag) RunID() string {
	if f == nil {
		return ""
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	return f.runID
}

// CommandID is the FERRY command id, or "".
func (f *Flag) CommandID() string {
	if f == nil {
		return ""
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	return f.commandID
}

// Profile is the profile the invocation resolved to, or "".
func (f *Flag) Profile() string {
	if f == nil {
		return ""
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	return f.profile
}

// Environment is the environment the credential resolved to, or "".
func (f *Flag) Environment() string {
	if f == nil {
		return ""
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	return f.env
}
