#!/usr/bin/env bash
# mode: shared. Resolves the caller's own kubeconfig and aws.ini — the
# same files a laptop uses — and proves the cluster identity before
# anything else in the job spends a credential. Runs after fork-guard.sh
# has already refused a fork pull request.
set -euo pipefail

: "${GITHUB_ENV:?}" "${GITHUB_WORKSPACE:?}" "${RUNNER_TEMP:?}"

kubeconfig_input=${KUBECONFIG_INPUT:-}
[ -n "$kubeconfig_input" ] || { echo "::error::kubeconfig is required for mode: shared"; exit 1; }
aws_config_input=${AWS_CONFIG_FILE_INPUT:-}
[ -n "$aws_config_input" ] || { echo "::error::aws-config-file is required for mode: shared"; exit 1; }

kubeconfig="$GITHUB_WORKSPACE/$kubeconfig_input"
aws_config="$GITHUB_WORKSPACE/$aws_config_input"
[ -f "$kubeconfig" ] || { echo "::error::kubeconfig not found at $kubeconfig"; exit 1; }
[ -f "$aws_config" ] || { echo "::error::aws-config-file not found at $aws_config"; exit 1; }

{
  echo "KUBECONFIG=$kubeconfig"
  echo "AWS_CONFIG_FILE=$aws_config"
} >>"$GITHUB_ENV"
export KUBECONFIG="$kubeconfig"
export AWS_CONFIG_FILE="$aws_config"

expected=${EXPECTED_IDENTITY:-}
if [ -z "$expected" ]; then
  echo "expected-identity is empty — skipping the identity proof"
  exit 0
fi

# Three tries, with stderr kept so a real refusal is readable, and stderr
# to a FILE rather than merged into the capture — copied from
# truvity/ci-workflows' integration.yaml rather than rewritten, because
# the failure it caught (a bare "exit status 1" that lost a whole lane to
# devbox's own banner landing in front of the JSON) has no reason to
# reappear just because the code moved repositories. This action assumes
# no devbox and calls kubectl directly, so that particular banner cannot
# recur here, but the shape — three tries, stderr kept, exit before any
# build — is kept exactly.
for attempt in 1 2 3; do
  if out=$(kubectl auth whoami -o json 2>"$RUNNER_TEMP/whoami.err"); then
    break
  fi
  echo "attempt $attempt:"
  cat "$RUNNER_TEMP/whoami.err"
  if [ "$attempt" = 3 ]; then
    echo "::error::kubectl auth whoami failed three times"
    exit 1
  fi
  sleep 10
done

who=$(printf '%s' "$out" | sed -n '/^{/,$p' | jq -r .status.userInfo.username)
echo "cluster identity: $who"
[ "$who" = "$expected" ] || { echo "::error::expected cluster identity $expected, got $who"; exit 1; }
