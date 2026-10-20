#!/usr/bin/env bash
# Thin wrapper: find the ci-actions binary and run `buildkit-tls` with it. The logic
# lives in cmd/ci-actions (internal/buildkittls); which binary is chosen is
# lib/ci-actions-bin.sh's job, shared by every wrapper. No `set -x`, here or
# in the library: the environment this runs in holds a private key.
set -euo pipefail
# shellcheck source=../lib/ci-actions-bin.sh
. "${ACTION_PATH:?ACTION_PATH is the directory of this action}/../lib/ci-actions-bin.sh"
ci_actions_run setup-buildkit-tls buildkit-tls
