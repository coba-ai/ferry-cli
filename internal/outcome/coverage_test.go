package outcome_test

import (
	"sort"
	"testing"

	"github.com/kurenn/ferry/cli/internal/outcome"
)

// AC19. The table covers every code in `docs/api/errors.md` for each of the
// four operation classes, parsed from the generated table, both directions.
//
// The two directions are separate examples on purpose. "Every code I handle is
// in the catalogue" and "every code in the catalogue is handled" are different
// claims, and only the second one can see an absence — which is the direction
// this project has shipped wrong before. Keeping them apart means a reader
// counting examples counts claims.

func TestEveryCatalogueCodeHasARow(t *testing.T) {
	catalogue := readCatalogue(t)

	var missing []string

	for _, code := range keys(catalogue) {
		if _, _, found := outcome.Lookup(code); !found {
			missing = append(missing, code)
		}
	}

	if len(missing) > 0 {
		t.Errorf("errors.md names %d code(s) the table does not handle: %v", len(missing), missing)
	}
}

func TestEveryRowNamesACatalogueCode(t *testing.T) {
	catalogue := readCatalogue(t)

	var phantom []string

	for _, code := range outcome.Codes() {
		if _, ok := catalogue[code]; !ok {
			phantom = append(phantom, code)
		}
	}

	sort.Strings(phantom)

	if len(phantom) > 0 {
		t.Errorf("the table handles %d code(s) errors.md does not name: %v", len(phantom), phantom)
	}
}

// Every (code, operation class) pair resolves to a declared verdict, not to
// the unknown-code fallback. Lookup is what makes that distinguishable:
// Classify's return value for an unhandled code is an ordinary Outcome and
// says nothing about whether a row existed.
func TestEveryCodeResolvesUnderEveryOperationClass(t *testing.T) {
	catalogue := readCatalogue(t)

	if len(outcome.OpClasses) != 4 {
		t.Fatalf("PLAN §5.6 declares four operation classes; this package declares %d: %v",
			len(outcome.OpClasses), outcome.OpClasses)
	}

	declared := map[outcome.Class]bool{}
	for _, c := range outcome.Classes {
		declared[c] = true
	}

	for _, code := range keys(catalogue) {
		for _, class := range outcome.OpClasses {
			got, found := outcome.VerdictFor(code, class)
			if !found {
				t.Errorf("%s under %s: no row", code, class)

				continue
			}

			if !declared[got] {
				t.Errorf("%s under %s resolves to %q, which is not in the class vocabulary", code, class, got)
			}
		}
	}
}

// AC19's second clause: codes `errors.md` says are never emitted are mapped
// and marked unreachable. Both directions, so a code leaving that section
// without the flag being dropped is as red as a code joining it.
func TestUnreachableFlagMatchesTheDocument(t *testing.T) {
	assertSetsEqual(t, "unreachable codes",
		outcome.Unreachable(), readNeverEmitted(t),
		"the table's `unreachable` flag", `errors.md's "Codes you will not see"`)
}

// The off-surface census, held to `openapi.yaml` in both directions.
func TestOffSurfaceFlagMatchesTheContract(t *testing.T) {
	catalogue := readCatalogue(t)

	assertSetsEqual(t, "webhook-only codes",
		outcome.OffSurface(), readWebhookOnlyCodes(t, catalogue),
		"the table's `offSurface` flag", "openapi.yaml's webhook operation")
}

// The read column is a second opinion, not a copy.
//
// `errors.md`'s "Retry identical" column answers exactly the question the read
// class's split turns on — could resending the byte-identical request ever
// succeed? — and it is generated from `Ferry::Api::Errors::CATALOG`. So the
// read verdicts are declared by hand in the Go table and held to it here:
// `transient` if and only if the catalogue says the identical request could
// succeed.
//
// Keeping the duplicate is the point. Deriving the read column from the
// document would make this a detector drawing its subject from its own
// expectation, which cannot fail.
func TestReadColumnAgreesWithTheCatalogueRetriableFlag(t *testing.T) {
	catalogue := readCatalogue(t)

	// The one declared exception, with its reason. `poll.Watch` is a read
	// (`GET /v1/commands/{id}`), and AC51 requires polling to stop at exit 7
	// when an operator holds the command — so the read column for this code
	// is `escalate` and not the `refused_fix` its non-retriable row would
	// otherwise give it.
	exceptions := map[string]outcome.Class{
		"COMMAND_UNRESOLVED": outcome.ClassEscalate,
	}

	for _, code := range keys(catalogue) {
		got, found := outcome.VerdictFor(code, outcome.OpRead)
		if !found {
			continue // TestEveryCatalogueCodeHasARow reports this.
		}

		if want, ok := exceptions[code]; ok {
			if got != want {
				t.Errorf("%s under read: got %q, want the declared exception %q", code, got, want)
			}

			continue
		}

		want := outcome.ClassRefusedFix
		if catalogue[code].retriable {
			want = outcome.ClassTransient
		}

		if got != want {
			t.Errorf("%s under read: got %q; errors.md says retry-identical=%v, so the read column must be %q",
				code, got, catalogue[code].retriable, want)
		}
	}

	// A floor on the exception list, so it cannot quietly absorb a
	// disagreement: every exception must still name a catalogued code.
	for code := range exceptions {
		if _, ok := catalogue[code]; !ok {
			t.Errorf("the exception list names %s, which errors.md does not", code)
		}
	}
}

// The class vocabulary and its properties are one list, held to each other.
// Without this, a class added to Classes with no properties row panics at
// first use rather than failing here.
func TestClassVocabularyAgrees(t *testing.T) {
	seen := map[outcome.Class]bool{}

	for _, c := range outcome.Classes {
		if seen[c] {
			t.Errorf("Classes repeats %q", c)
		}

		seen[c] = true

		if _, ok := outcome.Properties(c); !ok {
			t.Errorf("%q is in Classes but has no declared exit code, money answer or same-key rule", c)
		}
	}

	if len(outcome.Classes) != 10 {
		t.Errorf("PLAN §5.6 declares ten classes, exit 0 through 8 with two at 0; this package declares %d", len(outcome.Classes))
	}

	// Exit codes are what a wrapper branches on, so pin the whole mapping
	// rather than sampling it.
	want := map[outcome.Class]int{
		outcome.ClassDone:              0,
		outcome.ClassAcceptedUpstream:  0,
		outcome.ClassCLIFault:          1,
		outcome.ClassUsage:             2,
		outcome.ClassRefusedFix:        3,
		outcome.ClassRefusedResimulate: 4,
		outcome.ClassTransient:         5,
		outcome.ClassPending:           6,
		outcome.ClassEscalate:          7,
		outcome.ClassUpstreamFailed:    8,
	}

	for class, exit := range want {
		got, ok := outcome.Properties(class)
		if !ok {
			continue // Reported above.
		}

		if got.Exit != exit {
			t.Errorf("%q exits %d, want %d", class, got.Exit, exit)
		}
	}

	// And the other direction: a class added to the vocabulary without a
	// row here is a class whose exit code nothing pins.
	if len(want) != len(outcome.Classes) {
		t.Errorf("this example pins %d exit codes for %d classes", len(want), len(outcome.Classes))
	}
}

// The operation set and the operation-class map are held to each other, so an
// operation added to one and not the other fails here rather than falling
// through ClassOf into the "cannot classify" branch at runtime.
func TestEveryOperationHasAClass(t *testing.T) {
	if len(outcome.Operations) != 10 {
		t.Errorf("openapi.yaml declares ten operations outside the webhook and catch-all routes; this package declares %d", len(outcome.Operations))
	}

	for _, op := range outcome.Operations {
		class, ok := outcome.ClassOf(op)
		if !ok {
			t.Errorf("%s has no operation class", op)

			continue
		}

		found := false

		for _, c := range outcome.OpClasses {
			if c == class {
				found = true
			}
		}

		if !found {
			t.Errorf("%s is classed %q, which is not in OpClasses", op, class)
		}
	}
}

// Every operation class has a column. A class added to OpClasses without one
// would silently return the zero verdict, whose class is "" and whose
// Properties lookup panics — so this is what turns that into a test failure.
func TestEveryOpClassHasAColumn(t *testing.T) {
	for _, class := range outcome.OpClasses {
		got, found := outcome.VerdictFor("TOKEN_MISSING", class)
		if !found {
			t.Fatalf("TOKEN_MISSING has no row")
		}

		if got == "" {
			t.Errorf("operation class %q has no column in the table", class)
		}
	}
}
