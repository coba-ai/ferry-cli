package transfers_test

import (
	"bufio"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

// AC81's last arm, and the one that needs a second process: **a run whose
// lock a live process holds does not trigger the orphan refusal.**
//
// The three arms are one property seen from three sides. A run left
// unresolved by a process that is gone must refuse the next money command
// (C18, `TestAnUnresolvedRunRefusesTheNextMoneyCommand`); `--allow-pending`
// must get past it (`TestAllowPendingProceedsPastAnUnresolvedRun`); and a
// run that is unresolved *because another ferry is working on it right now*
// must not. Without the third, two terminals is a deadlock: the first
// invocation's run is unresolved for as long as it is in flight, so the
// second refuses, and refuses again, for a run that is about to settle.
//
// It has to be a real second process. `flock` is held per open file
// description, so a second descriptor opened *in this process* would
// conflict just as a second process's would — which means an in-process
// "hold" would pass this test against an implementation that could never
// work. But the reverse is the failure that matters: `runs.probe` reports
// held-by-another for `EWOULDBLOCK`, and only another process can produce
// the situation the CLI will actually meet. A345 is the rule — the layer
// decides whether it is a control — and the layer here is the operating
// system's lock table.
func TestARunLockedByALiveProcessIsNotAnOrphan(t *testing.T) {
	server, home := loggedIn(t, "execute.202.then_completed", "execute.replay.201")

	// Arrange an unresolved run the same way the refusal test does: a 202
	// and a timeout leave the step `answered`, which is unresolved.
	_, _, exit := run(t, invocation{
		home:  home,
		clock: frozenClock(),
		args: []string{"transfers", "execute", "--plan", recordedPlanToken, "--yes",
			"--timeout", "1s"},
	})

	if exit != 6 {
		t.Fatalf("the arranging run exited %d, want 6", exit)
	}

	unresolved := onlyRun(t, home)

	lockPath := ledgerOf(t, home).LockPath(unresolved.RunID)

	// The control first, in this order deliberately: with no holder the
	// run *does* refuse. Running it before the holder starts means the
	// pass below cannot be "the orphan check never fires", which is the
	// shape a vacuous version of this test would take.
	_, stderr, exit := run(t, invocation{
		home: home,
		args: []string{"transfers", "execute", "--plan", recordedPlanToken, "--yes"},
	})

	if exit != 1 {
		t.Fatalf("with no holder the unresolved run must refuse the next money command; "+
			"exit = %d\n%s", exit, stderr)
	}

	stop := holdTheLock(t, lockPath)

	sent := len(server.Requests())

	stdout, stderr, exit := run(t, invocation{
		home: home,
		args: []string{"transfers", "execute", "--plan", recordedPlanToken, "--yes"},
	})

	stop()

	// It proceeded — which against `execute.replay.201` means it sent and
	// settled. The exit code is whatever that recording settles as; what
	// this test claims is that the orphan check did **not** stop it, and
	// the way to see that is a request having left.
	if got := len(server.Requests()); got == sent {
		t.Errorf("no request was sent while another process held the run's lock, so the "+
			"orphan check refused a run that a live ferry is working on. Two terminals "+
			"would deadlock (AC81).\nexit = %d\n%s\n%s", exit, stdout, stderr)
	}

	if strings.Contains(stdout+stderr, "--allow-pending") {
		t.Errorf("the orphan refusal's guidance was printed for a locked run:\n%s\n%s",
			stdout, stderr)
	}
}

// holdTheLock starts a second process holding `flock(LOCK_EX)` on path and
// returns a function that stops it.
//
// It re-executes this test binary, which is the way to get a second process
// without adding a dependency or assuming `/usr/bin/flock` exists. The
// child signals that it has the lock on stdout, so the caller never races
// the acquisition — a sleep here would make the test pass on a slow machine
// and pass for the wrong reason on a fast one.
func holdTheLock(t *testing.T, path string) func() {
	t.Helper()

	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("finding this test binary: %v", err)
	}

	cmd := exec.Command(exe, "-test.run=TestHoldsALockAndWaits", "-test.v=false")
	cmd.Env = append(os.Environ(), lockHolderEnv+"="+path)

	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}

	if err := cmd.Start(); err != nil {
		t.Fatalf("starting the lock holder: %v", err)
	}

	stopped := false

	stop := func() {
		if stopped {
			return
		}

		stopped = true

		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}

	t.Cleanup(stop)

	// Wait for the child to say it has the lock.
	ready := make(chan string, 1)

	go func() {
		scanner := bufio.NewScanner(out)
		for scanner.Scan() {
			if strings.TrimSpace(scanner.Text()) == lockHeldMarker {
				ready <- ""

				return
			}
		}

		ready <- "the lock holder exited without taking the lock"
	}()

	select {
	case msg := <-ready:
		if msg != "" {
			stop()
			t.Fatal(msg)
		}
	case <-time.After(10 * time.Second):
		stop()
		t.Fatal("the lock holder did not take the lock within 10s")
	}

	return stop
}

const (
	lockHolderEnv  = "FERRY_TEST_HOLD_LOCK"
	lockHeldMarker = "LOCK-HELD"
)

// TestHoldsALockAndWaits is the child process.
//
// It is a test function because that is how a Go test binary is re-entered.
// Without the environment variable it does nothing at all, so it costs the
// normal run one no-op.
func TestHoldsALockAndWaits(t *testing.T) {
	path := os.Getenv(lockHolderEnv)
	if path == "" {
		t.Skip("not the lock holder")
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}

	defer f.Close()

	// LOCK_EX without LOCK_NB: if the parent is mid-probe this waits for
	// it rather than reporting a failure that is really a race.
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatalf("flock %s: %v", path, err)
	}

	//nolint:forbidigo // this is the child's only channel to the parent.
	os.Stdout.WriteString(lockHeldMarker + "\n")

	// Hold until killed. The parent kills it; the ceiling is there so a
	// parent that died cannot leave this behind forever.
	time.Sleep(2 * time.Minute)
}
