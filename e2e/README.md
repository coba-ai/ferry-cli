# The end-to-end suite

Drives the built `ferry` binary against a running FERRY with
`FERRY_OMS_BACKEND=fixture`. Covers AC61 (the happy path), AC62 (crash-resume at
both fault points) and three of AC63's four refusals.

    FERRY_E2E_RAILS_DIR=../ferry e2e/run.sh

`run.sh` prepares the database, bootstraps an organization and a token, builds
both binaries, starts the app, runs the suite and stops the app. CI's `cli-e2e`
job runs the same file, which is the point of it being a file: PLAN §5.14
described this as a list of workflow steps, and a list of workflow steps is a
thing that gets fixed in the workflow and not in the README.

## What it needs, and does not install

A Postgres 18 and a Redis, a checkout of `kurenn/ferry` with `bundle install`
already run, and a Go toolchain. Postgres must be 18: every FERRY primary key
defaults to `uuidv7()`.

    docker run -d --name ferry-e2e-pg --health-cmd 'pg_isready -U postgres' \
      -e POSTGRES_USER=postgres -e POSTGRES_PASSWORD=postgres \
      -p 127.0.0.1:5432:5432 postgres:18

    docker run -d --name ferry-e2e-redis -p 127.0.0.1:6379:6379 redis:8-alpine

Then, from this repository:

    FERRY_E2E_RAILS_DIR=../ferry e2e/run.sh

Override `FERRY_DATABASE_PORT`, `FERRY_RATE_LIMIT_REDIS_URL` or `FERRY_E2E_PORT`
if those ports are taken. `run.sh` refuses to start when something already holds
its port rather than testing against it — an earlier server on the same port
answers `/up` from a different database, and every test then fails on `401
TOKEN_INVALID` with the real cause forty lines down in a Puma backtrace.

## Why it is behind a build tag

`go test ./...` cannot make a Postgres, a Rails app and a credential. A file that
compiled into the default suite would have to decide what to do when they are
absent, and every answer is worse than not compiling: a skip reads as a pass in
the summary, and a failure makes the default suite unrunnable on a laptop.

Inside the tag there is **no skip**. Every variable is required and a missing one
is a `t.Fatal` naming it.

## The files

| | |
|---|---|
| `run.sh` | the whole suite, locally and in CI |
| `ci-preflight.sh` | whether CI has the token to run it at all (A411) |
| `bootstrap.rb` | Ruby, run by the API repository's Rails; mints the PAT |
| `commands_with_key.rb` | AC62's row count, asked of FERRY's own database |
| `harness_test.go` | the `cli` type every test drives |
| `sets_test.go` | the both-directions set comparison, and its control |
| `happy_test.go` `resume_test.go` `refusals_test.go` | AC61, AC62, AC63 |
| `version_test.go` `release_config_test.go` | AC64 without goreleaser |
| `release_test.go` | AC64 with it, behind `-tags goreleaser` |
| `workflow_test.go` | AC60 and AC65 |

The last four run in the default `go test ./...`: they read files and shell out
to `go build`, and need nothing this repository does not have.

## The Ruby in a Go repository

`bootstrap.rb` and `commands_with_key.rb` are Ruby, here. A400 split the CLI out
of the API repository and the app these tests drive stayed behind; the suite is
the CLI's, so its bootstrap is the CLI's too, and it is handed to the *API*
repository's Rails to execute:

    cd ../ferry && bin/rails runner ../ferry-cli/e2e/bootstrap.rb

`bootstrap.rb` refuses `RAILS_ENV=production` and any `FERRY_OMS_BACKEND` other
than `fixture` or `mock`. Both refusals are in the script rather than in the
workflow, because a workflow guard protects the workflow and the thing worth
protecting is an operator with a shell.
