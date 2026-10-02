#!/usr/bin/env bash
# Thin wrapper: find the ci-actions binary and run `openbao-secrets` with it.
# The logic lives in cmd/ci-actions (internal/openbaosecrets); which binary is
# chosen is lib/ci-actions-bin.sh's job, shared by every wrapper. Nothing here
# reads or prints a secret: the inputs reach the binary through the
# environment, and the binary does the rest.
set -euo pipefail
# shellcheck source=../lib/ci-actions-bin.sh
. "${ACTION_PATH:?ACTION_PATH is the directory of this action}/../lib/ci-actions-bin.sh"
ci_actions_run openbao-secrets openbao-secrets
