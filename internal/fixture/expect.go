package fixture

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"regexp"
	"sort"
	"strings"
)

// Checks counts the assertions the loader actually evaluated, per axis.
//
// It is incremented at the comparison, not derived from the manifest, so it
// answers "what did this run enforce" rather than "what could it have
// enforced". A floor over these counters is what stops the whole of AC77
// passing by asserting nothing: a loader that stopped finding recordings, or
// an axis whose enforcement was deleted, compares two empty sets and is green
// on every mismatch it was written to catch.
type Checks struct {
	Files        int // recordings read off disk
	Digests      int // sha256 comparisons made
	Interactions int // expectation-to-interaction pairings checked

	Status         int
	CodeLiteral    int
	CodePattern    int
	CodeAbsent     int
	HeadersPresent int
	HeadersAbsent  int
	BodyHas        int
	BodyLacks      int
	BodyEquals     int
}

func (c *Checks) add(other Checks) {
	c.Files += other.Files
	c.Digests += other.Digests
	c.Interactions += other.Interactions
	c.Status += other.Status
	c.CodeLiteral += other.CodeLiteral
	c.CodePattern += other.CodePattern
	c.CodeAbsent += other.CodeAbsent
	c.HeadersPresent += other.HeadersPresent
	c.HeadersAbsent += other.HeadersAbsent
	c.BodyHas += other.BodyHas
	c.BodyLacks += other.BodyLacks
	c.BodyEquals += other.BodyEquals
}

// Check asserts one expectation against one recorded response (AC77).
//
// It returns every disagreement rather than the first: a directory the loader
// refuses is a directory someone has to fix, and one mismatch at a time is
// several rounds of that.
func (e Expectation) Check(resp RecordedResponse, tally *Checks) []string {
	var problems []string

	tally.Status++

	if resp.Status != e.Status {
		problems = append(problems, fmt.Sprintf("status: expected %d, recorded %d", e.Status, resp.Status))
	}

	var body any
	if len(resp.Body) > 0 {
		if err := json.Unmarshal(resp.Body, &body); err != nil {
			problems = append(problems, fmt.Sprintf("body: recorded response body is not JSON: %v", err))
		}
	}

	problems = append(problems, e.checkCode(body, tally)...)
	problems = append(problems, e.checkHeaders(resp.Headers, tally)...)
	problems = append(problems, e.checkBody(body, tally)...)

	return problems
}

func (e Expectation) checkCode(body any, tally *Checks) []string {
	got, found := dig(body, "error.code")
	code, isString := got.(string)

	if e.Code == nil {
		tally.CodeAbsent++

		if found && got != nil {
			return []string{fmt.Sprintf("code: expected no error code, recorded %v", got)}
		}

		return nil
	}

	want := *e.Code

	if !found || got == nil {
		tally.CodeLiteral++

		return []string{fmt.Sprintf("code: expected %q, recorded body has no error.code", want)}
	}

	if !isString {
		tally.CodeLiteral++

		return []string{fmt.Sprintf("code: expected %q, recorded error.code is %T", want, got)}
	}

	if pattern, ok := patternOf(want); ok {
		tally.CodePattern++

		re, err := regexp.Compile(pattern)
		if err != nil {
			return []string{fmt.Sprintf("code: expectation %q is not a valid regular expression: %v", want, err)}
		}

		if !re.MatchString(code) {
			return []string{fmt.Sprintf("code: expected a match for %q, recorded %q", want, code)}
		}

		return nil
	}

	tally.CodeLiteral++

	if code != want {
		return []string{fmt.Sprintf("code: expected %q, recorded %q", want, code)}
	}

	return nil
}

// patternOf recognises the `/regex/` form the manifest uses for
// `execute.policy_denied.403`, whose code is one of several `POLICY_*`
// verdicts.
func patternOf(value string) (string, bool) {
	if len(value) >= 2 && strings.HasPrefix(value, "/") && strings.HasSuffix(value, "/") {
		return value[1 : len(value)-1], true
	}

	return "", false
}

func (e Expectation) checkHeaders(headers map[string]string, tally *Checks) []string {
	var problems []string

	present := make(map[string]string, len(headers))
	for name, value := range headers {
		present[http.CanonicalHeaderKey(name)] = value
	}

	for _, name := range e.HeadersPresent {
		tally.HeadersPresent++

		if _, ok := present[http.CanonicalHeaderKey(name)]; !ok {
			problems = append(problems, fmt.Sprintf("headers_present: %s is not in the recorded response", name))
		}
	}

	for _, name := range e.HeadersAbsent {
		tally.HeadersAbsent++

		if value, ok := present[http.CanonicalHeaderKey(name)]; ok {
			problems = append(problems, fmt.Sprintf("headers_absent: %s is in the recorded response (%q)", name, value))
		}
	}

	return problems
}

func (e Expectation) checkBody(body any, tally *Checks) []string {
	var problems []string

	for _, path := range e.BodyHas {
		tally.BodyHas++

		value, found := dig(body, path)
		if !found {
			problems = append(problems, fmt.Sprintf("body_has: %s is absent from the recorded body", path))
			continue
		}

		if value == nil {
			problems = append(problems, fmt.Sprintf("body_has: %s is null in the recorded body", path))
		}
	}

	for _, path := range e.BodyLacks {
		tally.BodyLacks++

		value, found := dig(body, path)
		if found && value != nil {
			problems = append(problems, fmt.Sprintf("body_lacks: %s is present in the recorded body (%v)", path, brief(value)))
		}
	}

	for _, path := range sortedKeys(e.BodyEquals) {
		tally.BodyEquals++

		want := e.BodyEquals[path]

		value, found := dig(body, path)
		if !found {
			problems = append(problems, fmt.Sprintf("body_equals: %s is absent from the recorded body, expected %v", path, brief(want)))
			continue
		}

		if !reflect.DeepEqual(value, want) {
			problems = append(problems, fmt.Sprintf("body_equals: %s is %v, expected %v", path, brief(value), brief(want)))
		}
	}

	return problems
}

// dig resolves a dotted path through decoded JSON.
//
// The second return distinguishes "absent" from "present and null", which is
// the whole difference between body_has and body_lacks: nineteen of the
// recordings' body_lacks paths are keys the app rendered as `null`.
func dig(value any, path string) (any, bool) {
	current := value

	for _, segment := range strings.Split(path, ".") {
		object, ok := current.(map[string]any)
		if !ok {
			return nil, false
		}

		next, ok := object[segment]
		if !ok {
			return nil, false
		}

		current = next
	}

	return current, true
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}

	sort.Strings(keys)

	return keys
}

func brief(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return fmt.Sprintf("%v", value)
	}

	const limit = 120
	if len(encoded) > limit {
		return string(encoded[:limit]) + "…"
	}

	return string(encoded)
}
