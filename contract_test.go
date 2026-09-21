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
	"os"
	"regexp"
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
