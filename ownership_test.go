// Package cli_test holds the ownership lint and nothing else. There is no
// non-test Go file in this directory: cli/ is the module root, and the
// commands live under cmd/ and internal/.
package cli_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// AC84: every file under cli/ matches exactly one ownership prefix in
// cli/OWNERSHIP, and every prefix names a unit in PLAN §4.1.
//
// This runs in every wave's `go test ./...`, which is the point: a unit that
// creates a file §4.1 gave to nobody fails its own wave rather than being
// found by the audit five waves later. Mutation M93 adds such a file.

const (
	ownershipFile = "OWNERSHIP"
	planFile      = "../docs/loops/cli/PLAN.md"

	// Floors. Every check below is over a collection gathered by walking or
	// parsing, and a walk that found nothing or a parse that matched nothing
	// would satisfy "every X is owned" vacuously. These are the assertions
	// that the collections are not empty in the first place.
	minFilesWalked = 20
	minUnitsInPlan = 8
)

type ownership struct {
	Units map[string]struct {
		Description string   `yaml:"description"`
		Paths       []string `yaml:"paths"`
	} `yaml:"units"`
	Ignored []struct {
		Path   string `yaml:"path"`
		Reason string `yaml:"reason"`
	} `yaml:"ignored"`
}

func loadOwnership(t *testing.T) ownership {
	t.Helper()
	b, err := os.ReadFile(ownershipFile)
	if err != nil {
		t.Fatalf("read %s: %v", ownershipFile, err)
	}
	var o ownership
	if err := yaml.Unmarshal(b, &o); err != nil {
		t.Fatalf("parse %s: %v", ownershipFile, err)
	}
	if len(o.Units) == 0 {
		t.Fatalf("%s declares no units", ownershipFile)
	}
	return o
}

// walkFiles lists every file under cli/, from the filesystem rather than from
// `git ls-files`.
//
// This is deliberate. A new file that has not been staged is invisible to git
// and would sail through the lint — which is exactly the file M93 adds, and
// exactly the shape of mistake a unit makes. The cost is that build output
// has to be named in OWNERSHIP's ignored list rather than inherited from
// .gitignore, which is a cost worth paying: an ignored path then carries a
// written reason.
func walkFiles(t *testing.T) []string {
	t.Helper()
	var files []string
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return fs.SkipDir
			}
			return nil
		}
		files = append(files, filepath.ToSlash(path))
		return nil
	})
	if err != nil {
		t.Fatalf("walk cli/: %v", err)
	}
	if len(files) < minFilesWalked {
		t.Fatalf("the walk found only %d files under cli/ (%v); every check below would be "+
			"vacuous, so the walk itself is broken", len(files), files)
	}
	return files
}

// unitsInPlan parses the unit headings of PLAN §4.1.
//
// The list is read from the plan rather than written here on purpose: a
// hardcoded copy would be this test asserting against its own expectations,
// and would keep passing after §4.1 changed. If the plan moves, this test
// fails loudly rather than skipping — it is the authority, and a lint with no
// authority to check against is not a lint.
func unitsInPlan(t *testing.T) map[string]bool {
	t.Helper()
	b, err := os.ReadFile(planFile)
	if err != nil {
		t.Fatalf("read %s: %v (AC84 checks OWNERSHIP against PLAN §4.1; without the plan "+
			"there is nothing to check against)", planFile, err)
	}
	text := string(b)

	start := strings.Index(text, "### 4.1 Units")
	if start < 0 {
		t.Fatalf("%s has no '### 4.1 Units' heading", planFile)
	}
	end := strings.Index(text[start:], "### 4.2")
	if end < 0 {
		t.Fatalf("%s has no '### 4.2' heading after §4.1", planFile)
	}
	section := text[start : start+end]

	heading := regexp.MustCompile(`(?m)^\*\*(U\d+[a-z]?) —`)
	units := map[string]bool{}
	for _, m := range heading.FindAllStringSubmatch(section, -1) {
		units[m[1]] = true
	}
	if len(units) < minUnitsInPlan {
		t.Fatalf("parsed only %d units from PLAN §4.1 (%v); the parse is broken, so every "+
			"unit name would be rejected or accepted for the wrong reason", len(units), keys(units))
	}
	return units
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// owns reports whether prefix owns path. A trailing slash owns a subtree;
// anything else is an exact file.
func owns(prefix, path string) bool {
	if strings.HasSuffix(prefix, "/") {
		return strings.HasPrefix(path, prefix)
	}
	return path == prefix
}

type owner struct {
	unit   string
	prefix string
}

func describe(owners []owner) string {
	parts := make([]string, 0, len(owners))
	for _, o := range owners {
		parts = append(parts, o.unit+" via "+o.prefix)
	}
	sort.Strings(parts)
	return strings.Join(parts, " and ")
}

func TestEveryFileUnderCLIHasExactlyOneOwner(t *testing.T) {
	o := loadOwnership(t)
	files := walkFiles(t)

	var unowned, ambiguous []string
	for _, f := range files {
		var owners []owner
		for unit, u := range o.Units {
			for _, p := range u.Paths {
				if owns(p, f) {
					owners = append(owners, owner{unit, p})
				}
			}
		}
		for _, ig := range o.Ignored {
			if owns(ig.Path, f) {
				owners = append(owners, owner{"(ignored)", ig.Path})
			}
		}

		switch len(owners) {
		case 0:
			unowned = append(unowned, f)
		case 1:
		default:
			ambiguous = append(ambiguous, f+" claimed by "+describe(owners))
		}
	}

	if len(unowned) > 0 {
		sort.Strings(unowned)
		t.Errorf("these files under cli/ match no prefix in %s:\n  %s\n\n"+
			"PLAN §4.1 gives every file to exactly one unit. Either the file belongs to a "+
			"prefix your unit already owns, or creating it is an amendment (§11.1) — not a "+
			"new line in %s.", ownershipFile, strings.Join(unowned, "\n  "), ownershipFile)
	}
	if len(ambiguous) > 0 {
		sort.Strings(ambiguous)
		t.Errorf("these files match more than one prefix in %s:\n  %s\n\n"+
			"Two owners coordinate no better than none.", ownershipFile, strings.Join(ambiguous, "\n  "))
	}
}

func TestEveryPrefixNamesAUnitInThePlan(t *testing.T) {
	o := loadOwnership(t)
	planUnits := unitsInPlan(t)

	for unit := range o.Units {
		// §4.1 splits U2 into U2a and U2b and both appear as headings; the
		// ownership map may name either the split or the whole.
		if planUnits[unit] || planUnits[unit+"a"] {
			continue
		}
		t.Errorf("%s names unit %q, which is not a unit in PLAN §4.1 (units there: %v)",
			ownershipFile, unit, keys(planUnits))
	}
}

// A unit of §4.1 with files under cli/ but no prefix here would leave those
// files to fail TestEveryFileUnderCLIHasExactlyOneOwner with no indication of
// whose they are. U8 owns nothing under cli/ and declares so explicitly.
func TestEveryPlanUnitWithCLIFilesIsRepresented(t *testing.T) {
	o := loadOwnership(t)
	planUnits := unitsInPlan(t)

	for unit := range planUnits {
		// U2a and U2b are the wave split of U2's files.
		base := strings.TrimRight(unit, "ab")
		if _, ok := o.Units[unit]; ok {
			continue
		}
		if _, ok := o.Units[base]; ok {
			continue
		}
		t.Errorf("PLAN §4.1 has unit %q but %s has no entry for it (nor for %q)",
			unit, ownershipFile, base)
	}
}

func TestPrefixesDoNotOverlap(t *testing.T) {
	o := loadOwnership(t)

	type entry struct {
		unit, path string
	}
	var all []entry
	for unit, u := range o.Units {
		for _, p := range u.Paths {
			all = append(all, entry{unit, p})
		}
	}
	for _, ig := range o.Ignored {
		all = append(all, entry{"(ignored)", ig.Path})
	}
	if len(all) < 10 {
		t.Fatalf("only %d prefixes declared; the overlap check would be vacuous", len(all))
	}

	for i, a := range all {
		for j, b := range all {
			if i == j {
				continue
			}
			// a contains b if a is a subtree prefix of b's path.
			if strings.HasSuffix(a.path, "/") && strings.HasPrefix(b.path, a.path) {
				t.Errorf("%s (%s) is inside %s (%s); a file under it would have two owners",
					b.path, b.unit, a.path, a.unit)
			}
		}
	}
}

func TestEveryIgnoredPathCarriesAReason(t *testing.T) {
	o := loadOwnership(t)
	for _, ig := range o.Ignored {
		if strings.TrimSpace(ig.Path) == "" {
			t.Errorf("an ignored entry has no path")
		}
		if len(strings.TrimSpace(ig.Reason)) < 20 {
			t.Errorf("ignored path %q has no usable reason (%q); an ignore list without "+
				"reasons is how a file quietly stops being owned", ig.Path, ig.Reason)
		}
	}
}

// The lint must fail on an unowned file. Without this, a bug in owns() or in
// the walk would make the lint pass for everything, and AC84 would be a test
// that cannot fail.
func TestTheLintRejectsAnUnownedPath(t *testing.T) {
	o := loadOwnership(t)

	unowned := "internal/somebodyelses/thing.go"
	for unit, u := range o.Units {
		for _, p := range u.Paths {
			if owns(p, unowned) {
				t.Fatalf("the fixture path %q is owned by %s via %q, so it cannot stand in "+
					"for an unowned file", unowned, unit, p)
			}
		}
	}
	for _, ig := range o.Ignored {
		if owns(ig.Path, unowned) {
			t.Fatalf("the fixture path %q is ignored via %q", unowned, ig.Path)
		}
	}
}

// And the matcher itself, since every check above rests on it.
func TestOwnsMatchesSubtreesAndExactFilesOnly(t *testing.T) {
	cases := []struct {
		prefix, path string
		want         bool
	}{
		{"internal/runs/", "internal/runs/ledger.go", true},
		{"internal/runs/", "internal/runs/sub/x.go", true},
		{"internal/runs/", "internal/runs", false},
		// The case that matters: U1's internal/runs/ must not swallow U5's
		// internal/noun/runs/.
		{"internal/runs/", "internal/noun/runs/show.go", false},
		{"internal/runs/", "internal/runsomething/x.go", false},
		{"go.mod", "go.mod", true},
		{"go.mod", "go.mod.bak", false},
		{"internal/noun/precheck.go", "internal/noun/precheck.go", true},
		{"internal/noun/precheck.go", "internal/noun/precheck_test.go", false},
	}
	for _, tc := range cases {
		if got := owns(tc.prefix, tc.path); got != tc.want {
			t.Errorf("owns(%q, %q) = %v, want %v", tc.prefix, tc.path, got, tc.want)
		}
	}
}
