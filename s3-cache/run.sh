#!/usr/bin/env bash
# Thin wrapper: find the ci-actions binary and run `s3-cache <restore|save>`
# with it. The logic lives in internal/s3cache; which binary is chosen is
# lib/ci-actions-bin.sh's job.
#
# Fail-open. A cache that cannot be reached is a miss and the binary says so
# itself; a binary that cannot be found is the same thing one level up, so
# say `fallback=true` and let the action hand the step to GitHub's cache.
set -uo pipefail
step="${1:?restore or save}"
# Called from s3-cache/<step> and from setup-devbox, so locate the repository
# root by this file and give the shared library what it expects: the
# directory of an action that sits directly under that root.
ACTION_PATH="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
export ACTION_PATH
# shellcheck source=../lib/ci-actions-bin.sh
. "$ACTION_PATH/../lib/ci-actions-bin.sh"

( ci_actions_run s3-cache s3-cache "$step" )
rc=$?
if [ "$rc" -ne 0 ]; then
  # A runner whose own identity only reads the caches (CI_RUNNER_CACHE_WRITES=
  # release-credentials, its own bucket in use) never lets GitHub's cache stand in
  # for a SAVE: without the binary the save is skipped, not handed over. Restore
  # may still fall back, it only reads.
  writes="${CI_RUNNER_CACHE_WRITES:-}"
  writes="${writes//[[:space:]]/}"
  if [ "$step" = save ] && [ -n "${CI_RUNNER_GOCACHE_S3_BUCKET:-}" ] && [ "$writes" = "release-credentials" ]; then
    echo "::warning::s3-cache: the ci-actions binary is unavailable (exit $rc); the save is skipped (this runner's cache writes need the job's own AWS credentials)"
    [ -n "${GITHUB_OUTPUT:-}" ] && echo "skipped=true" >> "$GITHUB_OUTPUT"
  else
    echo "::warning::s3-cache: the ci-actions binary is unavailable (exit $rc); using GitHub's cache for this step"
    [ -n "${GITHUB_OUTPUT:-}" ] && echo "fallback=true" >> "$GITHUB_OUTPUT"
  fi
fi
exit 0
