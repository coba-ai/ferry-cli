package auth_test

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"

	"golang.org/x/term"

	"github.com/kurenn/ferry-cli/internal/creds"
	"github.com/kurenn/ferry-cli/internal/fixture"
	"github.com/kurenn/ferry-cli/internal/noun"
	"github.com/kurenn/ferry-cli/internal/noun/auth"
)

// ---------------------------------------------------------------------------
// AC33 — the token comes from --token-stdin or a TTY prompt with echo off, and
// there is no --token flag, ever.
// ---------------------------------------------------------------------------

// The flag set of `auth login`, held to a declared list in both directions.
//
// A one-directional check is the defect this project keeps shipping. "No flag
// named token" alone would miss `--secret`, `--bearer` or `--api-token`; "every
// declared flag exists" alone would miss `--token` being added beside them.
// Equality catches both, and the floor catches a VisitAll that walked nothing.
//
// Mutation M35 adds `--token` and this is the example it must redden.
func TestLoginDeclaresExactlyTheseFlagsAndNoTokenValueFlag(t *testing.T) {
	globals := &noun.Globals{}
	cmd := auth.Command(noun.Deps{Globals: globals})
	noun.BindGlobals(cmd, globals)

	login, _, err := cmd.Find([]string{"login"})
	if err != nil {
		t.Fatalf("find login: %v", err)
	}

	// Cobra populates both sets lazily: parsing links the child to its
	// parents' persistent flags, and `--help` is added at execute time.
	// Both are done here so the set compared below is the set a caller sees.
	if err := login.ParseFlags(nil); err != nil {
		t.Fatalf("parse: %v", err)
	}

	login.InitDefaultHelpFlag()

	got := flagNames(login)
	sort.Strings(got)

	want := []string{
		// login's own
		auth.TokenStdinFlag,
		// the globals U5's root will define, bound here by the same binder
		"api", "debug", "env", "help", "no-color", "output", "profile", "retry-budget",
	}
	sort.Strings(want)

	if len(got) < len(want) {
		t.Fatalf("only %d flags were found (%v); the walk is broken and every claim below "+
			"would be vacuous", len(got), got)
	}

	if !reflect.DeepEqual(got, want) {
		t.Errorf("the flag set of `auth login` is\n  %v\nand the declared set is\n  %v\n\n"+
			"AC33: a token must not be nameable on the command line, and the only way to know "+
			"that is to state the whole set.", got, want)
	}

	for _, name := range got {
		if strings.Contains(name, "token") && name != auth.TokenStdinFlag {
			t.Errorf("`auth login` declares --%s. AC33: a token on the command line is in the "+
				"shell history and in /proc/<pid>/cmdline, and neither copy can be revoked.", name)
		}
	}
}

// And the behavioural half: `--token <value>` is a usage error, not a login.
func TestATokenOnTheCommandLineIsRefusedAndSendsNothing(t *testing.T) {
	server := fixture.New(t, "me.pat.200")
	home := newHome(t)

	_, stderr, exit := run(t, invocation{
		home: home,
		args: []string{"login", "--api", server.URL(), "--token", patToken(t)},
	})

	if exit != 2 {
		t.Errorf("exit %d, want 2 (usage)", exit)
	}

	if n := requestCount(server); n != 0 {
		t.Errorf("the fixture saw %d requests; a flag this CLI does not have must not reach a socket", n)
	}

	if readCredentials(t, home) != nil {
		t.Error("a credentials file was written for a command line that was refused")
	}

	if !strings.Contains(stderr, "token") {
		t.Errorf("the refusal does not mention the flag: %q", stderr)
	}
}

func TestTheTokenIsReadFromStdin(t *testing.T) {
	server := fixture.New(t, "me.pat.200")
	home := newHome(t)

	stdout, stderr, exit := run(t, invocation{
		home:  home,
		args:  []string{"login", "--api", server.URL(), "--token-stdin"},
		stdin: patToken(t) + "\n",
	})

	if exit != 0 {
		t.Fatalf("exit %d, want 0\nstdout: %s\nstderr: %s", exit, stdout, stderr)
	}

	if n := requestCount(server); n != 1 {
		t.Fatalf("the fixture saw %d requests, want 1", n)
	}

	if got := server.Requests()[0].Header.Get("Authorization"); got != "Bearer "+patToken(t) {
		t.Errorf("the token on the wire is %q; the one on stdin should have been sent", got)
	}

	if p := storedProfile(t, home); p.PAT == nil {
		t.Error("the verified token was not stored")
	}
}

// A pty, the real echo-off read, and the token typed at the prompt.
//
// This exercises `term.ReadPassword` itself rather than a seam. What it cannot
// exercise is echo: `harness.Run` puts the pty slave in raw mode before the
// command runs (A314), so ECHO is already off and a transcript assertion would
// pass whatever this code did. The echo-off claim is carried by the identity
// check below instead, and this test proves the read works on a terminal.
func TestTheTokenIsReadFromATerminalPrompt(t *testing.T) {
	server := fixture.New(t, "me.pat.200")
	home := newHome(t)

	merged, _, exit := run(t, invocation{
		home:  home,
		args:  []string{"login", "--api", server.URL()},
		stdin: patToken(t) + "\n",
		tty:   true,
	})

	if exit != 0 {
		t.Fatalf("exit %d, want 0\ntranscript: %s", exit, merged)
	}

	if !strings.Contains(merged, "FERRY token") {
		t.Errorf("no prompt was written:\n%s", merged)
	}

	if n := requestCount(server); n != 1 {
		t.Fatalf("the fixture saw %d requests, want 1", n)
	}

	if got := server.Requests()[0].Header.Get("Authorization"); got != "Bearer "+patToken(t) {
		t.Errorf("the token on the wire is %q", got)
	}

	if p := storedProfile(t, home); p.PAT == nil {
		t.Error("the typed token was not stored")
	}
}

// The prompt is echo-off because it goes through term.ReadPassword, and that
// is asserted by function identity.
//
// A mutation replacing the terminal read with `bufio.NewReader(os.Stdin).ReadString`
// would keep every transcript assertion green — the harness's raw mode hides
// the difference — and would redden exactly here.
func TestTheTerminalReadIsTheEchoOffOne(t *testing.T) {
	want := reflect.ValueOf(term.ReadPassword).Pointer()
	got := reflect.ValueOf(noun.DefaultReadSecret).Pointer()

	if got != want {
		t.Error("noun.DefaultReadSecret is not term.ReadPassword. A terminal read that leaves echo " +
			"on puts the token on the operator's screen and in whatever is recording it.")
	}
}

// And the login path uses the seam rather than reading the descriptor itself.
func TestTheLoginPathReadsTheSecretThroughTheSeam(t *testing.T) {
	server := fixture.New(t, "me.pat.200")
	home := newHome(t)

	calls := 0

	_, _, exit := run(t, invocation{
		home: home,
		args: []string{"login", "--api", server.URL()},
		tty:  true,
		deps: func(d *noun.Deps) {
			d.ReadSecret = func(int) ([]byte, error) {
				calls++

				return []byte(patToken(t)), nil
			}
		},
	})

	if exit != 0 {
		t.Fatalf("exit %d, want 0", exit)
	}

	if calls != 1 {
		t.Errorf("the echo-off read was called %d times, want 1; the token came from somewhere else", calls)
	}

	if got := server.Requests()[0].Header.Get("Authorization"); got != "Bearer "+patToken(t) {
		t.Errorf("the token on the wire is %q, not the one the seam returned", got)
	}
}

// Without a terminal and without --token-stdin there is no third way in.
func TestWithoutATerminalAndWithoutStdinThereIsNoWayToSupplyAToken(t *testing.T) {
	server := fixture.New(t, "me.pat.200")
	home := newHome(t)

	_, stderr, exit := run(t, invocation{home: home, args: []string{"login", "--api", server.URL()}})

	if exit != 2 {
		t.Errorf("exit %d, want 2", exit)
	}

	if n := requestCount(server); n != 0 {
		t.Errorf("the fixture saw %d requests", n)
	}

	if !strings.Contains(stderr, "--token-stdin") {
		t.Errorf("the refusal does not say how to supply the token: %q", stderr)
	}
}

// ---------------------------------------------------------------------------
// AC34 — --env refuses an API key of another environment (exit 3, zero
// requests) and refuses --env with a PAT (exit 2).
// ---------------------------------------------------------------------------

// Mutation M36 ignores `--env` on login, and both refusals below become a 200.
func TestEnvRefusesAKeyOfAnotherEnvironmentWithoutSending(t *testing.T) {
	server := fixture.New(t, "me.api_key.200")
	home := newHome(t)

	_, stderr, exit := run(t, invocation{
		home:  home,
		args:  []string{"login", "--api", server.URL(), "--token-stdin", "--env", "live"},
		stdin: sandboxKey(t),
	})

	if exit != 3 {
		t.Errorf("exit %d, want 3 (refused_fix: nothing sent, nothing spent)", exit)
	}

	if n := requestCount(server); n != 0 {
		t.Errorf("the fixture saw %d requests; a token of the wrong environment must not be "+
			"presented to a server before being refused", n)
	}

	if readCredentials(t, home) != nil {
		t.Error("a credentials file was written for a refused login")
	}

	if !strings.Contains(stderr, "live") || !strings.Contains(stderr, "sandbox") {
		t.Errorf("the refusal names neither environment: %q", stderr)
	}
}

func TestEnvWithAPersonalAccessTokenIsAUsageError(t *testing.T) {
	server := fixture.New(t, "me.pat.200")
	home := newHome(t)

	_, stderr, exit := run(t, invocation{
		home:  home,
		args:  []string{"login", "--api", server.URL(), "--token-stdin", "--env", "sandbox"},
		stdin: patToken(t),
	})

	if exit != 2 {
		t.Errorf("exit %d, want 2 (usage: a PAT belongs to no single environment)", exit)
	}

	if n := requestCount(server); n != 0 {
		t.Errorf("the fixture saw %d requests", n)
	}

	if !strings.Contains(stderr, "--env") {
		t.Errorf("the refusal does not name the flag: %q", stderr)
	}
}

// The floor for the two refusals above: `--env` matching the token is a login.
// Without this, an implementation that refused every `--env` would pass both.
func TestEnvMatchingTheTokenIsAccepted(t *testing.T) {
	server := fixture.New(t, "me.api_key.200")
	home := newHome(t)

	stdout, stderr, exit := run(t, invocation{
		home:  home,
		args:  []string{"login", "--api", server.URL(), "--token-stdin", "--env", "sandbox"},
		stdin: sandboxKey(t),
	})

	if exit != 0 {
		t.Fatalf("exit %d, want 0\nstdout: %s\nstderr: %s", exit, stdout, stderr)
	}

	if n := requestCount(server); n != 1 {
		t.Errorf("the fixture saw %d requests, want 1", n)
	}
}

// A live key is refused against `--env sandbox` too, so the assertion is not
// one-sided in the value it happens to test.
func TestEnvRefusesALiveKeyAssertedAsSandbox(t *testing.T) {
	server := fixture.New(t, "me.api_key.200")
	home := newHome(t)

	_, _, exit := run(t, invocation{
		home:  home,
		args:  []string{"login", "--api", server.URL(), "--token-stdin", "--env", "sandbox"},
		stdin: liveKey(t),
	})

	if exit != 3 {
		t.Errorf("exit %d, want 3", exit)
	}

	if n := requestCount(server); n != 0 {
		t.Errorf("the fixture saw %d requests", n)
	}
}

// ---------------------------------------------------------------------------
// AC35 — an endpoint is required, GET /v1/me is called there, and the record
// is stored only on a 200.
// ---------------------------------------------------------------------------

func TestWithoutAnEndpointLoginIsAUsageErrorAndSendsNothing(t *testing.T) {
	server := fixture.New(t, "me.pat.200")
	home := newHome(t)

	_, stderr, exit := run(t, invocation{
		home:  home,
		args:  []string{"login", "--token-stdin"},
		stdin: patToken(t),
	})

	if exit != 2 {
		t.Errorf("exit %d, want 2", exit)
	}

	if n := requestCount(server); n != 0 {
		t.Errorf("the fixture saw %d requests", n)
	}

	const want = "no API endpoint configured; pass `--api` or set `FERRY_API_URL`"
	if !strings.Contains(stderr, want) {
		t.Errorf("the message is %q, want it to contain %q", stderr, want)
	}
}

func TestTheEndpointMayComeFromTheEnvironment(t *testing.T) {
	server := fixture.New(t, "me.pat.200")
	home := newHome(t)

	_, _, exit := run(t, invocation{
		home:  home,
		args:  []string{"login", "--token-stdin"},
		stdin: patToken(t),
		env:   map[string]string{"FERRY_API_URL": server.URL()},
	})

	if exit != 0 {
		t.Fatalf("exit %d, want 0", exit)
	}

	if n := requestCount(server); n != 1 {
		t.Errorf("the fixture saw %d requests, want 1", n)
	}
}

// The request goes to GET /v1/me at the endpoint given, with that token.
func TestLoginCallsGetMeAtTheEndpointGiven(t *testing.T) {
	server := fixture.New(t, "me.pat.200")
	home := newHome(t)

	_, _, exit := run(t, invocation{
		home:  home,
		args:  []string{"login", "--api", server.URL(), "--token-stdin"},
		stdin: patToken(t),
	})

	if exit != 0 {
		t.Fatalf("exit %d, want 0", exit)
	}

	req := server.Requests()[0]

	if req.Method != "GET" || req.Path != "/v1/me" {
		t.Errorf("login sent %s %s, want GET /v1/me", req.Method, req.Path)
	}

	if req.IdempotencyKey() != "" {
		t.Errorf("login sent an Idempotency-Key (%q); /v1/me declares none", req.IdempotencyKey())
	}
}

// Mutation M37 stores before the request. The 401 recording is the example:
// nothing may be on disk afterwards.
func TestATokenTheAPIRefusesIsNotStored(t *testing.T) {
	server := fixture.New(t, "auth.token_invalid.401")
	home := newHome(t)

	_, stderr, exit := run(t, invocation{
		home:  home,
		args:  []string{"login", "--api", server.URL(), "--token-stdin"},
		stdin: sandboxKey(t),
	})

	if exit != 3 {
		t.Errorf("exit %d, want 3 (a 401 on a read is refused_fix)", exit)
	}

	if n := requestCount(server); n != 1 {
		t.Fatalf("the fixture saw %d requests, want 1", n)
	}

	if raw := readCredentials(t, home); raw != nil {
		t.Errorf("a credentials file was written for a token FERRY refused:\n%s", raw)
	}

	if !strings.Contains(stderr, "TOKEN_INVALID") {
		t.Errorf("the refusal does not name the code: %q", stderr)
	}
}

// A login that fails must not damage a credential that was already there.
func TestARefusedLoginLeavesAnExistingProfileIntact(t *testing.T) {
	server := fixture.New(t, "auth.token_invalid.401")
	home := newHome(t)

	writeProfile(t, home, server.URL(), patToken(t))

	before := readCredentials(t, home)

	_, _, exit := run(t, invocation{
		home:  home,
		args:  []string{"login", "--api", server.URL(), "--token-stdin"},
		stdin: sandboxKey(t),
	})

	if exit != 3 {
		t.Errorf("exit %d, want 3", exit)
	}

	if after := readCredentials(t, home); string(after) != string(before) {
		t.Errorf("the credentials file changed under a refused login:\nbefore %s\nafter  %s", before, after)
	}
}

// AC35's record: api_url, token_prefix, environment, principal_id.
//
// Both directions over the stored key set, so a field quietly dropped is as
// red as a field quietly added — and the values are checked against
// independent authorities: the prefix against `creds.Prefix`, the environment
// against the token's own shape, the principal against the recording's
// `credential.id`.
func TestTheStoredRecordCarriesExactlyTheDeclaredFields(t *testing.T) {
	cases := []struct {
		name        string
		scenario    string
		token       func(*testing.T) string
		slot        func(creds.Profile) *creds.Credential
		environment string
		principal   string
	}{
		{
			name:     "an api key",
			scenario: "me.api_key.200",
			token:    sandboxKey,
			slot:     func(p creds.Profile) *creds.Credential { return p.APIKey },
			// The token's shape says sandbox and `credential.id` in the
			// recording is a key id. A PAT answers with neither.
			environment: "sandbox",
			principal:   "key_PLACEHOLDER_1",
		},
		{
			name:        "a personal access token",
			scenario:    "me.pat.200",
			token:       patToken,
			slot:        func(p creds.Profile) *creds.Credential { return p.PAT },
			environment: "",
			principal:   "pat_PLACEHOLDER_1",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := fixture.New(t, tc.scenario)
			home := newHome(t)
			token := tc.token(t)

			_, _, exit := run(t, invocation{
				home:  home,
				args:  []string{"login", "--api", server.URL(), "--token-stdin"},
				stdin: token,
			})

			if exit != 0 {
				t.Fatalf("exit %d, want 0", exit)
			}

			profile := storedProfile(t, home)

			if profile.APIURL != server.URL() {
				t.Errorf("api_url is %q, want the endpoint the token was verified against (%q)",
					profile.APIURL, server.URL())
			}

			cred := tc.slot(profile)
			if cred == nil {
				t.Fatal("the slot for this credential class is empty")
			}

			if cred.TokenPrefix != creds.Prefix(token) {
				t.Errorf("token_prefix is %q, want %q", cred.TokenPrefix, creds.Prefix(token))
			}

			switch {
			case tc.environment == "" && cred.Environment != nil:
				t.Errorf("environment is %q; a personal access token belongs to none", *cred.Environment)
			case tc.environment != "" && cred.Environment == nil:
				t.Errorf("environment is null, want %q", tc.environment)
			case tc.environment != "" && string(*cred.Environment) != tc.environment:
				t.Errorf("environment is %q, want %q", *cred.Environment, tc.environment)
			}

			if cred.PrincipalID != tc.principal {
				t.Errorf("principal_id is %q, want %q from the answer's credential.id",
					cred.PrincipalID, tc.principal)
			}

			assertStoredKeySet(t, home)
		})
	}
}

// assertStoredKeySet holds the stored credential's JSON key set to the
// declared one, in both directions.
func assertStoredKeySet(t *testing.T, home string) {
	t.Helper()

	raw := readCredentials(t, home)

	var file struct {
		Profiles map[string]map[string]json.RawMessage `json:"profiles"`
	}

	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatalf("parse credentials: %v", err)
	}

	want := []string{"environment", "principal_id", "stored_at", "token", "token_prefix"}

	slots := 0

	for _, profile := range file.Profiles {
		if _, ok := profile["api_url"]; !ok {
			t.Error("the profile carries no api_url; a credential bound to no endpoint is one " +
				"that can be sent to any (AC9, C7)")
		}

		for slotName, slot := range profile {
			if slotName != "api_key" && slotName != "pat" {
				continue
			}

			slots++

			var fields map[string]json.RawMessage
			if err := json.Unmarshal(slot, &fields); err != nil {
				t.Fatalf("parse %s slot: %v", slotName, err)
			}

			var got []string
			for k := range fields {
				got = append(got, k)
			}

			sort.Strings(got)

			if !reflect.DeepEqual(got, want) {
				t.Errorf("the %s slot holds %v, the declared record is %v", slotName, got, want)
			}
		}
	}

	if slots == 0 {
		t.Fatal("no credential slot was found to check; the comparison above asserted nothing")
	}
}
