#!/usr/bin/env bash
# Thin wrapper: find the ci-actions binary and run one step of `auto-release`
# with it: gate | tag. The logic lives in cmd/ci-actions (internal/autorelease);
# which binary is chosen is lib/ci-actions-bin.sh's job, shared by every wrapper.
set -euo pipefail
# shellcheck source=../lib/ci-actions-bin.sh
. "${ACTION_PATH:?ACTION_PATH is the directory of this action}/../lib/ci-actions-bin.sh"
ci_actions_run auto-release auto-release "${1:?the auto-release step to run}"
