package consent_test

import (
	"bytes"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/kurenn/ferry-cli/internal/consent"
)

// closed is an already-signalled interrupt channel.
func closed() <-chan struct{} {
	ch := make(chan struct{})
	close(ch)

	return ch
}

// TestAskEstablishesConsentPerInvocation walks every combination of the three
// inputs that can grant consent, so that the table is a census rather than a
// list of the cases that came to mind.
func TestAskEstablishesConsentPerInvocation(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		req     consent.Request
		in      string
		want    consent.Verdict
		prompts bool
	}{
		{
			name: "--yes grants without asking",
			req:  consent.Request{Yes: true, TTY: true},
			want: consent.Granted,
			// The point of putting `--yes` first: a command line that
			// already said yes must not stop to ask, or every scripted
			// caller hangs on a terminal it happens to have.
			prompts: false,
		},
		{
			name:    "--yes grants with no terminal",
			req:     consent.Request{Yes: true},
			want:    consent.Granted,
			prompts: false,
		},
		{
			name:    "y at a terminal grants",
			req:     consent.Request{TTY: true, CommandIsConsent: true},
			in:      "y\n",
			want:    consent.Granted,
			prompts: true,
		},
		{
			name:    "yes at a terminal grants",
			req:     consent.Request{TTY: true, CommandIsConsent: true},
			in:      "YES\n",
			want:    consent.Granted,
			prompts: true,
		},
		{
			name:    "n at a terminal declines",
			req:     consent.Request{TTY: true, CommandIsConsent: true},
			in:      "n\n",
			want:    consent.Declined,
			prompts: true,
		},
		{
			// A human pressing return at `[y/N]` chose the capital.
			name:    "an empty line declines",
			req:     consent.Request{TTY: true, CommandIsConsent: true},
			in:      "\n",
			want:    consent.Declined,
			prompts: true,
		},
		{
			name:    "EOF declines",
			req:     consent.Request{TTY: true, CommandIsConsent: true},
			in:      "",
			want:    consent.Declined,
			prompts: true,
		},
		{
			// `yesterday` is not an authorisation to move money. A prefix
			// match would make it one.
			name:    "a word beginning with yes declines",
			req:     consent.Request{TTY: true, CommandIsConsent: true},
			in:      "yesterday\n",
			want:    consent.Declined,
			prompts: true,
		},
		{
			name:    "a command that is itself consent grants with no terminal",
			req:     consent.Request{CommandIsConsent: true},
			want:    consent.Granted,
			prompts: false,
		},
		{
			// `runs resume` is the arm that is not consent. It is the one
			// that would otherwise let a record stand in for a human.
			name:    "a command that is not consent is unavailable with no terminal",
			req:     consent.Request{},
			want:    consent.Unavailable,
			prompts: false,
		},
		{
			name:    "a signal at the prompt declines",
			req:     consent.Request{TTY: true, Interrupted: closed()},
			want:    consent.Declined,
			prompts: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var out bytes.Buffer

			asked := 0

			req := tc.req
			req.Out = &out
			req.In = strings.NewReader(tc.in)
			req.BeforePrompt = func() error { asked++; return nil }
			req.Display = func(w io.Writer) { fmt.Fprintln(w, "THE-PLAN") }

			got, err := consent.Ask(req)
			if err != nil {
				t.Fatalf("Ask: %v", err)
			}

			if got != tc.want {
				t.Errorf("verdict = %q, want %q", got, tc.want)
			}

			// Both directions on "did it prompt". Only asserting that the
			// prompting arms prompt would pass a package that prompts on
			// every arm, which is a `--yes` that blocks.
			if tc.prompts {
				if asked != 1 {
					t.Errorf("BeforePrompt ran %d times, want 1", asked)
				}

				if !strings.Contains(out.String(), "THE-PLAN") {
					t.Errorf("the plan was not shown before the question:\n%s", out.String())
				}

				if !strings.Contains(out.String(), "[y/N]") {
					t.Errorf("the question does not default to no:\n%s", out.String())
				}
			} else {
				if asked != 0 {
					t.Errorf("BeforePrompt ran %d times on an arm that must not ask", asked)
				}

				if out.Len() != 0 {
					t.Errorf("an arm that must not ask wrote:\n%s", out.String())
				}
			}
		})
	}
}

// AC72's precondition: a prompt whose state could not be written is not
// shown at all.
//
// `awaiting_confirmation` is the evidence that a prompt was up. A prompt
// displayed after the write failed would be a prompt a `kill -9` leaves no
// trace of, and the next invocation would find a `not_started` step and
// treat it as a run that never began.
func TestAPromptIsNotShownIfItsStateCannotBeWritten(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer

	boom := errors.New("no space left on device")

	got, err := consent.Ask(consent.Request{
		TTY:          true,
		Out:          &out,
		In:           strings.NewReader("y\n"),
		BeforePrompt: func() error { return boom },
		Display:      func(w io.Writer) { fmt.Fprintln(w, "THE-PLAN") },
	})

	if !errors.Is(err, boom) {
		t.Fatalf("error = %v, want the write failure", err)
	}

	if got != consent.Unavailable {
		t.Errorf("verdict = %q, want unavailable", got)
	}

	if out.Len() != 0 {
		t.Errorf("the prompt was shown after the state write failed:\n%s", out.String())
	}
}

// A read that fails for a reason that is not an answer is not a decline.
//
// Declining writes a terminal state. Doing that on a broken pipe would
// settle a run on the strength of an I/O error, and the run could never be
// resumed.
func TestAFailedReadIsNotAnAnswer(t *testing.T) {
	t.Parallel()

	got, err := consent.Ask(consent.Request{
		TTY: true,
		Out: io.Discard,
		In:  brokenReader{},
	})

	if !errors.Is(err, consent.ErrPromptFailed) {
		t.Fatalf("error = %v, want ErrPromptFailed", err)
	}

	if got != consent.Unavailable {
		t.Errorf("verdict = %q, want unavailable", got)
	}
}

type brokenReader struct{}

func (brokenReader) Read([]byte) (int, error) { return 0, errors.New("input/output error") }

// A signal wins over an answer that is already waiting to be read.
//
// The reverse would mean a `y` buffered in the terminal by a paste, or by a
// script that wrote ahead, could authorise a send the human then tried to
// stop with ^C.
// A signal that has already arrived answers the prompt, even with a "y"
// sitting in the buffer — and it does so *every* time, not most times.
//
// The repetition is the test, and the reason is that the defect it guards
// is a `select` with both cases ready. `Ask` waits on the interrupt and on
// the answer together, and Go chooses uniformly at random among ready
// cases, so a version without the priority check returns `granted` on a
// fraction of runs and `declined` on the rest: the same invocation sends
// money or does not depending on the scheduler.
//
// A single call is a hopeless control for that, and the numbers are worth
// writing down because they are unintuitive. With the priority check
// removed and `-race` on:
//
//   - one call per process, 2000 processes: **never** red. The reader
//     goroutine has not been scheduled by the time the select runs, so
//     only the interrupt is ready and the toss never happens.
//   - 3000 calls in one process: red in 4 runs out of 5.
//
// So the window opens only once the runtime is warm, and a `-count=1`
// gate would have shipped the defect. The loop below is sized so that the
// toss cannot hide: it went red in 10 runs out of 10 with the check
// removed, and the whole test costs well under a second.
func TestASignalWinsOverABufferedYes(t *testing.T) {
	t.Parallel()

	const attempts = 20000

	for i := range attempts {
		got, err := consent.Ask(consent.Request{
			TTY:         true,
			Out:         io.Discard,
			In:          strings.NewReader("y\n"),
			Interrupted: closed(),
		})
		if err != nil {
			t.Fatalf("Ask: %v", err)
		}

		if got != consent.Declined {
			t.Fatalf("verdict = %q on attempt %d of %d, want declined: a signal at the "+
				"prompt is a no, and a buffered \"y\" is bytes that were already there "+
				"rather than an answer given after the terminal went away",
				got, i+1, attempts)
		}
	}
}

// PromptShown fires after the prompt is written and before the read begins.
//
// It is the window the fault point and the end-to-end signal tests both
// need, and a hook that ran before the write or after the read would put
// them in a different window and quietly test nothing.
func TestPromptShownFiresBetweenTheQuestionAndTheRead(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer

	var whenShown string

	_, err := consent.Ask(consent.Request{
		TTY:         true,
		Out:         &out,
		In:          &recordingReader{onRead: func() { whenShown += "read" }},
		PromptShown: func() { whenShown += "shown|" },
	})
	if err != nil && !errors.Is(err, consent.ErrPromptFailed) {
		t.Fatalf("Ask: %v", err)
	}

	if !strings.HasPrefix(whenShown, "shown|") {
		t.Errorf("the order was %q, want the hook before the first read", whenShown)
	}

	if out.Len() == 0 {
		t.Error("the hook fired but nothing had been written")
	}
}

type recordingReader struct {
	onRead func()
	done   bool
}

func (r *recordingReader) Read(p []byte) (int, error) {
	r.onRead()

	if r.done {
		return 0, io.EOF
	}

	r.done = true

	return 0, io.EOF
}

// C16, structurally: this package cannot store consent because it cannot
// store anything.
//
// The behavioural tests above show that each arm asks. They cannot show that
// no *fourth* arm is ever added which reads a file, an environment variable
// or a run record — and that fourth arm is precisely the defect CRITIQUE B3
// found in revision 1. So this walks the package's own syntax tree and fails
// on an import that could reach durable state.
//
// The allowed set is closed and small: a package that needs `os` for
// `os.Stdin` is one `os.WriteFile` away from a consent cache, which is why
// `os` is here with a comment rather than absent.
func TestTheConsentPackageCannotStoreAnything(t *testing.T) {
	t.Parallel()

	allowed := map[string]string{
		"bufio":   "reading the answer",
		"errors":  "ErrPromptFailed",
		"fmt":     "wrapping the read failure",
		"io":      "the prompt writer and the answer reader",
		"os":      "os.Stdin and os.Stderr defaults only",
		"strings": "matching y and yes",
	}

	fset := token.NewFileSet()

	pkgs, err := parser.ParseDir(fset, ".", notATest, 0)
	if err != nil {
		t.Fatalf("parse the consent package: %v", err)
	}

	pkg, ok := pkgs["consent"]
	if !ok {
		t.Fatalf("no package `consent` in this directory; found %v", names(pkgs))
	}

	// The floor. A census over an empty parse is true of nothing, and a
	// change to the build tags or the file names could empty it silently.
	if len(pkg.Files) == 0 {
		t.Fatal("the parse found no files; this census would be vacuous")
	}

	var seen []string

	for path, file := range pkg.Files {
		for _, spec := range file.Imports {
			imported, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				t.Fatalf("import path %s: %v", spec.Path.Value, err)
			}

			seen = append(seen, imported)

			if _, ok := allowed[imported]; !ok {
				t.Errorf("%s imports %q, which is not in the set a package that "+
					"cannot store consent may import. If consent now needs it, C16 needs "+
					"re-reading first.", filepath.Base(path), imported)
			}
		}
	}

	// The other direction, which is the one that decays. An allowed entry
	// nothing imports is a permission granted for a reason that no longer
	// exists, and the next reader takes the set as the truth about what
	// this package does.
	for imported := range allowed {
		if !contains(seen, imported) {
			t.Errorf("%q is permitted for %q but nothing imports it; "+
				"drop it so the set keeps meaning something", imported, allowed[imported])
		}
	}
}

// The other half of C16 in this package: no declaration here holds state
// across an Ask.
//
// An import census would not catch `var granted bool` at package scope, and
// a memoised grant is a stored consent even when it never reaches a disk —
// in a binary that runs one invocation it would be indistinguishable from
// one that does.
func TestTheConsentPackageHoldsNoState(t *testing.T) {
	t.Parallel()

	fset := token.NewFileSet()

	pkgs, err := parser.ParseDir(fset, ".", notATest, 0)
	if err != nil {
		t.Fatalf("parse the consent package: %v", err)
	}

	pkg := pkgs["consent"]
	if pkg == nil || len(pkg.Files) == 0 {
		t.Fatal("the parse found no files; this census would be vacuous")
	}

	checked := 0

	for path, file := range pkg.Files {
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.VAR {
				continue
			}

			for _, spec := range gen.Specs {
				value, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}

				for _, name := range value.Names {
					checked++

					// `ErrPromptFailed` is a sentinel: it is compared, never
					// assigned to. Anything else at package scope is memory
					// that outlives an Ask.
					if name.Name != "ErrPromptFailed" {
						t.Errorf("%s declares package-scope var %q; consent is established "+
							"per invocation and may not be remembered between asks",
							filepath.Base(path), name.Name)
					}
				}
			}
		}
	}

	if checked == 0 {
		t.Error("no package-scope var was examined, so this census proves nothing; " +
			"ErrPromptFailed should have been one")
	}
}

// notATest keeps the census over the package's own source, not its tests.
func notATest(fi fs.FileInfo) bool { return !strings.HasSuffix(fi.Name(), "_test.go") }

func names(pkgs map[string]*ast.Package) []string {
	var out []string
	for name := range pkgs {
		out = append(out, name)
	}

	sort.Strings(out)

	return out
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}

	return false
}
