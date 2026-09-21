package runs_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/kurenn/ferry/cli/internal/runs"
)

// This file makes runs.AllStates a census rather than a list.
//
// The distinction is the whole point. A test that iterates a hand-written set
// of states proves the states it names exist; it proves nothing about a state
// it does not name, because a set it never contained cannot fail it. That is
// the one-directional subset check, and on this enum the state it would miss
// is the one C16 exists to forbid: a StateConfirmed added later would never
// enter the list, and TestNoStateMeansTheHumanAgreed would stay green while
// recorded consent shipped.
//
// The remedy is the house pattern used by spec/lib/spec_suite/
// leaked_constants_spec.rb and spec/docs/api_docs_spec.rb: derive the subject
// from the source text rather than from the expectation, guard that the
// derivation found something so a broken parse cannot pass for a third
// reason, and assert **set equality** so the check runs in both directions.

// minStatesDeclared is the non-emptiness guard. Every assertion in this file
// is over a set the parser produced, and a parser that stopped recognising
// declarations would produce an empty set and satisfy "every declared state
// is in the census" vacuously. PLAN §5.3 draws seven states; fewer than that
// means the derivation is broken, not that the enum shrank.
const minStatesDeclared = 7

// declaredState is one State constant as the source declares it.
type declaredState struct {
	name  string // the Go identifier, e.g. StateNotStarted
	value string // the JSON value, e.g. not_started
	pos   string // file:line, so a failure names the line to look at
}

// declaredStates derives every State this package declares by parsing the
// package's own non-test sources.
//
// Both spellings count, because a state added carelessly could use either:
//
//	const ( StateFoo State = "foo" )   // typed const, how the enum is written
//	var StateFoo = State("foo")        // a conversion, which reads as innocent
//
// Test files are excluded on purpose: a state declared in a _test.go file is
// a fixture, not part of the package's vocabulary, and including them would
// make this file's own mutation fixtures fail it.
func declaredStates(t *testing.T) []declaredState {
	t.Helper()

	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse the runs package: %v", err)
	}

	var out []declaredState
	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			for _, decl := range file.Decls {
				gen, ok := decl.(*ast.GenDecl)
				if !ok || (gen.Tok != token.CONST && gen.Tok != token.VAR) {
					continue
				}
				// Within one const block a spec may omit the type and inherit
				// it from the spec above, so the last type seen is carried.
				var carried ast.Expr
				for _, spec := range gen.Specs {
					vs, ok := spec.(*ast.ValueSpec)
					if !ok {
						continue
					}
					if vs.Type != nil {
						carried = vs.Type
					}
					for i, name := range vs.Names {
						var value ast.Expr
						if i < len(vs.Values) {
							value = vs.Values[i]
						}
						if !isStateDecl(carried, value) {
							continue
						}
						out = append(out, declaredState{
							name:  name.Name,
							value: literalValue(value),
							pos:   fset.Position(name.Pos()).String(),
						})
					}
				}
			}
		}
	}

	if len(out) < minStatesDeclared {
		t.Fatalf("the parse found only %d State declarations (%v); PLAN §5.3 draws at "+
			"least %d, so the derivation is broken and every check in this file would "+
			"pass vacuously", len(out), names(out), minStatesDeclared)
	}
	return out
}

// isStateDecl reports whether a declaration declares a State: either its type
// is State, or its value is a State(...) conversion.
func isStateDecl(typ, value ast.Expr) bool {
	if id, ok := typ.(*ast.Ident); ok && id.Name == "State" {
		return true
	}
	call, ok := value.(*ast.CallExpr)
	if !ok {
		return false
	}
	id, ok := call.Fun.(*ast.Ident)
	return ok && id.Name == "State"
}

// literalValue pulls the string out of `"foo"` or `State("foo")`.
func literalValue(value ast.Expr) string {
	if call, ok := value.(*ast.CallExpr); ok && len(call.Args) == 1 {
		value = call.Args[0]
	}
	lit, ok := value.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return ""
	}
	s, err := strconv.Unquote(lit.Value)
	if err != nil {
		return ""
	}
	return s
}

func names(ds []declaredState) []string {
	out := make([]string, 0, len(ds))
	for _, d := range ds {
		out = append(out, d.name)
	}
	sort.Strings(out)
	return out
}

// TestAllStatesIsCompleteAgainstTheSource is the bidirectional assertion.
//
// Direction one: a state the source declares but the census omits. This is
// the mutation that matters — adding StateConfirmed to the package without
// touching a test must fail here.
//
// Direction two: a state the census names that the source does not declare.
// Without it the census could accumulate stale entries, and a state removed
// from the source would leave a name no longer meaning anything while the
// check that "every census member exists" kept passing.
func TestAllStatesIsCompleteAgainstTheSource(t *testing.T) {
	declared := declaredStates(t)

	inSource := map[string]declaredState{}
	for _, d := range declared {
		if prev, dup := inSource[d.value]; dup {
			t.Errorf("two State constants share the value %q: %s at %s and %s at %s",
				d.value, prev.name, prev.pos, d.name, d.pos)
		}
		inSource[d.value] = d
	}

	inCensus := map[string]bool{}
	for _, s := range runs.AllStates() {
		if s == "" {
			t.Error("AllStates contains an empty state")
		}
		if inCensus[string(s)] {
			t.Errorf("AllStates lists %q twice", s)
		}
		inCensus[string(s)] = true
	}

	for value, d := range inSource {
		if !inCensus[value] {
			t.Errorf("%s declares %s = %q at %s, but runs.AllStates() omits it.\n\n"+
				"Every test that iterates states reads AllStates, so a state missing "+
				"from it is a state no control examines. If this state can stand in "+
				"for a human's consent, C16 forbids it outright; if it cannot, add it "+
				"to AllStates and to the tables in sweep_test.go and "+
				"adversarial_test.go that must now account for it.",
				"the runs package", d.name, value, d.pos)
		}
	}
	for value := range inCensus {
		if _, ok := inSource[value]; !ok {
			t.Errorf("runs.AllStates() lists %q, which no State constant in the package "+
				"declares. Either the constant was removed and the census is stale, or "+
				"the census has a typo and one real state is going unchecked.",
				value)
		}
	}

	if len(inSource) != len(inCensus) {
		t.Errorf("the source declares %d states and the census lists %d", len(inSource), len(inCensus))
	}
}

// The derivation must see both spellings a state can be declared with, or the
// completeness check could be evaded by writing the declaration differently.
// These fixtures are in a _test.go file, which declaredStates excludes, so
// they are checked through the predicate directly.
func TestTheDerivationRecognisesBothSpellingsOfAStateDeclaration(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want []string
	}{
		{
			name: "a typed const block, how the enum is written",
			src: `package p
const (
	StateFoo State = "foo"
	StateBar State = "bar"
)`,
			want: []string{"StateBar", "StateFoo"},
		},
		{
			name: "a const block inheriting the type from the line above",
			src: `package p
const (
	StateFoo State = "foo"
	StateBar       = "bar"
)`,
			want: []string{"StateBar", "StateFoo"},
		},
		{
			name: "a var holding a conversion, which reads as innocent",
			src: `package p
var StateConfirmed = State("confirmed")`,
			want: []string{"StateConfirmed"},
		},
		{
			name: "a const holding a conversion",
			src: `package p
const StateConfirmed = State("confirmed")`,
			want: []string{"StateConfirmed"},
		},
		{
			name: "declarations of other types are not states",
			src: `package p
const (
	StepSimulate       = "simulate"
	Schema         int = 2
	ScrubbedToken      = "[SCRUBBED]"
)`,
			want: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseStatesFromSource(t, tc.src)
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("derived %v, want %v", got, tc.want)
			}
		})
	}
}

// parseStatesFromSource runs the same predicate and type-carrying logic as
// declaredStates over one source string.
func parseStatesFromSource(t *testing.T, src string) []string {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "fixture.go", src, 0)
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}

	var out []string
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || (gen.Tok != token.CONST && gen.Tok != token.VAR) {
			continue
		}
		var carried ast.Expr
		for _, spec := range gen.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			if vs.Type != nil {
				carried = vs.Type
			}
			for i, name := range vs.Names {
				var value ast.Expr
				if i < len(vs.Values) {
					value = vs.Values[i]
				}
				if isStateDecl(carried, value) {
					out = append(out, name.Name)
				}
			}
		}
	}
	sort.Strings(out)
	return out
}

// assertCoversEveryState checks a table-driven test's coverage against the
// census, so a table is verified to be exhaustive rather than trusted to be.
// It runs both ways: a state the table omits fails, and so does a "state" the
// table covers that the census does not know.
func assertCoversEveryState(t *testing.T, covered map[runs.State]bool) {
	t.Helper()

	var missing []string
	for _, s := range runs.AllStates() {
		if !covered[s] {
			missing = append(missing, string(s))
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Errorf("the table does not cover these states: %s.\n\n"+
			"A table that omits a state asserts nothing about it, which is how a state "+
			"added later goes unexamined. Add a case, or state in the table why the "+
			"state cannot occur here.", strings.Join(missing, ", "))
	}
	for s := range covered {
		if !hasState(s) {
			t.Errorf("the table covers %q, which is not a state in runs.AllStates()", s)
		}
	}
}

func hasState(s runs.State) bool {
	for _, known := range runs.AllStates() {
		if known == s {
			return true
		}
	}
	return false
}

// Every state must be classified by both predicates, and the two must agree
// with PLAN §5.3 rather than with each other. Iterating the census means a
// state added later arrives here unclassified rather than unnoticed.
func TestEveryStateIsClassifiedByBothPredicates(t *testing.T) {
	want := map[runs.State]struct{ terminal, mayHaveSent bool }{
		runs.StateNotStarted:           {false, false},
		runs.StateAwaitingConfirmation: {false, false},
		runs.StateDeclined:             {true, false},
		runs.StatePending:              {false, true},
		runs.StateAnswered:             {false, true},
		runs.StateTerminal:             {true, true},
		runs.StateUnreachable:          {true, false},
	}

	covered := map[runs.State]bool{}
	for s, w := range want {
		covered[s] = true
		if got := s.Terminal(); got != w.terminal {
			t.Errorf("%s.Terminal() = %v, want %v", s, got, w.terminal)
		}
		if got := s.MayHaveSent(); got != w.mayHaveSent {
			t.Errorf("%s.MayHaveSent() = %v, want %v", s, got, w.mayHaveSent)
		}
	}
	assertCoversEveryState(t, covered)

	// A state that is terminal and may have sent is settled; one that is
	// neither is resolvable without a send. The pair that must not exist is
	// "terminal and cannot be declined and never sent", because such a state
	// would be a dead end a headless caller could not clear (V4).
	for _, s := range runs.AllStates() {
		if s.Terminal() && !s.MayHaveSent() && s != runs.StateDeclined && s != runs.StateUnreachable {
			t.Errorf("%s is terminal and never sent, but is neither declined nor "+
				"unreachable: a run in it could not be resolved or explained", s)
		}
	}
}
