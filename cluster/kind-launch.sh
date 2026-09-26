#!/usr/bin/env bash
# mode: kind. Fetches truvity/policy's hack/kind/ box AT A PINNED RELEASE,
# runs it (up.sh already runs verify.sh at its own last step — see that
# script), wires a local image registry into the cluster, and exports the
# five things every caller reads regardless of tier.
#
# The box is fetched from a release TARBALL rather than vendored here or
# published as a release asset of its own: truvity/policy already tags
# releases, a tag's tarball is a stable, content-addressed artifact GitHub
# serves for any public repository with no token, and "download the
# archive, keep hack/kind/" is one curl and one tar — no new publishing
# step for policy to maintain, and no second copy of the box to keep in
# sync with the one policy's own CI runs. A sparse clone at the tag would
# get the same content at the cost of a full git invocation for four
# files; the tarball is what "prefer that over inventing a release asset"
# in this action's brief means in practice.
#
# The box needs nothing but kind, kubectl, helm and openssl on PATH
# (action.yml installs the first three at pinned versions; openssl ships
# on GitHub's hosted images) — it is a self-contained directory, not
# something that reaches into the rest of a policy checkout. That is what
# makes running it from a tarball, in a repository that is not policy,
# possible at all.
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

: "${GITHUB_ENV:?}" "${GITHUB_OUTPUT:?}"
# shellcheck source=cluster/emit.sh
source "$here/emit.sh"

state_dir=${STATE_DIR:-}
[ -n "$state_dir" ] || state_dir="${RUNNER_TEMP:?RUNNER_TEMP is not set}/cluster-action"
mkdir -p "$state_dir"

namespace=${NAMESPACE:-e2e}
[ -n "$namespace" ] || namespace=e2e

release=${RELEASE:-}
if [ -z "$release" ]; then
  # Same shape truvity/ci-workflows' integration.yaml uses for a lane:
  # `<name>-r<run id>-a<run attempt>`, unique per job and per re-run so
  # parallel jobs and retries never collide. `job` stands in for `lane`
  # here — a composite action has no lane concept of its own.
  release="${GITHUB_JOB:-e2e}-r${GITHUB_RUN_ID:-0}-a${GITHUB_RUN_ATTEMPT:-1}"
fi

policy_version=${POLICY_VERSION:-}
[ -n "$policy_version" ] || { echo "::error::policy-version is required for mode: kind"; exit 1; }

box="$state_dir/policy-kind-box"
if [ ! -x "$box/up.sh" ]; then
  echo "fetching truvity/policy@${policy_version}'s hack/kind/ box"
  tmp=$(mktemp -d)
  trap 'rm -rf "$tmp"' EXIT
  curl -fsSL -o "$tmp/policy.tgz" \
    "https://github.com/truvity/policy/archive/refs/tags/${policy_version}.tar.gz"
  mkdir -p "$box"
  # Restricted to the one subtree: this never sees the rest of policy's
  # checkout, so it cannot start depending on anything outside hack/kind/
  # without the extraction itself failing to find it.
  tar xzf "$tmp/policy.tgz" -C "$box" --strip-components=3 \
    "policy-${policy_version#v}/hack/kind"
  chmod +x "$box"/*.sh
fi

# Never the default ~/.kube/config: a caller's own job may run other
# kubectl/helm commands against a different cluster (mode: shared,
# earlier or later in the same workflow, or its own tooling), and a box
# that quietly repoints the default context would step on that. kind,
# kubectl and helm all honour $KUBECONFIG, and up.sh itself never
# hard-codes a path — it only ever calls `kubectl config use-context`.
kubeconfig="$state_dir/kubeconfig"

outputs_file="$state_dir/outputs.env"
cat >"$outputs_file" <<ENV
KUBECONFIG=$kubeconfig
SNAPSHOT_REGISTRY=localhost:5001
GEMAAL_TIER=kind
GEMAAL_NAMESPACE=$namespace
GEMAAL_RELEASE=$release
ENV

# Emitted immediately, background or not: every value above is a path or
# a fixed string, known before the cluster exists. What background: true
# defers is the cluster being USABLE, not what it will be called.
emit_from_file "$outputs_file"

run_script="$state_dir/run.sh"
log="$state_dir/log"
pid_file="$state_dir/pid"
exit_file="$state_dir/exit-code"
rm -f "$exit_file"

cat >"$run_script" <<SCRIPT
#!/usr/bin/env bash
set -euo pipefail
export KUBECONFIG="$kubeconfig"
export PATH="$PATH"
cd "$box"
./up.sh
"$here/kind-registry-up.sh" "$kubeconfig"
SCRIPT
chmod +x "$run_script"

if [ "${BACKGROUND:-false}" = "true" ]; then
  # setsid + full redirection + disown: the box must keep running after
  # THIS step's shell exits, which is the whole point of background:
  # true. Without all three a backgrounded job is liable to be reaped
  # with the step's process group, or to keep the step "running" because
  # its stdout is still the step's own pipe.
  setsid bash -c '
    set +e
    bash "$1" >"$2" 2>&1
    echo $? >"$3"
  ' _ "$run_script" "$log" "$exit_file" </dev/null &
  echo $! >"$pid_file"
  disown
  echo "kind box launching in the background — log at $log"
  echo "call this action again with mode: wait (same state-dir) before using the cluster"
else
  set +e
  bash "$run_script" 2>&1 | tee "$log"
  code=${PIPESTATUS[0]}
  set -e
  echo "$code" >"$exit_file"
  if [ "$code" != 0 ]; then
    echo "::error::the kind box failed to come up (exit $code) — see the log above"
    exit "$code"
  fi
  echo "kind box ready — kubeconfig at $kubeconfig"
fi
