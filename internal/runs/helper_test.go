package runs_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/kurenn/ferry-cli/internal/runs"
)

// AC12 and AC90 are two-process claims: a lock that is only ever tested
// within one process proves nothing about flock, whose whole subtlety is that
// it is per open file description and per inode. The test binary re-executes
// itself as the second process.
const (
	helperModeEnv = "FERRY_RUNS_TEST_HELPER"
	helperDirEnv  = "FERRY_RUNS_TEST_DIR"
	helperRunEnv  = "FERRY_RUNS_TEST_RUN"
	helperMSEnv   = "FERRY_RUNS_TEST_MS"
)

func TestMain(m *testing.M) {
	switch os.Getenv(helperModeEnv) {
	case "":
		os.Exit(m.Run())
	case "hold-open":
		os.Exit(helperHoldOpen())
	case "hold-lock":
		os.Exit(helperHoldLock())
	case "probe-orphans":
		os.Exit(helperProbeOrphans())
	default:
		fmt.Fprintf(os.Stderr, "unknown helper mode %q\n", os.Getenv(helperModeEnv))
		os.Exit(2)
	}
}

// helperHoldOpen opens a run as a holder, rewrites the record once, and holds
// the lock until a stop file appears. The rewrite is the point: it is what
// makes the record a different inode from the one the lock was taken on, and
// a lock on the record file would stop working there (mutation M5).
func helperHoldOpen() int {
	dir, runID := os.Getenv(helperDirEnv), os.Getenv(helperRunEnv)
	l := runs.New(nil, dir, nil)

	h, err := l.Open(runID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "helper open: %v\n", err)
		return 1
	}
	defer h.Close()

	if err := h.Record(runs.StepSimulate, &runs.Response{Status: 503}, &runs.Outcome{Class: "pending", ExitCode: 6}); err != nil {
		fmt.Fprintf(os.Stderr, "helper record: %v\n", err)
		return 1
	}
	if err := os.WriteFile(filepath.Join(dir, "ready"), []byte("1"), 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "helper ready: %v\n", err)
		return 1
	}
	waitForFile(filepath.Join(dir, "stop"), 30*time.Second)
	return 0
}

// helperHoldLock takes the sidecar lock with a bare flock — deliberately not
// through the package under test — and holds it for a fixed window, then
// releases. It models the window a probe opens, widened to something a test
// can observe: if a holder's retry covers 100 ms it covers the microseconds a
// real probe takes.
func helperHoldLock() int {
	dir, runID := os.Getenv(helperDirEnv), os.Getenv(helperRunEnv)
	ms, _ := strconv.Atoi(os.Getenv(helperMSEnv))

	f, err := os.OpenFile(filepath.Join(dir, runID+".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		fmt.Fprintf(os.Stderr, "helper open lock: %v\n", err)
		return 1
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		fmt.Fprintf(os.Stderr, "helper flock: %v\n", err)
		return 1
	}
	if err := os.WriteFile(filepath.Join(dir, "ready"), []byte("1"), 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "helper ready: %v\n", err)
		return 1
	}
	time.Sleep(time.Duration(ms) * time.Millisecond)
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_UN); err != nil {
		fmt.Fprintf(os.Stderr, "helper unlock: %v\n", err)
		return 1
	}
	return 0
}

// helperProbeOrphans runs the real Orphans probe repeatedly until a stop file
// appears. A probe that failed to release would make the parent's concurrent
// Open fail (mutation M105).
func helperProbeOrphans() int {
	dir := os.Getenv(helperDirEnv)
	l := runs.New(nil, dir, nil)
	if err := os.WriteFile(filepath.Join(dir, "ready"), []byte("1"), 0o600); err != nil {
		return 1
	}
	stop := filepath.Join(dir, "stop")
	for {
		if _, err := os.Stat(stop); err == nil {
			return 0
		}
		if _, err := l.Orphans("", ""); err != nil {
			fmt.Fprintf(os.Stderr, "helper orphans: %v\n", err)
			return 1
		}
	}
}

func waitForFile(path string, within time.Duration) bool {
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return true
		}
		time.Sleep(2 * time.Millisecond)
	}
	return false
}

// startHelper launches the test binary in a helper mode and returns a
// function that stops it.
func startHelper(t *testing.T, mode, dir, runID string, extra ...string) func() {
	t.Helper()

	env := append(os.Environ(),
		helperModeEnv+"="+mode,
		helperDirEnv+"="+dir,
		helperRunEnv+"="+runID,
	)
	env = append(env, extra...)

	cmd := exec.Command(os.Args[0])
	cmd.Env = env
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper %s: %v", mode, err)
	}

	stopped := false
	stop := func() {
		if stopped {
			return
		}
		stopped = true
		_ = os.WriteFile(filepath.Join(dir, "stop"), []byte("1"), 0o600)
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
		_ = os.Remove(filepath.Join(dir, "stop"))
		_ = os.Remove(filepath.Join(dir, "ready"))
	}
	t.Cleanup(stop)

	if !waitForFile(filepath.Join(dir, "ready"), 15*time.Second) {
		stop()
		t.Fatalf("helper %s never reported ready", mode)
	}
	return stop
}
