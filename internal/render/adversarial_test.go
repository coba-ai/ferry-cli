package render_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/kurenn/ferry-cli/internal/harness"
	"github.com/kurenn/ferry-cli/internal/noun"
	"github.com/kurenn/ferry-cli/internal/render"
)

// The adversarial pass for AC42: what makes a secret reach a place AC42 says
// it cannot?
//
// The answer that survived the longest was `--debug`. Every command takes it,
// it writes the request and the answer to stderr, and the request carries
// `Authorization: Bearer <the operator's token>`. Nothing in the sweep above
// covers it, because none of those runs passed the flag.
//
// `api.Redact` is U2's and is where the control lives; this is U4 measuring
// that the control is actually reached from every command it owns, with the
// flag its own `BindGlobals` declares.
func TestDebugTracesCarryNoSecret(t *testing.T) {
	swept := 0

	for _, s := range sweeps() {
		// `auth login` reads its token from stdin and the sweep gives it
		// none of the profile's; the row below runs it with the canary on
		// stdin, so it is the sharpest case rather than an omitted one.
		swept++

		t.Run(s.name, func(t *testing.T) {
			server, home := arrange(t, s)

			globals := &noun.Globals{}

			deps := noun.Deps{
				Globals: globals,
				HTTP:    &http.Client{},
				Now:     func() time.Time { return time.Unix(1_700_000_000, 0).UTC() },
			}

			cmd := s.command(deps)
			noun.BindGlobals(cmd, globals)

			args := append(append([]string{}, s.args...), "--api", server.URL(), "--debug")

			stdout, stderr, exit := harness.Run(t, cmd, args, s.stdin,
				map[string]string{"FERRY_HOME": home}, false)

			if exit != 0 {
				t.Fatalf("exit %d\nstdout: %s\nstderr: %s", exit, stdout, stderr)
			}

			// `auth whoami` and `auth logout` send nothing, so there is no
			// trace for them to leak through — which is worth asserting in
			// its own right, and is why the row is kept rather than
			// skipped. Everything else must have traced, or the redaction
			// assertion below would hold over an empty string.
			sends := len(s.scenarios) > 0

			traced := strings.Contains(stderr, "> GET") || strings.Contains(stderr, "> POST")

			switch {
			case sends && !traced:
				t.Fatalf("`--debug` wrote no request trace for `ferry %s`, so there is nothing "+
					"here to have leaked:\n%s", s.name, stderr)

			case !sends && traced:
				t.Fatalf("`ferry %s` traced a request; it is a local command and must send nothing",
					s.name)
			}

			if sends && !strings.Contains(stderr, "Authorization") {
				t.Fatalf("the trace carries no Authorization header, so the line most likely to "+
					"leak was never written:\n%s", stderr)
			}

			// The credential that authenticated must not be in the trace,
			// nor any secret shape at all. The `keys create` token is the
			// one exception — the 201's body is traced verbatim — and that
			// is `Redact`'s business, checked separately below.
			for _, token := range append([]string{s.stdin}, s.tokens...) {
				if token == "" {
					continue
				}

				if strings.Contains(stderr, token) {
					t.Errorf("`ferry %s --debug` wrote a bearer credential to stderr", s.name)
				}
			}

			if found := render.FindSecrets(stderr); len(found) > 0 {
				t.Errorf("`ferry %s --debug` wrote %d secret-shaped string(s) to the trace: %q",
					s.name, len(found), found)
			}
		})
	}

	if swept != len(sweeps()) {
		t.Errorf("only %d of %d commands were traced", swept, len(sweeps()))
	}
}
