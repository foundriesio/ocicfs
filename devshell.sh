#!/usr/bin/env bash
#
# Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
# SPDX-License-Identifier: Apache-2.0
#
# devshell.sh — open a shell in the knarr dev container (.devcontainer).
#
#   ./devshell.sh        Bring the dev container up, open a shell in it, and
#                        remove the container when that shell exits.
#   ./devshell.sh exec   Open another shell in the already-running container,
#                        leaving it running when the shell exits.
#
# Both attach with `devcontainer exec`, so the workspace and toolchain match CI.

set -euo pipefail

cd "$(dirname "$0")"

DOCKER="${DOCKER:-docker}"

case "${1:-}" in
exec)
    # Reset the cursor to column 0 first; some devcontainer CLI / terminal
    # combinations leave it parked mid-line after their progress output.
    printf '\r\033[K'
    exec devcontainer exec bash
    ;;
"")
    # Capture the id from `devcontainer up` so the trap removes exactly this
    # container on exit, not any other dev container that may be running.
    container_id="$(devcontainer up | sed -n 's/.*"containerId":"\([^"]*\)".*/\1/p' | tail -1)"
    [ -n "$container_id" ] || { echo "devshell: failed to start the dev container" >&2; exit 1; }
    teardown() {
        trap - EXIT INT TERM HUP
        echo "devshell: removing dev container" >&2
        "$DOCKER" rm -f "$container_id" >/dev/null 2>&1 || true
    }
    trap teardown EXIT INT TERM HUP
    # devcontainer's in-place progress output can leave the cursor parked
    # mid-line; reset it to column 0 so the shell prompt starts at the left.
    printf '\r\033[K'
    devcontainer exec bash || true
    ;;
*)
    echo "usage: $(basename "$0") [exec]" >&2
    exit 2
    ;;
esac
