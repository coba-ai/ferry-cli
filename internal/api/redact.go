package api

import (
	"regexp"
	"strings"
)

// Redaction is a choke point, not a habit.
//
// C6. `--debug` output exists to be pasted into a bug report, so the question
// is not "did we remember to redact this call site" but "can a secret reach
// the stream at all". Every byte the debug writer emits goes through Redact,
// and nothing else writes to that stream — which is why the rules below are
// about *shapes* rather than about the two or three places a token is known to
// appear today.
//
// The `"token"` rule is the one that earns its keep. `plan.token` is printed
// once and never stored, and a caller debugging a failing execute is exactly
// the caller most likely to paste the simulate trace that carries it.

// Placeholder is what a redacted value is replaced with. A fixed string, so a
// reader of a trace can tell "removed" from "absent" — the second would be a
// bug worth reporting and the first is not.
const Placeholder = "[redacted]"

var (
	// Any `Authorization` header, whatever the scheme and however the header
	// is capitalised. The value is taken to end at the line, because that is
	// where a header value ends in every format this CLI prints.
	authorizationRE = regexp.MustCompile(`(?i)(authorization\s*:\s*)[^\r\n]*`)

	// The three token prefixes, keeping the prefix so a trace still shows
	// *which kind* of credential was involved. `ferry_plan_` is 43
	// alphanumerics per the contract; the others are not pinned to a length,
	// so this matches greedily over the token character set rather than
	// counting.
	tokenRE = regexp.MustCompile(`\bferry_(sk|pat|plan)_[A-Za-z0-9_\-]+`)

	// Any JSON member named `token`, at any nesting depth, including
	// `plan.token`. Matches the rendered form, which is what the debug
	// writer emits.
	jsonTokenRE = regexp.MustCompile(`("(?:[a-z_]*_)?token"\s*:\s*)"[^"]*"`)

	// The same member in Go's `%v` rendering of a map, which is what an
	// unstructured debug dump of a decoded body looks like.
	goMapTokenRE = regexp.MustCompile(`\b((?:[a-z_]*_)?token:)\s*[^\s\]}]+`)
)

// Redact removes every credential shape from a string bound for a debug trace.
//
// The order is deliberate: the `Authorization` rule runs first and takes the
// whole line, so a scheme this CLI has never seen is still removed rather than
// surviving because its value did not look like a token.
func Redact(s string) string {
	s = authorizationRE.ReplaceAllString(s, "${1}"+Placeholder)
	s = jsonTokenRE.ReplaceAllString(s, `${1}"`+Placeholder+`"`)
	s = goMapTokenRE.ReplaceAllString(s, "${1}"+Placeholder)
	s = tokenRE.ReplaceAllStringFunc(s, func(m string) string {
		// Keep the prefix, drop the secret.
		i := strings.LastIndex(m, "_")

		return m[:i+1] + Placeholder
	})

	return s
}

// RedactedHeaderValue is what a header's value becomes in a debug trace.
// Exported so the same rule can be applied to a structured dump without
// re-implementing it.
func RedactedHeaderValue(name, value string) string {
	if strings.EqualFold(name, "authorization") {
		return Placeholder
	}

	return Redact(value)
}
