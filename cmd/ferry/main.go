// Command ferry is the FERRY CLI.
//
// It is deliberately thin. Everything that decides an exit code lives in
// `internal/cli`, because that is what `internal/harness` runs: a `main`
// that classified anything would be a path no test exercises, and the paths
// that matter here are the ones where a money request may already have left.
package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/coba-ai/ferry-cli/internal/cli"
)

// exitCoder is how an error carries a process exit code.
//
// Declared here rather than imported from `internal/harness`, which is the
// other reader of it: that package imports `testing`, and a released binary
// must not.
type exitCoder interface {
	ExitCode() int
}

func main() {
	root := cli.New(cli.Options{Signals: cli.DefaultSignals})

	err := root.Execute()
	if err == nil {
		os.Exit(0)
	}

	var coder exitCoder
	if errors.As(err, &coder) {
		// Cobra has already written the message to stderr, except for a
		// simulated process death, which writes nothing by design.
		os.Exit(coder.ExitCode())
	}

	// Unreachable while `internal/cli`'s wrapper classifies everything it
	// returns. 1 is `cli_fault`: an error that escaped the wrapper never
	// reached the money path, so "nothing was sent" is true of it.
	fmt.Fprintf(os.Stderr, "ferry: %v\n", err)
	os.Exit(1)
}
