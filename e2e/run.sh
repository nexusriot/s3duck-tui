#!/bin/sh
# Run the end-to-end suite hermetically: MinIO and the runner in Docker, on a
# private compose network, torn down afterwards whether the suite passed or not.
#
#   ./e2e/run.sh                         every scenario
#   ./e2e/run.sh -run TestCreateFolder   extra arguments go to `go test`
#   make e2e E2E_RUN=Undo                the same through make
#
# Needs Docker with the Compose v2 plugin (`docker compose`). The first run
# builds the runner image (Go toolchain + tmux + the module cache); later runs
# reuse it until go.mod or go.sum change.
#
# To drive a MinIO you already have instead, skip Docker and run the suite
# directly (it needs tmux and Go on the host):
#
#   S3DUCK_TEST_ENDPOINT=http://localhost:9000 \
#       go test -tags e2e -count=1 -v ./e2e/
#
# S3DUCK_E2E_BINARY points the suite at a prebuilt binary instead of building
# the working tree — a release build, or an older commit to show a scenario
# catches the bug it was written for.
set -eu

COMPOSE_FILE="$(dirname "$0")/docker-compose.yml"

if ! docker compose version >/dev/null 2>&1; then
    echo "docker compose is required (Docker Compose v2)." >&2
    exit 1
fi

cleanup() {
    docker compose -f "$COMPOSE_FILE" down -v --remove-orphans >/dev/null 2>&1 || true
}
# On success, failure and Ctrl-C alike; `|| true` so it can never replace the
# suite's own exit status.
trap cleanup EXIT INT TERM

docker compose -f "$COMPOSE_FILE" build runner

# The suite's status is the script's status: `set -e` is lifted for the run so
# a failing test reaches the explicit exit below instead of aborting here.
set +e
if [ "$#" -gt 0 ]; then
    docker compose -f "$COMPOSE_FILE" run --rm runner \
        go test -tags e2e -count=1 -v ./e2e/ "$@"
else
    docker compose -f "$COMPOSE_FILE" run --rm runner
fi
status=$?
set -e

exit "$status"
