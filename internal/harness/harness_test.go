package harness_test

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/kurenn/ferry-cli/internal/harness"
)

// exitErr is the shape internal/cli's errors will have: an error carrying the
// exit code the process should use.
type exitErr struct {
	code int
	msg  string
}

func (e exitErr) Error() string { return e.msg }
func (e exitErr) ExitCode() int { return e.code }

func newExit(code int, m string) error { return exitErr{code: code, msg: m} }

// cmd builds a command whose Run is fn, with cobra's own output silenced so a
// test's assertions are about what the command wrote.
func cmd(use string, fn func(c *cobra.Command, args []string) error) *cobra.Command {
	c := &cobra.Command{Use: use, RunE: fn, SilenceUsage: true, SilenceErrors: true}
	return c
}

func TestRunCapturesBothStreamsSeparately(t *testing.T) {
	c := cmd("say", func(c *cobra.Command, args []string) error {
		fmt.Fprint(c.OutOrStdout(), "on stdout")
		fmt.Fprint(c.ErrOrStderr(), "on stderr")
		return nil
	})

	stdout, stderr, exit := harness.Run(t, c, nil, "", nil, false)

	if stdout != "on stdout" {
		t.Errorf("stdout = %q", stdout)
	}
	if stderr != "on stderr" {
		t.Errorf("stderr = %q", stderr)
	}
	if exit != 0 {
		t.Errorf("exit = %d, want 0", exit)
	}
}

// A package that writes to os.Stdout directly rather than through cobra must
// still be captured, or its output would escape into the test log and an
// assertion that nothing was printed would pass vacuously.
func TestRunCapturesWritesToTheProcessStreams(t *testing.T) {
	c := cmd("say", func(c *cobra.Command, args []string) error {
		fmt.Fprint(os.Stdout, "direct to os.Stdout")
		fmt.Fprint(os.Stderr, "direct to os.Stderr")
		return nil
	})

	stdout, stderr, _ := harness.Run(t, c, nil, "", nil, false)

	if stdout != "direct to os.Stdout" {
		t.Errorf("stdout = %q", stdout)
	}
	if stderr != "direct to os.Stderr" {
		t.Errorf("stderr = %q", stderr)
	}
}

func TestRunPassesArgsAndFlags(t *testing.T) {
	var got []string
	var flag string
	c := cmd("say", func(c *cobra.Command, args []string) error {
		got = args
		return nil
	})
	c.Flags().StringVar(&flag, "env", "", "")

	if _, _, exit := harness.Run(t, c, []string{"a", "b", "--env", "live"}, "", nil, false); exit != 0 {
		t.Fatalf("exit = %d", exit)
	}
	if strings.Join(got, ",") != "a,b" {
		t.Errorf("args = %v", got)
	}
	if flag != "live" {
		t.Errorf("--env = %q", flag)
	}
}

func TestRunExitCodes(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"no error", nil, 0},
		{"an ExitCoder", newExit(6, "pending"), 6},
		{"an ExitCoder wrapping zero", newExit(0, "fine"), 0},
		{"a plain error", errors.New("boom"), 1},
		{"a wrapped ExitCoder", fmt.Errorf("context: %w", exitErr{code: 4, msg: "refused"}), 4},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := cmd("x", func(c *cobra.Command, args []string) error { return tc.err })
			if _, _, exit := harness.Run(t, c, nil, "", nil, false); exit != tc.want {
				t.Fatalf("exit = %d, want %d", exit, tc.want)
			}
		})
	}
}

func TestRunFeedsStdin(t *testing.T) {
	var read string
	c := cmd("ask", func(c *cobra.Command, args []string) error {
		line, err := bufio.NewReader(c.InOrStdin()).ReadString('\n')
		if err != nil {
			return err
		}
		read = strings.TrimSpace(line)
		return nil
	})

	if _, _, exit := harness.Run(t, c, nil, "yes\n", nil, false); exit != 0 {
		t.Fatalf("exit = %d", exit)
	}
	if read != "yes" {
		t.Errorf("the command read %q", read)
	}
}

// A command reading os.Stdin directly — which term.ReadPassword and any
// prompt that checks the descriptor must do — sees the same input.
func TestRunFeedsTheProcessStdin(t *testing.T) {
	var read string
	c := cmd("ask", func(c *cobra.Command, args []string) error {
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil {
			return err
		}
		read = strings.TrimSpace(line)
		return nil
	})

	harness.Run(t, c, nil, "transfer\n", nil, false)
	if read != "transfer" {
		t.Errorf("the command read %q", read)
	}
}

// An empty stdin must be EOF, not a hang: this is the headless path, where a
// prompt reading EOF must decline rather than block forever (AC49).
func TestEmptyStdinIsEOFRatherThanAHang(t *testing.T) {
	var readErr error
	c := cmd("ask", func(c *cobra.Command, args []string) error {
		_, readErr = bufio.NewReader(os.Stdin).ReadString('\n')
		return nil
	})

	harness.Run(t, c, nil, "", nil, false)
	if !errors.Is(readErr, os.ErrClosed) && readErr == nil {
		t.Fatalf("read of an empty stdin returned %v, want EOF", readErr)
	}
	if readErr != nil && readErr.Error() != "EOF" {
		t.Fatalf("read of an empty stdin returned %v, want EOF", readErr)
	}
}

// ------------------------------------------------------------- the env ----

func TestRunSetsTheNamedEnvironment(t *testing.T) {
	var got string
	c := cmd("x", func(c *cobra.Command, args []string) error {
		got = os.Getenv("FERRY_API_KEY")
		return nil
	})

	harness.Run(t, c, nil, "", map[string]string{"FERRY_API_KEY": "ferry_sk_sandbox_x"}, false)
	if got != "ferry_sk_sandbox_x" {
		t.Errorf("FERRY_API_KEY = %q", got)
	}
}

// The ambient environment must not reach the command. A developer with
// FERRY_API_URL exported would otherwise get different results from the same
// test than CI does.
func TestRunClearsAmbientFerryAndXDGVariables(t *testing.T) {
	t.Setenv("FERRY_API_URL", "https://leaked.example")
	t.Setenv("XDG_CONFIG_HOME", "/leaked")

	var apiURL, config, unrelated string
	t.Setenv("UNRELATED_VAR", "kept")
	c := cmd("x", func(c *cobra.Command, args []string) error {
		apiURL = os.Getenv("FERRY_API_URL")
		config = os.Getenv("XDG_CONFIG_HOME")
		unrelated = os.Getenv("UNRELATED_VAR")
		return nil
	})

	harness.Run(t, c, nil, "", map[string]string{"FERRY_PROFILE": "test"}, false)

	if apiURL != "" {
		t.Errorf("FERRY_API_URL leaked as %q", apiURL)
	}
	if config != "" {
		t.Errorf("XDG_CONFIG_HOME leaked as %q", config)
	}
	// Only FERRY_* and XDG_* are the CLI's; PATH and the like must survive.
	if unrelated != "kept" {
		t.Errorf("an unrelated variable was cleared: %q", unrelated)
	}
}

// HOME must not be the developer's, or a command falling back to ~/.config
// would read and write a real credential file during a test.
func TestRunRedirectsHOMEUnlessTheTestNamesIt(t *testing.T) {
	realHome := os.Getenv("HOME")

	var seen string
	c := cmd("x", func(c *cobra.Command, args []string) error {
		seen = os.Getenv("HOME")
		return nil
	})

	harness.Run(t, c, nil, "", nil, false)
	if seen == realHome {
		t.Fatalf("HOME was left as the real home directory %q", seen)
	}
	if seen == "" {
		t.Fatal("HOME was cleared rather than redirected; a fallback would resolve to a relative path")
	}
	if entries, err := os.ReadDir(seen); err != nil || len(entries) != 0 {
		t.Fatalf("HOME %q is not an empty directory (err %v)", seen, err)
	}

	harness.Run(t, c, nil, "", map[string]string{"HOME": "/explicit"}, false)
	if seen != "/explicit" {
		t.Fatalf("an explicit HOME was overridden: %q", seen)
	}
}

// ------------------------------------------------------------- the tty ----

// The point of tty=true: term.IsTerminal is true on all three descriptors, so
// a prompt takes its interactive path rather than the headless one.
func TestTTYModeGivesTheCommandATerminal(t *testing.T) {
	var in, out, errOut bool
	c := cmd("x", func(c *cobra.Command, args []string) error {
		in = term.IsTerminal(int(os.Stdin.Fd()))
		out = term.IsTerminal(int(os.Stdout.Fd()))
		errOut = term.IsTerminal(int(os.Stderr.Fd()))
		return nil
	})

	harness.Run(t, c, nil, "", nil, true)

	if !in || !out || !errOut {
		t.Fatalf("IsTerminal: stdin=%v stdout=%v stderr=%v, want all true", in, out, errOut)
	}
}

// And the converse, which is what makes the test above meaningful: without
// tty the same command must see no terminal.
func TestPipeModeGivesTheCommandNoTerminal(t *testing.T) {
	var in, out bool
	c := cmd("x", func(c *cobra.Command, args []string) error {
		in = term.IsTerminal(int(os.Stdin.Fd()))
		out = term.IsTerminal(int(os.Stdout.Fd()))
		return nil
	})

	harness.Run(t, c, nil, "", nil, false)

	if in || out {
		t.Fatalf("IsTerminal: stdin=%v stdout=%v, want both false", in, out)
	}
}

// A prompt on a terminal: the harness must carry the prompt out and the
// answer in, which is the mechanism every consent test depends on.
func TestTTYModeCarriesAPromptAndItsAnswer(t *testing.T) {
	var answer string
	c := cmd("confirm", func(c *cobra.Command, args []string) error {
		fmt.Fprint(os.Stdout, "Type the amount to confirm: ")
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil {
			return err
		}
		answer = strings.TrimSpace(line)
		return nil
	})

	stdout, _, exit := harness.Run(t, c, nil, "100.00\n", nil, true)

	if exit != 0 {
		t.Fatalf("exit = %d", exit)
	}
	if !strings.Contains(stdout, "Type the amount to confirm:") {
		t.Errorf("the prompt did not reach the transcript: %q", stdout)
	}
	if answer != "100.00" {
		t.Errorf("the command read %q, want 100.00", answer)
	}
	// Raw mode is on, so the answer must not be echoed back into the
	// transcript — otherwise an assertion looking for the typed amount in the
	// output would match the echo rather than anything the command printed.
	if strings.Contains(stdout, "100.00") {
		t.Errorf("the pty echoed the answer into the transcript: %q", stdout)
	}
}

// The merge is a property of a pty, not a bug, and it is documented. This
// pins it so a later change that silently separated the streams — or silently
// dropped stderr — is noticed.
func TestTTYModeReturnsOneMergedTranscriptInBothValues(t *testing.T) {
	c := cmd("x", func(c *cobra.Command, args []string) error {
		fmt.Fprint(os.Stdout, "out;")
		fmt.Fprint(os.Stderr, "err;")
		return nil
	})

	stdout, stderr, _ := harness.Run(t, c, nil, "", nil, true)

	if stdout != stderr {
		t.Fatalf("the two values differ: %q vs %q", stdout, stderr)
	}
	for _, want := range []string{"out;", "err;"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("the transcript lost %q: %q", want, stdout)
		}
	}
}

func TestTTYModeReportsTheExitCode(t *testing.T) {
	c := cmd("x", func(c *cobra.Command, args []string) error { return newExit(7, "policy") })
	if _, _, exit := harness.Run(t, c, nil, "", nil, true); exit != 7 {
		t.Fatalf("exit = %d, want 7", exit)
	}
}

// After a Run the process streams must be the ones the test framework owns,
// or every later test's output would go into a closed pipe.
func TestRunRestoresTheProcessStreams(t *testing.T) {
	in, out, errOut := os.Stdin, os.Stdout, os.Stderr

	c := cmd("x", func(c *cobra.Command, args []string) error {
		fmt.Fprint(os.Stdout, "x")
		return nil
	})
	harness.Run(t, c, nil, "", nil, false)
	if os.Stdin != in || os.Stdout != out || os.Stderr != errOut {
		t.Fatal("the pipe path did not restore the process streams")
	}

	harness.Run(t, c, nil, "", nil, true)
	if os.Stdin != in || os.Stdout != out || os.Stderr != errOut {
		t.Fatal("the tty path did not restore the process streams")
	}
}

// A panic must reach the test framework. A harness that turned it into an
// exit code would let a command that panicked after sending money report a
// tidy failure.
func TestRunRepanicsAndRestoresTheStreams(t *testing.T) {
	out := os.Stdout

	c := cmd("x", func(c *cobra.Command, args []string) error {
		fmt.Fprint(os.Stdout, "before the panic")
		panic("render failed after the 201")
	})

	func() {
		defer func() {
			v := recover()
			if v == nil {
				t.Fatal("the panic did not propagate")
			}
			if s, ok := v.(string); !ok || s != "render failed after the 201" {
				t.Fatalf("recovered %v", v)
			}
			if os.Stdout != out {
				t.Fatal("the streams were not restored before the re-panic")
			}
		}()
		harness.Run(t, c, nil, "", nil, false)
	}()
}

// The harness must not need the root command: it takes whatever it is given,
// including a subcommand reached through its parent.
func TestRunTakesASubcommandThroughItsParent(t *testing.T) {
	var ran bool
	child := cmd("create", func(c *cobra.Command, args []string) error {
		ran = true
		fmt.Fprint(c.OutOrStdout(), "created")
		return nil
	})
	parent := cmd("transfers", nil)
	parent.RunE = nil
	parent.AddCommand(child)

	stdout, _, exit := harness.Run(t, parent, []string{"create"}, "", nil, false)

	if !ran || exit != 0 || stdout != "created" {
		t.Fatalf("ran=%v exit=%d stdout=%q", ran, exit, stdout)
	}
}

// Output larger than a pipe buffer must not deadlock. 64 KiB is the usual
// Linux pipe capacity, so this is the case a synchronous drain would hang on.
func TestRunHandlesOutputLargerThanAPipeBuffer(t *testing.T) {
	const size = 256 * 1024
	c := cmd("x", func(c *cobra.Command, args []string) error {
		_, err := os.Stdout.Write([]byte(strings.Repeat("a", size)))
		return err
	})

	stdout, _, exit := harness.Run(t, c, nil, "", nil, false)
	if exit != 0 {
		t.Fatalf("exit = %d", exit)
	}
	if len(stdout) != size {
		t.Fatalf("captured %d bytes, want %d", len(stdout), size)
	}
}

// Stdin larger than a pipe buffer likewise: the fill is asynchronous, so a
// command that reads all of it gets all of it.
func TestRunHandlesStdinLargerThanAPipeBuffer(t *testing.T) {
	const size = 256 * 1024
	in := strings.Repeat("b", size)

	var n int
	c := cmd("x", func(c *cobra.Command, args []string) error {
		b, err := io.ReadAll(os.Stdin)
		n = len(b)
		return err
	})

	if _, _, exit := harness.Run(t, c, nil, in, nil, false); exit != 0 {
		t.Fatalf("exit = %d", exit)
	}
	if n != size {
		t.Fatalf("the command read %d bytes, want %d", n, size)
	}
}
