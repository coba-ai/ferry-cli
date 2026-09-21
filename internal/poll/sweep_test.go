package poll

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// AC59 — **there is one poll loop**, and this is the sweep that says so.
//
// C3 is the invariant. A second place that waits and re-reads a command is
// a second schedule, and the schedules disagree: this one honours
// `Retry-After` on the `202`, then the command's own
// `retry_after_seconds`, and stops at the caller's deadline with `pending`
// rather than `transient`. A loop written somewhere else will honour some
// of that, and the half it drops is the half that decides whether a caller
// is told to resend a request that may already have moved money.
//
// The sweep is over the **AST**, not over the text. A390's lesson in this
// project was that the layer decides: a grep for `time.Sleep` finds it in a
// comment and in a string and misses `sleep := time.Sleep`, and a sweep
// that reports a comment is a sweep somebody adds an exception to.

// sleepAllowed is every non-test `time.Sleep` outside this package, with the
// reason it is not a poll loop.
//
// It is keyed by `file:function` rather than by file, so moving the call to
// another function in the same file does not inherit the permission.
var sleepAllowed = map[string]string{
	"internal/runs/lock.go:acquire": "the flock retry, 5ms between non-blocking attempts for " +
		"at most 250ms. It waits for a lock and never re-reads a command.",
	"internal/api/retry.go:Sleep": "realClock.Sleep — the one-line implementation of the " +
		"Clock this package and internal/api both take, so a test can supply a fake. " +
		"It is where the real sleep finally happens for both loops, including this one.",
}

// commandPathAllowed is every non-test mention of the commands route outside
// this package.
var commandPathAllowed = map[string]string{
	"internal/api/routes.go": "the route table. Naming a route is not requesting it, and " +
		"this package reaches the route through `api.RouteFor(outcome.OpGetCommand)` " +
		"rather than by writing the path — which is what keeps the two in step.",
}

// TestThereIsOnePollLoop sweeps the tree for a second one.
func TestThereIsOnePollLoop(t *testing.T) {
	t.Parallel()

	sleeps, paths := sweep(t)

	// The floor, and it is not decorative: this sweep's whole output is
	// "I found nothing", which is exactly what a walker pointed at the
	// wrong directory returns. If it cannot see the two sleeps and the one
	// route mention that are known to be there, it cannot see a third.
	if len(sleeps) == 0 {
		t.Fatal("the sweep found no time.Sleep anywhere, including the two that are known " +
			"to exist; it is not reading the tree")
	}

	if len(paths) == 0 {
		t.Fatal("the sweep found no mention of /v1/commands anywhere, including the route " +
			"table; it is not reading the tree")
	}

	for _, site := range sleeps {
		if strings.HasPrefix(site.file, "internal/poll/") {
			continue
		}

		if _, ok := sleepAllowed[site.key()]; !ok {
			t.Errorf("%s:%d: %s sleeps, and it is not internal/poll and not in "+
				"sleepAllowed. If this is a second poll loop it must not exist (C3); "+
				"if it is a wait for something else, say so in sleepAllowed.",
				site.file, site.line, site.function)
		}
	}

	for _, site := range paths {
		if strings.HasPrefix(site.file, "internal/poll/") {
			continue
		}

		if _, ok := commandPathAllowed[site.file]; !ok {
			t.Errorf("%s:%d: names %q outside internal/poll. Every read of a command goes "+
				"through poll.Watch (C3).", site.file, site.line, site.literal)
		}
	}

	// The other direction, for both lists. An allowlist entry whose site
	// has gone is a permission granted for a reason that no longer holds,
	// and the next reader takes the list as a description of the tree.
	assertEveryExemptionIsUsed(t, "sleepAllowed", keysOf(sleepAllowed), sleepKeys(sleeps))
	assertEveryExemptionIsUsed(t, "commandPathAllowed", keysOf(commandPathAllowed), pathFiles(paths))
}

// TestThisPackageIsTheOnlyOneThatReadsACommand is the positive half.
//
// The sweep above says nowhere else polls. This says *here* does — because
// a sweep over a tree where nothing polls at all would pass it, and that is
// the state this CLI would be in if `poll.Watch` were deleted.
func TestThisPackageIsTheOnlyOneThatReadsACommand(t *testing.T) {
	t.Parallel()

	sleeps, _ := sweep(t)

	found := false

	for _, site := range sleeps {
		if strings.HasPrefix(site.file, "internal/poll/") {
			found = true

			break
		}
	}

	if !found {
		t.Error("internal/poll contains no time.Sleep, so either the loop has gone or it " +
			"waits by some means this sweep cannot see — and in the second case the " +
			"sweep above is no longer looking for the right thing")
	}
}

type site struct {
	file     string
	line     int
	function string
	literal  string
}

func (s site) key() string { return s.file + ":" + s.function }

// sweep walks every non-test Go file under `cli/` and collects the two
// things AC59 names.
func sweep(t *testing.T) (sleeps, paths []site) {
	t.Helper()

	root := filepath.Join("..", "..")

	fset := token.NewFileSet()

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if d.IsDir() {
			// `testdata` holds recordings, and a vendored tree would hold
			// somebody else's loops.
			if d.Name() == "testdata" || d.Name() == "vendor" || d.Name() == ".git" {
				return fs.SkipDir
			}

			return nil
		}

		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}

		file, parseErr := parser.ParseFile(fset, path, nil, 0)
		if parseErr != nil {
			t.Fatalf("parsing %s: %v", path, parseErr)
		}

		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			rel = path
		}

		rel = filepath.ToSlash(rel)

		var enclosing string

		ast.Inspect(file, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.FuncDecl:
				enclosing = node.Name.Name
			case *ast.CallExpr:
				sel, ok := node.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "Sleep" {
					return true
				}

				pkg, ok := sel.X.(*ast.Ident)
				if !ok || pkg.Name != "time" {
					return true
				}

				sleeps = append(sleeps, site{
					file:     rel,
					line:     fset.Position(node.Pos()).Line,
					function: enclosing,
				})
			case *ast.BasicLit:
				if node.Kind != token.STRING {
					return true
				}

				value, unquoteErr := strconv.Unquote(node.Value)
				if unquoteErr != nil || !strings.Contains(value, "/v1/commands") {
					return true
				}

				paths = append(paths, site{
					file:     rel,
					line:     fset.Position(node.Pos()).Line,
					function: enclosing,
					literal:  value,
				})
			}

			return true
		})

		return nil
	})
	if err != nil {
		t.Fatalf("walking the tree: %v", err)
	}

	return sleeps, paths
}

func assertEveryExemptionIsUsed(t *testing.T, name string, allowed, seen []string) {
	t.Helper()

	present := make(map[string]bool, len(seen))
	for _, s := range seen {
		present[s] = true
	}

	for _, entry := range allowed {
		if !present[entry] {
			t.Errorf("%s exempts %q, which the sweep no longer finds. An exemption for a "+
				"site that has gone is a hole held open for nothing.", name, entry)
		}
	}
}

func keysOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}

	sort.Strings(out)

	return out
}

func sleepKeys(sites []site) []string {
	out := make([]string, 0, len(sites))
	for _, s := range sites {
		out = append(out, s.key())
	}

	return out
}

func pathFiles(sites []site) []string {
	out := make([]string, 0, len(sites))
	for _, s := range sites {
		out = append(out, s.file)
	}

	return out
}
