package api

import (
	"time"

	"github.com/kurenn/ferry-cli/internal/outcome"
)

// DefaultRetryBudget is `--retry-budget`'s default (§5.8).
const DefaultRetryBudget = 60 * time.Second

// FixtureUnmatched is the status `internal/fixture` answers for a request no
// recording matches (AC29).
//
// It is never retried. CRITIQUE N5: a fixture saying "nothing here matches
// what you sent" is a test defect, and retrying it converts a clear failure
// into a slow one — the same class of mistake as a flaky test that is re-run
// until it passes. It is a real status in the 5xx range, so the "never" has to
// be written down rather than left to the 5xx rules below.
const FixtureUnmatched = 599

// Clock is the time this package depends on, injected so retry can be tested
// without waiting. AC27's test runs the whole budget in no time at all.
type Clock interface {
	Now() time.Time
	Sleep(d time.Duration)
}

type realClock struct{}

func (realClock) Now() time.Time        { return time.Now() }
func (realClock) Sleep(d time.Duration) { time.Sleep(d) }

// Retryable answers whether this answer may be resent byte-for-byte under the
// same key.
//
// It is a predicate over the answer, deliberately separate from the decision
// table: "is it worth sending again" and "what does this mean if it is the
// last answer" are different questions, and one code answers them differently.
// `UPSTREAM_BUSY` with a `Ferry-Command-Id` is the clearest case — not worth
// resending immediately, because FERRY is already retrying it itself, and
// `pending` when it is the last word.
//
// What is *not* here is as important as what is. No 4xx is retried: a refusal
// FERRY computed from the request will be computed the same way again. No
// `control_create` is retried at any status, because an unanswered key mint
// may have minted a key whose token this CLI no longer holds (AC38), and a
// second attempt mints a second one.
func Retryable(class outcome.OpClass, status int, code, commandID string) bool {
	if class == outcome.OpControlCreate {
		return false
	}

	if status == FixtureUnmatched {
		return false
	}

	if status == 429 {
		return true
	}

	if status < 500 {
		return false
	}

	switch code {
	case "UPSTREAM_BUSY":
		// Without a command, `read_quote` refused and nothing was sent
		// (`executor.rb:289-294`): resend. With one, FERRY holds a
		// `failed_retriable` command and has scheduled recovery
		// (`executor.rb:467-481`); the caller polls instead.
		return commandID == ""
	case "SERVICE_UNAVAILABLE", "UPSTREAM_AUTH_UNAVAILABLE", "INTERNAL_ERROR":
		return true
	case "UPSTREAM_UNAVAILABLE", "UPSTREAM_NOT_CONFIGURED", "UPSTREAM_CREDENTIALS_INVALID":
		// 503s that are not transient. `UPSTREAM_UNAVAILABLE` arrives only
		// replayed from a command recovery gave up on, and the plan behind
		// it is already spent; the other two need an operator.
		return false
	}

	// A 5xx this CLI cannot name. On a read, another try costs nothing and
	// often works. On a money request it is left alone: §5.8 enumerates what
	// is resent, and an answer we cannot read is not on the list.
	return !class.IsMoney()
}
