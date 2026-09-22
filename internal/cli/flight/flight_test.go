package flight_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"strings"
	"sync"
	"testing"

	"github.com/coba-ai/ferry-cli/internal/cli/flight"
	"github.com/coba-ai/ferry-cli/internal/outcome"
)

// C17: the flag is monotonic.
//
// Behaviourally that is one claim — once set it reads true — and it is
// trivially satisfied by a flag nobody clears. The defect V1 found was a
// clear that *was* added later, in revision 2, when `Record` returned nil.
// So the behavioural test is paired with a census over this package's own
// syntax tree, which fails on any assignment to the field storing anything
// but `true`.

func TestTheFlagStartsFalseAndStaysTrue(t *testing.T) {
	t.Parallel()

	f := flight.New()

	if f.MayHaveSent() {
		t.Error("a new flag reports that money may have been sent")
	}

	f.MarkSent()

	if !f.MayHaveSent() {
		t.Fatal("MarkSent did not set the flag")
	}

	// Called again, and again, because the money path calls it once per
	// step and a resume calls it on a step that already sent.
	for i := 0; i < 5; i++ {
		f.MarkSent()

		if !f.MayHaveSent() {
			t.Fatalf("the flag went false after %d further MarkSent calls", i+1)
		}
	}
}

// A nil flag reports false, which is what every read command needs.
//
// The alternative is a nil check at each of the dozen call sites, and the
// one that gets forgotten is a panic on a command that moves no money.
func TestANilFlagIsSafeAndReportsNothingSent(t *testing.T) {
	t.Parallel()

	var f *flight.Flag

	if f.MayHaveSent() {
		t.Error("a nil flag reports that money may have been sent")
	}

	// None of these may panic.
	f.MarkSent()
	f.NoteRun("01JQBN8Z5K0000000000000001")
	f.NoteCommand("cmd_1")
	f.NoteProfile("default", "sandbox")

	if f.RunID() != "" || f.CommandID() != "" || f.Profile() != "" || f.Environment() != "" {
		t.Error("a nil flag answered with something")
	}

	// And it still reports false after MarkSent, because there is nowhere
	// to record it. A nil flag that started answering true would make
	// every read command exit 6 on any failure.
	if f.MayHaveSent() {
		t.Error("a nil flag reports sent after MarkSent")
	}
}

// The flag is written from the money path and read from the wrapper's
// deferred classification, which runs on a different goroutine's stack after
// a signal handler has fired. The race detector is the point of this test.
func TestTheFlagIsSafeUnderConcurrentUse(t *testing.T) {
	t.Parallel()

	f := flight.New()

	var wg sync.WaitGroup

	for i := 0; i < 8; i++ {
		wg.Add(2)

		go func() { defer wg.Done(); f.MarkSent() }()
		go func() { defer wg.Done(); _ = f.MayHaveSent() }()
	}

	for i := 0; i < 8; i++ {
		wg.Add(2)

		go func() { defer wg.Done(); f.NoteRun("01JQBN8Z5K0000000000000001") }()
		go func() { defer wg.Done(); _ = f.RunID() }()
	}

	wg.Wait()

	if !f.MayHaveSent() {
		t.Error("the flag is false after sixteen MarkSent calls")
	}
}

// The census. This is the control that survives the next person's
// refactor.
//
// It walks this package's declarations and requires that every assignment
// whose target is the `sent` field stores the literal `true`. A `Clear`, a
// `Reset`, a `SetSent(bool)` or a `sent.Store(false)` anywhere in the
// package fails it — including inside a method that looks unrelated, which
// is where revision 2's clear lived.
func TestNothingInThisPackageCanClearTheFlag(t *testing.T) {
	t.Parallel()

	fset := token.NewFileSet()

	pkgs, err := parser.ParseDir(fset, ".", notATest, 0)
	if err != nil {
		t.Fatalf("parse the flight package: %v", err)
	}

	pkg := pkgs["flight"]
	if pkg == nil || len(pkg.Files) == 0 {
		t.Fatal("the parse found no files; this census would be vacuous")
	}

	writes := 0

	for path, file := range pkg.Files {
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}

			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}

			// `f.sent.Store(x)` and `f.sent.Swap(x)` and
			// `f.sent.CompareAndSwap(_, x)` — every atomic.Bool writer.
			if !writesAnAtomicBool(sel.Sel.Name) {
				return true
			}

			field, ok := sel.X.(*ast.SelectorExpr)
			if !ok || field.Sel.Name != "sent" {
				return true
			}

			writes++

			stored := call.Args[len(call.Args)-1]

			ident, ok := stored.(*ast.Ident)
			if !ok || ident.Name != "true" {
				t.Errorf("%s:%d writes %s to the in-flight flag. C17 says the flag is "+
					"monotonic: it is set before a money request and never cleared, so any "+
					"abnormal exit after a send reports that money may have moved. "+
					"Revision 2 cleared it when Record returned nil and reported "+
					"\"nothing was sent\" for a transfer that had been accepted.",
					fset.Position(call.Pos()).Filename, fset.Position(call.Pos()).Line,
					render(stored))

				return true
			}

			_ = path

			return true
		})
	}

	// The floor. If the field is renamed, or the implementation moves to
	// something other than an atomic, this census silently stops looking
	// at anything — and would then be true of a package with a Clear.
	if writes != 1 {
		t.Errorf("the census found %d write(s) to the in-flight flag, want exactly 1 "+
			"(MarkSent). Zero means this test is no longer looking at the flag and "+
			"proves nothing; more than one means there is a second writer to read.", writes)
	}
}

func writesAnAtomicBool(method string) bool {
	switch method {
	case "Store", "Swap", "CompareAndSwap":
		return true
	}

	return false
}

func render(n ast.Node) string {
	if ident, ok := n.(*ast.Ident); ok {
		return ident.Name
	}

	return "a non-literal expression"
}

// The other half of the census: no exported method name reads as a clear.
//
// A `Clear` that stored `true` would pass the census above and would still
// be a method a caller reaches for when they want the flag off.
func TestNoMethodNameReadsAsClearingTheFlag(t *testing.T) {
	t.Parallel()

	fset := token.NewFileSet()

	pkgs, err := parser.ParseDir(fset, ".", notATest, 0)
	if err != nil {
		t.Fatalf("parse the flight package: %v", err)
	}

	pkg := pkgs["flight"]
	if pkg == nil || len(pkg.Files) == 0 {
		t.Fatal("the parse found no files; this census would be vacuous")
	}

	forbidden := []string{"clear", "reset", "unset", "cancel", "rollback", "undo", "setsent"}

	found := 0

	for _, file := range pkg.Files {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv == nil {
				continue
			}

			found++

			lower := strings.ToLower(fn.Name.Name)

			for _, bad := range forbidden {
				if strings.Contains(lower, bad) {
					t.Errorf("%s has a method %q. Nothing may offer to turn the in-flight "+
						"flag off, not even a method that happens to store true: the name is "+
						"what the next caller reaches for.", "flight", fn.Name.Name)
				}
			}
		}
	}

	if found == 0 {
		t.Error("no methods were examined, so this census proves nothing")
	}
}

func notATest(fi fs.FileInfo) bool { return !strings.HasSuffix(fi.Name(), "_test.go") }

// OutcomeFor takes its exit code from the one table.
//
// A caller that could choose an exit code could pair `pending` with 0, and a
// transfer that may have moved money would be reported as a success.
func TestOutcomeForReadsTheTable(t *testing.T) {
	t.Parallel()

	for _, class := range outcome.Classes {
		want, ok := outcome.Properties(class)
		if !ok {
			t.Fatalf("the class census names %q but the table has no properties for it", class)
		}

		got := flight.OutcomeFor(class, "do this next", "a warning")

		if got.Class != class {
			t.Errorf("class = %q, want %q", got.Class, class)
		}

		if got.Exit != want.Exit {
			t.Errorf("%s exits %d, want the table's %d", class, got.Exit, want.Exit)
		}

		if got.Money != want.Money {
			t.Errorf("%s reports money %q, want the table's %q", class, got.Money, want.Money)
		}

		if got.SameKeySafe != want.SameKeySafe {
			t.Errorf("%s reports same_key_safe %v, want the table's %v",
				class, got.SameKeySafe, want.SameKeySafe)
		}

		if got.Next != "do this next" {
			t.Errorf("next = %q, want the caller's sentence", got.Next)
		}

		if len(got.Warnings) != 1 || got.Warnings[0] != "a warning" {
			t.Errorf("warnings = %v, want the caller's", got.Warnings)
		}
	}

	// The floor: a census over no classes would be true of a function that
	// returned a zero outcome for everything. Ten, because §5.6 declares
	// nine exit codes and two classes share 0.
	if got := len(outcome.Classes); got != 10 {
		t.Errorf("the class census has %d entries, want 10", got)
	}
}

// An undeclared class panics rather than exiting 0.
//
// Returning a zero `outcome.Outcome` would exit 0 — the one bug on this path
// that nothing downstream could detect, because a success is never
// questioned.
func TestOutcomeForRefusesAnUndeclaredClass(t *testing.T) {
	t.Parallel()

	defer func() {
		if recover() == nil {
			t.Error("an undeclared outcome class did not panic; it would have exited 0")
		}
	}()

	flight.OutcomeFor(outcome.Class("not_a_class"), "")
}
