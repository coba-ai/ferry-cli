// Package fault is the process-level fault injector of PLAN §6.1.
//
// It exists so that the windows on the money path can be opened in a test:
// the instant after the run record is on disk and before the request leaves,
// the instant after the request left and before the answer is recorded, the
// instant the prompt is on the screen and nobody has answered. Those windows
// are where C1, C16 and C17 live, and none of them can be reached by any
// input the CLI accepts.
//
// # Compiled out by default
//
// Every injection point is a call to a function that is a no-op unless the
// binary was built with `-tags faultinject`. A release binary therefore
// cannot be made to crash by an environment variable, and
// `fault_release_test.go` asserts exactly that in the default build rather
// than leaving it to the reader of the build tags.
//
// # A simulated death is a panic, not os.Exit
//
// [Die] panics with a [Crash] value. `cmd/ferry/main.go` recognises it and
// exits [CrashExit] without writing anything further, which is as close to a
// killed process as a process can get to itself; a test running the command
// in-process through `harness.Run` sees the same exit code.
//
// The honest caveat, stated here because it would otherwise be discovered by
// someone reading a green test: unwinding runs deferred functions, and a
// SIGKILL does not. The only deferred work on the money path is releasing the
// sidecar lock and closing the response body — neither writes to the run
// record — so the record a fault-point test observes is the record a killed
// process would have left. A deferred write added later would break that
// equivalence silently, which is why `crash_test.go` asserts the step's state
// and not merely that the process stopped.
package fault

import (
	"fmt"
	"io/fs"
	"os"
	"sync/atomic"

	"github.com/kurenn/ferry-cli/internal/fsx"
)

// EnvVar names the fault point to fire.
const EnvVar = "FERRY_CLI_FAULT"

// CrashExit is the exit code a simulated process death produces. 137 is
// 128+SIGKILL, so it cannot be confused with any outcome class of PLAN §5.6 —
// a crash that exited 1 or 6 would be indistinguishable from the CLI
// reporting one, which is the very thing these tests exist to tell apart.
const CrashExit = 137

// The fault points of PLAN §6.1.
const (
	// AfterRecordWritten fires after runs.Begin has returned and before the
	// request is sent (AC44).
	AfterRecordWritten = "after_record_written"

	// AfterSendBeforeRecord fires after the answer arrived and before it is
	// recorded (AC45).
	AfterSendBeforeRecord = "after_send_before_record"

	// RecordFailsAfterSend makes the filesystem answer ENOSPC to the write
	// runs.Record makes (AC69(b)). It is not a death: the process stays up
	// and has to decide what to say, which is the whole point.
	RecordFailsAfterSend = "record_fails_after_send"

	// RenderPanicsAfter201 panics after Record returned nil and before a
	// byte of the report is written (AC69(c)).
	RenderPanicsAfter201 = "render_panics_after_201"

	// AtPrompt fires after the prompt text has been written and before the
	// read begins (AC72, V9). The position is the whole of the control: a
	// CLI that wrote awaiting_confirmation only on `y` would leave
	// not_started here.
	AtPrompt = "at_prompt"

	// AfterSimulateRecordedBeforeExecuteBegin is the first --broadcast
	// inter-step window (AC80).
	AfterSimulateRecordedBeforeExecuteBegin = "after_simulate_recorded_before_execute_begin"

	// AfterSimulateSendBeforeRecord is the second (AC80).
	AfterSimulateSendBeforeRecord = "after_simulate_send_before_record"
)

// Points is every declared fault point.
//
// It is a census in the sense `runs.AllStates` is: `fault_test.go` derives
// the declared constants from this package's own syntax tree and holds them
// equal to this list in both directions, so a point added without a row here
// fails rather than being silently unreachable from FERRY_CLI_FAULT.
func Points() []string {
	return []string{
		AfterRecordWritten,
		AfterSendBeforeRecord,
		RecordFailsAfterSend,
		RenderPanicsAfter201,
		AtPrompt,
		AfterSimulateRecordedBeforeExecuteBegin,
		AfterSimulateSendBeforeRecord,
	}
}

// Known reports whether name is a declared point.
func Known(name string) bool {
	for _, p := range Points() {
		if p == name {
			return true
		}
	}

	return false
}

// Crash is the panic value a simulated process death carries.
type Crash struct{ Point string }

func (c Crash) Error() string {
	return "fault: simulated process death at " + c.Point
}

// Requested is the point named by the environment, or "".
//
// It is read here, in the always-compiled half, so that the release build can
// be asserted to ignore it rather than to be unable to see it.
func Requested() string { return os.Getenv(EnvVar) }

// Guard is a filesystem that starts failing writes once [Guard.Trip] is
// called.
//
// It is the seam AC69(b) names: "the fs seam returns ENOSPC from Record". The
// wrapper is installed on every money command in every build, and only
// [RecordFailsAfterSend] ever trips it, so the production path runs through
// exactly the code the test does.
type Guard struct {
	fsx.FS

	tripped atomic.Bool
	err     error
}

// NewGuard wraps base. A nil base is the real filesystem.
func NewGuard(base fsx.FS) *Guard {
	if base == nil {
		base = fsx.OS()
	}

	return &Guard{FS: base, err: fsx.ENOSPC}
}

// Trip makes every subsequent write fail.
func (g *Guard) Trip() { g.tripped.Store(true) }

// Tripped reports whether writes are failing.
func (g *Guard) Tripped() bool { return g.tripped.Load() }

// Create fails once tripped. The failure is at Create rather than at Write so
// that nothing of the new record reaches the temp file: a half-written temp
// file that was never renamed is what a real ENOSPC leaves, and the record at
// the final path is the one from before.
func (g *Guard) Create(name string, perm fs.FileMode) (fsx.File, error) {
	if g.tripped.Load() {
		return nil, fmt.Errorf("create %s: %w", name, g.err)
	}

	return g.FS.Create(name, perm)
}
