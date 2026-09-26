#!/usr/bin/env bash
# Writes the five outputs BOTH ways — $GITHUB_ENV, because the rest of a
# job's steps are plain shell that reads KUBECONFIG etc. directly, and
# $GITHUB_OUTPUT, because a step that calls this action wants
# `steps.<id>.outputs.*` without depending on env leaking across a
# `uses:` boundary the way it does across `run:` steps.
#
# Sourced, not run: it needs the caller's $GITHUB_ENV and $GITHUB_OUTPUT,
# and turning it into a subprocess would only mean re-deriving them.
#
#   emit KUBECONFIG "$kubeconfig" kubeconfig
#   emit SNAPSHOT_REGISTRY "$registry" snapshot-registry
#
# arg1: env var name (UPPER_SNAKE)  arg2: value  arg3: output name (kebab-case)
emit() {
  local env_name=$1 value=$2 output_name=$3
  echo "${env_name}=${value}" >>"$GITHUB_ENV"
  echo "${output_name}=${value}" >>"$GITHUB_OUTPUT"
}

# Reads a KEY=VALUE file (as written by kind-launch.sh for kind-wait.sh to
# pick back up — a later step has none of this step's env, only files) and
# emits the five outputs from it.
emit_from_file() {
  local file=$1 key value
  while IFS='=' read -r key value; do
    [ -n "$key" ] || continue
    case "$key" in
      KUBECONFIG)          emit KUBECONFIG "$value" kubeconfig ;;
      SNAPSHOT_REGISTRY)   emit SNAPSHOT_REGISTRY "$value" snapshot-registry ;;
      GEMAAL_TIER)         emit GEMAAL_TIER "$value" gemaal-tier ;;
      GEMAAL_NAMESPACE)    emit GEMAAL_NAMESPACE "$value" gemaal-namespace ;;
      GEMAAL_RELEASE)      emit GEMAAL_RELEASE "$value" gemaal-release ;;
    esac
  done <"$file"
}
