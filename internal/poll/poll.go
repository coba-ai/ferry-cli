// Package poll is the one loop that follows a command to its end (AC59).
//
// There is exactly one, and `sweep_test.go` holds the claim by walking the
// tree: no other package under `cli/` requests `GET /v1/commands/{id}` and no
// command package sleeps. A second loop is not a duplication problem, it is a
// correctness one — the rules for stopping are C9's (`contradiction` before
// `state`, `needs_operator` is not a wait) and a second implementation is a
// second chance to get them the other way round.
//
// The loop decides nothing about what a command *means*. It reads the body,
// hands it to `outcome.ClassifyCommand`, and keeps going only while the class
// is `pending`. A392: `last_error.code` is not the wire error vocabulary, and
// this package must not read it through the wire table — the one place that
// rule is enforced is `internal/outcome`, and re-deriving it here would be a
// second authority for a decision about whether money moved.
package poll

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/kurenn/ferry-cli/internal/api"
	"github.com/kurenn/ferry-cli/internal/cli/flight"
	"github.com/kurenn/ferry-cli/internal/outcome"
)

// Clock is the time this loop depends on. It is `api.Clock` under another
// name so a test installs one fake and both the retry loop inside `api.Do`
// and the wait between polls read it.
type Clock = api.Clock

// ErrInterrupted is returned when a signal reached the process while the loop
// was waiting or sending. It is not an outcome: the caller — which is the
// money path — answers for it by consulting the in-flight flag (C17).
var ErrInterrupted = errors.New("poll: interrupted")

// Deps is what the loop needs.
type Deps struct {
	// Client is built for `get_command`, so the credential class was
	// pre-checked for that operation and not merely borrowed from the money
	// one.
	Client *api.Client

	Clock Clock

	// Progress receives one line per state transition, in text mode. It is
	// stderr: a poll that wrote to stdout would put a second thing there in
	// JSON mode, which C10 forbids.
	Progress io.Writer

	// Interrupted closes when a signal reaches the process.
	Interrupted <-chan struct{}
}

// Result is what the loop saw.
type Result struct {
	// Command is the last body that decoded, or nil.
	Command *Command

	// Body is that body's bytes, for the `response` block of the document.
	Body json.RawMessage

	// Last is the last HTTP answer, whatever it was.
	Last api.Result

	// Outcome is the classification of the state the loop stopped in.
	Outcome outcome.Outcome

	// Polls counts the `GET /v1/commands/{id}` requests sent.
	Polls int

	// TimedOut is true when the deadline stopped the loop.
	TimedOut bool

	// Unreadable is true when the loop stopped because the poll itself did
	// not answer with a command — a refusal, a transport fault, a body that
	// did not decode. The caller decides what that means for its own
	// operation: for a money step it is `pending`, because the command's
	// state is exactly what is not known.
	Unreadable bool
}

// CommandID is the id the loop followed, for a message.
func (r Result) CommandID() string {
	if r.Command == nil {
		return ""
	}

	return r.Command.ID
}

// Watch follows commandID until it stops moving, the deadline passes, or a
// signal arrives.
//
// initialDelay is the interval the answer that produced this command asked
// for — a `202`'s `retry_after_seconds`. It is waited out before the first
// poll: FERRY said how long it needed and polling immediately would ignore
// it. A caller with no such answer (`ferry commands watch`) passes 0 and the
// first read is immediate.
func Watch(ctx context.Context, d Deps, commandID string, initialDelay int, deadline time.Time) (Result, error) {
	var out Result

	if commandID == "" {
		return out, fmt.Errorf("poll: no command id to follow")
	}

	clock := d.Clock
	if clock == nil {
		clock = realClock{}
	}

	delay := initialDelay
	previousState := ""

	for {
		if delay > 0 {
			if !clock.Now().Add(time.Duration(delay) * time.Second).Before(deadline) {
				// The wait would run past the deadline, so waiting it out
				// would spend the whole timeout to learn nothing. Stop now
				// and say the outcome is not established.
				out.TimedOut = true
				out.Outcome = timedOut(commandID)

				return out, nil
			}

			if interrupted(d.Interrupted) {
				return out, ErrInterrupted
			}

			d.report("waiting %ds before polling %s\n", delay, commandID)
			clock.Sleep(time.Duration(delay) * time.Second)
		}

		if interrupted(d.Interrupted) {
			return out, ErrInterrupted
		}

		res, err := d.Client.Do(ctx, api.Request{
			Op:         outcome.OpGetCommand,
			PathParams: map[string]string{"id": commandID},
		})
		if err != nil {
			return out, err
		}

		out.Polls++
		out.Last = res

		if interrupted(d.Interrupted) {
			// The answer may have arrived; the signal still ends the
			// invocation, and the caller reports it against the in-flight
			// flag rather than against this answer.
			return out, ErrInterrupted
		}

		cmd, ok := decode(res)
		if !ok {
			out.Unreadable = true
			out.Outcome = outcome.Classify(res.Input(false, ""))

			return out, nil
		}

		out.Command = cmd
		out.Body = append(json.RawMessage(nil), res.Body...)
		out.Outcome = outcome.ClassifyCommand(cmd.Input())

		if cmd.State != previousState {
			d.report("%s is %s\n", cmd.ID, cmd.State)
			previousState = cmd.State
		}

		// The loop continues on exactly one class. `pending` is "the
		// outcome is not established", which is what a command still
		// moving is; everything else — a contradiction, needs_operator, a
		// terminal state, a state outside the enum — is a stop, and the
		// stop is the table's decision and not this loop's.
		if out.Outcome.Class != outcome.ClassPending {
			return out, nil
		}

		delay = cmd.RetryDelaySeconds()
	}
}

func decode(res api.Result) (*Command, bool) {
	if res.Transport != nil || res.Meta.Status < 200 || res.Meta.Status >= 300 {
		return nil, false
	}

	var cmd Command
	if err := json.Unmarshal(res.Body, &cmd); err != nil {
		return nil, false
	}

	if cmd.Object != "command" || cmd.ID == "" {
		// Something answered 200 with a body that is not a command. C3: on
		// a money path that is `pending`, and the caller gets there through
		// Unreadable rather than through this loop inventing a state.
		return nil, false
	}

	return &cmd, true
}

// DecodeCommand reads a command body, for a caller holding a `202` rather
// than a poll answer. It is the same decoder, so a `202` the loop would
// refuse is a `202` the caller refuses too.
func DecodeCommand(body []byte) (*Command, bool) {
	var cmd Command
	if err := json.Unmarshal(body, &cmd); err != nil {
		return nil, false
	}

	if cmd.Object != "command" || cmd.ID == "" {
		return nil, false
	}

	return &cmd, true
}

func timedOut(commandID string) outcome.Outcome {
	return flight.OutcomeFor(outcome.ClassPending, fmt.Sprintf(
		"The command did not reach a terminal state before this invocation's timeout, so its outcome is not established. "+
			"Money may or may not have moved — do not resend under a new key. Follow it with `ferry commands watch %s`.",
		commandID))
}

func interrupted(ch <-chan struct{}) bool {
	if ch == nil {
		return false
	}

	select {
	case <-ch:
		return true
	default:
		return false
	}
}

func (d Deps) report(format string, args ...any) {
	if d.Progress == nil {
		return
	}

	fmt.Fprintf(d.Progress, format, args...)
}

// Now reads a clock that may be nil, so a caller computing a deadline uses
// the same clock the loop will wait on. A caller that reached for
// `time.Now()` here would build a deadline in real time and compare it
// against a fake clock's instants, which is a test that either never times
// out or always does.
func Now(clock Clock) time.Time {
	if clock == nil {
		return time.Now()
	}

	return clock.Now()
}

type realClock struct{}

func (realClock) Now() time.Time        { return time.Now() }
func (realClock) Sleep(d time.Duration) { time.Sleep(d) }
