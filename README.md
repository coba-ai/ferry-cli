# FERRY CLI

The Go command line client for the [FERRY API](https://github.com/kurenn/ferry).

## Status

Incomplete, and not yet installable. There is no `ferry` binary: the root
command and every money verb belong to U5, which is still being written. What
is here is the layer underneath — the API client, the fixture server, the
outcome decision table, the typed response bodies and the `auth`, `keys` and
`corridors` commands.

Nothing in this repository has spoken to Polygon. Every test runs against
recorded interactions or a local Rails app backed by a fixture, because the
sandbox credentials are still outstanding.

## Layout

    contract/              vendored copy of the API contract; see below
    docs/PLAN.md           the implementation plan this repository follows
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

`go.mod` and `go.sum` are frozen (PLAN §4.3.4). `go mod tidy` reclassifies
pflag and rewrites them, and CI fails on a dirty tree.

Every file matches exactly one prefix in `OWNERSHIP`, checked by
`contract_test.go`'s sibling `ownership_test.go` on every run.

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
