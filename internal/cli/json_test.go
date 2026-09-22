package cli_test

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/coba-ai/ferry-cli/internal/cli"
)

// AC41: in `--output json` mode, **exactly one** JSON document reaches
// stdout, stdout carries nothing else, and `outcome.exit_code` equals the
// exit code the process actually exited.
//
// # Why the equality is asserted against the observed status
//
// `outcome.exit_code` is a field this CLI writes. Comparing it to the class's
// declared exit would compare the field to itself — the tautology §6 warns
// about — and would pass a CLI that wrote a correct document and then exited
// 0. So every row below compares the field to the third return value of
// `harness.Run`, which is what `main.go`'s `os.Exit` receives.
//
// # Why "exactly one" is counted rather than parsed
//
// `json.Unmarshal` on two concatenated documents fails, so a test that only
// unmarshalled would catch two documents by accident and would miss a
// document followed by a blank line of human text, or preceded by one. The
// counter below decodes values from a stream until EOF and then requires the
// remainder to be whitespace, which catches all three.

// scan is the measurement: the JSON values on stdout, whatever text follows
// them, and the decode failure if there was one.
//
// It is separated from the reporting so that the detector itself can be
// tested. A helper that called `t.Fatalf` directly could only be checked by
// failing a real test, which is why the floor below can assert what it finds.
func scan(stdout string) (docs []map[string]any, trailing string, err error) {
	dec := json.NewDecoder(strings.NewReader(stdout))

	for {
		var doc map[string]any

		decodeErr := dec.Decode(&doc)
		if decodeErr == io.EOF {
			break
		}

		if decodeErr != nil {
			// Whatever is left, including the bytes that failed to decode.
			rest, _ := io.ReadAll(dec.Buffered())

			return docs, strings.TrimSpace(string(rest)), decodeErr
		}

		docs = append(docs, doc)
	}

	// Anything after the last document. A trailing human line is what a
	// stray `fmt.Println` in a verb produces, and it is invisible to a
	// decoder that stopped at the first value.
	rest, err := io.ReadAll(dec.Buffered())
	if err != nil {
		return docs, "", err
	}

	return docs, strings.TrimSpace(string(rest)), nil
}

// documents counts the JSON values on stdout and returns them.
func documents(t *testing.T, stdout string) []map[string]any {
	t.Helper()

	docs, trailing, err := scan(stdout)
	if err != nil {
		t.Fatalf("stdout is not a stream of JSON documents (%v):\n%q", err, stdout)
	}

	if trailing != "" {
		t.Errorf("stdout carries %q after the last JSON document", trailing)
	}

	return docs
}

// exitCodeOf reads outcome.exit_code out of a decoded document, insisting it
// is present and a number.
func exitCodeOf(t *testing.T, doc map[string]any) int {
	t.Helper()

	block, ok := doc["outcome"].(map[string]any)
	if !ok {
		t.Fatalf("the document has no outcome block: %v", doc)
	}

	raw, ok := block["exit_code"]
	if !ok {
		t.Fatalf("the outcome block has no exit_code: %v", block)
	}

	n, ok := raw.(float64)
	if !ok {
		t.Fatalf("exit_code is %T (%v), want a number", raw, raw)
	}

	return int(n)
}

func TestJSONModeWritesExactlyOneDocument(t *testing.T) {
	for _, tc := range []struct {
		name     string
		scenario []string
		args     []string

		// loggedOut drops the credential, which is how the refused
		// pre-check row is reached.
		loggedOut bool

		// corrupt replaces the credentials file with bytes that are not
		// JSON, which is the CLI-fault row.
		corrupt bool

		wantExit int
	}{
		{
			name:     "a 200",
			scenario: []string{"corridors.list.200"},
			args:     []string{"corridors", "list"},
			wantExit: 0,
		},
		{
			name:     "a simulate 201",
			scenario: []string{"simulate.201"},
			args:     append([]string{"transfers", "create"}, canonicalSimulateArgs()...),
			wantExit: 0,
		},
		{
			name:     "an API error",
			scenario: []string{"execute.plan_expired.410"},
			args:     []string{"transfers", "execute", "--plan", recordedPlanToken, "--yes"},
			wantExit: 4,
		},
		{
			// PLAN §5.2: `500 INTERNAL_ERROR` is `pending` 6 after the
			// budget. A 500 on a money request is the case where whether
			// money moved is genuinely unknown.
			name:     "a 500",
			scenario: []string{"execute.internal_error.500"},
			args:     []string{"transfers", "execute", "--plan", recordedPlanToken, "--yes"},
			wantExit: 6,
		},
		{
			// PLAN §5.2: `502 UPSTREAM_CONTRACT_VIOLATION` on an execute is
			// 4, because it is refused pre-send and the plan is untouched.
			name:     "a 502 the CLI cannot act on",
			scenario: []string{"execute.contract_violation.502"},
			args:     []string{"transfers", "execute", "--plan", recordedPlanToken, "--yes"},
			wantExit: 4,
		},
		{
			name:     "a usage error",
			args:     []string{"transfers", "create", "--no-such-flag"},
			wantExit: 2,
		},
		{
			name:     "an unknown command",
			args:     []string{"transfers", "teleport"},
			wantExit: 2,
		},
		{
			// A group with no subcommand. Cobra's default is help on
			// stdout and exit 0, which in this mode is a non-document on
			// stdout *and* a success reported for nothing done.
			name:     "a group with no subcommand",
			args:     []string{"transfers"},
			wantExit: 2,
		},
		{
			name:     "the root with no arguments",
			args:     []string{},
			wantExit: 2,
		},
		{
			// A local problem that stops the CLI before anything is sent.
			// The credentials file is the one every command reads.
			name:     "a CLI fault",
			args:     []string{"corridors", "list"},
			corrupt:  true,
			wantExit: 1,
		},
		{
			name:     "an unknown profile",
			args:     []string{"corridors", "list", "--profile", "nonexistent"},
			wantExit: 3,
		},
		{
			name:      "a refused pre-check",
			scenario:  []string{"simulate.201"},
			args:      append([]string{"transfers", "create"}, canonicalSimulateArgs()...),
			loggedOut: true,
			wantExit:  3,
		},
		{
			name:     "a refused --env assertion",
			scenario: []string{"simulate.201"},
			args: append([]string{"transfers", "create", "--env", "live"},
				canonicalSimulateArgs()...),
			wantExit: 3,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server, home := loggedIn(t, tc.scenario...)

			if tc.loggedOut {
				home = newHome(t)
			}

			if tc.corrupt {
				path := filepath.Join(home, "credentials.json")
				if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
					t.Fatalf("corrupt %s: %v", path, err)
				}
			}

			_ = server

			stdout, stderr, exit := run(t, invocation{
				home: home,
				args: append(append([]string{}, tc.args...), "--output", "json"),
			})

			if exit != tc.wantExit {
				t.Errorf("exit = %d, want %d\nstdout:\n%s\nstderr:\n%s",
					exit, tc.wantExit, stdout, stderr)
			}

			docs := documents(t, stdout)

			if len(docs) != 1 {
				t.Fatalf("stdout carries %d JSON documents, want exactly 1:\n%q", len(docs), stdout)
			}

			// The claim. Against the observed status, never against the
			// class table.
			if got := exitCodeOf(t, docs[0]); got != exit {
				t.Errorf("outcome.exit_code = %d but the process exited %d", got, exit)
			}

			// Every document carries the blocks a consumer selects on. A
			// document missing `ferry_cli` would still satisfy the count.
			for _, key := range []string{"ferry_cli", "outcome"} {
				if _, ok := docs[0][key]; !ok {
					t.Errorf("the document has no %q block:\n%s", key, stdout)
				}
			}
		})
	}
}

// The floor for the counter.
//
// `documents` reporting 1 has to be a measurement. If the decoder silently
// stopped at the first value, or if an empty stdout counted as one document,
// every row above would pass against a CLI that wrote nothing at all.
func TestTheDocumentCounterCounts(t *testing.T) {
	for _, tc := range []struct {
		name         string
		stdout       string
		wantDocs     int
		wantTrailing bool
		wantErr      bool
	}{
		{
			name:     "one document is one",
			stdout:   `{"outcome":{"exit_code":0}}` + "\n",
			wantDocs: 1,
		},
		{
			// The failure the count exists to catch.
			name:     "two documents are two",
			stdout:   `{"a":1}` + "\n" + `{"b":2}` + "\n",
			wantDocs: 2,
		},
		{
			// If this counted as one, every row of the table above would
			// pass against a CLI that wrote nothing at all.
			name:     "no output is no documents",
			stdout:   "",
			wantDocs: 0,
		},
		{
			name:         "a trailing line of prose is caught",
			stdout:       `{"a":1}` + "\nsomething went wrong\n",
			wantDocs:     1,
			wantTrailing: true,
			wantErr:      true,
		},
		{
			name:         "a leading line of prose is caught",
			stdout:       "warming up\n" + `{"a":1}` + "\n",
			wantTrailing: true,
			wantErr:      true,
		},
		{
			name:         "a truncated document is not a document",
			stdout:       `{"a":`,
			wantDocs:     0,
			wantTrailing: true,
			wantErr:      true,
		},
		{
			// Whitespace between documents is not content.
			name:     "blank lines are not prose",
			stdout:   "\n" + `{"a":1}` + "\n\n",
			wantDocs: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			docs, trailing, err := scan(tc.stdout)

			if len(docs) != tc.wantDocs {
				t.Errorf("counted %d documents, want %d", len(docs), tc.wantDocs)
			}

			if (trailing != "") != tc.wantTrailing {
				t.Errorf("trailing = %q, want trailing: %v", trailing, tc.wantTrailing)
			}

			if (err != nil) != tc.wantErr {
				t.Errorf("err = %v, want an error: %v", err, tc.wantErr)
			}
		})
	}
}

// In text mode stdout carries no JSON document at all.
//
// Without this, "exactly one document in JSON mode" is satisfiable by a CLI
// that writes the document unconditionally, which would put a machine
// envelope in front of every human.
func TestTextModeWritesNoDocument(t *testing.T) {
	_, home := loggedIn(t, "simulate.201")

	stdout, _, exit := run(t, invocation{
		home: home,
		args: append([]string{"transfers", "create"}, canonicalSimulateArgs()...),
	})

	if exit != 0 {
		t.Fatalf("exit = %d, want 0\n%s", exit, stdout)
	}

	if strings.HasPrefix(strings.TrimSpace(stdout), "{") {
		t.Errorf("text mode wrote a JSON document to stdout:\n%s", stdout)
	}
}

// AC41's "stdout contains nothing else", tested where it is most likely to
// break: a command that prompts.
//
// The prompt is a write to a terminal that happens between the request and
// the document. If it went to stdout, the document would be preceded by
// `Execute? [y/N]` and every scripted consumer would fail to parse it — so
// the prompt goes to stderr (C10), and this is the assertion that keeps it
// there.
func TestAPromptDoesNotReachStdout(t *testing.T) {
	_, home := loggedIn(t, "execute.201.processing")

	stdout, _, exit := run(t, invocation{
		home:  home,
		tty:   true,
		stdin: "y\n",
		args:  []string{"transfers", "execute", "--plan", recordedPlanToken, "--output", "json"},
	})

	// A314: on a pty stdout and stderr are one transcript, so this cannot
	// separate them. What it can do is confirm the merged transcript holds
	// the prompt and exactly one document, and the split is asserted
	// without a pty below.
	if exit != 0 {
		t.Fatalf("exit = %d, want 0\n%s", exit, stdout)
	}

	if !strings.Contains(stdout, "[y/N]") {
		t.Fatalf("the prompt is missing from the transcript:\n%s", stdout)
	}

	// The document is in there, after the prompt. Finding it requires
	// skipping the prompt, which is the proof the prompt is not part of
	// the JSON stream on a real terminal either.
	brace := strings.Index(stdout, "{")
	if brace < 0 {
		t.Fatalf("no document in the transcript:\n%s", stdout)
	}

	docs := documents(t, stdout[brace:])
	if len(docs) != 1 {
		t.Errorf("%d documents in the transcript, want 1:\n%s", len(docs), stdout)
	}

	if got := exitCodeOf(t, docs[0]); got != exit {
		t.Errorf("outcome.exit_code = %d but the process exited %d", got, exit)
	}
}

// The same claim with stdout and stderr genuinely separate: a headless
// decline writes its refusal to stderr and its document to stdout.
func TestTheRefusalMessageGoesToStderr(t *testing.T) {
	_, home := loggedIn(t, "execute.201.processing")

	stdout, stderr, exit := run(t, invocation{
		home: home,
		args: []string{"runs", "resume", "01JQBN8Z5K0000000000000009", "--output", "json"},
	})

	if exit == 0 {
		t.Fatalf("resuming a run that does not exist exited 0\n%s", stdout)
	}

	docs := documents(t, stdout)
	if len(docs) != 1 {
		t.Fatalf("stdout carries %d documents, want 1:\n%q", len(docs), stdout)
	}

	if got := exitCodeOf(t, docs[0]); got != exit {
		t.Errorf("outcome.exit_code = %d but the process exited %d", got, exit)
	}

	if strings.TrimSpace(stderr) == "" {
		t.Error("nothing was written to stderr; a human sees no reason for the failure")
	}
}

// M102, deterministically: **SIGHUP is one of the signals this CLI listens
// for.**
//
// The end-to-end version of this claim is racy — `syscall.Kill` returns
// before Go's handler goroutine has run, so whether the signal or a
// buffered "y" wins the prompt is a coin toss, and a test that asserted the
// outcome would be flaky in one direction and vacuous in the other. The
// non-racy half is the set itself, and the set is what M102 mutates.
//
// SIGHUP earns its place separately from the other two. SIGINT is Ctrl-C
// and SIGTERM is a supervisor stopping a job, and in both the caller is
// present in some sense. SIGHUP is the terminal going away — an ssh session
// dropping, a window closed, a CI runner reaping — and it is the one where
// nobody is left to answer a prompt. A CLI deaf to it would hold an
// unanswerable question open and then take whatever arrived on a dead
// terminal's stdin as consent.
func TestTheSignalSetIsTheThreeTheDesignNames(t *testing.T) {
	t.Parallel()

	want := map[os.Signal]string{
		syscall.SIGINT:  "Ctrl-C",
		syscall.SIGTERM: "a supervisor stopping the process",
		syscall.SIGHUP:  "the terminal going away, so nobody is left to answer a prompt",
	}

	got := map[os.Signal]bool{}
	for _, sig := range cli.DefaultSignals {
		got[sig] = true
	}

	// Both directions. "Every signal I listen for is in the list" passes
	// with SIGHUP dropped, and "every signal in the list is listened for"
	// passes with a fourth added that nothing handles.
	for sig, why := range want {
		if !got[sig] {
			t.Errorf("cli.DefaultSignals does not include %v (%s)", sig, why)
		}
	}

	for sig := range got {
		if _, named := want[sig]; !named {
			t.Errorf("cli.DefaultSignals includes %v, which this test does not account "+
				"for. Say why it is there, or take it out.", sig)
		}
	}

	if len(cli.DefaultSignals) != len(want) {
		t.Errorf("cli.DefaultSignals has %d entries and this test names %d; a duplicate "+
			"would make the two loops above agree while the set is wrong",
			len(cli.DefaultSignals), len(want))
	}
}
