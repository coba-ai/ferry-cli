//go:build faultinject

package cli_test

import (
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/kurenn/ferry-cli/internal/fault"
)

// AC41's last two cases: a panic in a renderer, and SIGINT.
//
// They are here rather than in `json_test.go` because reaching them needs the
// injector, and the gate runs both builds. The claim is the same one: exactly
// one document, nothing else on stdout, and `outcome.exit_code` equal to the
// observed exit status.

func TestAPanicInARendererWritesOneDocument(t *testing.T) {
	server, home := loggedIn(t, "execute.201.processing")

	stdout, stderr, exit := run(t, invocation{
		home: home,
		env:  map[string]string{fault.EnvVar: fault.RenderPanicsAfter201},
		args: []string{"transfers", "execute", "--plan", recordedPlanToken, "--yes", "--output", "json"},
	})

	// C17: the request left, so the panic is 6 and not 1.
	if exit != 6 {
		t.Fatalf("exit = %d, want 6\nstdout:\n%s\nstderr:\n%s", exit, stdout, stderr)
	}

	if got := requestsTo(server, executePath); got != 1 {
		t.Errorf("execute requests = %d, want 1", got)
	}

	docs := documents(t, stdout)

	// The interesting failure here is *two* documents: the panic fires
	// inside the report, and a report that had already written its
	// envelope before panicking would leave a partial document on stdout
	// with a whole one appended after it.
	if len(docs) != 1 {
		t.Fatalf("stdout carries %d documents, want 1:\n%q", len(docs), stdout)
	}

	if got := exitCodeOf(t, docs[0]); got != exit {
		t.Errorf("outcome.exit_code = %d but the process exited %d", got, exit)
	}

	// The document has to name the run, or a caller holding a machine
	// document has nothing to pass to `runs resume`.
	block, _ := docs[0]["ferry_cli"].(map[string]any)
	if block == nil || block["run_id"] == nil {
		t.Errorf("the document does not name the run:\n%s", stdout)
	}
}

func TestSIGINTWritesOneDocument(t *testing.T) {
	server, home := loggedIn(t, "execute.201.processing")

	arrived, release := server.Hold("execute.201.processing")

	go func() {
		<-arrived

		_ = syscall.Kill(os.Getpid(), syscall.SIGINT)

		time.Sleep(100 * time.Millisecond)
		release()
	}()

	stdout, stderr, exit := run(t, invocation{
		home:    home,
		signals: true,
		args:    []string{"transfers", "execute", "--plan", recordedPlanToken, "--yes", "--output", "json"},
	})

	if exit != 6 {
		t.Fatalf("exit = %d, want 6\nstdout:\n%s\nstderr:\n%s", exit, stdout, stderr)
	}

	docs := documents(t, stdout)

	if len(docs) != 1 {
		t.Fatalf("stdout carries %d documents, want 1:\n%q", len(docs), stdout)
	}

	if got := exitCodeOf(t, docs[0]); got != exit {
		t.Errorf("outcome.exit_code = %d but the process exited %d", got, exit)
	}
}

// A simulated process death writes **no** document, and this is AC41's
// boundary rather than an exception to it.
//
// The document is the CLI's answer. A killed process gives none, so a test
// that found one here would have found the CLI reporting an outcome for an
// exit it did not survive — and `internal/fault`'s whole purpose is to be
// indistinguishable from `kill -9` at that point.
func TestASimulatedDeathWritesNothing(t *testing.T) {
	server, home := loggedIn(t, "simulate.201")

	stdout, stderr, exit := run(t, invocation{
		home: home,
		env:  map[string]string{fault.EnvVar: fault.AfterRecordWritten},
		args: append(append([]string{"transfers", "create"}, canonicalSimulateArgs()...),
			"--output", "json"),
	})

	if exit != fault.CrashExit {
		t.Fatalf("exit = %d, want %d\n%s", exit, fault.CrashExit, stdout)
	}

	if stdout != "" {
		t.Errorf("a killed process wrote to stdout:\n%q", stdout)
	}

	if stderr != "" {
		t.Errorf("a killed process wrote to stderr:\n%q", stderr)
	}

	if got := len(server.Requests()); got != 0 {
		t.Errorf("the fixture saw %d request(s), want 0", got)
	}

	// The exit code is outside the range any outcome class produces, so a
	// caller cannot mistake it for a classification. If a class ever
	// claimed 137 this test is the one that should be revisited.
	if fault.CrashExit <= 8 {
		t.Errorf("the crash exit code is %d, which collides with the outcome classes", fault.CrashExit)
	}
}
