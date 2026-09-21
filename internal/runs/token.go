package runs

import (
	"encoding/json"
	"regexp"
)

// planTokenRE matches a plan token anywhere in a document.
//
// The shape is the one the API hands out (ferry_plan_ followed by the
// token's characters). The lower bound of 8 is there so the recorder's
// ferry_plan_PLACEHOLDER is matched too: a fixture token on disk would make
// AC82's sweep pass for the wrong reason.
var planTokenRE = regexp.MustCompile(`ferry_plan_[0-9A-Za-z_-]{8,}`)

// RedactedToken is what a swept token becomes in a stored response body.
const RedactedToken = "[REDACTED]"

// stripPlanToken removes a plan token from a response body before it is
// written to the record (AC82).
//
// Two passes, and the second is the one that makes the claim true rather than
// likely:
//
//  1. Structural. If the body is a JSON object with a "plan" object holding
//     "token", the key is deleted. This is the only place the API puts it
//     today (simulator.rb:125-129).
//  2. Textual. Whatever survives is swept for the token shape. A body the CLI
//     cannot parse, or a field a later version of the API adds, would
//     otherwise put a live bearer authorisation for a priced transfer on disk
//     in a file the run ledger is not allowed to hold one in.
//
// The fresh simulate 201 is the token's only carrier, and the execute step's
// plan_token field — written by SetPlan, not by this — is its only permitted
// resting place (C6).
func stripPlanToken(body json.RawMessage) json.RawMessage {
	if len(body) == 0 {
		return body
	}

	if stripped, ok := stripPlanTokenStructurally(body); ok {
		body = stripped
	}

	if planTokenRE.Match(body) {
		body = planTokenRE.ReplaceAll(body, []byte(RedactedToken))
	}
	return body
}

func stripPlanTokenStructurally(body json.RawMessage) (json.RawMessage, bool) {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(body, &doc); err != nil {
		return body, false
	}
	rawPlan, ok := doc["plan"]
	if !ok {
		return body, false
	}
	var plan map[string]json.RawMessage
	if err := json.Unmarshal(rawPlan, &plan); err != nil {
		return body, false
	}
	if _, has := plan["token"]; !has {
		return body, false
	}
	delete(plan, "token")

	newPlan, err := json.Marshal(plan)
	if err != nil {
		return body, false
	}
	doc["plan"] = newPlan
	out, err := json.Marshal(doc)
	if err != nil {
		return body, false
	}
	return out, true
}

// ContainsPlanToken reports whether b holds something shaped like a plan
// token. It is exported so a later unit's render test can assert the same
// property over its own output (AC57).
func ContainsPlanToken(b []byte) bool { return planTokenRE.Match(b) }
