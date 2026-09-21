package poll

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// A394 asks that a Go struct for `Command` be pinned to `openapi.yaml` "the
// way cli/internal/api/schema_pin_test.go does — both directions, with a
// floor", and notes that that reader resolves `oneOf` into arms and leaves
// `allOf`, `anyOf` and `not` fatal. `internal/api`'s reader is in its test
// package and cannot be imported, so this is the same discipline at the
// scale this pin needs: one schema, one level of properties, one enum.
//
// The three composition keywords are fatal here for the same reason they are
// there. A reader that walked past an `allOf` would return the properties of
// the outer node alone, and the pin would then pass while saying nothing
// about the half of the schema that had moved into the branch — the quiet
// failure, not the loud one.

// loadCommandSchema reads `components.schemas.Command`.
func loadCommandSchema(t *testing.T, path string) map[string]any {
	t.Helper()

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}

	var doc map[string]any
	if err := yaml.Unmarshal(b, &doc); err != nil {
		t.Fatalf("parsing %s: %v", path, err)
	}

	cur := any(doc)

	for _, key := range []string{"components", "schemas", "Command"} {
		m, ok := cur.(map[string]any)
		if !ok {
			t.Fatalf("%s: the path to components.schemas.Command runs through a non-mapping", path)
		}

		cur, ok = m[key]
		if !ok {
			t.Fatalf("%s has no components.schemas.Command", path)
		}
	}

	schema, ok := cur.(map[string]any)
	if !ok {
		t.Fatalf("%s: components.schemas.Command is not a mapping", path)
	}

	for _, keyword := range []string{"allOf", "anyOf", "not"} {
		if _, present := schema[keyword]; present {
			t.Fatalf("%s: Command uses %s, which this reader does not resolve. Resolving it "+
				"wrongly would make this pin pass while describing half the schema, so it "+
				"stops here instead.", path, keyword)
		}
	}

	return schema
}

// commandSchema is the set of property names `Command` declares.
func commandSchema(t *testing.T, path string) map[string]bool {
	t.Helper()

	schema := loadCommandSchema(t, path)

	properties, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("%s: Command has no properties mapping", path)
	}

	out := make(map[string]bool, len(properties))
	for name := range properties {
		out[name] = true
	}

	return out
}

// commandStateEnum is `Command.properties.state.enum`.
func commandStateEnum(t *testing.T, path string) []string {
	t.Helper()

	schema := loadCommandSchema(t, path)

	properties, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("%s: Command has no properties mapping", path)
	}

	state, ok := properties["state"].(map[string]any)
	if !ok {
		t.Fatalf("%s: Command.properties.state is not a mapping", path)
	}

	list, ok := state["enum"].([]any)
	if !ok {
		t.Fatalf("%s: Command.properties.state has no enum. Without one every state is "+
			"unknown and this CLI escalates each of them.", path)
	}

	out := make([]string, 0, len(list))

	for i, entry := range list {
		s, ok := entry.(string)
		if !ok {
			t.Fatalf("%s: Command.properties.state.enum[%d] is not a string", path, i)
		}

		out = append(out, s)
	}

	sort.Strings(out)

	return out
}

// statesMentionedIn collects the string literals this package compares a
// command's `state` against.
//
// The point is narrow: a literal here that the contract's enum does not
// contain is a branch that can never be taken, and the state it was meant
// for is going somewhere else. It reads assignments and comparisons through
// the AST rather than grepping, so a state named in a comment or an error
// message is not mistaken for one the code branches on.
func statesMentionedIn(t *testing.T, files ...string) []string {
	t.Helper()

	seen := map[string]bool{}
	fset := token.NewFileSet()

	for _, name := range files {
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", name, err)
		}

		ast.Inspect(file, func(n ast.Node) bool {
			cmp, ok := n.(*ast.BinaryExpr)
			if !ok || (cmp.Op != token.EQL && cmp.Op != token.NEQ) {
				return true
			}

			for _, side := range []ast.Expr{cmp.X, cmp.Y} {
				lit, ok := side.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}

				value, err := strconv.Unquote(lit.Value)
				if err != nil || value == "" {
					continue
				}

				// Only the comparisons whose other side is something
				// called `State`.
				if !comparesState(cmp) {
					continue
				}

				seen[value] = true
			}

			return true
		})
	}

	out := make([]string, 0, len(seen))
	for state := range seen {
		out = append(out, state)
	}

	sort.Strings(out)

	return out
}

func comparesState(cmp *ast.BinaryExpr) bool {
	for _, side := range []ast.Expr{cmp.X, cmp.Y} {
		switch e := side.(type) {
		case *ast.SelectorExpr:
			if strings.EqualFold(e.Sel.Name, "state") {
				return true
			}
		case *ast.Ident:
			if strings.EqualFold(e.Name, "state") {
				return true
			}
		}
	}

	return false
}

// TestTheReaderReadsTheRealDocument is the floor under the two pins above.
//
// Both of them are "compare what I found against what the struct says", and
// both pass trivially against a reader that found nothing. This one asserts
// that the reader reached the real schema, naming properties and states from
// the contract that no plausible restructuring would leave in place by
// accident.
func TestTheReaderReadsTheRealDocument(t *testing.T) {
	t.Parallel()

	path := contractPath(t)

	// The path is resolved the same way `internal/api`'s pin resolves it,
	// so a moved document fails here rather than skipping every pin.
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the contract is not where this test looks (%s): %v", path, err)
	}

	schema := commandSchema(t, path)

	for _, want := range []string{"object", "id", "operation", "state", "poll", "result", "last_error", "contradiction"} {
		if !schema[want] {
			t.Errorf("the reader did not find Command.%s; it is not reading the real schema", want)
		}
	}

	states := commandStateEnum(t, path)

	for _, want := range []string{"completed", "failed_terminal", "inflight", "needs_operator"} {
		if !contains(states, want) {
			t.Errorf("the reader did not find the state %q in the enum: %v", want, states)
		}
	}

	// And the AST walk, which is the other thing a floor is needed under:
	// if it found no comparisons at all, its half of the state pin says
	// nothing.
	mentioned := statesMentionedIn(t, "command.go", "poll.go")
	if len(mentioned) == 0 {
		t.Log("no state literals are compared in this package; the classification lives in " +
			"internal/outcome, which is where the census is held")
	}
}

// contractRoot is where the repository root sits relative to this package.
var contractRoot = filepath.Join("..", "..")
