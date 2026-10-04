#!/usr/bin/env bash
# Thin wrapper: find the ci-actions binary and run one step of `pkl-fleet` with
# it: rewrite | resolve | publish. The logic lives in cmd/ci-actions
# (internal/pklfleet); which binary is chosen is lib/ci-actions-bin.sh's job,
# shared by every wrapper.
set -euo pipefail
# shellcheck source=../lib/ci-actions-bin.sh
. "${ACTION_PATH:?ACTION_PATH is the directory of this action}/../lib/ci-actions-bin.sh"
ci_actions_run pkl-fleet pkl-fleet "${1:?the pkl-fleet step to run}"
