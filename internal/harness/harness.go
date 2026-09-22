// Package harness runs a constructed cobra command in-process and returns
// what a user would have seen: stdout, stderr and the exit code.
//
// It takes a *cobra.Command rather than building one. The harness cannot know
// the root command, because the root is assembled by internal/cli, which is
// built after every package it wires together — a harness that constructed
// the root would make the build order circular.
//
// # Not parallel-safe
//
// Run swaps os.Stdout, os.Stderr and os.Stdin for the duration of the call,
// and sets environment variables with t.Setenv. Both are process-global, so
// no test that calls Run may call t.Parallel(). t.Setenv already enforces
// this by panicking in a parallel test; the stream swap does not, so this is
// the only warning for that half.
package harness

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// Timeout bounds a single Run. A command that blocks forever on a read from
// an empty stdin would otherwise hang the package's whole test binary until
// the go test timeout fires, which reports a stack dump for every goroutine
// instead of naming the command that hung.
var Timeout = 10 * time.Second

// ExitCoder is how a command's error carries a process exit code. It is
// declared here structurally so internal/cli need not import this package:
// any error with an ExitCode() int method satisfies it.
type ExitCoder interface {
	ExitCode() int
}

// Run executes cmd with args, feeding stdin and the environment in env, and
// returns what the command wrote and the exit code it would have produced.
//
// exit is 0 when Execute returns nil, the code from an [ExitCoder] error, and
// 1 for any other error — which is what a main that prints the error and
// exits non-zero would do.
//
// env is the whole of the command's view of FERRY_* and XDG_*: any such
// variable set in the ambient environment but absent from env is unset for
// the call, so a test cannot pass because of the developer's shell. HOME is
// redirected to an empty temp directory unless env names it, so a command
// that falls back to ~/.config cannot touch a real home directory.
//
// When tty is true the command runs on a pseudo-terminal, so term.IsTerminal
// on any of the three descriptors is true and a consent prompt takes its
// interactive path. A pty has one buffer, so stdout and stderr are
// unavoidably interleaved; both return values are that one merged transcript.
// Assert on the merged text, and do not read stderr being non-empty as
// meaning the command wrote to stderr.
func Run(t *testing.T, cmd *cobra.Command, args []string, stdin string, env map[string]string, tty bool) (stdout, stderr string, exit int) {
	t.Helper()

	applyEnv(t, env)

	if tty {
		merged, code := runOnPTY(t, cmd, args, stdin)
		return merged, merged, code
	}
	return runOnPipes(t, cmd, args, stdin)
}

// applyEnv gives the command a closed environment.
//
// Inheriting the ambient FERRY_* and XDG_* is how a test starts passing on
// one machine and failing on another, so anything the test did not name is
// removed rather than left.
func applyEnv(t *testing.T, env map[string]string) {
	t.Helper()

	for _, kv := range os.Environ() {
		key, _, _ := strings.Cut(kv, "=")
		if !strings.HasPrefix(key, "FERRY_") && !strings.HasPrefix(key, "XDG_") {
			continue
		}
		if _, named := env[key]; !named {
			t.Setenv(key, "")
			_ = os.Unsetenv(key)
		}
	}

	if _, named := env["HOME"]; !named {
		t.Setenv("HOME", t.TempDir())
	}
	for k, v := range env {
		t.Setenv(k, v)
	}
}

// execute runs the command and converts its error into an exit code.
//
// A panic is not swallowed: the captured output is restored and logged first
// so the transcript leading up to it is readable, and then it is re-panicked,
// because a harness that turned a panic into an exit code would let AC69's
// "the renderer panicked after a 201" pass without the panic being noticed.
func execute(cmd *cobra.Command, args []string, out, errOut io.Writer, in io.Reader) (exit int) {
	cmd.SetArgs(args)
	cmd.SetOut(out)
	cmd.SetErr(errOut)
	cmd.SetIn(in)

	if err := cmd.Execute(); err != nil {
		var coder ExitCoder
		if errors.As(err, &coder) {
			return coder.ExitCode()
		}
		return 1
	}
	return 0
}

func runOnPipes(t *testing.T, cmd *cobra.Command, args []string, stdin string) (string, string, int) {
	t.Helper()

	outR, outW := mustPipe(t)
	errR, errW := mustPipe(t)
	inR, inW := mustPipe(t)

	// Fill stdin from a goroutine: a string larger than the pipe buffer would
	// deadlock a synchronous write before the command ever ran.
	go func() {
		_, _ = io.WriteString(inW, stdin)
		_ = inW.Close()
	}()

	outDone := drain(outR)
	errDone := drain(errR)

	// The command's own writers are set to these pipes, and the process
	// globals are swapped too, so output from a package that reaches for
	// os.Stdout directly is captured rather than escaping into the test log.
	restore := swapStd(inR, outW, errW)

	exit, panicked := runBounded(t, func() int {
		return execute(cmd, args, outW, errW, inR)
	}, func() {
		// Unblock a command stuck reading stdin.
		_ = inR.Close()
	})

	restore()
	_ = outW.Close()
	_ = errW.Close()

	stdout, stderr := <-outDone, <-errDone
	_ = outR.Close()
	_ = errR.Close()
	_ = inR.Close()

	if panicked != nil {
		t.Logf("captured stdout before the panic:\n%s", stdout)
		t.Logf("captured stderr before the panic:\n%s", stderr)
		panic(panicked)
	}
	return stdout, stderr, exit
}

func runOnPTY(t *testing.T, cmd *cobra.Command, args []string, stdin string) (string, int) {
	t.Helper()

	master, slave, err := openPTY()
	if err != nil {
		t.Fatalf("harness: %v", err)
	}

	// Raw mode on the slave turns off ECHO and ONLCR. Without it the line
	// discipline echoes the fed stdin back into the captured output, so a
	// test asserting on the prompt would match the answer it just typed, and
	// every newline would arrive as \r\n.
	// Nothing restores the mode afterwards: this pty exists for one Run and
	// is closed below, so there is no terminal left whose state could matter.
	if _, err := term.MakeRaw(int(slave.Fd())); err != nil {
		t.Fatalf("harness: raw mode on the pty: %v", err)
	}

	transcript := drain(master)

	if stdin != "" {
		go func() { _, _ = io.WriteString(master, stdin) }()
	}

	restore := swapStd(slave, slave, slave)

	exit, panicked := runBounded(t, func() int {
		return execute(cmd, args, slave, slave, slave)
	}, func() {
		// Closing the master makes the slave's pending read return EIO,
		// which is the only way to unblock an in-process read on a pty.
		_ = master.Close()
	})

	restore()

	// The order matters. Closing the slave is what ends the transcript: with
	// no slave open, the pending read on the master returns EIO, which drain
	// treats as EOF. Closing the master first would instead discard whatever
	// the kernel had buffered but drain had not yet read, and the loss is
	// timing-dependent — the command's last line, including a prompt, would
	// go missing in some runs and not others.
	_ = slave.Close()
	merged := drainBounded(t, transcript, master)
	_ = master.Close()

	if panicked != nil {
		t.Logf("captured pty transcript before the panic:\n%s", merged)
		panic(panicked)
	}
	return merged, exit
}

// drainBounded waits for the transcript, and gives up rather than hanging.
//
// Closing the slave normally ends the drain: with no slave open the pending
// read on the master returns EIO. "No slave open" is the part that can fail.
// The fd was installed as the process stdin, so a goroutine the command
// abandoned mid-read still holds a reference, the close above drops nothing,
// and the receive below never returns (A358). U5 hit exactly this: a consent
// prompt interrupted by a signal returns from Ask while its reader goroutine
// is still blocked on stdin.
//
// runBounded already bounds the command for the same class of reason. This is
// the other half — a command that returned cleanly can still leave a reader
// behind, and until now that hung the test rather than failing it. A hang
// costs the whole CI job's budget and reports nothing; a failure names the
// cause.
func drainBounded(t *testing.T, transcript <-chan string, master *os.File) string {
	t.Helper()

	select {
	case merged := <-transcript:
		return merged
	case <-time.After(Timeout):
	}

	// Closing the master is the only remaining way to end the drain. It
	// discards whatever the kernel had buffered but drain had not yet read,
	// which is why it is not the normal path — but this is already a
	// failure, and a truncated transcript makes a better diagnostic than
	// none.
	_ = master.Close()

	select {
	case merged := <-transcript:
		t.Errorf("harness: the transcript did not end within %s after the pty slave was closed, "+
			"so something still held the slave open — an abandoned goroutine blocked on a read "+
			"of the swapped stdin is the shape that does it (A358). Closing the master released "+
			"it. The transcript below may be truncated:\n%s", Timeout, merged)

		return merged
	case <-time.After(2 * time.Second):
		t.Fatalf("harness: the transcript did not end within %s and closing the pty master did "+
			"not release it either", Timeout)

		return ""
	}
}

// runBounded runs fn, and if it has not returned within Timeout calls unblock
// and fails the test. It returns the recovered panic value, if any, rather
// than re-panicking, so the caller can restore the process streams first.
func runBounded(t *testing.T, fn func() int, unblock func()) (exit int, panicked any) {
	t.Helper()

	type result struct {
		exit     int
		panicked any
	}
	done := make(chan result, 1)
	go func() {
		r := result{}
		defer func() {
			if v := recover(); v != nil {
				r.panicked = v
			}
			done <- r
		}()
		r.exit = fn()
	}()

	select {
	case r := <-done:
		return r.exit, r.panicked
	case <-time.After(Timeout):
		unblock()
	}

	select {
	case r := <-done:
		// It was blocked on a read that unblock released. That is still a
		// test defect — the command wanted input the test did not supply.
		t.Errorf("harness: the command did not return within %s; closing its streams unblocked "+
			"it, so it was waiting on input the test did not supply.", Timeout)
		return r.exit, r.panicked
	case <-time.After(2 * time.Second):
		t.Fatalf("harness: the command did not return within %s and closing its streams did not "+
			"unblock it", Timeout)
		return 1, nil
	}
}

// drain reads r to EOF in the background. A pty master returns EIO rather
// than EOF once the slave is closed, and that is a normal end of transcript,
// not a failure.
func drain(r io.Reader) <-chan string {
	ch := make(chan string, 1)
	go func() {
		var b strings.Builder
		_, err := io.Copy(&b, r)
		if err != nil && !isEIO(err) {
			fmt.Fprintf(&b, "\n[harness: read error: %v]", err)
		}
		ch <- b.String()
	}()
	return ch
}

func isEIO(err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, os.ErrClosed) ||
		strings.Contains(err.Error(), "input/output error") ||
		strings.Contains(err.Error(), "file already closed")
}

// swapStd points the process streams at the given files and returns a
// function restoring them. The mutex makes concurrent misuse fail as a
// deadlock-free lock contention rather than as interleaved corruption.
var stdMu sync.Mutex

func swapStd(in, out, errOut *os.File) func() {
	stdMu.Lock()
	oldIn, oldOut, oldErr := os.Stdin, os.Stdout, os.Stderr
	os.Stdin, os.Stdout, os.Stderr = in, out, errOut
	return func() {
		os.Stdin, os.Stdout, os.Stderr = oldIn, oldOut, oldErr
		stdMu.Unlock()
	}
}

func mustPipe(t *testing.T) (*os.File, *os.File) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("harness: pipe: %v", err)
	}
	return r, w
}
