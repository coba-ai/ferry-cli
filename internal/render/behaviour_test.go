package render_test

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/kurenn/ferry/cli/internal/creds"
	"github.com/kurenn/ferry/cli/internal/fixture"
	"github.com/kurenn/ferry/cli/internal/fsx"
	"github.com/kurenn/ferry/cli/internal/harness"
	"github.com/kurenn/ferry/cli/internal/noun"
	"github.com/kurenn/ferry/cli/internal/noun/auth"
	"github.com/kurenn/ferry/cli/internal/noun/corridors"
	"github.com/kurenn/ferry/cli/internal/noun/keys"
	"github.com/kurenn/ferry/cli/internal/render"
	"github.com/kurenn/ferry/cli/internal/xdg"
)

// The behavioural half of AC42 (check 3 of the three in `secrets_test.go`).
//
// Every command this unit owns is run against the fixture with canary
// credentials in the profile, in both output modes, and both streams are
// scanned with `render.FindSecrets`. Exactly one command may match.
//
// No test here calls t.Parallel(): `harness.Run` swaps the process streams and
// uses t.Setenv (A314).

const canaryBody = "CANARYRENDER00000000000000000000000000000000"

func canaryPAT() string     { return "ferry_pat_" + canaryBody[:43] }
func canaryKey() string     { return "ferry_sk_sandbox_" + canaryBody[:43] }
func canaryLive() string    { return "ferry_sk_live_" + canaryBody[:43] }
func planToken() string     { return "ferry_plan_" + canaryBody[:32] }
func recorderToken() string { return "API_KEY_TOKEN_PLACEHOLDER_1" }

// sweep is one command, arranged so it reaches as far into its own path as the
// fixture allows.
type sweep struct {
	// name is `<noun> <verb>` and is matched against the cobra trees below,
	// so a command added to one of the three packages and not listed here
	// fails the coverage check rather than going unswept.
	name string

	command   func(noun.Deps) *cobra.Command
	args      []string
	stdin     string
	scenarios []string

	// tokens seed the profile. Empty means no credentials file at all,
	// which is what `auth login` needs.
	tokens []string

	// secretExpected is the whole of AC42's positive set. Exactly one row
	// may set it.
	secretExpected bool
}

func sweeps() []sweep {
	return []sweep{
		{
			name:      "auth login",
			command:   auth.Command,
			args:      []string{"login", "--token-stdin"},
			stdin:     canaryPAT(),
			scenarios: []string{"me.pat.200"},
		},
		{
			name:    "auth whoami",
			command: auth.Command,
			args:    []string{"whoami"},
			tokens:  []string{canaryPAT(), canaryKey()},
		},
		{
			name:    "auth logout",
			command: auth.Command,
			args:    []string{"logout"},
			tokens:  []string{canaryPAT(), canaryKey()},
		},
		{
			name:      "keys list",
			command:   keys.Command,
			args:      []string{"list", "--limit", "2"},
			scenarios: []string{"keys.list.page1.200"},
			tokens:    []string{canaryPAT()},
		},
		{
			name:      "keys get",
			command:   keys.Command,
			args:      []string{"get", "key_PLACEHOLDER_1"},
			scenarios: []string{"keys.get.200"},
			tokens:    []string{canaryPAT()},
		},
		{
			name:    "keys create",
			command: keys.Command,
			args: []string{
				"create", "--env", "sandbox",
				"--name", "cli-recorded-key", "--scopes", "read,money:simulate",
			},
			scenarios:      []string{"keys.create.sandbox.201"},
			tokens:         []string{canaryPAT()},
			secretExpected: true,
		},
		{
			name:      "keys revoke",
			command:   keys.Command,
			args:      []string{"revoke", "key_PLACEHOLDER_1", "--reason", "rotated"},
			scenarios: []string{"keys.revoke.200"},
			tokens:    []string{canaryPAT()},
		},
		{
			name:      "corridors list",
			command:   corridors.Command,
			args:      []string{"list"},
			scenarios: []string{"corridors.list.200"},
			tokens:    []string{canaryKey()},
		},
		{
			name:      "corridors get",
			command:   corridors.Command,
			args:      []string{"get", "walletCrypto->cash"},
			scenarios: []string{"corridors.get.walletCrypto_cash.200"},
			tokens:    []string{canaryKey()},
		},
	}
}

// Exactly one command prints a secret, and it is the one AC42 names.
//
// Both directions, per command and per mode: the positive row must match and
// every other row must not. A run that failed before it printed anything would
// satisfy the negative rows for the wrong reason, so each is required to have
// reached the server it was given.
func TestExactlyOneCommandPrintsASecret(t *testing.T) {
	all := sweeps()

	if len(all) == 0 {
		t.Fatal("the sweep is empty")
	}

	positives := 0

	for _, s := range all {
		if s.secretExpected {
			positives++
		}
	}

	if positives != 1 {
		t.Fatalf("%d commands are expected to print a secret; AC42 gives this unit exactly one "+
			"(the other is U5's plan token)", positives)
	}

	for _, s := range all {
		for _, mode := range []string{"text", "json"} {
			t.Run(s.name+" in "+mode, func(t *testing.T) {
				server, home := arrange(t, s)

				args := append([]string{}, s.args...)
				args = append(args, "--api", server.URL(), "--output", mode)

				globals := &noun.Globals{}

				deps := noun.Deps{
					Globals: globals,
					HTTP:    &http.Client{},
					Now:     func() time.Time { return time.Unix(1_700_000_000, 0).UTC() },
				}

				cmd := s.command(deps)
				noun.BindGlobals(cmd, globals)

				stdout, stderr, exit := harness.Run(t, cmd, args, s.stdin,
					map[string]string{"FERRY_HOME": home}, false)

				if exit != 0 {
					t.Fatalf("exit %d; a command that failed early proves nothing about what it "+
						"prints\nstdout: %s\nstderr: %s", exit, stdout, stderr)
				}

				found := render.FindSecrets(stdout + stderr)

				switch {
				case s.secretExpected && len(found) == 0:
					t.Errorf("`ferry %s` printed no secret in %s mode. It is the one command that "+
						"must: FERRY hands the plaintext key out once and never again.", s.name, mode)

				case !s.secretExpected && len(found) > 0:
					t.Errorf("`ferry %s` printed %d secret-shaped string(s) in %s mode: %q\n\n"+
						"AC42: a secret is printed at the sites render.Sites declares and nowhere else.\n"+
						"stdout: %s\nstderr: %s", s.name, len(found), mode, found, stdout, stderr)
				}

				// The canary credentials in the profile must never appear,
				// whatever the command. These are the tokens a real
				// operator has stored, and no command in this unit has a
				// reason to echo one back.
				for _, token := range s.tokens {
					if strings.Contains(stdout+stderr, token) {
						t.Errorf("`ferry %s` echoed a stored credential in %s mode", s.name, mode)
					}
				}
			})
		}
	}
}

// And the token that is printed goes out on stdout only, once, at the
// permitted site — measured from the runtime census rather than inferred from
// the text.
func TestThePrintedSecretIsTheOneTheRegistryPermits(t *testing.T) {
	var target sweep

	for _, s := range sweeps() {
		if s.secretExpected {
			target = s
		}
	}

	server, home := arrange(t, target)

	before := render.RevealCount(render.SiteKeysCreateToken)

	globals := &noun.Globals{}

	deps := noun.Deps{
		Globals: globals,
		HTTP:    &http.Client{},
		Now:     func() time.Time { return time.Unix(1_700_000_000, 0).UTC() },
	}

	cmd := target.command(deps)
	noun.BindGlobals(cmd, globals)

	args := append(append([]string{}, target.args...), "--api", server.URL())

	stdout, stderr, exit := harness.Run(t, cmd, args, "", map[string]string{"FERRY_HOME": home}, false)

	if exit != 0 {
		t.Fatalf("exit %d\nstdout: %s\nstderr: %s", exit, stdout, stderr)
	}

	if got := render.RevealCount(render.SiteKeysCreateToken) - before; got != 1 {
		t.Errorf("the permitted site fired %d times, want 1", got)
	}

	if got := strings.Count(stdout, recorderToken()); got != 1 {
		t.Errorf("the token appears %d times on stdout, want 1", got)
	}

	if strings.Contains(stderr, recorderToken()) {
		t.Error("the token was written to the error stream as well")
	}
}

// `--output json` echoes the API body verbatim, token and all, and that is a
// decision rather than a leak.
//
// AC42 is a claim about *text* mode, and this is why the wording matters. The
// JSON document's `response` block is the answer FERRY sent, unaltered — which
// is the only thing a program consuming `keys create --output json` can use,
// since the token is in no other answer and in no other field. What must hold
// is that this is the one body in the unit that carries one, and that the
// text-mode sink is not involved: `render.Reveal` does not fire here, so the
// count in `secrets_test.go` stays a count of text-mode prints.
//
// It is recorded rather than merely permitted so that a later unit reading
// AC42 as "json never prints a secret either" finds the measurement instead of
// guessing.
func TestTheJSONDocumentEchoesTheAPIBodyVerbatimIncludingTheToken(t *testing.T) {
	var target sweep

	for _, s := range sweeps() {
		if s.secretExpected {
			target = s
		}
	}

	server, home := arrange(t, target)

	before := render.RevealCount(render.SiteKeysCreateToken)

	globals := &noun.Globals{}

	deps := noun.Deps{
		Globals: globals,
		HTTP:    &http.Client{},
		Now:     func() time.Time { return time.Unix(1_700_000_000, 0).UTC() },
	}

	cmd := target.command(deps)
	noun.BindGlobals(cmd, globals)

	args := append(append([]string{}, target.args...), "--api", server.URL(), "--output", "json")

	stdout, stderr, exit := harness.Run(t, cmd, args, "", map[string]string{"FERRY_HOME": home}, false)

	if exit != 0 {
		t.Fatalf("exit %d\nstdout: %s\nstderr: %s", exit, stdout, stderr)
	}

	var doc struct {
		Response struct {
			Token       string `json:"token"`
			TokenPrefix string `json:"token_prefix"`
		} `json:"response"`
	}

	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("parse document: %v\n%s", err, stdout)
	}

	if doc.Response.Token != recorderToken() {
		t.Errorf("response.token is %q, want the token FERRY sent", doc.Response.Token)
	}

	if got := strings.Count(stdout, recorderToken()); got != 1 {
		t.Errorf("the token appears %d times in the document, want once — in response.token", got)
	}

	if strings.Contains(stderr, recorderToken()) {
		t.Error("the token reached the error stream")
	}

	if got := render.RevealCount(render.SiteKeysCreateToken) - before; got != 0 {
		t.Errorf("the text-mode sink fired %d times during a JSON run; AC42's count would then be "+
			"counting two different things", got)
	}
}

// The coverage floor: every leaf command in the three packages is swept.
//
// Both directions. A command added to `keys` and not listed above would be
// unswept and the claim "no other path prints a secret" would quietly stop
// covering it — which is exactly how the one-directional subset check gets
// shipped.
func TestTheSweepCoversEveryCommandThisUnitOwns(t *testing.T) {
	deps := noun.Deps{Globals: &noun.Globals{}}

	var declared []string

	for _, root := range []*cobra.Command{
		auth.Command(deps), keys.Command(deps), corridors.Command(deps),
	} {
		for _, sub := range root.Commands() {
			if sub.Name() == "help" || sub.Name() == "completion" {
				continue
			}

			declared = append(declared, root.Name()+" "+sub.Name())
		}
	}

	sort.Strings(declared)

	swept := make([]string, 0, len(sweeps()))
	for _, s := range sweeps() {
		swept = append(swept, s.name)
	}

	sort.Strings(swept)

	if len(declared) == 0 {
		t.Fatal("the three packages declare no subcommands; the comparison below is two empty sets")
	}

	if strings.Join(declared, "\n") != strings.Join(swept, "\n") {
		t.Errorf("the commands this unit declares are\n  %v\nand the sweep covers\n  %v",
			declared, swept)
	}
}

// The canaries themselves have to be recognisable, or every negative row above
// would be satisfied by a detector that cannot see them.
func TestTheCanaryCredentialsAreDetectableSecrets(t *testing.T) {
	for _, token := range []string{canaryPAT(), canaryKey(), canaryLive(), planToken(), recorderToken()} {
		if !render.ContainsSecret(token) {
			t.Errorf("the canary %q is not recognised as a secret, so a command that printed it "+
				"would pass the sweep", token)
		}
	}

	for _, token := range []string{canaryPAT(), canaryKey(), canaryLive()} {
		if _, err := creds.Classify(token); err != nil {
			t.Errorf("the canary %q is not a token this CLI would store: %v", token, err)
		}
	}
}

// arrange builds the fixture server and the profile a sweep row needs.
func arrange(t *testing.T, s sweep) (*fixture.Server, string) {
	t.Helper()

	server := fixture.New(t, s.scenarios...)

	home := t.TempDir()
	paths := xdg.Paths{ConfigDir: home, StateDir: home, Home: home}

	if err := paths.EnsureAll(fsx.OS()); err != nil {
		t.Fatalf("create %s: %v", home, err)
	}

	if len(s.tokens) == 0 {
		return server, home
	}

	store := creds.NewStore(fsx.OS(), filepath.Join(home, "credentials.json"))

	file, err := store.Load()
	if err != nil {
		t.Fatalf("load credentials: %v", err)
	}

	for _, token := range s.tokens {
		err := file.Put(creds.DefaultProfile, server.URL(), creds.Credential{
			Token:       token,
			PrincipalID: "pat_PLACEHOLDER_1",
			StoredAt:    time.Unix(0, 0).UTC(),
		})
		if err != nil {
			t.Fatalf("put: %v", err)
		}
	}

	if err := store.Save(file); err != nil {
		t.Fatalf("save credentials: %v", err)
	}

	return server, home
}
