#!/usr/bin/env bash
# Thin wrapper: find the ci-actions binary and run `policy-conformance` with
# it, from the caller's working directory (the repository being judged). The
# rules live in cmd/ci-actions (internal/policyconformance); which binary is
# chosen is lib/ci-actions-bin.sh's job, shared by every wrapper.
set -euo pipefail
# shellcheck source=../lib/ci-actions-bin.sh
. "${ACTION_PATH:?ACTION_PATH is the directory of this action}/../lib/ci-actions-bin.sh"
ci_actions_run policy-conformance policy-conformance
