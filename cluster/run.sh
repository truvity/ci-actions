#!/usr/bin/env bash
# Thin wrapper: find the ci-actions binary and run one step of `cluster`
# with it: fork-guard | kind | wait | shared-connect | shared-finish. The
# logic lives in cmd/ci-actions (internal/cluster); which binary is chosen is
# lib/ci-actions-bin.sh's job, shared by every wrapper.
set -euo pipefail
# shellcheck source=../lib/ci-actions-bin.sh
. "${ACTION_PATH:?ACTION_PATH is the directory of this action}/../lib/ci-actions-bin.sh"
ci_actions_run cluster cluster "${1:?the cluster step to run}"
