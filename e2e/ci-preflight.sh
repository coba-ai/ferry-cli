#!/usr/bin/env bash
#
# Whether CI can run the end-to-end suite, and a loud failure when it cannot.
#
# Called by the `cli-e2e-preflight` job in .github/workflows/ci.yml with
# FERRY_API_REPO_TOKEN set from the secret of the same name. Writes
# `token=yes|no` to $GITHUB_OUTPUT for the `cli-e2e` job's `if:`, and exits
# non-zero when the answer is `no`.
#
# It is a script and not an inline `run:` block for one reason: the workflow
# cannot be tested and this can. `e2e/workflow_test.go` executes it with the
# variable set and unset and asserts the output and the exit code both times,
# which is what stops the gate being the thing nobody measured — the failure
# mode PLAN §6 names twice (a consent test green with the safety check deleted;
# a schema pin blind to the wrong envelope).
#
# Exiting non-zero when the secret is absent is the decision A411 records. See
# the comment above `cli-e2e-preflight` for why a skip and a warning were both
# rejected.
set -euo pipefail

token="${FERRY_API_REPO_TOKEN:-}"

# $GITHUB_OUTPUT and $GITHUB_STEP_SUMMARY exist only inside Actions. Falling
# back to /dev/null lets the test run this file as-is rather than against a
# reimplementation of it.
output="${GITHUB_OUTPUT:-/dev/null}"
summary="${GITHUB_STEP_SUMMARY:-/dev/null}"

if [ -n "$token" ]; then
	printf 'token=yes\n' >>"$output"
	printf 'FERRY_API_REPO_TOKEN is configured; the end-to-end suite will run.\n'
	exit 0
fi

printf 'token=no\n' >>"$output"

# `::error` puts this in the checks UI rather than only in the log, where it is
# one click deep and stays visible after the log has scrolled.
printf '::error title=No end-to-end coverage::AC61-AC63 are not enforced in CI. FERRY_API_REPO_TOKEN is not configured, so this repository cannot check out kurenn/ferry and the suite cannot run.\n'

cat >>"$summary" <<'MARKDOWN'
### The end-to-end suite did not run

`AC61`, `AC62` and `AC63` drive the CLI against a running FERRY. Amendment
`A400` moved the CLI into its own repository and left that app in
`kurenn/ferry`, which is private, and `GITHUB_TOKEN` is scoped to this
repository only.

**To make this check pass**, an operator with access to both repositories must:

1. Create a fine-grained personal access token with `Contents: read` on
   `kurenn/ferry` and nothing else.
2. Add it to this repository as the secret `FERRY_API_REPO_TOKEN`
   (*Settings → Secrets and variables → Actions*).

Until then the suite is runnable locally only — `FERRY_E2E_RAILS_DIR=../ferry
e2e/run.sh`, documented in `README.md`.

This job fails rather than skipping on purpose (`A411`): a skipped job renders
grey and reads as a completed check, and a warning is green by tomorrow.
MARKDOWN

cat >&2 <<'MESSAGE'

The end-to-end suite did not run, and this job fails to say so.

FERRY_API_REPO_TOKEN is not configured. AC61-AC63 drive the CLI against a
running FERRY, that app is in the private kurenn/ferry (A400), and GITHUB_TOKEN
cannot check out another repository.

An operator must add a fine-grained token with `Contents: read` on kurenn/ferry
as the secret FERRY_API_REPO_TOKEN. Until then, run the suite locally:

    FERRY_E2E_RAILS_DIR=../ferry e2e/run.sh

MESSAGE

exit 1
