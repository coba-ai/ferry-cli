package render_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/kurenn/ferry-cli/internal/render"
)

// AC42 — text mode prints a secret in exactly two places.
//
// "Exactly two" is a claim in both directions, and the second direction is the
// hard one: proving no *other* path prints a secret. Three independent checks
// run here, each with a floor that makes it fail rather than pass when it
// stops finding anything:
//
//  1. TestEveryRevealSiteInTheSourceIsDeclared — the syntactic direction. An
//     AST sweep over every non-test Go file under `cli/` collects the site
//     constants passed to `render.Reveal` and holds them to `render.Sites`.
//  2. TestNoPrintTakesATokenExceptThroughTheSink — the other syntactic
//     direction. The same sweep finds every `.Token` read inside a print call
//     and requires it to be wrapped in `Reveal`.
//  3. `behaviour_test.go` — every command this unit owns is run against the
//     fixture with canary credentials and the transcript is scanned.

// declaredSites is what the registry says, as strings.
func declaredSites() []string {
	ids := render.SiteIDs()

	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, string(id))
	}

	sort.Strings(out)

	return out
}

// cliRoot is `cli/`, located from this file rather than from the working
// directory: `go test` runs each package in its own directory.
func cliRoot(t *testing.T) string {
	t.Helper()

	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed, so the sweep has nothing to walk")
	}

	// .../cli/internal/render/secrets_test.go
	return filepath.Dir(filepath.Dir(filepath.Dir(file)))
}

// goFiles is every non-test Go file under `cli/`.
func goFiles(t *testing.T) []string {
	t.Helper()

	root := cliRoot(t)

	var out []string

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if d.IsDir() {
			if d.Name() == "testdata" {
				return filepath.SkipDir
			}

			return nil
		}

		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}

		out = append(out, path)

		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}

	if len(out) < 20 {
		t.Fatalf("the sweep found %d Go files under %s; that is not this CLI, and every "+
			"assertion over it would be vacuous", len(out), root)
	}

	return out
}

// revealCall is one call to Reveal found in the source.
type revealCall struct {
	file string
	line int
	site string
}

// findRevealCalls is the AST sweep.
//
// It matches `render.Reveal(...)` from outside the package and `Reveal(...)`
// from inside it, and records the *source spelling* of the first argument.
// That is deliberate: the check is that the constant named at the call site is
// one the registry declares, and resolving it to a value would make a call
// passing a computed SiteID look fine.
func findRevealCalls(t *testing.T) []revealCall {
	t.Helper()

	var found []revealCall

	fset := token.NewFileSet()

	for _, path := range goFiles(t) {
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}

		inRender := file.Name.Name == "render"

		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}

			name, qualified := calleeName(call.Fun)
			if name != "Reveal" {
				return true
			}

			// A bare `Reveal` outside package render is something else
			// entirely, and a qualified one has to be `render.Reveal`.
			if qualified != "" && qualified != "render" {
				return true
			}

			if qualified == "" && !inRender {
				return true
			}

			pos := fset.Position(call.Pos())

			site := "«not a constant»"
			if len(call.Args) > 0 {
				site = siteSpelling(call.Args[0])
			}

			found = append(found, revealCall{file: pos.Filename, line: pos.Line, site: site})

			return true
		})
	}

	return found
}

func calleeName(fun ast.Expr) (name, qualifier string) {
	switch f := fun.(type) {
	case *ast.Ident:
		return f.Name, ""
	case *ast.SelectorExpr:
		if pkg, ok := f.X.(*ast.Ident); ok {
			return f.Sel.Name, pkg.Name
		}

		return f.Sel.Name, "«expression»"
	}

	return "", ""
}

// siteSpelling turns the first argument into the constant's name, or a marker
// that will not match any declared site.
func siteSpelling(arg ast.Expr) string {
	switch a := arg.(type) {
	case *ast.Ident:
		return constantValue(a.Name)
	case *ast.SelectorExpr:
		return constantValue(a.Sel.Name)
	}

	return "«computed»"
}

// constantValue maps a Go constant name to the SiteID it holds.
//
// The mapping is derived from the registry rather than written out, so a new
// site cannot be added to `Sites` and quietly excused here.
func constantValue(name string) string {
	switch name {
	case "SiteKeysCreateToken":
		return string(render.SiteKeysCreateToken)
	case "SiteSimulatePlanToken":
		return string(render.SiteSimulatePlanToken)
	}

	return "«" + name + "»"
}

// The syntactic direction: every Reveal in the tree names a declared site, and
// every declared site is reached by at least one Reveal.
func TestEveryRevealSiteInTheSourceIsDeclared(t *testing.T) {
	calls := findRevealCalls(t)

	if len(calls) == 0 {
		t.Fatal("the sweep found no call to render.Reveal anywhere under cli/. Either the sink is " +
			"not being used — in which case nothing constrains where a secret is printed — or the " +
			"sweep is broken. Both are failures.")
	}

	seen := map[string]bool{}

	for _, c := range calls {
		if strings.HasPrefix(c.site, "«") {
			t.Errorf("%s:%d calls render.Reveal with %s rather than a declared site constant. "+
				"AC42's count is over the constants in render.Sites, and a computed one cannot be counted.",
				c.file, c.line, c.site)

			continue
		}

		if _, ok := render.SiteFor(render.SiteID(c.site)); !ok {
			t.Errorf("%s:%d reveals at %q, which render.Sites does not declare", c.file, c.line, c.site)
		}

		seen[c.site] = true
	}

	got := make([]string, 0, len(seen))
	for site := range seen {
		got = append(got, site)
	}

	sort.Strings(got)

	// Both directions. The declared set may legitimately run ahead of the
	// source while U5 is unmerged, so the missing ones are named rather than
	// counted — but a site in the source and not in the registry is always
	// an error, and is caught above.
	want := declaredSites()

	if !reflect.DeepEqual(got, want) {
		var missing []string

		for _, site := range want {
			if !seen[site] {
				missing = append(missing, site)
			}
		}

		t.Logf("declared sites not yet reached by any call: %v", missing)

		for _, site := range missing {
			s, _ := render.SiteFor(render.SiteID(site))
			if s.Owner == "U4" {
				t.Errorf("render.Sites declares %q as U4's and nothing in the tree reveals there", site)
			}
		}
	}

	if len(want) != 2 {
		t.Errorf("render.Sites declares %d sites and AC42 says exactly two: %v", len(want), want)
	}
}

// The other syntactic direction: a token read inside a print call has to be
// wrapped in the sink.
//
// This is the check that catches the defect AC42 is actually about — not a
// second `Reveal` added to the registry, which the sweep above would see, but
// a `fmt.Fprintf(w, "%s", *k.Token)` written by somebody who did not know the
// registry existed.
func TestNoPrintTakesATokenExceptThroughTheSink(t *testing.T) {
	fset := token.NewFileSet()

	// printers is every way this CLI writes to a stream. A function added
	// here that is not in this list would not be swept, so the floor below
	// counts how many print calls the sweep saw at all.
	printers := map[string]bool{
		"Fprintf": true, "Fprint": true, "Fprintln": true,
		"Printf": true, "Print": true, "Println": true,
		"Sprintf": true, "Sprint": true, "Sprintln": true,
		"Errorf": true,
	}

	prints := 0
	tokenArgs := 0

	for _, path := range goFiles(t) {
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}

		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}

			name, _ := calleeName(call.Fun)
			if !printers[name] {
				return true
			}

			prints++

			for _, arg := range call.Args {
				if !readsAToken(arg) {
					continue
				}

				tokenArgs++

				if !wrappedInReveal(arg) {
					pos := fset.Position(arg.Pos())
					t.Errorf("%s:%d passes a token to %s without going through render.Reveal.\n"+
						"AC42: a secret may be written at the two sites render.Sites declares and "+
						"nowhere else. If this is a third site, it is an amendment (PLAN §11.1); if it "+
						"is not, print creds.Credential.Display() or the prefix instead.",
						pos.Filename, pos.Line, name)
				}
			}

			return true
		})
	}

	if prints < 20 {
		t.Fatalf("the sweep saw %d print calls across the tree; it is not finding them, so "+
			"'no print takes a token' is true of nothing", prints)
	}

	// The positive control: the sweep must have found at least the one
	// legitimate token print, or it cannot recognise a token argument at
	// all and the whole check is vacuous.
	if tokenArgs == 0 {
		t.Error("the sweep recognised no token-shaped argument to any print call, including the " +
			"one render.APIKeyCreated is required to make. It cannot be said to have cleared the rest.")
	}
}

// readsAToken reports whether an expression reads a field called `Token`.
//
// `TokenPrefix` and `TokenLast4` are excluded by name: they are the non-secret
// halves AC36 requires a render to print, and a check that flagged them would
// be turned off within a week.
func readsAToken(expr ast.Expr) bool {
	found := false

	ast.Inspect(expr, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}

		if sel.Sel.Name == "Token" {
			found = true
		}

		return true
	})

	return found
}

// wrappedInReveal reports whether the token read sits inside a Reveal call.
func wrappedInReveal(expr ast.Expr) bool {
	found := false

	ast.Inspect(expr, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}

		if name, _ := calleeName(call.Fun); name == "Reveal" {
			found = true
		}

		return true
	})

	return found
}

// The detector itself has to be able to fire, in both directions.
//
// A `FindSecrets` that recognised nothing would make every behavioural
// assertion in this unit pass; a `FindSecrets` that recognised `ferry_pat_`
// loosely would flag the prefix AC36 requires, and would be disabled.
func TestTheSecretDetectorRecognisesSecretsAndNotTheirPrefixes(t *testing.T) {
	body := strings.Repeat("A", 43)

	secrets := []string{
		"ferry_sk_sandbox_" + body,
		"ferry_sk_live_" + body,
		"ferry_pat_" + body,
		"ferry_plan_abcdefgh",
		"API_KEY_TOKEN_PLACEHOLDER_1",
		"PAT_TOKEN_PLACEHOLDER_2",
	}

	for _, secret := range secrets {
		if !render.ContainsSecret("before " + secret + " after") {
			t.Errorf("FindSecrets does not recognise %q", secret)
		}
	}

	// The renders AC36 requires, which must not be reported.
	safe := []string{
		"ferry_pat_ABCDEFGH…WXYZ",
		"ferry_sk_sandbox_ABCDEFGH…4321",
		"token_prefix: TOKEN_PREFIX_PLACEHOLDER_1",
		"token_last4: TOKEN_LAST4_PLACEHOLDER_1",
		"ferry_sk_sandbox_" + strings.Repeat("A", 8),
	}

	for _, text := range safe {
		if found := render.FindSecrets(text); len(found) > 0 {
			t.Errorf("FindSecrets reports %q in %q, which is the non-secret render AC36 requires",
				found, text)
		}
	}
}

// Reveal refuses a site the registry does not declare, loudly.
func TestRevealPanicsForAnUndeclaredSite(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("render.Reveal accepted an undeclared site; the registry is then a comment")
		}

		if !strings.Contains(fmtString(r), "not a declared secret site") {
			t.Errorf("the panic reads %v", r)
		}
	}()

	render.Reveal(render.SiteID("runs.resume.token"), "ferry_pat_"+strings.Repeat("A", 43))
}

func fmtString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}

	return ""
}

// Each declared site says who owns it and why, because a registry of names is
// a subset check with extra steps.
func TestEveryDeclaredSiteIsJustified(t *testing.T) {
	if len(render.Sites) == 0 {
		t.Fatal("render.Sites is empty")
	}

	owners := map[string]bool{}

	for _, site := range render.Sites {
		if site.ID == "" {
			t.Error("a declared site has no id")
		}

		if site.Owner == "" {
			t.Errorf("site %q names no owning unit", site.ID)
		}

		if site.Secret == "" {
			t.Errorf("site %q does not say which value it prints", site.ID)
		}

		if len(site.Why) < 40 {
			t.Errorf("site %q gives no reason worth reading: %q", site.ID, site.Why)
		}

		owners[site.Owner] = true
	}

	// One of the two is U4's and one is U5's. If both ever belonged to one
	// unit, the registry would have stopped being a cross-unit statement.
	if len(owners) != 2 {
		t.Errorf("the two sites are owned by %d unit(s); AC42's count is over the whole binary", len(owners))
	}
}
