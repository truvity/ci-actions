#!/usr/bin/env bash
# mode: shared refuses a fork pull request. First, before anything else in
# that mode resolves a kubeconfig, reads an aws.ini or logs into ECR — a
# fork's pull request receives no secrets from GitHub already, but the
# lesson public-runners carries applies here just as much: the check
# belongs before the thing it is guarding, not after, so a caller who
# copies mode: shared into the wrong workflow is told in seconds rather
# than finding out when a credential exchange succeeds for code it should
# never have reached.
#
# Reads $GITHUB_EVENT_PATH directly rather than trusting an input, so a
# caller cannot pass `fork: false` by accident (or a stale cached value)
# and defeat the refusal it exists to make. No event payload, or an event
# that is not a pull_request, is not a fork PR — pushes, schedules and
# workflow_dispatch all take this path.
set -euo pipefail

event_path=${GITHUB_EVENT_PATH:-}

if [ -z "$event_path" ] || [ ! -f "$event_path" ]; then
  echo "no event payload — not a pull request, mode: shared may proceed"
  exit 0
fi

is_fork=$(jq -r '.pull_request.head.repo.fork // false' "$event_path")

if [ "$is_fork" = "true" ]; then
  echo "::error::mode: shared refuses a fork pull request — a fork's code must never receive this repository's cluster credentials, ECR login or kubeconfig. Use mode: kind for pull requests, and mode: shared only for pushes/merges this repository trusts."
  exit 1
fi

echo "not a fork pull request — mode: shared may proceed"
