#!/usr/bin/env bash
#
# Run the CLI end-to-end suite against a real FERRY (PLAN §5.14, AC61–AC63).
#
#   FERRY_E2E_RAILS_DIR=../ferry e2e/run.sh
#
# This is the only way the suite is run — locally and in CI's `cli-e2e` job
# alike. PLAN §5.14 described the CI job as a list of workflow steps, which is
# how the local and CI paths drift: the workflow gets a fix and the README does
# not, and six weeks later nobody can run the suite on a laptop. So the steps
# live here, the workflow calls this file, and `e2e/workflow_test.go` asserts
# that it does.
#
# What it needs, and does not install:
#
#   * a Postgres 18 the API repository's config/database.yml can reach (18 for
#     uuidv7(), which every primary key defaults to)
#   * a Redis, for the rate-limit counters
#   * the API repository checked out, with `bundle install` already run
#   * a Go toolchain
#
# See README.md "Running the end-to-end suite" for the docker one-liners.
set -euo pipefail

cli_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

die() {
	printf '%s\n' "$*" >&2
	exit 1
}

: "${FERRY_E2E_RAILS_DIR:?set FERRY_E2E_RAILS_DIR to a checkout of kurenn/ferry. The app the suite drives is in that repository (A400), and this one cannot invent it.}"

rails_dir="$(cd "$FERRY_E2E_RAILS_DIR" && pwd)"

[ -f "$rails_dir/bin/rails" ] || die "$rails_dir has no bin/rails, so it is not a checkout of kurenn/ferry."

# The app's own defaults live in its config/database.yml; these only fill in
# what a local docker container needs and what CI's service containers publish.
export RAILS_ENV=test
export FERRY_OMS_BACKEND=fixture
export FERRY_DATABASE_HOST="${FERRY_DATABASE_HOST:-127.0.0.1}"
export FERRY_DATABASE_PORT="${FERRY_DATABASE_PORT:-5432}"
export FERRY_DATABASE_USER="${FERRY_DATABASE_USER:-postgres}"
export FERRY_DATABASE_PASSWORD="${FERRY_DATABASE_PASSWORD:-postgres}"
export FERRY_RATE_LIMIT_REDIS_URL="${FERRY_RATE_LIMIT_REDIS_URL:-redis://127.0.0.1:6379/0}"

# PLAN §4.1 gives U6 its own database. The suite's fixture cleanup is fleet-wide
# by necessity — `ledger_control` is a singleton and a sweep cannot scope itself
# to one run — so concurrent units need separate databases or they truncate each
# other's rows mid-test.
export FERRY_TEST_DATABASE="${FERRY_TEST_DATABASE:-ferry_test_cli_u6}"

port="${FERRY_E2E_PORT:-3000}"
api_url="http://127.0.0.1:${port}"

work="$(mktemp -d)"
server_log="$work/server.log"
server_pid=""

cleanup() {
	local status=$?

	if [ -n "$server_pid" ] && kill -0 "$server_pid" 2>/dev/null; then
		# The Rails server is a process group leader with a Puma worker under
		# it; killing only the pid leaves the worker holding the port, and the
		# next run fails to bind for a reason that looks nothing like this one.
		kill -TERM -- "-$server_pid" 2>/dev/null || kill -TERM "$server_pid" 2>/dev/null || true
		wait "$server_pid" 2>/dev/null || true
	fi

	if [ "$status" -ne 0 ] && [ -f "$server_log" ]; then
		printf '\n--- the last 80 lines of the server log ---\n' >&2
		tail -n 80 "$server_log" >&2
	fi

	rm -rf "$work"

	return "$status"
}

trap cleanup EXIT

step() { printf '\n==> %s\n' "$1"; }

step "Preparing $FERRY_TEST_DATABASE"
# ferry:db:provision is named explicitly because nothing is chained onto
# db:prepare, and a database without ledger_control's row refuses to boot.
(cd "$rails_dir" && bin/rails db:prepare ferry:db:provision)

step "Bootstrapping an organization and a personal access token"
# `bin/rails runner` with a path outside the app, which is the shape A400 left:
# the bootstrap is the CLI suite's, the Rails that can execute it is the API's.
bootstrap="$(cd "$rails_dir" && bin/rails runner "$cli_dir/e2e/bootstrap.rb")"

pat="$(printf '%s' "$bootstrap" | ruby -rjson -e 'print JSON.parse(STDIN.read).fetch("pat")')"
[ -n "$pat" ] || die "the bootstrap printed no personal access token:\n$bootstrap"

step "Building the two binaries"
# Two, because AC62 kills a process at a named fault point and the fault
# package is a no-op without its build tag (internal/fault/inject_off.go). The
# release-shaped binary is the one every other test drives, and it is built
# *without* the tag on purpose: a suite that exercised only the tagged binary
# would never run the code that ships.
contract_sha256="$(sha256sum "$cli_dir/contract/openapi.yaml" | cut -d' ' -f1)"
stamps=(
	-X "github.com/kurenn/ferry-cli/internal/version.Version=0.0.0-e2e"
	-X "github.com/kurenn/ferry-cli/internal/version.Commit=$(git -C "$cli_dir" rev-parse HEAD)"
	-X "github.com/kurenn/ferry-cli/internal/version.ContractSHA256=$contract_sha256"
)

(cd "$cli_dir" && CGO_ENABLED=0 go build -trimpath -ldflags "${stamps[*]}" -o "$work/ferry" ./cmd/ferry)
(cd "$cli_dir" && CGO_ENABLED=0 go build -trimpath -tags faultinject -ldflags "${stamps[*]}" -o "$work/ferry-fault" ./cmd/ferry)

# Refuse a port somebody else owns.
#
# Measured, not anticipated: with a leftover server from an earlier run still
# holding this port, the app below failed to bind, exited, and the /up poll
# answered 200 from the *stale* process — which was attached to a different
# database, so every test failed with `401 TOKEN_INVALID` on `auth login` and the
# real cause was forty lines further down in a Puma backtrace. Binding is the
# server's job; noticing that the port is taken has to happen before the poll,
# because the poll cannot tell the two servers apart.
if (exec 3<>"/dev/tcp/127.0.0.1/$port") 2>/dev/null; then
	exec 3>&-
	die "something is already listening on 127.0.0.1:$port. The suite would drive that, not the app this script starts — and it has its own database, so every test would fail on the credential. Stop it, or set FERRY_E2E_PORT to a free port."
fi

step "Starting the app on $api_url with FERRY_OMS_BACKEND=fixture"
(
	cd "$rails_dir"
	setsid bin/rails server -p "$port" -b 127.0.0.1 >"$server_log" 2>&1 &
	printf '%s' "$!" >"$work/server.pid"
)
server_pid="$(cat "$work/server.pid")"

# Poll /up rather than sleeping. A fixed sleep is either too short on a cold
# bundle or wasted on a warm one, and when it is too short the failure is a
# connection refused from the first test rather than "the app did not start".
for _ in $(seq 1 120); do
	if curl -fsS -o /dev/null "$api_url/up" 2>/dev/null; then
		break
	fi

	if ! kill -0 "$server_pid" 2>/dev/null; then
		die "the app exited before answering /up; its log follows above."
	fi

	sleep 1
done

curl -fsS -o /dev/null "$api_url/up" || die "the app did not answer $api_url/up within 120s."

step "Running the suite"
cd "$cli_dir"

FERRY_API_URL="$api_url" \
	FERRY_E2E_PAT="$pat" \
	FERRY_E2E_BIN="$work/ferry" \
	FERRY_E2E_FAULT_BIN="$work/ferry-fault" \
	FERRY_E2E_RAILS_DIR="$rails_dir" \
	go test -count=1 -tags e2e -timeout 20m "$@" ./e2e/...
