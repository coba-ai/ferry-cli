package render

import (
	"fmt"
	"regexp"
	"sort"
	"sync/atomic"
)

// AC42: text mode prints a secret in exactly two places.
//
// "Exactly two" is a claim in both directions and this file is what makes the
// second direction checkable. Every secret this CLI is ever allowed to write
// to a stream goes through [Reveal], which refuses an undeclared site; the
// declared sites are [Sites] and there are two of them.
//
// A sink alone would only be a convention. Three independent things hold it
// up, and `secrets_test.go` runs all three:
//
//  1. **Syntax.** An AST sweep over every non-test Go file under `cli/` finds
//     every call to Reveal and holds the set of site constants it passes to
//     [Sites] — so a third site cannot be added without the sweep seeing it,
//     and a unit that prints a token without the sink is caught by (2).
//  2. **Behaviour.** Every command this unit owns is run against the fixture
//     with a canary credential, and the transcript is scanned with
//     [FindSecrets]. The only (command, secret) pair that may match is the
//     one [SiteKeysCreateToken] names.
//  3. **Runtime.** [RevealCount] says which sites actually fired during a
//     command, so "the token was printed by the permitted site" is measured
//     rather than inferred from the text.
//
// The floors matter as much as the assertions. A sweep that found no calls, or
// a scan whose detector cannot recognise a secret, would satisfy "no other
// path prints one" by comparing two empty sets — the defect this project has
// shipped four times. Each of the three has a positive control that proves it
// can fire.

// SiteID names one place a secret may be written.
type SiteID string

const (
	// SiteKeysCreateToken is the plaintext API key on the `201` of
	// `POST /v1/api_keys`. FERRY returns it once and has no endpoint that
	// will return it again (`api_key_serializer.rb:40`), so a CLI that did
	// not print it would have minted a credential nobody holds.
	SiteKeysCreateToken SiteID = "keys.create.token"

	// SiteSimulatePlanToken is `plan.token` on a fresh simulate `201`. U5's,
	// and declared here because AC42's count is over the whole binary: a
	// registry that only listed this unit's site could not make the claim.
	SiteSimulatePlanToken SiteID = "transfers.simulate.plan_token"
)

// Site is one declared place, with the unit that writes it.
type Site struct {
	ID SiteID

	// Owner is the unit of PLAN §4.1 whose code calls Reveal here.
	Owner string

	// Secret names the value, and Why says why printing it is the only
	// honest thing to do. A registry without reasons is a subset check with
	// extra steps.
	Secret string
	Why    string
}

// Sites is the whole of AC42's "exactly two".
var Sites = []Site{
	{
		ID:     SiteKeysCreateToken,
		Owner:  "U4",
		Secret: "ApiKey.token on the POST /v1/api_keys 201",
		Why:    "FERRY hands the plaintext key out once and has no endpoint that returns it again; a key minted and not shown is a credential nobody holds and nobody can revoke by prefix.",
	},
	{
		ID:     SiteSimulatePlanToken,
		Owner:  "U5",
		Secret: "Simulation.plan.token on a fresh POST /v1/transfers/simulate 201",
		Why:    "The plan token is the single-use authorisation POST /v1/transfers takes; it is never stored and a replay does not carry it, so the one answer that has it is the only chance to hand it to the caller.",
	},
}

// revealCounts is the runtime half. The map is built once at init and never
// written again, so concurrent reads are safe; the counters are atomic.
var revealCounts = func() map[SiteID]*atomic.Int64 {
	m := make(map[SiteID]*atomic.Int64, len(Sites))
	for _, s := range Sites {
		m[s.ID] = &atomic.Int64{}
	}

	return m
}()

// SiteFor looks a declared site up.
func SiteFor(id SiteID) (Site, bool) {
	for _, s := range Sites {
		if s.ID == id {
			return s, true
		}
	}

	return Site{}, false
}

// SiteIDs is the declared set, sorted.
func SiteIDs() []SiteID {
	out := make([]SiteID, 0, len(Sites))
	for _, s := range Sites {
		out = append(out, s.ID)
	}

	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })

	return out
}

// Reveal returns a secret for printing, at a declared site.
//
// It panics for a site [Sites] does not declare. A returned error would be
// ignorable, and the thing being decided is whether a bearer credential
// reaches a terminal: the safe failure is the loud one.
func Reveal(id SiteID, secret string) string {
	if _, ok := SiteFor(id); !ok {
		panic(fmt.Sprintf(
			"render: %q is not a declared secret site; AC42 says a secret is printed in exactly "+
				"the places render.Sites names, and adding one is an amendment (PLAN §11.1)", id))
	}

	revealCounts[id].Add(1)

	return secret
}

// RevealCount is how many times a site has revealed in this process.
func RevealCount(id SiteID) int64 {
	c, ok := revealCounts[id]
	if !ok {
		return 0
	}

	return c.Load()
}

// RevealCounts is the whole runtime census.
func RevealCounts() map[SiteID]int64 {
	out := make(map[SiteID]int64, len(revealCounts))
	for id, c := range revealCounts {
		out[id] = c.Load()
	}

	return out
}

// secretShapes is what a secret looks like on a stream.
//
// The shapes are the token regexes of `lib/ferry/tokens.rb:57-58` (PLAN
// §1.2.2) with their exact 43-character bodies, the plan token, and the two
// stand-ins the recorder writes. The exact lengths are load-bearing in the
// other direction: `creds.Credential.Display` prints `ferry_pat_` plus the
// first eight characters of the secret and the last four, which AC36 requires
// and which a loose `ferry_pat_\w+` would report as a leak — a detector that
// cried wolf on the render AC36 demands would be turned off within a week.
var secretShapes = []*regexp.Regexp{
	regexp.MustCompile(`ferry_sk_(?:sandbox|live)_[0-9A-Za-z]{43}`),
	regexp.MustCompile(`ferry_pat_[0-9A-Za-z]{43}`),
	regexp.MustCompile(`ferry_plan_[0-9A-Za-z_-]{8,}`),

	// The recorder substitutes these for the real values it captured
	// (`spec/support/cli/recorder.rb`). A fixture-shaped secret is still a
	// secret for the purpose of this scan: without them the behavioural half
	// of AC42 would have nothing to find, and "no command printed a secret"
	// would be true because no secret was ever in play.
	regexp.MustCompile(`API_KEY_TOKEN_PLACEHOLDER_\d+`),
	regexp.MustCompile(`PAT_TOKEN_PLACEHOLDER_\d+`),
}

// FindSecrets reports every secret-shaped run in text, in order of appearance.
func FindSecrets(text string) []string {
	type hit struct {
		at    int
		value string
	}

	var hits []hit

	for _, re := range secretShapes {
		for _, loc := range re.FindAllStringIndex(text, -1) {
			hits = append(hits, hit{at: loc[0], value: text[loc[0]:loc[1]]})
		}
	}

	sort.SliceStable(hits, func(i, j int) bool { return hits[i].at < hits[j].at })

	out := make([]string, 0, len(hits))
	for _, h := range hits {
		out = append(out, h.value)
	}

	return out
}

// ContainsSecret reports whether text holds anything secret-shaped.
func ContainsSecret(text string) bool { return len(FindSecrets(text)) > 0 }
