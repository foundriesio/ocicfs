#!/usr/bin/env bash
#
# Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
# SPDX-License-Identifier: Apache-2.0
#
# devshell.sh — run a shell or a command in the ocicfs dev container (.devcontainer).
#
#   ./devshell.sh             Bring the dev container up, open a shell in it, and
#                             remove the container when that shell exits.
#   ./devshell.sh <cmd...>    Bring the dev container up, run <cmd...> in it, remove
#                             the container on exit, and exit with <cmd>'s status
#                             (e.g. ./devshell.sh make smoke).
#   ./devshell.sh exec [cmd]  Run a shell (or [cmd]) in the already-running
#                             container, leaving it running when done.
#
# All forms attach with `devcontainer exec`, so the workspace and toolchain match CI.

set -euo pipefail

cd "$(dirname "$0")"

DOCKER="${DOCKER:-docker}"

case "${1:-}" in
exec)
    # Run a shell (or the given command) in the already-running container,
    # leaving it up. Reset the cursor to column 0 first; some devcontainer CLI /
    # terminal combinations leave it parked mid-line after their progress output.
    shift
    [ "$#" -gt 0 ] || set -- bash
    printf '\r\033[K'
    exec devcontainer exec "$@"
    ;;
*)
    # No args -> an interactive shell; any args -> run that command. Either way
    # bring the container up and remove exactly this one on exit (capture the id
    # from `devcontainer up` so the trap never touches another running container).
    container_id="$(devcontainer up | sed -n 's/.*"containerId":"\([^"]*\)".*/\1/p' | tail -1)"
    [ -n "$container_id" ] || { echo "devshell: failed to start the dev container" >&2; exit 1; }
    teardown() {
        trap - EXIT INT TERM HUP
        echo "devshell: removing dev container" >&2
        "$DOCKER" rm -f "$container_id" >/dev/null 2>&1 || true
    }
    trap teardown EXIT INT TERM HUP
    # devcontainer's in-place progress output can leave the cursor parked
    # mid-line; reset it to column 0 so output starts at the left.
    printf '\r\033[K'
    if [ "$#" -eq 0 ]; then
        devcontainer exec bash || true   # interactive: tolerate the shell's exit status
    else
        devcontainer exec "$@"           # command: propagate its exit status (set -e)
    fi
    ;;
esac
