// Package consent decides whether *this invocation* may send an execute
// request (C16).
//
// # What consent is
//
// One of exactly three things, each of them a fact about the invocation that
// is asking:
//
//  1. `--yes` on this command line;
//  2. a command that is itself the consent — `transfers create --broadcast`
//     or `transfers execute` — running with no terminal to ask at;
//  3. `y` typed at a terminal, at a prompt that showed the plan.
//
// # What consent is not
//
// A stored state. There is no argument this package takes, and no state
// `internal/runs` can be in, that means "a human already agreed". CRITIQUE
// B3 found revision 1 recording the plan token before the prompt, so a
// *declined* transfer could be executed by a later `resume`; the answer was
// to make the ledger unable to express agreement at all — `runs.AllStates`
// is a census, and a state whose name reads as recorded consent fails a test
// in that package — and to route every one of the three arms that can send
// an execute through [Ask].
//
// `runs resume` is deliberately not arm 2. Resuming is not a command that
// carries an intention about money; it is a command about a record. On a
// terminal it asks, and with no terminal it requires `--yes` and otherwise
// refuses, which is [Unavailable].
package consent

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// Verdict is what this invocation established.
type Verdict string

const (
	// Granted — this invocation carried consent.
	Granted Verdict = "granted"

	// Declined — a human said no, or the read ended without an answer
	// (EOF), or a signal arrived while the prompt was up. All three are the
	// same answer, and all three are terminal for the step: PLAN §5.3 V4
	// makes SIGHUP a decline because a closed terminal is the ordinary way
	// an unattended prompt dies, and treating it as "unknown" would leave a
	// state only a human at a terminal could clear.
	Declined Verdict = "declined"

	// Unavailable — there was nobody to ask and the command did not carry
	// consent. Nothing was sent and nothing was declined: the caller has to
	// say `--yes` or `runs decline`.
	Unavailable Verdict = "unavailable"
)

// ErrPromptFailed is a terminal read that failed for a reason that is not an
// answer — not EOF, which is a decline.
var ErrPromptFailed = errors.New("consent: the prompt could not be read")

// Request is one ask.
type Request struct {
	// Yes is `--yes` on this invocation's command line.
	Yes bool

	// TTY is whether stdin is a terminal.
	TTY bool

	// CommandIsConsent is arm 2: true for `transfers create --broadcast`
	// and `transfers execute --plan`, false for `runs resume`.
	CommandIsConsent bool

	// BeforePrompt is called once, durably, *before* a byte of the prompt is
	// written, and only on the arm that prompts. It writes
	// `awaiting_confirmation`. An error from it aborts the ask: a prompt
	// whose state did not reach the disk is a prompt a kill would leave no
	// evidence of, and AC72 asserts that evidence.
	BeforePrompt func() error

	// Display writes what the human is agreeing to. It runs after
	// BeforePrompt and before the question.
	Display func(w io.Writer)

	// Question is the line the answer is typed after. Empty is
	// "Execute? [y/N] ".
	Question string

	// Out is where the prompt is written. Stderr, so that `--output json`'s
	// single document is the only thing on stdout even when a human is
	// answering a question (C10).
	Out io.Writer

	// In is the terminal to read the answer from. Nil is os.Stdin.
	In io.Reader

	// Interrupted closes when SIGINT, SIGTERM or SIGHUP reaches this
	// process. A signal while the prompt is up is a decline.
	Interrupted <-chan struct{}

	// PromptShown is called after the prompt has been written and before
	// the read begins. Production passes nil; the fault point AtPrompt and
	// the signal tests are the two callers, and both need exactly this
	// instant — after the display, before the read (V9).
	PromptShown func()
}

// Ask establishes consent for one execute send.
//
// The order of the branches is C16's and none of them may be reordered:
// `--yes` first, because a command line that said yes needs no terminal and
// must not be given a prompt; then the terminal, because a human present is
// the one who should be asked; then the command itself, which is consent only
// where the landing page promises it is.
func Ask(r Request) (Verdict, error) {
	if r.Yes {
		return Granted, nil
	}

	if !r.TTY {
		if r.CommandIsConsent {
			return Granted, nil
		}

		return Unavailable, nil
	}

	if r.BeforePrompt != nil {
		if err := r.BeforePrompt(); err != nil {
			return Unavailable, err
		}
	}

	out := r.Out
	if out == nil {
		out = os.Stderr
	}

	if r.Display != nil {
		r.Display(out)
	}

	question := r.Question
	if question == "" {
		question = "Execute? [y/N] "
	}

	if _, err := io.WriteString(out, question); err != nil {
		return Unavailable, fmt.Errorf("%w: %v", ErrPromptFailed, err)
	}

	if r.PromptShown != nil {
		r.PromptShown()
	}

	return read(r)
}

// read waits for the answer, for the end of input, or for a signal.
//
// The read runs in a goroutine because a blocking read on a terminal cannot
// be interrupted from inside the process, and a signal arriving while it is
// blocked has to be able to answer for it. The goroutine is left behind when
// a signal wins: the invocation is about to end, its terminal is about to be
// closed, and the alternative — closing stdin here to unblock it — would
// take the descriptor away from a caller that might still want it.
func read(r Request) (Verdict, error) {
	in := r.In
	if in == nil {
		in = os.Stdin
	}

	type answer struct {
		line string
		err  error
	}

	lines := make(chan answer, 1)

	go func() {
		line, err := bufio.NewReader(in).ReadString('\n')
		lines <- answer{line: line, err: err}
	}()

	// A signal that has already been seen answers on its own, before the
	// read is consulted at all. Without this the two cases below can both be
	// ready — an interrupted process with a "y" already buffered on stdin —
	// and `select` chooses between them at random, so the same invocation
	// declines or sends money depending on the scheduler. Money is not a
	// coin toss: once the signal is in, the answer is no.
	select {
	case <-r.Interrupted:
		return Declined, nil
	default:
	}

	select {
	case <-r.Interrupted:
		// A signal while the prompt was up. Nothing was typed and nothing
		// was sent, so "no" is still an available answer and is the safe one.
		return Declined, nil

	case a := <-lines:
		switch {
		case a.err != nil && !errors.Is(a.err, io.EOF):
			return Unavailable, fmt.Errorf("%w: %v", ErrPromptFailed, a.err)

		case affirmative(a.line):
			return Granted, nil

		default:
			// Everything else, EOF included. A prompt that defaults to yes
			// on an unreadable answer is a prompt that sends money when the
			// terminal closes.
			return Declined, nil
		}
	}
}

// affirmative is the whole of what counts as yes.
//
// `y` and `yes`, case-insensitively, and nothing else — not an empty line,
// which is what a human pressing return at `[y/N]` sends, and not a prefix
// match, which would make `yesterday` an authorisation to move money.
func affirmative(line string) bool {
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true
	}

	return false
}
