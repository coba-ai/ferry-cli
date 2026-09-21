package cli_test

import (
	"testing"
)

// The four spellings of `--output json`, each in front of a usage error.
//
// A usage error is the case that cannot read the parsed flag, because the
// parse is what failed — so the mode is recovered from the argv, and a scan
// that understands three spellings out of four answers one caller in four
// with a line of prose where they asked for a document.
//
// The ordering matters as much as the spelling. `--no-such-flag --output
// json` puts the unknown flag *first*, which is what defeats pflag's
// unknown-flag whitelist: it consumes `--output` as the unknown flag's
// value. Both orders are here.
func TestTheOutputModeSurvivesAFailedFlagParse(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{
			name: "--output json after the bad flag",
			args: []string{"transfers", "create", "--no-such-flag", "--output", "json"},
		},
		{
			name: "--output json before the bad flag",
			args: []string{"transfers", "create", "--output", "json", "--no-such-flag"},
		},
		{
			name: "--output=json",
			args: []string{"transfers", "create", "--no-such-flag", "--output=json"},
		},
		{
			name: "-o json",
			args: []string{"transfers", "create", "--no-such-flag", "-o", "json"},
		},
		{
			name: "-ojson",
			args: []string{"transfers", "create", "--no-such-flag", "-ojson"},
		},
		{
			name: "-o=json",
			args: []string{"transfers", "create", "--no-such-flag", "-o=json"},
		},
		{
			// The last occurrence wins, as pflag does.
			name: "text then json",
			args: []string{"transfers", "create", "--no-such-flag", "-o", "text", "--output", "json"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, home := loggedIn(t)

			stdout, stderr, exit := run(t, invocation{home: home, args: tc.args})

			if exit != 2 {
				t.Errorf("exit = %d, want 2\n%s\n%s", exit, stdout, stderr)
			}

			docs := documents(t, stdout)

			if len(docs) != 1 {
				t.Fatalf("stdout carries %d documents, want 1:\n%q", len(docs), stdout)
			}

			if got := exitCodeOf(t, docs[0]); got != exit {
				t.Errorf("outcome.exit_code = %d but the process exited %d", got, exit)
			}
		})
	}
}

// The other direction: a caller who did not ask for JSON does not get a
// document from the same path.
//
// Without this, the scan above is satisfied by one that returns "json"
// unconditionally — which would answer every mistyped flag with an envelope.
func TestAFailedFlagParseInTextModeWritesNoDocument(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{
			name: "no --output at all",
			args: []string{"transfers", "create", "--no-such-flag"},
		},
		{
			name: "--output text",
			args: []string{"transfers", "create", "--no-such-flag", "--output", "text"},
		},
		{
			// `--` ends the scan, so this `--output json` is an argument
			// and not a request.
			name: "--output json after a terminator",
			args: []string{"transfers", "create", "--no-such-flag", "--", "--output", "json"},
		},
		{
			// A value that names no mode is a usage error of its own, and
			// text is the mode whose failure is a sentence rather than a
			// malformed document.
			name: "--output yaml",
			args: []string{"transfers", "create", "--no-such-flag", "--output", "yaml"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, home := loggedIn(t)

			stdout, stderr, exit := run(t, invocation{home: home, args: tc.args})

			if exit != 2 {
				t.Errorf("exit = %d, want 2\n%s\n%s", exit, stdout, stderr)
			}

			if docs, _, _ := scan(stdout); len(docs) != 0 {
				t.Errorf("a text-mode usage error wrote %d document(s):\n%q", len(docs), stdout)
			}

			if stderr == "" {
				t.Error("nothing on stderr; the caller is told nothing at all")
			}
		})
	}
}

// AC41 for the argument validators, which are the other pre-RunE failure.
//
// `ferry transfers teleport` is rejected by cobra's `Args` before any RunE,
// exactly as a bad flag is, and needed the same hook. The `keys get` row is
// the control that wrapping the validators did not replace them: it has its
// own `ExactArgs`, and a wrapper that dropped the existing validator would
// let it through to a request with no key id.
func TestArgumentErrorsWriteADocument(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{
			name: "an unknown subcommand",
			args: []string{"transfers", "teleport"},
		},
		{
			name: "an unknown noun",
			args: []string{"teleport"},
		},
		{
			name: "too few arguments",
			args: []string{"keys", "get"},
		},
		{
			name: "too many arguments",
			args: []string{"keys", "get", "key_1", "key_2"},
		},
		{
			name: "an argument where none is taken",
			args: []string{"corridors", "list", "extra"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, home := loggedIn(t)

			stdout, stderr, exit := run(t, invocation{
				home: home,
				args: append(append([]string{}, tc.args...), "--output", "json"),
			})

			if exit != 2 {
				t.Errorf("exit = %d, want 2\n%s\n%s", exit, stdout, stderr)
			}

			docs := documents(t, stdout)

			if len(docs) != 1 {
				t.Fatalf("stdout carries %d documents, want 1:\n%q", len(docs), stdout)
			}

			if got := exitCodeOf(t, docs[0]); got != exit {
				t.Errorf("outcome.exit_code = %d but the process exited %d", got, exit)
			}
		})
	}
}

// And the control for *that*: the argument counts that are correct still
// work.
//
// Wrapping every command's `Args` is a change with the potential to refuse
// every invocation in the CLI, and a test suite made only of the rows above
// would be perfectly happy with that.
func TestCorrectArgumentCountsStillRun(t *testing.T) {
	for _, tc := range []struct {
		name     string
		scenario string
		args     []string
	}{
		{
			name:     "no arguments where none are taken",
			scenario: "corridors.list.200",
			args:     []string{"corridors", "list"},
		},
		{
			name:     "one argument where one is taken",
			scenario: "keys.get.200",
			args:     []string{"keys", "get", "key_PLACEHOLDER_1"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, home := loggedIn(t, tc.scenario)

			stdout, stderr, exit := run(t, invocation{
				home: home,
				args: append(append([]string{}, tc.args...), "--output", "json"),
			})

			if exit != 0 {
				t.Fatalf("exit = %d, want 0\n%s\n%s", exit, stdout, stderr)
			}

			if docs := documents(t, stdout); len(docs) != 1 {
				t.Errorf("stdout carries %d documents, want 1", len(docs))
			}
		})
	}
}
