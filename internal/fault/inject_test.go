//go:build faultinject

package fault_test

import (
	"errors"
	"testing"

	"github.com/coba-ai/ferry-cli/internal/fault"
)

// The armed counterpart to `release_test.go`.
//
// Between them they establish that the tag is the only difference: the same
// environment variable that does nothing in a release build arms exactly one
// point here, and only the one it names.

func TestOnlyTheNamedPointIsArmed(t *testing.T) {
	if !fault.Enabled {
		t.Fatal("this file is tagged faultinject but Enabled is false")
	}

	points := fault.Points()

	if len(points) < 2 {
		t.Fatal("fewer than two points are declared, so 'only the named one' is untestable")
	}

	for _, armed := range points {
		t.Setenv(fault.EnvVar, armed)

		for _, point := range points {
			want := point == armed

			if got := fault.Armed(point); got != want {
				t.Errorf("with %s requested, Armed(%s) = %v, want %v", armed, point, got, want)
			}
		}
	}
}

// An unset variable arms nothing, which is the state of every ordinary test
// in this repository.
//
// Without it, a `fault.Die` left in a hot path would kill every invocation
// and the suite's failures would all be in the wrong place.
func TestAnUnsetVariableArmsNothing(t *testing.T) {
	t.Setenv(fault.EnvVar, "")

	for _, point := range fault.Points() {
		if fault.Armed(point) {
			t.Errorf("%s is armed with the variable unset", point)
		}

		fault.Die(point)
		fault.Panic(point)
	}
}

// A name the package does not declare is refused loudly.
//
// Arming nothing would be the safe-looking choice and is the wrong one: a
// misspelled point in a test would then run the ordinary path and report
// green, and the test would be believed to have proved something about a
// crash window it never entered. Refusing at the first `Armed` call turns
// that into a failure at the line that set it.
func TestAMisspelledNameIsRefusedRatherThanIgnored(t *testing.T) {
	t.Setenv(fault.EnvVar, "after_record_writen")

	defer func() {
		v := recover()
		if v == nil {
			t.Fatal("a misspelled point was accepted; a test arming it would pass by " +
				"running the ordinary path")
		}

		if _, ok := v.(fault.Crash); ok {
			t.Error("a misspelled point produced a simulated process death, which a test " +
				"could mistake for the crash it was asking for")
		}
	}()

	fault.Armed(fault.AfterRecordWritten)
}

// Die panics with a Crash, and Panic does not.
//
// The distinction is the whole design: a `Crash` is recovered by the root
// and turned into exit 137 with no output, modelling a process that was
// killed; an ordinary panic is a bug in this CLI, which the root classifies
// and reports. Confusing them would make a renderer bug exit 137 and a
// simulated death print a stack trace.
func TestDieCrashesAndPanicDoesNot(t *testing.T) {
	t.Setenv(fault.EnvVar, fault.AfterRecordWritten)

	t.Run("Die panics with a Crash", func(t *testing.T) {
		defer func() {
			v := recover()
			if v == nil {
				t.Fatal("Die did not panic")
			}

			crash, ok := v.(fault.Crash)
			if !ok {
				t.Fatalf("Die panicked with %T, want fault.Crash", v)
			}

			if crash.Point != fault.AfterRecordWritten {
				t.Errorf("the crash names %q, want %q", crash.Point, fault.AfterRecordWritten)
			}

			// It is an error, so the root can carry it through the
			// error-returning plumbing rather than re-panicking.
			var asError error = crash
			if asError.Error() == "" {
				t.Error("the crash has no message")
			}
		}()

		fault.Die(fault.AfterRecordWritten)
	})

	t.Run("Panic panics with something that is not a Crash", func(t *testing.T) {
		defer func() {
			v := recover()
			if v == nil {
				t.Fatal("Panic did not panic")
			}

			if _, ok := v.(fault.Crash); ok {
				t.Error("Panic panicked with a fault.Crash; it must be an ordinary bug, " +
					"because a bug in this CLI is exit 6 after a send and not a killed process")
			}

			if err, ok := v.(error); ok && errors.Is(err, fault.Crash{}) {
				t.Error("Panic's value is a Crash by errors.Is")
			}
		}()

		fault.Panic(fault.AfterRecordWritten)
	})
}
