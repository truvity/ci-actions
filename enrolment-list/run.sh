#!/usr/bin/env bash
# Thin wrapper: find the ci-actions binary and run `enrolment-list` with it. The rule
# itself lives in cmd/ci-actions (internal/workflowinputs); which binary is
# chosen is lib/ci-actions-bin.sh's job, shared by every wrapper.
set -euo pipefail
# shellcheck source=../lib/ci-actions-bin.sh
. "${ACTION_PATH:?ACTION_PATH is the directory of this action}/../lib/ci-actions-bin.sh"
ci_actions_run enrolment-list enrolment-list
