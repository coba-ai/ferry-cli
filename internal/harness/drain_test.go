package harness_test

// A406 — the transcript receive is bounded, so an abandoned reader fails the
// test instead of hanging it.
//
// `Run` ends the transcript by closing the pty slave, which makes the pending
// read on the master return EIO. That depends on the close dropping the last
// reference to the slave, and it does not when the command left a goroutine
// blocked on a read of the swapped stdin — the fd is still open, the master
// never sees EIO, and the receive never returns.
//
// U5 hit this for real: a consent prompt interrupted by a signal returns from
// `Ask` while its reader is still parked on stdin. The symptom was a 167
// second test that ended in `signal: killed` with no diagnosis, which in CI
// means the job's whole budget spent on a stack trace nobody reads.
//
// Asserting a `t.Errorf` needs a `*testing.T` this test controls, and the real
// one cannot be faked — `testing.TB` has an unexported method precisely to
// stop that. So the failing case runs in a re-executed copy of this binary,
// the same device `pending_test.go` uses for a second flock holder, and the
// parent asserts on what the child reported.

import (
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/kurenn/ferry-cli/internal/harness"
	"github.com/spf13/cobra"
)

// abandonReaderEnv, when set, makes the child run the command that leaves a
// reader behind. Without it that test is skipped, so a normal `go test ./...`
// does not pay its timeout.
const abandonReaderEnv = "FERRY_HARNESS_ABANDON_READER"

// TestAbandonsAReaderOnStdin is the child. It runs a command that returns
// while a goroutine is still blocked reading the process stdin, which is the
// shape that holds the pty slave open.
func TestAbandonsAReaderOnStdin(t *testing.T) {
	if os.Getenv(abandonReaderEnv) == "" {
		t.Skip("child of TestAnAbandonedReaderFailsRatherThanHangs")
	}

	// Short, because the point is the bound firing, not waiting out the
	// production one twice.
	harness.Timeout = 2 * time.Second

	cmd := &cobra.Command{
		Use: "abandon",
		RunE: func(c *cobra.Command, _ []string) error {
			started := make(chan struct{})

			go func() {
				close(started)

				// Never returns: nothing will be written, and the
				// deliberate omission is that nobody waits for this.
				_, _ = os.Stdin.Read(make([]byte, 1))
			}()

			<-started

			// Give the reader time to actually enter the read. Without
			// this the goroutine may not have reached the syscall when
			// Run closes the slave, and the bug does not reproduce — the
			// test would pass for the wrong reason.
			time.Sleep(200 * time.Millisecond)

			return nil
		},
	}

	harness.Run(t, cmd, nil, "", nil, true)
}

func TestAnAbandonedReaderFailsRatherThanHangs(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("finding this test binary: %v", err)
	}

	child := exec.Command(exe, "-test.run=TestAbandonsAReaderOnStdin", "-test.v=true")
	child.Env = append(os.Environ(), abandonReaderEnv+"=1")

	// Generously more than the child's own 2s bound plus its 2s escalation,
	// so this failing is "the bound did not work", never "the machine was
	// slow".
	done := make(chan struct{})

	var out []byte

	var runErr error

	go func() {
		out, runErr = child.CombinedOutput()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(30 * time.Second):
		_ = child.Process.Kill()
		t.Fatal("the child hung, so the transcript receive is still unbounded — which is the " +
			"defect A358 named and A406 claims to have closed")
	}

	if runErr == nil {
		t.Fatalf("the child passed. It abandons a reader on the pty slave, so Run should have "+
			"failed it rather than returning a transcript:\n%s", out)
	}

	report := string(out)

	if !strings.Contains(report, "A358") {
		t.Errorf("the child failed, but not with the diagnosis that names the cause. A hang "+
			"replaced by an unexplained failure is barely an improvement:\n%s", report)
	}

	if !strings.Contains(report, "still held the slave open") {
		t.Errorf("the failure does not say what went wrong:\n%s", report)
	}
}
