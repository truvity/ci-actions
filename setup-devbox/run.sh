#!/usr/bin/env bash
# Thin wrapper: find the ci-actions binary and run one step of `setup-devbox`
# with it (preflight | aws-config | detect-baked | strip-tools | install-devbox
# | materialize | proto | expose-token | go-private | codeartifact |
# retired-cache-server | guard-goproxy | guard-aws). The logic lives in
# cmd/ci-actions (internal/setupdevbox); which binary is chosen is
# lib/ci-actions-bin.sh's job, shared by every wrapper.
set -euo pipefail
# shellcheck source=../lib/ci-actions-bin.sh
. "${ACTION_PATH:?ACTION_PATH is the directory of this action}/../lib/ci-actions-bin.sh"
ci_actions_run setup-devbox setup-devbox "${1:?the setup-devbox step to run}"
