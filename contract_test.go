package cli_test

// The vendored contract and its provenance.
//
// A400 split this CLI out of the Rails repository that generates the API it
// speaks to. The pins in internal/api (AC85, AC86) and internal/outcome read
// `contract/openapi.yaml` and `contract/errors.md`, which before the split
// were the API repository's own files, read across the directory boundary.
//
// Vendoring moved them under this repository's control, and that is the
// hazard: a pin that reads a copy you can edit is a pin against yourself. If
// a Go struct and the contract disagree, editing the vendored YAML turns the
// test green while the real API goes on disagreeing. The check below does not
// prevent that edit — nothing here can — but it makes the edit visible, by
// holding both files to the digests `contract/SOURCE` records alongside the
// upstream commit they came from.
//
// What this does not check is the other direction: upstream changing while
// the vendored copy stands still. That needs the API repository's CI to look
// at this one, which needs a cross-repository token. Until that exists,
// re-vendoring is manual and `contract/SOURCE` is the record of when it last
// happened.

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"testing"
)

// digestLine matches the "name  sha256" rows at the foot of contract/SOURCE.
var digestLine = regexp.MustCompile(`(?m)^(\S+\.(?:yaml|md))\s+([0-9a-f]{64})$`)

// minVendoredFiles is the floor. Every assertion below is over a set parsed
// out of SOURCE, and a parse that matched nothing would satisfy "every
// vendored file matches its digest" vacuously.
const minVendoredFiles = 2

func TestTheVendoredContractMatchesItsRecordedDigest(t *testing.T) {
	source, err := os.ReadFile("contract/SOURCE")
	if err != nil {
		t.Fatalf("reading contract/SOURCE: %v", err)
	}

	rows := digestLine.FindAllStringSubmatch(string(source), -1)
	if len(rows) < minVendoredFiles {
		t.Fatalf("contract/SOURCE names %d vendored files; expected at least %d. "+
			"Either the file was restructured or the reader has stopped matching it, "+
			"and in both cases this check has gone quiet rather than passing",
			len(rows), minVendoredFiles)
	}

	for _, row := range rows {
		name, want := row[1], row[2]

		b, err := os.ReadFile("contract/" + name)
		if err != nil {
			t.Errorf("contract/SOURCE names %s, which is not there: %v", name, err)

			continue
		}

		sum := sha256.Sum256(b)
		if got := hex.EncodeToString(sum[:]); got != want {
			t.Errorf("contract/%s does not match the digest contract/SOURCE records.\n"+
				"  recorded %s\n  actual   %s\n\n"+
				"If you re-vendored from the API repository, update contract/SOURCE with the "+
				"new commit and digests in the same change. If you edited the file by hand, "+
				"do not: the pins that read it are only worth anything while it is a faithful "+
				"copy of what the API actually serves.",
				name, want, got)
		}
	}
}

// TestEveryVendoredFileIsAccountedFor is the other direction. The check above
// walks SOURCE and looks for files; this walks the files and looks for rows,
// so a copy added to contract/ without a digest is caught rather than
// travelling unpinned.
func TestEveryVendoredFileIsAccountedFor(t *testing.T) {
	entries, err := os.ReadDir("contract")
	if err != nil {
		t.Fatalf("reading contract/: %v", err)
	}

	source, err := os.ReadFile("contract/SOURCE")
	if err != nil {
		t.Fatalf("reading contract/SOURCE: %v", err)
	}

	recorded := map[string]bool{}
	for _, row := range digestLine.FindAllStringSubmatch(string(source), -1) {
		recorded[row[1]] = true
	}

	found := 0

	for _, e := range entries {
		if e.IsDir() || e.Name() == "SOURCE" {
			continue
		}

		found++

		if !recorded[e.Name()] {
			t.Errorf("contract/%s is vendored but contract/SOURCE records no digest for it, "+
				"so nothing would notice it being edited", e.Name())
		}
	}

	if found < minVendoredFiles {
		t.Fatalf("contract/ holds %d vendored files; expected at least %d", found, minVendoredFiles)
	}
}

// The recordings are vendored too, and they are the larger copy.
//
// `contract/SOURCE` pinned only the two contract files, which left the 74
// files under `testdata/recorded/` — every byte the fixture server answers
// with — held to nothing. A re-vendor that copied half the directory, or a
// scenario edited by hand to make a failing test pass, would have been
// invisible here: the fixture would simply agree with itself.
//
// The row is a digest over the directory rather than one per file, because
// per-file rows would need a row added by hand for every new scenario and
// the failure of that is a scenario travelling unpinned.
var recordedDigestLine = regexp.MustCompile(`(?m)^testdata/recorded\s+([0-9a-f]{64})$`)

// minRecordedFiles is this check's floor: the digest of an empty directory is
// a perfectly good sha256, so without a count "the recordings match" passes
// over a directory that was deleted.
const minRecordedFiles = 70

func TestTheVendoredRecordingsMatchTheirRecordedDigest(t *testing.T) {
	source, err := os.ReadFile("contract/SOURCE")
	if err != nil {
		t.Fatalf("reading contract/SOURCE: %v", err)
	}

	row := recordedDigestLine.FindStringSubmatch(string(source))
	if row == nil {
		t.Fatalf("contract/SOURCE records no digest for testdata/recorded. It is the larger " +
			"half of what this repository vendors, and unpinned it can be edited to agree " +
			"with whatever the CLI does today.")
	}

	got, files := recordedDigest(t)

	if files < minRecordedFiles {
		t.Fatalf("testdata/recorded holds %d files; expected at least %d", files, minRecordedFiles)
	}

	if got != row[1] {
		t.Errorf("testdata/recorded does not match the digest contract/SOURCE records.\n"+
			"  recorded %s\n  actual   %s\n\n"+
			"If you re-vendored from the API repository, update contract/SOURCE with the new "+
			"commit and digest in the same change. If you edited a scenario by hand, do not: "+
			"the fixture is worth something only while it is what the API actually answered.\n\n"+
			"Reproduce with:\n"+
			"  cd testdata/recorded && find . -type f | sed 's|^\\./||' | LC_ALL=C sort \\\n"+
			"    | xargs sha256sum | sha256sum",
			row[1], got)
	}
}

// recordedDigest hashes the directory: each file's digest against its path
// relative to the directory root, sorted by path, then hashed.
//
// Paths are relative to `testdata/recorded` rather than to this repository,
// so the number describes the recordings and not where they happen to sit.
// The API repository's `contract/CONSUMERS.yml` hashes the same bytes under
// its own `contract/recorded/` paths, so its digest for this directory is a
// different value — the two are each a tripwire within one repository and are
// not comparable across the boundary.
func recordedDigest(t *testing.T) (digest string, files int) {
	t.Helper()

	const root = "testdata/recorded"

	var paths []string

	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if entry.IsDir() {
			return nil
		}

		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}

		paths = append(paths, rel)

		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}

	// Byte order, which is what `LC_ALL=C sort` gives the shell: a
	// locale-aware sort orders these differently and produces a different
	// digest for the same bytes.
	sort.Strings(paths)

	whole := sha256.New()

	for _, rel := range paths {
		b, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("reading %s: %v", rel, err)
		}

		sum := sha256.Sum256(b)
		fmt.Fprintf(whole, "%s  %s\n", hex.EncodeToString(sum[:]), rel)
	}

	return hex.EncodeToString(whole.Sum(nil)), len(paths)
}
