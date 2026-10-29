#!/usr/bin/env bash
# Thin wrapper: find the ci-actions binary and run `company-validation` with
# it. The request, dispatch, wait and correlation live in cmd/ci-actions
# (internal/companyvalidation); which binary is chosen is
# lib/ci-actions-bin.sh's job, shared by every wrapper.
set -euo pipefail
# shellcheck source=../lib/ci-actions-bin.sh
. "${ACTION_PATH:?ACTION_PATH is the directory of this action}/../lib/ci-actions-bin.sh"
ci_actions_run company-validation company-validation
