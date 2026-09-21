// Package render turns an API answer into what a human or a program reads.
//
// It owns two things the rest of the CLI must not duplicate:
//
//   - **The shape of the JSON document** (PLAN §5.9). One document per
//     invocation, with the outcome's exit code inside it, so a program reading
//     stdout and a shell reading `$?` cannot be told different things.
//   - **The one sink a secret may be written through** (`secret.go`, AC42).
//
// Text rendering is deliberately plain: no colour by default, no "✓", no
// "success". A transfer that FERRY accepted upstream is not settled, and a
// vocabulary that cannot say so in the read path will not say so in the money
// path either (C9, AC49 — U5's, but the habit is set here).
package render

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/kurenn/ferry-cli/internal/api"
	"github.com/kurenn/ferry-cli/internal/outcome"
)

// Mode is `--output`.
type Mode string

const (
	// ModeText is the default. PLAN §5.1: never inferred from the TTY — a
	// pipeline's bytes must not depend on whether a terminal was attached.
	ModeText Mode = "text"
	ModeJSON Mode = "json"
)

// Modes is the vocabulary `--output` accepts.
var Modes = []Mode{ModeText, ModeJSON}

// ParseMode validates `--output`.
func ParseMode(raw string) (Mode, error) {
	for _, m := range Modes {
		if string(m) == raw {
			return m, nil
		}
	}

	names := make([]string, 0, len(Modes))
	for _, m := range Modes {
		names = append(names, string(m))
	}

	return "", Usage("--output %q is not one of %s", raw, strings.Join(names, ", "))
}

// Document is PLAN §5.9's single JSON document.
type Document struct {
	FerryCLI CLIBlock     `json:"ferry_cli"`
	Outcome  OutcomeBlock `json:"outcome"`

	// HTTP is null when no request was made — a local refusal, a usage
	// error, a pre-check (AC43). AC63 reads exactly that.
	HTTP *HTTPBlock `json:"http"`

	// Response is the API body verbatim, or null.
	Response json.RawMessage `json:"response"`

	// Error is the FERRY error envelope verbatim, or null.
	Error json.RawMessage `json:"error"`
}

// CLIBlock identifies the invocation.
type CLIBlock struct {
	Version string `json:"version"`

	// RunID is null for every command that mints no run. Only the money
	// path has one.
	RunID       *string `json:"run_id"`
	Profile     string  `json:"profile"`
	Environment *string `json:"environment"`
}

// OutcomeBlock is the decision table's answer.
type OutcomeBlock struct {
	Class       outcome.Class `json:"class"`
	ExitCode    int           `json:"exit_code"`
	Money       outcome.Money `json:"money"`
	SameKeySafe bool          `json:"same_key_safe"`
	Next        string        `json:"next"`
	Warnings    []string      `json:"warnings"`
}

// HTTPBlock is what the answer's headers said.
type HTTPBlock struct {
	Status            int     `json:"status"`
	RequestID         *string `json:"request_id"`
	CommandID         *string `json:"command_id"`
	IdempotencyKey    *string `json:"idempotency_key"`
	Replayed          bool    `json:"replayed"`
	Environment       *string `json:"environment"`
	RetryAfterSeconds *int    `json:"retry_after_seconds"`
}

// NewOutcomeBlock converts an outcome, normalising a nil warning slice to `[]`.
//
// C11's rule applied to our own document: `null` and `[]` are different
// answers, and "this answer carried no warnings" is an empty list, not an
// absent one.
func NewOutcomeBlock(out outcome.Outcome) OutcomeBlock {
	warnings := out.Warnings
	if warnings == nil {
		warnings = []string{}
	}

	return OutcomeBlock{
		Class:       out.Class,
		ExitCode:    out.Exit,
		Money:       out.Money,
		SameKeySafe: out.SameKeySafe,
		Next:        out.Next,
		Warnings:    warnings,
	}
}

// NewHTTPBlock converts an answer's metadata.
func NewHTTPBlock(res api.Result) *HTTPBlock {
	b := &HTTPBlock{
		Status:            res.Meta.Status,
		Replayed:          res.Meta.Replayed,
		RetryAfterSeconds: res.Meta.RetryAfter,
	}

	b.CommandID = optional(res.Meta.CommandID)
	b.IdempotencyKey = optional(res.Meta.IdempotencyKey)
	b.Environment = optional(res.Meta.Environment)

	if res.Envelope != nil {
		b.RequestID = optional(res.Envelope.RequestID)
	}

	return b
}

func optional(s string) *string {
	if s == "" {
		return nil
	}

	return &s
}

// Optional exports [optional] for callers building a Document by hand.
func Optional(s string) *string { return optional(s) }

// WriteDocument writes exactly one JSON document and one newline.
//
// Indented, because the primary reader of `--output json` is a human piping to
// `jq` and the secondary is a program, and only the first cares.
func WriteDocument(w io.Writer, doc Document) error {
	encoded, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return Fault(err, "the outcome document could not be encoded: %v", err)
	}

	if _, err := w.Write(append(encoded, '\n')); err != nil {
		return Fault(err, "the outcome document could not be written: %v", err)
	}

	return nil
}

// Errorf writes a line to the error stream in text mode.
func Errorf(w io.Writer, format string, args ...any) {
	fmt.Fprintf(w, format+"\n", args...)
}

// fields writes an aligned key/value block.
//
// Alignment is computed over the keys actually present, so a block never
// carries the padding of a key it did not print.
type fields struct {
	keys   []string
	values []string
}

func (f *fields) add(key, value string) {
	f.keys = append(f.keys, key)
	f.values = append(f.values, value)
}

func (f *fields) write(w io.Writer, indent string) {
	width := 0
	for _, k := range f.keys {
		if len(k) > width {
			width = len(k)
		}
	}

	for i, k := range f.keys {
		fmt.Fprintf(w, "%s%-*s  %s\n", indent, width, k+":", f.values[i])
	}
}

// None is what an absent scalar renders as. It is a word rather than an empty
// column so that a reader can tell "FERRY did not say" from "the renderer
// dropped it".
const None = "(none)"

// NullList is how a `null` array renders, and EmptyList how `[]` does.
//
// C11 and AC40: these are different answers and must not converge. `null` is
// "the contract enumerates nothing here" — any asset is allowed as far as this
// corridor is concerned; `[]` is "it enumerates an empty set" — none is. A
// renderer that printed the same words for both would tell a caller they may
// send nothing when they may send anything, or the reverse.
const (
	NullList  = "(not enumerated)"
	EmptyList = "(none allowed)"
)

// list renders a StringList keeping `null` and `[]` apart.
func list(l api.StringList) string {
	if !l.Present {
		return NullList
	}

	if len(l.Values) == 0 {
		return EmptyList
	}

	return strings.Join(l.Values, ", ")
}

// List exports [list] so a test can assert the two spellings from outside.
func List(l api.StringList) string { return list(l) }

func stringOr(p *string, fallback string) string {
	if p == nil || *p == "" {
		return fallback
	}

	return *p
}

func sortedScopes(scopes []string) string {
	if len(scopes) == 0 {
		return None
	}

	out := make([]string, len(scopes))
	copy(out, scopes)
	sort.Strings(out)

	return strings.Join(out, ", ")
}
