package fixture

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
)

// ManifestName is the manifest's file name inside a recordings directory.
const ManifestName = "MANIFEST.json"

// ErrRecordings is every way a recordings directory can be refused. The
// loader reports all of them at once; one round of fixing per run is enough.
var ErrRecordings = errors.New("fixture: recordings refused")

// Set is a loaded, checked recordings directory.
type Set struct {
	Dir      string
	Manifest Manifest

	// Order is the manifest's scenario order, and Recordings the files by
	// scenario name. Both are read-only once Load returns: New hands the
	// same *Set to every server, and the per-server cursor lives on the
	// server.
	Order      []string
	Recordings map[string]*Recording

	// Checks is what this load enforced, counted at each comparison.
	Checks Checks
}

// Scenarios returns the scenario names, in manifest order.
func (s *Set) Scenarios() []string {
	out := make([]string, len(s.Order))
	copy(out, s.Order)

	return out
}

// Load reads a recordings directory and refuses it unless every one of these
// holds (AC29, AC77):
//
//   - the manifest and the directory name exactly the same scenarios — both
//     directions, because a one-directional subset check is green while
//     recordings rot, and this project has shipped that defect more than once;
//   - each file's sha256 is the digest the manifest records;
//   - each file's own `expect` equals the manifest's for that scenario;
//   - `expect` has one entry per interaction (A301);
//   - every expectation holds against the response recorded beneath it — on
//     every axis, including `body_equals` (A300).
//
// A directory that names nothing at all is refused too. "Both sets are empty"
// is the way a comparison stops being a comparison.
func Load(dir string) (*Set, error) {
	manifestPath := filepath.Join(dir, ManifestName)

	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil, fmt.Errorf("%w: reading %s: %v", ErrRecordings, manifestPath, err)
	}

	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()

	var manifest Manifest
	if err := decoder.Decode(&manifest); err != nil {
		return nil, fmt.Errorf("%w: decoding %s: %v", ErrRecordings, manifestPath, err)
	}

	set := &Set{
		Dir:        dir,
		Manifest:   manifest,
		Recordings: map[string]*Recording{},
	}

	var problems []string

	onDisk, listProblems, err := scenarioFiles(dir)
	if err != nil {
		return nil, err
	}

	problems = append(problems, listProblems...)

	named, namingProblems := manifestScenarios(manifest)
	problems = append(problems, namingProblems...)

	problems = append(problems, compareSets(named, onDisk)...)
	problems = append(problems, checkUnreachable(manifest, named, onDisk)...)

	for _, entry := range manifest.Scenarios {
		if !onDisk[entry.Scenario] {
			continue
		}

		recording, entryProblems := set.loadOne(dir, entry)
		problems = append(problems, entryProblems...)

		if recording != nil {
			set.Order = append(set.Order, entry.Scenario)
			set.Recordings[entry.Scenario] = recording
		}
	}

	if len(problems) > 0 {
		return nil, fmt.Errorf("%w: %s:\n  %s", ErrRecordings, dir, strings.Join(problems, "\n  "))
	}

	return set, nil
}

// scenarioFiles is the directory's half of AC29's equality, derived from the
// filesystem and from nothing the manifest says.
func scenarioFiles(dir string) (map[string]bool, []string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: reading %s: %v", ErrRecordings, dir, err)
	}

	found := map[string]bool{}

	var problems []string

	for _, entry := range entries {
		name := entry.Name()

		if entry.IsDir() {
			problems = append(problems, fmt.Sprintf("%s/ is a directory; recordings are one flat file per scenario", name))
			continue
		}

		if name == ManifestName {
			continue
		}

		if !strings.HasSuffix(name, ".json") {
			problems = append(problems, fmt.Sprintf("%s is not a recording and is not the manifest", name))
			continue
		}

		found[strings.TrimSuffix(name, ".json")] = true
	}

	if len(found) == 0 {
		problems = append(problems, "no recordings on disk; an empty directory compares equal to an empty manifest and asserts nothing")
	}

	return found, problems, nil
}

func manifestScenarios(manifest Manifest) (map[string]bool, []string) {
	named := map[string]bool{}

	var problems []string

	if len(manifest.Scenarios) == 0 {
		problems = append(problems, "the manifest names no scenarios")
	}

	for _, entry := range manifest.Scenarios {
		switch {
		case entry.Scenario == "":
			problems = append(problems, "a manifest entry has no scenario name")
		case named[entry.Scenario]:
			problems = append(problems, fmt.Sprintf("%s is named twice in the manifest", entry.Scenario))
		default:
			named[entry.Scenario] = true
		}

		if entry.SHA256 == "" {
			problems = append(problems, fmt.Sprintf("%s has no sha256 in the manifest", entry.Scenario))
		}

		if len(entry.Expect) == 0 {
			problems = append(problems, fmt.Sprintf("%s declares no expectation (AC77)", entry.Scenario))
		}
	}

	return named, problems
}

// compareSets is the equality itself, stated in both directions because only
// one of them can be read off either side alone.
func compareSets(named, onDisk map[string]bool) []string {
	var problems []string

	for _, scenario := range sortedKeys(onDisk) {
		if !named[scenario] {
			problems = append(problems, fmt.Sprintf(
				"%s.json is on disk and the manifest does not name it", scenario))
		}
	}

	for _, scenario := range sortedKeys(named) {
		if !onDisk[scenario] {
			problems = append(problems, fmt.Sprintf(
				"the manifest names %s and there is no %s.json", scenario, scenario))
		}
	}

	return problems
}

// checkUnreachable holds the manifest's second list to what it claims: a
// scenario declared unreachable has a reason, is not also a recorded scenario,
// and has no file.
func checkUnreachable(manifest Manifest, named, onDisk map[string]bool) []string {
	var problems []string

	seen := map[string]bool{}

	for _, entry := range manifest.Unreachable {
		switch {
		case entry.Scenario == "":
			problems = append(problems, "an unreachable entry has no scenario name")
		case seen[entry.Scenario]:
			problems = append(problems, fmt.Sprintf("%s is listed twice as unreachable", entry.Scenario))
		}

		seen[entry.Scenario] = true

		if strings.TrimSpace(entry.Reason) == "" {
			problems = append(problems, fmt.Sprintf(
				"%s is declared unreachable with no reason; an exclusion list without reasons is a subset check with extra steps",
				entry.Scenario))
		}

		if named[entry.Scenario] {
			problems = append(problems, fmt.Sprintf("%s is both a recorded scenario and unreachable", entry.Scenario))
		}

		if onDisk[entry.Scenario] {
			problems = append(problems, fmt.Sprintf("%s is declared unreachable and %s.json exists", entry.Scenario, entry.Scenario))
		}
	}

	return problems
}

func (s *Set) loadOne(dir string, entry ManifestEntry) (*Recording, []string) {
	path := filepath.Join(dir, entry.Scenario+".json")

	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, []string{fmt.Sprintf("%s: %v", entry.Scenario, err)}
	}

	s.Checks.Files++
	s.Checks.Digests++

	if digest := hex.EncodeToString(sha256Of(raw)); digest != entry.SHA256 {
		return nil, []string{fmt.Sprintf(
			"%s: sha256 is %s, the manifest records %s", entry.Scenario, digest, entry.SHA256)}
	}

	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()

	var recording Recording
	if err := decoder.Decode(&recording); err != nil {
		return nil, []string{fmt.Sprintf("%s: %v", entry.Scenario, err)}
	}

	var problems []string

	if recording.Scenario != entry.Scenario {
		problems = append(problems, fmt.Sprintf(
			"%s: the file calls itself %q", entry.Scenario, recording.Scenario))
	}

	if !reflect.DeepEqual(recording.Expect, entry.Expect) {
		problems = append(problems, fmt.Sprintf(
			"%s: the file's expect and the manifest's expect differ", entry.Scenario))
	}

	if len(recording.Interactions) == 0 {
		problems = append(problems, fmt.Sprintf("%s: the recording has no interactions", entry.Scenario))
	}

	if len(entry.Expect) != len(recording.Interactions) {
		problems = append(problems, fmt.Sprintf(
			"%s: %d expectations for %d interactions; `expect` is one entry per interaction (A301)",
			entry.Scenario, len(entry.Expect), len(recording.Interactions)))

		return nil, problems
	}

	for i, interaction := range recording.Interactions {
		s.Checks.Interactions++

		if interaction.Request.Method == "" || interaction.Request.Path == "" {
			problems = append(problems, fmt.Sprintf(
				"%s[%d]: the recorded request has no method or no path", entry.Scenario, i))
		}

		if asserts(entry.Expect[i]) == 0 {
			problems = append(problems, fmt.Sprintf(
				"%s[%d]: the expectation asserts nothing but its status; an expectation that can be "+
					"emptied without going red is not one (A333)", entry.Scenario, i))
		}

		for _, problem := range entry.Expect[i].Check(interaction.Response, &s.Checks) {
			problems = append(problems, fmt.Sprintf("%s[%d]: %s", entry.Scenario, i, problem))
		}
	}

	if len(problems) > 0 {
		return nil, problems
	}

	return &recording, nil
}

// asserts counts what an expectation claims beyond its status.
//
// Every recorded expectation claims something: a code, a header, a path, a
// value. The check exists so that weakening one to `status` alone — the
// cheapest way to make a failing recording pass — is itself a failure.
func asserts(e Expectation) int {
	n := len(e.HeadersPresent) + len(e.HeadersAbsent) + len(e.BodyHas) + len(e.BodyLacks) + len(e.BodyEquals)
	if e.Code != nil {
		n++
	}

	return n
}

func sha256Of(raw []byte) []byte {
	sum := sha256.Sum256(raw)

	return sum[:]
}

// RecordedDir is `cli/testdata/recorded/`, located from this file rather than
// from the working directory: `go test` runs each package in its own
// directory, so a relative path here would mean a different place in every
// package that consumes the fixture.
func RecordedDir() string {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return ""
	}

	return filepath.Join(filepath.Dir(file), "..", "..", "testdata", "recorded")
}

var (
	recordedOnce sync.Once
	recordedSet  *Set
	recordedErr  error
)

// Recorded loads `cli/testdata/recorded/` once per process.
//
// The result is shared and never mutated: a server takes its cursor with it,
// so two tests running in parallel read the same recordings and write nothing.
func Recorded() (*Set, error) {
	recordedOnce.Do(func() {
		dir := RecordedDir()
		if dir == "" {
			recordedErr = fmt.Errorf("%w: cannot locate cli/testdata/recorded from the compiled package", ErrRecordings)

			return
		}

		recordedSet, recordedErr = Load(dir)
	})

	return recordedSet, recordedErr
}
