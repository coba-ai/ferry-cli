#!/usr/bin/env bash
#
# Whether CI can run the end-to-end suite, and a loud failure when it cannot.
#
# Called by the `cli-e2e-preflight` job in .github/workflows/ci.yml with
# FERRY_API_REPO_SSH_KEY set from the secret of the same name. Writes
# `credential=yes|no` to $GITHUB_OUTPUT for the `cli-e2e` job's `if:`, and
# exits non-zero when the answer is `no`.
#
# It is a script and not an inline `run:` block for one reason: the workflow
# cannot be tested and this can. `e2e/workflow_test.go` executes it with the
# variable set and unset and asserts the output and the exit code both times,
# which is what stops the gate being the thing nobody measured — the failure
# mode PLAN §6 names twice (a consent test green with the safety check deleted;
# a schema pin blind to the wrong envelope).
#
# Exiting non-zero when the credential is absent is the decision A411 records,
# and it is still the decision now that one exists: a rotated or revoked key
# turns this red again rather than quietly dropping AC61-AC63. See the comment
# above `cli-e2e-preflight` for why a skip and a warning were both rejected.
#
# A424 replaced a fine-grained personal access token with a deploy key. The
# variable holds a private key rather than a token, which is why it is not
# called one: a name that lies about what it holds is how the wrong credential
# gets pasted into it.
set -euo pipefail

credential="${FERRY_API_REPO_SSH_KEY:-}"

# $GITHUB_OUTPUT and $GITHUB_STEP_SUMMARY exist only inside Actions. Falling
# back to /dev/null lets the test run this file as-is rather than against a
# reimplementation of it.
output="${GITHUB_OUTPUT:-/dev/null}"
summary="${GITHUB_STEP_SUMMARY:-/dev/null}"

if [ -n "$credential" ]; then
	printf 'credential=yes\n' >>"$output"
	printf 'FERRY_API_REPO_SSH_KEY is configured; the end-to-end suite will run.\n'
	exit 0
fi

printf 'credential=no\n' >>"$output"

# `::error` puts this in the checks UI rather than only in the log, where it is
# one click deep and stays visible after the log has scrolled.
printf '::error title=No end-to-end coverage::AC61-AC63 are not enforced in CI. FERRY_API_REPO_SSH_KEY is not configured, so this repository cannot check out coba-ai/ferry and the suite cannot run.\n'

cat >>"$summary" <<'MARKDOWN'
### The end-to-end suite did not run

`AC61`, `AC62` and `AC63` drive the CLI against a running FERRY. Amendment
`A400` moved the CLI into its own repository and left that app in
`coba-ai/ferry`, which is private, and `GITHUB_TOKEN` is scoped to this
repository only.

**To make this check pass**, an operator with access to both repositories must:

1. Generate a keypair that exists for this and nothing else:
   `ssh-keygen -t ed25519 -N '' -C 'ferry-cli CI' -f ./key`
2. Add `key.pub` to `coba-ai/ferry` as a **read-only** deploy key
   (*Settings → Deploy keys*), or
   `gh repo deploy-key add key.pub --repo coba-ai/ferry`.
3. Add the private half to this repository as the secret
   `FERRY_API_REPO_SSH_KEY` (*Settings → Secrets and variables → Actions*),
   or `gh secret set FERRY_API_REPO_SSH_KEY --repo coba-ai/ferry-cli < key`.
4. Destroy the local copies: `shred -u key key.pub`.

A deploy key rather than a personal access token because it is read-only and
scoped to one repository, so it cannot write to `coba-ai/ferry` and cannot see
anything else the operator can (`A424`).

Until then the suite is runnable locally only — `FERRY_E2E_RAILS_DIR=../ferry
e2e/run.sh`, documented in `README.md`.

This job fails rather than skipping on purpose (`A411`): a skipped job renders
grey and reads as a completed check, and a warning is green by tomorrow.
MARKDOWN

cat >&2 <<'MESSAGE'

The end-to-end suite did not run, and this job fails to say so.

FERRY_API_REPO_SSH_KEY is not configured. AC61-AC63 drive the CLI against a
running FERRY, that app is in the private coba-ai/ferry (A400), and GITHUB_TOKEN
cannot check out another repository.

An operator must add a read-only deploy key for coba-ai/ferry as the secret
FERRY_API_REPO_SSH_KEY; the job summary has the four commands. Until then, run
the suite locally:

    FERRY_E2E_RAILS_DIR=../ferry e2e/run.sh

MESSAGE

exit 1
