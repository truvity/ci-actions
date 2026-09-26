#!/usr/bin/env bash
# mode: shared, last step. Emits the same five things mode: kind emits,
# now that identity is proved and ECR (if asked for) is logged into.
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

: "${GITHUB_ENV:?}" "${GITHUB_OUTPUT:?}" "${KUBECONFIG:?shared-connect.sh should have set this}"
# shellcheck source=cluster/emit.sh
source "$here/emit.sh"

namespace=${NAMESPACE:-e2e}
[ -n "$namespace" ] || namespace=e2e

release=${RELEASE:-}
if [ -z "$release" ]; then
  # Same shape as mode: kind and as truvity/ci-workflows' integration.yaml:
  # `<name>-r<run id>-a<run attempt>`.
  release="${GITHUB_JOB:-e2e}-r${GITHUB_RUN_ID:-0}-a${GITHUB_RUN_ATTEMPT:-1}"
fi

# amazon-ecr-login's own `registry` output: a comma-delimited list when
# `ecr-registries` named more than one account. "The ECR registry host"
# in this action's brief is singular, matching how SNAPSHOT_REGISTRY is
# used everywhere else (one push target) — so the first one wins. Empty
# when ecr-registries was empty: this mode's caller then has no
# SNAPSHOT_REGISTRY, the same as if it never ran a login step at all.
registry=${ECR_REGISTRY:-}
registry=${registry%%,*}

emit KUBECONFIG "$KUBECONFIG" kubeconfig
emit SNAPSHOT_REGISTRY "$registry" snapshot-registry
# "shared", not unset: gemaal's harness.DetectTier (pkg/harness/tier.go,
# v0.24.0) reads GEMAAL_TIER "when set (any value — TierKind is the only
# one the harness treats specially today)", so a non-"kind" value falls
# back to exactly the shared-cluster behaviour every caller already gets
# from leaving it unset — and a caller of THIS action gets to branch on
# GEMAAL_TIER without a third "empty means shared" rule to remember.
emit GEMAAL_TIER shared gemaal-tier
emit GEMAAL_NAMESPACE "$namespace" gemaal-namespace
emit GEMAAL_RELEASE "$release" gemaal-release
