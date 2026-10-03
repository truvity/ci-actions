#!/usr/bin/env bash
# Thin wrapper: find the ci-actions binary and run `publish-nix-flakes` with
# it. The logic lives in cmd/ci-actions (internal/releasepublic); which binary
# is chosen is lib/ci-actions-bin.sh's job, shared by every wrapper.
set -euo pipefail
# shellcheck source=../lib/ci-actions-bin.sh
. "${ACTION_PATH:?ACTION_PATH is the directory of this action}/../lib/ci-actions-bin.sh"
ci_actions_run publish-nix-flakes publish-nix-flakes
