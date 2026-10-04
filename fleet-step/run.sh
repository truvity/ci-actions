#!/usr/bin/env bash
# Thin wrapper: find the ci-actions binary and run one `fleet-step` with it:
# oidc-claims | commit-author | parity-settings | approve-renovate |
# available-majors. The logic lives in cmd/ci-actions (internal/fleetsteps);
# which binary is chosen is lib/ci-actions-bin.sh's job, shared by every wrapper.
set -euo pipefail
# shellcheck source=../lib/ci-actions-bin.sh
. "${ACTION_PATH:?ACTION_PATH is the directory of this action}/../lib/ci-actions-bin.sh"
ci_actions_run fleet-step fleet-step "${1:?the fleet step to run}"
