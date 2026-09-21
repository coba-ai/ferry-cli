package api

import (
	"net/http/httptrace"
	"sync/atomic"

	"github.com/kurenn/ferry/cli/internal/outcome"
)

// writeWatch records whether any of the request left this host.
//
// C3 and AC23 turn on that fact and nothing else, and it has to be *measured*.
// The alternative — reading it off the error — does not work: `context
// deadline exceeded` is the same string whether the deadline fired during the
// TCP handshake or while waiting for a response to a request already sent, and
// those two are exit 5 and exit 6. A CLI that guesses gets one of them wrong
// every time.
//
// `httptrace.WroteHeaders` is the earliest hook that can only fire after bytes
// were handed to the socket, so it is the conservative edge: it may be set for
// a request the server never processed, which classifies `pending` when
// `transient` would also have been safe. The opposite error is not safe.
type writeWatch struct {
	wrote atomic.Bool
}

func (w *writeWatch) trace() *httptrace.ClientTrace {
	return &httptrace.ClientTrace{
		WroteHeaders: func() {
			w.wrote.Store(true)
		},
		WroteRequest: func(httptrace.WroteRequestInfo) {
			w.wrote.Store(true)
		},
	}
}

func (w *writeWatch) phase() outcome.TransportPhase {
	if w.wrote.Load() {
		return outcome.PhaseAfterWrite
	}

	return outcome.PhaseDial
}
