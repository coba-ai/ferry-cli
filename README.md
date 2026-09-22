# FERRY CLI

The Go command line client for the [FERRY API](https://github.com/coba-ai/ferry).

## Status

Complete enough to build and drive end to end, and not yet released: no tag has
been pushed, so there is nothing to `brew install` yet. Every command is wired —
`auth`, `keys`, `corridors`, `transfers`, `runs`, `commands` — and the
end-to-end suite runs the money path against a real Rails app.

Nothing in this repository has spoken to Polygon. Every test runs against
recorded interactions or a local Rails app backed by a fixture, because the
sandbox credentials are still outstanding.

## Layout

    contract/              vendored copy of the API contract; see below
    docs/PLAN.md           the implementation plan this repository follows
    e2e/                   the end-to-end suite and the release pins; see e2e/README.md
    internal/api/          HTTP client, routes, retry, redaction, typed bodies
    internal/fixture/      replays testdata/recorded/ as a local server
    internal/outcome/      what a response means for money that may have moved
    internal/noun/         one package per command noun
    internal/render/       output, including the two places a secret may print
    internal/runs/         the on-disk run ledger
    testdata/recorded/     74 interactions captured from the real Rails app

## Working on it

    go build ./...
    go test -race ./...
    go test -race -tags faultinject ./...     # the crash-recovery paths

`go.mod` and `go.sum` are frozen (PLAN §4.3.4). `go mod tidy` reclassifies
pflag and rewrites them, and CI fails on a dirty tree.

Every file matches exactly one prefix in `OWNERSHIP`, checked by
`contract_test.go`'s sibling `ownership_test.go` on every run.

## Running the end-to-end suite

    FERRY_E2E_RAILS_DIR=../ferry e2e/run.sh

Needs a Postgres 18, a Redis and a checkout of the API repository. Full
instructions, including the two `docker run` lines, are in `e2e/README.md`.

**CI runs this suite via a read-only deploy key.** A400 moved the CLI into this
repository and left the Rails app it drives in `coba-ai/ferry`, which is private;
`GITHUB_TOKEN` is scoped to one repository, so a job here cannot check that one
out. The credential is a deploy key rather than a personal access token because
it is read-only and scoped to that single repository: it cannot write to
`coba-ai/ferry` and cannot reach anything else its creator can see, which a
fine-grained token is only promising not to do.

If the key is ever rotated or revoked, `cli-e2e-preflight` goes red rather than
quietly dropping AC61–AC63 — a skipped job renders grey in the PR check list and
reads as a completed check, which is coverage that is not coverage. To restore
it, an operator with access to both repositories runs:

    ssh-keygen -t ed25519 -N '' -C 'ferry-cli CI' -f ./key
    gh repo deploy-key add key.pub --repo coba-ai/ferry   # read-only
    gh secret set FERRY_API_REPO_SSH_KEY --repo coba-ai/ferry-cli < key
    shred -u key key.pub

The last line matters: the private half should exist in exactly one place, which
is this repository's Actions secrets.

Recorded as amendments A411 and A424.

## Releasing

Tags are `v<semver>`. Pushing one runs `.github/workflows/cli-release.yml`, which
builds the four targets with goreleaser, attaches `checksums.txt`, creates the
GitHub Release and pushes `Casks/ferry.rb` to `coba-ai/homebrew-tap`.

    git tag v0.1.0 && git push origin v0.1.0

Before the first tag, an operator must add the Actions secret
`HOMEBREW_TAP_GITHUB_TOKEN` — a token with `Contents: write` on
`coba-ai/homebrew-tap`. `GITHUB_TOKEN` cannot write to another repository, and
without this one goreleaser fails the Homebrew step *after* it has already
published the GitHub Release, leaving a half-done release. Recorded as A414.

Two things about the release worth knowing before you cut one:

- It is a **cask**, not a formula. `brews` is deprecated and `goreleaser check`
  fails on it (A416). Casks are macOS-only, so `brew install coba-ai/tap/ferry`
  works on macOS and Linux users install the tarball from the GitHub Release.
- The binaries are unsigned and unnotarised (PLAN §10.1 row 7). The cask clears
  the macOS quarantine attribute on install, which is what makes an unsigned
  binary runnable at all; a tarball downloaded by hand on macOS needs
  `xattr -dr com.apple.quarantine ./ferry` first.

`ferry version` prints the version, the commit, the Go version and the sha256 of
the `contract/openapi.yaml` the binary was built against. The digest is computed
by the workflow and referenced with no default, so a release that lost that step
fails instead of shipping a binary that says `contract unknown`.

## The vendored contract

`contract/openapi.yaml` and `contract/errors.md` are copies of files generated
and hand-authored in the API repository. The schema pins read them, which is
how a Go struct that has drifted from the API's response body fails a test
here rather than failing a transfer in production.

Because they are copies, they can go stale. `contract/SOURCE` records the
upstream commit and the sha256 of each file, and `contract_test.go` holds the
copies to those digests, so editing one to make a pin pass is at least loud.

It does not catch upstream moving while the copy stands still. Re-vendoring is
manual today:

    cp ../ferry/docs/api/openapi.yaml contract/openapi.yaml
    cp ../ferry/docs/api/errors.md    contract/errors.md
    # then update contract/SOURCE with the new commit and digests

Closing that gap properly needs the API repository's CI to check this one,
which needs a cross-repository token.
