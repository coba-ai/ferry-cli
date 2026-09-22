package noun

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/coba-ai/ferry-cli/internal/creds"
	"github.com/coba-ai/ferry-cli/internal/fsx"
	"github.com/coba-ai/ferry-cli/internal/outcome"
	"github.com/coba-ai/ferry-cli/internal/render"
	"github.com/coba-ai/ferry-cli/internal/xdg"
)

// AC43: a command whose endpoint requires a class the profile lacks exits 3
// with zero requests and names the remediation.
//
// "Zero requests" is asserted here structurally — no client exists until the
// last line of Session — and over the wire in `keys/precheck_test.go`, which
// counts the fixture's request log. Both, because this file can only prove
// that *this* function sends nothing, and the claim is about the command.

// Canary credentials. Forty-three characters after the prefix, which is what
// `lib/ferry/tokens.rb:57-58` requires and what `creds.Classify` enforces; the
// helper asserts the length rather than trusting a hand count.
func canary(t *testing.T, tag string) string {
	t.Helper()

	body := strings.ToUpper("canary" + tag)
	if len(body) > 43 {
		t.Fatalf("canary tag %q is too long", tag)
	}

	body += strings.Repeat("0", 43-len(body))

	if len(body) != 43 {
		t.Fatalf("canary body is %d characters, want 43", len(body))
	}

	return body
}

func sandboxKey(t *testing.T) string { return "ferry_sk_sandbox_" + canary(t, "sk") }
func patToken(t *testing.T) string   { return "ferry_pat_" + canary(t, "pat") }

// profileWith writes a credentials file holding exactly the tokens named.
func profileWith(t *testing.T, apiURL string, tokens ...string) string {
	t.Helper()

	home := t.TempDir()
	paths := xdg.Paths{ConfigDir: home, StateDir: home, Home: home}

	if err := paths.EnsureAll(fsx.OS()); err != nil {
		t.Fatalf("create %s: %v", home, err)
	}

	file := creds.NewFile()

	for _, token := range tokens {
		err := file.Put(creds.DefaultProfile, apiURL, creds.Credential{
			Token:       token,
			PrincipalID: "principal_for_" + token[:12],
			StoredAt:    time.Unix(0, 0).UTC(),
		})
		if err != nil {
			t.Fatalf("put %s: %v", token, err)
		}
	}

	if err := creds.NewStore(fsx.OS(), paths.CredentialsFile()).Save(file); err != nil {
		t.Fatalf("save credentials: %v", err)
	}

	return home
}

func depsFor(home string) Deps {
	g := &Globals{Profile: creds.DefaultProfile, Output: string(render.ModeText)}

	return Deps{
		Globals: g,
		Lookup:  xdg.MapEnv(map[string]string{"FERRY_HOME": home}),
		Now:     func() time.Time { return time.Unix(0, 0).UTC() },
	}
}

func bareCommand() *cobra.Command {
	return NewCommand("probe", "a command with no verbs", "")
}

func exitOf(t *testing.T, err error) int {
	t.Helper()

	if err == nil {
		return 0
	}

	coder, ok := err.(interface{ ExitCode() int })
	if !ok {
		t.Fatalf("error %v carries no exit code; every refusal must", err)
	}

	return coder.ExitCode()
}

// The requirement table is the pre-check's whole authority, so it is held to
// `outcome.Operations` in both directions. A subset check in either direction
// is green while the table is half empty: an operation with no row would be
// refused at the call site with a message nobody wrote, and a row for an
// operation that does not exist is a rule nothing consults.
func TestEveryOperationHasExactlyOneCredentialRequirement(t *testing.T) {
	if len(outcome.Operations) < 10 {
		t.Fatalf("the contract declares %d operations; with fewer than ten this comparison is "+
			"not the census it claims to be", len(outcome.Operations))
	}

	declared := map[outcome.Operation]bool{}
	for op := range Requirements {
		declared[op] = true
	}

	for _, op := range outcome.Operations {
		if !declared[op] {
			t.Errorf("operation %q has no row in noun.Requirements, so no command can say which "+
				"credential class its endpoint accepts", op)
		}

		delete(declared, op)
	}

	for op := range declared {
		t.Errorf("noun.Requirements has a row for %q, which is not an operation this CLI performs", op)
	}
}

// The other side of the same table: the classes it names are the ones
// §1.2.1 measured in the controllers.
func TestTheRequirementTableMatchesTheControllers(t *testing.T) {
	want := map[outcome.Operation]creds.Requirement{
		// transfers_controller.rb:48, commands_controller.rb:32
		outcome.OpSimulateTransfer: creds.RequireAPIKey,
		outcome.OpExecuteTransfer:  creds.RequireAPIKey,
		outcome.OpGetCommand:       creds.RequireAPIKey,
		// api_keys_controller.rb:27
		outcome.OpListAPIKeys:  creds.RequirePAT,
		outcome.OpCreateAPIKey: creds.RequirePAT,
		outcome.OpGetAPIKey:    creds.RequirePAT,
		outcome.OpRevokeAPIKey: creds.RequirePAT,
		// corridors_controller.rb:35, me_controller.rb:14
		outcome.OpListCorridors: creds.RequireEither,
		outcome.OpGetCorridor:   creds.RequireEither,
		outcome.OpGetMe:         creds.RequireEither,
	}

	if len(want) != len(Requirements) {
		t.Fatalf("this test names %d operations and the table has %d", len(want), len(Requirements))
	}

	for op, req := range want {
		if got := Requirements[op]; got != req {
			t.Errorf("%s requires %v, the controllers require %v", op, got, req)
		}
	}
}

// AC43 as a matrix: for every operation, a profile holding only the wrong
// class is refused, and a profile holding the right one is not.
//
// The second half is the floor. Without it, a Session that refused everything
// would satisfy "the wrong class is refused" completely.
func TestEveryOperationRefusesTheWrongClassAndAcceptsTheRight(t *testing.T) {
	apiURL := "https://ferry.example"

	refusals := 0
	acceptances := 0

	for _, op := range outcome.Operations {
		req, ok := RequirementFor(op)
		if !ok {
			t.Fatalf("%s has no requirement; the census above should have caught this", op)
		}

		var wrong, right []string

		switch req {
		case creds.RequireAPIKey:
			wrong = []string{patToken(t)}
			right = []string{sandboxKey(t)}
		case creds.RequirePAT:
			wrong = []string{sandboxKey(t)}
			right = []string{patToken(t)}
		case creds.RequireEither:
			// "Either" has no wrong class, so the refusal it can make is
			// "neither is stored". A profile with no credential at all is
			// the only way this endpoint is unreachable.
			wrong = nil
			right = []string{patToken(t)}
		}

		local, err := OpenLocal(depsFor(profileWith(t, apiURL, wrong...)))
		if err != nil {
			t.Fatalf("%s: open local: %v", op, err)
		}

		session, err := local.Session(bareCommand(), op)
		if err == nil {
			t.Errorf("%s: a profile holding %v was given a session for an endpoint that accepts %v",
				op, wrong, req)
		} else {
			refusals++

			if code := exitOf(t, err); code != 3 {
				t.Errorf("%s: the refusal exited %d, want 3 (refused_fix: nothing sent, nothing spent)", op, code)
			}

			if !strings.Contains(err.Error(), "ferry auth login") && !strings.Contains(err.Error(), "ferry keys create") {
				t.Errorf("%s: the refusal names no remediation: %q", op, err.Error())
			}

			if session != nil {
				t.Errorf("%s: a session was returned alongside the refusal; the client must not exist", op)
			}
		}

		local, err = OpenLocal(depsFor(profileWith(t, apiURL, right...)))
		if err != nil {
			t.Fatalf("%s: open local: %v", op, err)
		}

		session, err = local.Session(bareCommand(), op)
		if err != nil {
			t.Errorf("%s: a profile holding %v was refused: %v", op, right, err)

			continue
		}

		acceptances++

		if session.Client == nil {
			t.Errorf("%s: the session has no client", op)
		}

		if session.Client.BaseURL != apiURL {
			t.Errorf("%s: the client is pointed at %q, not the profile's %q", op, session.Client.BaseURL, apiURL)
		}
	}

	if refusals != len(outcome.Operations) || acceptances != len(outcome.Operations) {
		t.Errorf("%d refusals and %d acceptances over %d operations; both must be every operation "+
			"or this matrix is not the census it reads as",
			refusals, acceptances, len(outcome.Operations))
	}
}

// The remediation has to name the class a human can go and get. "pat" is the
// store's spelling and nobody types it; the sentence must name the thing.
func TestTheRemediationNamesHowToGetTheMissingCredential(t *testing.T) {
	cases := []struct {
		req      creds.Requirement
		contains []string
	}{
		{creds.RequirePAT, []string{"personal access token", "ferry:pat:issue", "--token-stdin"}},
		{creds.RequireAPIKey, []string{"ferry keys create", "ferry_sk_"}},
		{creds.RequireEither, []string{"ferry auth login"}},
	}

	for _, tc := range cases {
		got := remediationFor(tc.req)
		for _, want := range tc.contains {
			if !strings.Contains(got, want) {
				t.Errorf("the remediation for %v does not mention %q: %q", tc.req, want, got)
			}
		}
	}
}

// The refusal for an empty profile has to read as a sentence (A419).
//
// It did not, for commands accepting either credential class: the branch
// wrapped `remediationFor`'s complete sentence in "Run `ferry auth login
// --api <url>` with a … first." and produced "…with a Store either
// credential with `ferry auth login --api <url> --token-stdin`. first."
//
// This is the branch a new operator reaches first — no profile at all — and
// the only one of the three whose text nothing read. So the assertion is not
// "it mentions the login command", which the mangled sentence also satisfied.
// It is that no remediation is embedded mid-clause, over every requirement.
func TestTheEmptyProfileRefusalReadsAsASentence(t *testing.T) {
	// The refusal reads nothing but the profile name, so this needs no
	// store, no file and no filesystem: an empty Globals answers
	// `Profile()` with the default, which is the name an operator with no
	// profile at all would see.
	local := &Local{Globals: &Globals{}}

	for _, req := range []creds.Requirement{creds.RequirePAT, creds.RequireAPIKey, creds.RequireEither} {
		err := local.refuseCredential(req, fmt.Errorf("%w: default", creds.ErrNoProfile))
		if err == nil {
			t.Fatalf("%v: an absent profile was not refused", req)
		}

		got := err.Error()

		// The remediation must be its own sentence: preceded by the end of
		// one and not followed by the tail of another.
		remediation := remediationFor(req)

		at := strings.Index(got, remediation)
		if at < 0 {
			t.Errorf("%v: the refusal does not carry the remediation at all:\n%s", req, got)

			continue
		}

		if before := got[:at]; !strings.HasSuffix(before, "\n") {
			t.Errorf("%v: the remediation is spliced into a clause rather than starting a "+
				"line — %q precedes it:\n%s", req, lastRunes(before, 40), got)
		}

		if after := strings.TrimSpace(got[at+len(remediation):]); after != "" {
			t.Errorf("%v: %q trails the remediation, so the sentence continues after it had "+
				"ended:\n%s", req, after, got)
		}
	}
}

// lastRunes is the tail of s, for a message that should quote the words next
// to the fault rather than the whole refusal.
func lastRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}

	return string(r[len(r)-n:])
}

// A credential bound to one endpoint is not handed to another (C7, AC9).
func TestACredentialIsNotHandedToADifferentEndpoint(t *testing.T) {
	home := profileWith(t, "https://ferry.example", patToken(t))

	deps := depsFor(home)
	deps.Globals.APIURL = "https://other.example"

	local, err := OpenLocal(deps)
	if err != nil {
		t.Fatalf("open local: %v", err)
	}

	if _, err := local.Session(bareCommand(), outcome.OpListAPIKeys); err == nil {
		t.Fatal("a credential verified against ferry.example was offered to other.example")
	} else if code := exitOf(t, err); code != 3 {
		t.Errorf("exit %d, want 3", code)
	}
}

// FERRY_TOKEN replaces the slot of its own class and no other (§5.2), and it
// is never written to the file.
func TestTheEnvironmentTokenOverridesOnlyItsOwnSlotAndIsNeverStored(t *testing.T) {
	apiURL := "https://ferry.example"
	home := profileWith(t, apiURL, sandboxKey(t))

	ephemeral := "ferry_pat_" + canary(t, "ephemeral")

	deps := depsFor(home)
	deps.Lookup = xdg.MapEnv(map[string]string{"FERRY_HOME": home, "FERRY_TOKEN": ephemeral})

	local, err := OpenLocal(deps)
	if err != nil {
		t.Fatalf("open local: %v", err)
	}

	// The PAT slot was empty and the endpoint needs one: the override fills
	// it.
	session, err := local.Session(bareCommand(), outcome.OpListAPIKeys)
	if err != nil {
		t.Fatalf("FERRY_TOKEN did not satisfy a PAT endpoint: %v", err)
	}

	if session.Client.Token != ephemeral {
		t.Errorf("the client sends %q, not the FERRY_TOKEN", session.Client.Token)
	}

	// The API key slot is untouched: a PAT in the environment does not make
	// a money command send a PAT.
	session, err = local.Session(bareCommand(), outcome.OpGetCommand)
	if err != nil {
		t.Fatalf("the stored API key was displaced by FERRY_TOKEN: %v", err)
	}

	if session.Client.Token != sandboxKey(t) {
		t.Errorf("an API-key endpoint was given %q", session.Client.Token)
	}

	raw, err := fsx.OS().ReadFile(filepath.Join(home, "credentials.json"))
	if err != nil {
		t.Fatalf("read credentials: %v", err)
	}

	if strings.Contains(string(raw), ephemeral) {
		t.Error("FERRY_TOKEN reached the credentials file; §5.2 says it is ephemeral and never written")
	}
}

// Both directions on the `--output` vocabulary, so a third mode cannot be
// added without the parser and the list agreeing.
func TestOutputModeVocabulary(t *testing.T) {
	for _, mode := range render.Modes {
		if _, err := render.ParseMode(string(mode)); err != nil {
			t.Errorf("render.Modes names %q and ParseMode refuses it", mode)
		}
	}

	if _, err := render.ParseMode("yaml"); err == nil {
		t.Error("ParseMode accepted a mode render.Modes does not name")
	}
}

func TestEnvironmentVocabulary(t *testing.T) {
	var names []string

	for _, env := range Environments {
		names = append(names, string(env))

		if _, err := ParseEnvironment(string(env)); err != nil {
			t.Errorf("Environments names %q and ParseEnvironment refuses it", env)
		}
	}

	sort.Strings(names)

	if strings.Join(names, ",") != "live,sandbox" {
		t.Errorf("the environments are %v; the contract has exactly sandbox and live", names)
	}

	if _, err := ParseEnvironment("staging"); err == nil {
		t.Error("ParseEnvironment accepted an environment the contract does not declare")
	}
}
