package outcome_test

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The catalogue readers.
//
// Both read a document generated from, or pinned to, the Ruby side:
// `docs/api/errors.md` is written by `bin/rails ferry:docs:generate` from
// `Ferry::Api::Errors::CATALOG` and `spec/docs/api_docs_spec.rb` fails if they
// disagree; `docs/api/openapi.yaml` is hand-authored and pinned to
// `config/routes.rb` by the same spec. So these are independent authorities in
// the sense that matters: nothing in this Go package can edit either of them,
// and neither is derived from the table under test.
//
// Both readers **raise** on a line they cannot read rather than skipping it. A
// reader that skips the unfamiliar reports a smaller vocabulary and a green
// example, which is the failure the parse was adopted to avoid; and both
// callers assert a floor on what was found, so a selector that stops matching
// fails instead of passing vacuously.

type catalogueEntry struct {
	code      string
	status    int
	retriable bool
}

var (
	// | `TOKEN_MISSING` | 401 | no | Authenticate with … |
	catalogueRowRE = regexp.MustCompile(`^\|\s*(.+?)\s*\|\s*(.+?)\s*\|\s*(.+?)\s*\|\s*(.*?)\s*\|$`)
	backtickedRE   = regexp.MustCompile("^`([A-Z][A-Z0-9_]*)`$")
	bulletCodeRE   = regexp.MustCompile("^- `([A-Z][A-Z0-9_]+)`")
	codeTokenRE    = regexp.MustCompile(`\b[A-Z][A-Z0-9_]{3,}\b`)
)

func repoRoot(t *testing.T) string {
	t.Helper()

	// This file lives at cli/internal/outcome/.
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatalf("resolving the repository root: %v", err)
	}

	return root
}

func readFile(t *testing.T, rel string) string {
	t.Helper()

	b, err := os.ReadFile(filepath.Join(repoRoot(t), rel))
	if err != nil {
		t.Fatalf("reading %s: %v", rel, err)
	}

	return string(b)
}

// readCatalogue parses the code table out of `docs/api/errors.md`.
func readCatalogue(t *testing.T) map[string]catalogueEntry {
	t.Helper()

	entries := map[string]catalogueEntry{}
	inTable := false

	for i, line := range strings.Split(readFile(t, "docs/api/errors.md"), "\n") {
		switch {
		case strings.HasPrefix(line, "| Code | HTTP |"):
			inTable = true

			continue
		case inTable && strings.HasPrefix(line, "|---"):
			continue
		case inTable && !strings.HasPrefix(line, "|"):
			inTable = false

			continue
		case !inTable:
			continue
		}

		m := catalogueRowRE.FindStringSubmatch(line)
		if m == nil {
			t.Fatalf("errors.md:%d is inside the code table and this reader cannot parse it: %q", i+1, line)
		}

		code := backtickedRE.FindStringSubmatch(m[1])
		if code == nil {
			t.Fatalf("errors.md:%d has a Code cell this reader cannot parse: %q", i+1, m[1])
		}

		status, err := strconv.Atoi(m[2])
		if err != nil {
			t.Fatalf("errors.md:%d has an HTTP cell this reader cannot parse: %q", i+1, m[2])
		}

		var retriable bool

		switch m[3] {
		case "yes":
			retriable = true
		case "no":
			retriable = false
		default:
			t.Fatalf("errors.md:%d has a Retry-identical cell this reader cannot parse: %q", i+1, m[3])
		}

		if _, dup := entries[code[1]]; dup {
			t.Fatalf("errors.md:%d repeats the code %s", i+1, code[1])
		}

		entries[code[1]] = catalogueEntry{code: code[1], status: status, retriable: retriable}
	}

	// A floor, so a heading rename that silently empties the table fails
	// here rather than making every coverage example vacuously green.
	if len(entries) < 50 {
		t.Fatalf("read only %d codes out of errors.md; the reader has stopped matching", len(entries))
	}

	return entries
}

// readNeverEmitted parses the "Codes you will not see" section. It covers both
// the bullet list and the "one further code" paragraph below it, because both
// sit under that heading and both say the same thing: no branch may depend on
// the code arriving.
func readNeverEmitted(t *testing.T) []string {
	t.Helper()

	var (
		codes []string
		in    bool
	)

	for _, line := range strings.Split(readFile(t, "docs/api/errors.md"), "\n") {
		if strings.HasPrefix(line, "## ") {
			in = strings.TrimSpace(line) == "## Codes you will not see"

			continue
		}

		if !in {
			continue
		}

		if m := bulletCodeRE.FindStringSubmatch(line); m != nil {
			codes = append(codes, m[1])
		}
	}

	if len(codes) == 0 {
		t.Fatal(`read no codes out of errors.md's "Codes you will not see" section; the reader has stopped matching`)
	}

	return codes
}

// readWebhookOnlyCodes returns the catalogue codes `openapi.yaml` names inside
// the `POST /oms/webhooks/{locator}` operation and nowhere else.
//
// Derived from the document rather than from the `WEBHOOK_` prefix: a prefix
// is a naming convention, and a convention is not an authority. `RATE_LIMITED`
// is the row that proves the difference — it is named in the webhook
// operation *and* in the shared `RateLimited` response, so it is not
// webhook-only.
func readWebhookOnlyCodes(t *testing.T, catalogue map[string]catalogueEntry) []string {
	t.Helper()

	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(readFile(t, "docs/api/openapi.yaml")), &doc); err != nil {
		t.Fatalf("parsing openapi.yaml: %v", err)
	}

	paths := mappingValue(t, root(t, &doc), "paths")
	webhook := mappingValue(t, paths, "/oms/webhooks/{locator}")

	inside := codesIn(t, webhook, catalogue)

	deleteKey(t, paths, "/oms/webhooks/{locator}")

	outside := codesIn(t, &doc, catalogue)

	var only []string

	for code := range inside {
		if !outside[code] {
			only = append(only, code)
		}
	}

	if len(only) == 0 {
		t.Fatal("found no webhook-only codes in openapi.yaml; the reader has stopped matching")
	}

	return only
}

func codesIn(t *testing.T, node *yaml.Node, catalogue map[string]catalogueEntry) map[string]bool {
	t.Helper()

	b, err := yaml.Marshal(node)
	if err != nil {
		t.Fatalf("re-marshalling an openapi.yaml subtree: %v", err)
	}

	found := map[string]bool{}

	for _, token := range codeTokenRE.FindAllString(string(b), -1) {
		if _, ok := catalogue[token]; ok {
			found[token] = true
		}
	}

	return found
}

func root(t *testing.T, doc *yaml.Node) *yaml.Node {
	t.Helper()

	if doc.Kind != yaml.DocumentNode || len(doc.Content) != 1 {
		t.Fatalf("openapi.yaml is not a single YAML document")
	}

	return doc.Content[0]
}

func mappingValue(t *testing.T, node *yaml.Node, key string) *yaml.Node {
	t.Helper()

	if node.Kind != yaml.MappingNode {
		t.Fatalf("looking for %q in a %v, which is not a mapping", key, node.Kind)
	}

	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return node.Content[i+1]
		}
	}

	t.Fatalf("openapi.yaml has no %q key where this reader expects one", key)

	return nil
}

func deleteKey(t *testing.T, node *yaml.Node, key string) {
	t.Helper()

	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			node.Content = append(node.Content[:i], node.Content[i+2:]...)

			return
		}
	}

	t.Fatalf("openapi.yaml has no %q key to delete", key)
}

// diff reports what each side has that the other does not. Set equality is
// asserted from it in both directions; a one-directional check is the defect
// this project keeps finding.
func diff(a, b []string) (onlyA, onlyB []string) {
	inB := map[string]bool{}
	for _, s := range b {
		inB[s] = true
	}

	inA := map[string]bool{}
	for _, s := range a {
		inA[s] = true
	}

	for s := range inA {
		if !inB[s] {
			onlyA = append(onlyA, s)
		}
	}

	for s := range inB {
		if !inA[s] {
			onlyB = append(onlyB, s)
		}
	}

	sort.Strings(onlyA)
	sort.Strings(onlyB)

	return onlyA, onlyB
}

func assertSetsEqual(t *testing.T, what string, got, want []string, gotLabel, wantLabel string) {
	t.Helper()

	onlyGot, onlyWant := diff(got, want)

	if len(onlyGot) > 0 {
		t.Errorf("%s: %s has %d entr%s %s does not: %s",
			what, gotLabel, len(onlyGot), plural(len(onlyGot)), wantLabel, strings.Join(onlyGot, ", "))
	}

	if len(onlyWant) > 0 {
		t.Errorf("%s: %s has %d entr%s %s does not: %s",
			what, wantLabel, len(onlyWant), plural(len(onlyWant)), gotLabel, strings.Join(onlyWant, ", "))
	}
}

func plural(n int) string {
	if n == 1 {
		return "y"
	}

	return "ies"
}

func keys(m map[string]catalogueEntry) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}

	sort.Strings(out)

	return out
}
