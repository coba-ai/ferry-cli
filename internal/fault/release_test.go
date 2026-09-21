package fault_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/kurenn/ferry-cli/internal/fault"
	"github.com/kurenn/ferry-cli/internal/fsx"
)

// This file has no build tag, so it runs in the **release** configuration.
// Its subject is the guarantee that matters most about this package: a
// shipped `ferry` cannot be made to die in the middle of a money request by
// anything in the environment.

// The release build ignores the variable entirely.
//
// This is the claim a released binary depends on. `FERRY_CLI_FAULT` is a
// plain environment variable; if it worked in a shipped binary, anyone who
// could set it could make a colleague's transfer die after the send, and the
// colleague would be told the outcome was not established.
func TestTheReleaseBuildIgnoresTheEnvironment(t *testing.T) {
	if fault.Enabled {
		t.Skip("built with -tags faultinject; the armed behaviour is tested in inject_test.go")
	}

	for _, point := range append(fault.Points(), "not_a_point") {
		t.Setenv(fault.EnvVar, point)

		if fault.Armed(point) {
			t.Errorf("%s is armed in a release build", point)
		}

		// Neither may do anything. A panic here fails the test by
		// escaping, which is the assertion.
		fault.Die(point)
		fault.Panic(point)
	}

	if fault.Requested() == "" {
		t.Error("Requested() reads nothing even though the variable is set; " +
			"this test would then be asserting nothing")
	}
}

// The floor for the test above, and the reason it is not vacuous: the same
// file in the armed build asserts the opposite, and this pins that the two
// builds are the only difference.
//
// `Enabled` is the constant the tag switches. If both files declared it
// `false`, the armed test would skip and nothing would ever test the
// injector.
func TestEnabledMatchesTheBuildTag(t *testing.T) {
	// Which file is compiled is decided by the tag, so this cannot be
	// checked from inside the package at run time. What can be checked is
	// that the two declarations disagree, which is what makes the tag mean
	// something.
	on := declaredEnabled(t, "inject_on.go")
	off := declaredEnabled(t, "inject_off.go")

	if on != "true" {
		t.Errorf("inject_on.go declares Enabled = %s, want true", on)
	}

	if off != "false" {
		t.Errorf("inject_off.go declares Enabled = %s, want false", off)
	}

	// And the tags themselves, because a file with the wrong constraint
	// would compile into both builds and the last one would win silently.
	if got := constraintOf(t, "inject_on.go"); !strings.Contains(got, "faultinject") ||
		strings.Contains(got, "!faultinject") {
		t.Errorf("inject_on.go's build constraint is %q, want faultinject", got)
	}

	if got := constraintOf(t, "inject_off.go"); !strings.Contains(got, "!faultinject") {
		t.Errorf("inject_off.go's build constraint is %q, want !faultinject", got)
	}
}

func declaredEnabled(t *testing.T, file string) string {
	t.Helper()

	fset := token.NewFileSet()

	parsed, err := parser.ParseFile(fset, file, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}

	for _, decl := range parsed.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}

		for _, spec := range gen.Specs {
			value, ok := spec.(*ast.ValueSpec)
			if !ok || len(value.Names) == 0 || value.Names[0].Name != "Enabled" {
				continue
			}

			if len(value.Values) != 1 {
				t.Fatalf("%s declares Enabled with %d values", file, len(value.Values))
			}

			ident, ok := value.Values[0].(*ast.Ident)
			if !ok {
				t.Fatalf("%s declares Enabled as a non-literal", file)
			}

			return ident.Name
		}
	}

	t.Fatalf("%s declares no Enabled", file)

	return ""
}

func constraintOf(t *testing.T, file string) string {
	t.Helper()

	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read %s: %v", file, err)
	}

	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, "//go:build ") {
			return strings.TrimPrefix(line, "//go:build ")
		}
	}

	t.Fatalf("%s has no //go:build line", file)

	return ""
}

// The point names are a closed set, held to the uses in the tree.
//
// Both directions, with a floor (`docs/dev-loop-learnings.md`: a
// one-directional subset check has shipped at least five times here). A
// declared point nothing calls is a fault a test believes it can reach and
// cannot; a called point nothing declares is a typo that arms nothing, and
// both failures look exactly like a passing test.
func TestEveryDeclaredPointIsUsedAndEveryUsedPointIsDeclared(t *testing.T) {
	declared := map[string]bool{}
	for _, name := range fault.Points() {
		declared[name] = true
	}

	if len(declared) == 0 {
		t.Fatal("no points are declared; this census would be vacuous")
	}

	// The constant *identifiers*, so a call site can be matched by the
	// name it uses rather than by the string it resolves to.
	idents := identifiersFor(t, declared)

	used := map[string]bool{}

	root := filepath.Join("..", "..")

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}

		fset := token.NewFileSet()

		parsed, parseErr := parser.ParseFile(fset, path, nil, 0)
		if parseErr != nil {
			return nil
		}

		ast.Inspect(parsed, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) != 1 {
				return true
			}

			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}

			pkg, ok := sel.X.(*ast.Ident)
			if !ok || pkg.Name != "fault" {
				return true
			}

			// `Armed` is here with `Die` and `Panic` because not every
			// point is a death. `record_fails_after_send` arms the
			// filesystem guard instead, and a census that only knew about
			// the two dying injectors reported it as unreachable — which
			// it is not, and which is the kind of false positive that gets
			// a census deleted rather than fixed.
			switch sel.Sel.Name {
			case "Die", "Panic", "Armed":
			default:
				return true
			}

			arg, ok := call.Args[0].(*ast.SelectorExpr)
			if !ok {
				// `fault.Armed(name)` inside the package itself takes a
				// parameter, not a constant. Only calls from outside are
				// census material.
				if parsed.Name.Name == "fault" {
					return true
				}

				t.Errorf("%s calls fault.%s with a non-constant argument; a point name that "+
					"is not one of the declared constants arms nothing and reads as a test "+
					"that passes", path, sel.Sel.Name)

				return true
			}

			name, ok := idents[arg.Sel.Name]
			if !ok {
				t.Errorf("%s calls fault.%s(fault.%s), which the package does not declare",
					path, sel.Sel.Name, arg.Sel.Name)

				return true
			}

			used[name] = true

			return true
		})

		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}

	if len(used) == 0 {
		t.Fatal("no call site was found, so this census proves nothing")
	}

	for name := range declared {
		if !used[name] {
			t.Errorf("the point %q is declared and never injected anywhere. A test arming it "+
				"would pass by doing nothing at all.", name)
		}
	}

	for name := range used {
		if !declared[name] {
			t.Errorf("the point %q is injected but not declared, so Known() refuses it", name)
		}
	}
}

// identifiersFor maps each declared point's Go identifier to its string
// value, by reading the package's own constant block.
func identifiersFor(t *testing.T, declared map[string]bool) map[string]string {
	t.Helper()

	fset := token.NewFileSet()

	parsed, err := parser.ParseFile(fset, "fault.go", nil, 0)
	if err != nil {
		t.Fatalf("parse fault.go: %v", err)
	}

	out := map[string]string{}

	for _, decl := range parsed.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}

		for _, spec := range gen.Specs {
			value, ok := spec.(*ast.ValueSpec)
			if !ok || len(value.Names) != 1 || len(value.Values) != 1 {
				continue
			}

			lit, ok := value.Values[0].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				continue
			}

			unquoted, err := strconv.Unquote(lit.Value)
			if err != nil {
				continue
			}

			if declared[unquoted] {
				out[value.Names[0].Name] = unquoted
			}
		}
	}

	if len(out) != len(declared) {
		t.Fatalf("matched %d identifiers to %d declared points: %v vs %v",
			len(out), len(declared), sorted(out), fault.Points())
	}

	return out
}

func sorted(m map[string]string) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}

	sort.Strings(out)

	return out
}

// Known refuses a name it does not declare, in both directions.
func TestKnownIsTheDeclaredSet(t *testing.T) {
	for _, name := range fault.Points() {
		if !fault.Known(name) {
			t.Errorf("Known(%q) is false for a declared point", name)
		}
	}

	for _, name := range []string{"", "after_record", "AFTER_RECORD_WRITTEN", "after_record_written "} {
		if fault.Known(name) {
			t.Errorf("Known(%q) is true for a name the package does not declare", name)
		}
	}
}

// The guard is the filesystem seam AC69(b) uses, and it fails only after it
// is tripped.
//
// A guard that failed from the start would break `Begin`, which writes
// *before* the send — and the test that wanted a post-send record failure
// would quietly become a test of a pre-send one, which is exit 1 and the
// opposite answer.
func TestTheGuardFailsOnlyAfterItIsTripped(t *testing.T) {
	dir := t.TempDir()

	guard := fault.NewGuard(fsx.OS())

	if guard.Tripped() {
		t.Fatal("a new guard is already tripped")
	}

	before := filepath.Join(dir, "before")

	if err := fsx.WriteFileAtomic(guard, before, []byte("ok"), 0o600); err != nil {
		t.Fatalf("an untripped guard refused a write: %v", err)
	}

	if got, err := os.ReadFile(before); err != nil || string(got) != "ok" {
		t.Fatalf("the untripped write did not land: %q %v", got, err)
	}

	guard.Trip()

	if !guard.Tripped() {
		t.Fatal("Trip did not trip the guard")
	}

	after := filepath.Join(dir, "after")

	if err := fsx.WriteFileAtomic(guard, after, []byte("ok"), 0o600); err == nil {
		t.Error("a tripped guard allowed a write")
	}

	// And nothing landed, which is what makes the record on disk the one
	// from before the failed write.
	if _, err := os.Stat(after); !os.IsNotExist(err) {
		t.Errorf("a tripped guard left %s behind: %v", after, err)
	}
}
