package e2e_test

import (
	"sort"
	"testing"
)

// The set comparison every check in this package is built on, and its own
// control.
//
// No build tag: the tagged e2e suite and the untagged workflow pins share one
// implementation. Two copies of a both-directions comparison is two chances for
// one of them to lose a direction, which is exactly the defect the comparison
// exists to catch.

// setDiff is the comparison itself, as a function of its inputs.
//
// Separate from the assertion so it can have a control that is not a test of
// `testing.T`'s internals: "this comparison reports an extra element" is a
// claim about these two return values, and
// `TestTheSetComparisonReportsBothDirections` reads them.
func setDiff(want, got []string) (missing, unexpected []string) {
	wantSet := toSet(want)
	gotSet := toSet(got)

	for v := range wantSet {
		if !gotSet[v] {
			missing = append(missing, v)
		}
	}

	for v := range gotSet {
		if !wantSet[v] {
			unexpected = append(unexpected, v)
		}
	}

	return sorted(missing), sorted(unexpected)
}

// assertSetsEqual compares two collections as sets, in both directions, and
// refuses to compare against an empty expectation.
//
// Both directions is the point. "Every expected element is present" passes for
// a collection with extra elements — a CI job added and never looked at, a
// workflow trigger nobody meant — and "every actual element is expected" passes
// for an empty collection. PLAN §6 records the first of those as the most common
// defect found in this build.
//
// The empty-expectation guard is the floor for the comparison itself: a caller
// that gathered its expectations by parsing and parsed nothing would otherwise
// get a green from comparing nothing with nothing.
func assertSetsEqual(t *testing.T, what string, want, got []string) {
	t.Helper()

	if len(toSet(want)) == 0 {
		t.Fatalf("%s: the expected set is empty, so the comparison would be vacuous", what)
	}

	missing, unexpected := setDiff(want, got)

	if len(missing) > 0 {
		t.Errorf("%s: expected but absent: %v", what, missing)
	}

	if len(unexpected) > 0 {
		t.Errorf("%s: present but not expected: %v", what, unexpected)
	}
}

func toSet(values []string) map[string]bool {
	set := make(map[string]bool, len(values))
	for _, v := range values {
		set[v] = true
	}

	return set
}

func sorted(values []string) []string {
	out := append([]string(nil), values...)
	sort.Strings(out)

	return out
}

// Every assertion in this package rests on the comparison above, so it needs its
// own control: a comparison that could not report one of its two directions
// would make every pin over it green for the wrong reason, which is the
// precedent PLAN §6 names — a consent test that stayed green with the safety
// check deleted.
func TestTheSetComparisonReportsBothDirections(t *testing.T) {
	cases := []struct {
		name           string
		want, got      []string
		missing, extra []string
	}{
		{
			name: "equal, in a different order",
			want: []string{"a", "b"}, got: []string{"b", "a"},
		},
		{
			name: "an element is absent",
			want: []string{"a", "b"}, got: []string{"a"},
			missing: []string{"b"},
		},
		{
			name: "an element is extra — the direction a subset check loses",
			want: []string{"a"}, got: []string{"a", "b"},
			extra: []string{"b"},
		},
		{
			name: "both at once",
			want: []string{"a"}, got: []string{"b"},
			missing: []string{"a"}, extra: []string{"b"},
		},
		{
			name: "nothing at all",
			want: []string{"a", "b"}, got: nil,
			missing: []string{"a", "b"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			missing, extra := setDiff(tc.want, tc.got)

			if !equalSlices(missing, tc.missing) {
				t.Errorf("missing = %v, want %v", missing, tc.missing)
			}

			if !equalSlices(extra, tc.extra) {
				t.Errorf("unexpected = %v, want %v", extra, tc.extra)
			}
		})
	}
}

func equalSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}

	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}

	return true
}
