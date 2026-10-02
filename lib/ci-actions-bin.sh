#!/usr/bin/env bash
# Shared by every thin wrapper (<action>/run.sh): find the ci-actions binary
# and exec one subcommand of it. Sourced, never run. The rule itself lives in
# cmd/ci-actions; this only decides WHICH binary, in this order:
#
#   1. $CI_ACTIONS_BIN, or `ci-actions` already on PATH
#   2. the release archive of the tag the action is pinned at: consumers
#      pin the COMMIT of a tag, so the commit resolves back to the tag and
#      to its goreleaser archive, verified against checksums.txt
#   3. built from this checkout with Go, when Go is on PATH (a commit that
#      is not a release, a local `./<action>`, a developer's machine)
#
# No privilege is needed at any step: everything lands in $RUNNER_TEMP.
#
#   ci_actions_run <action-name> <subcommand> [args...]
#
# Reads ACTION_PATH (required), ACTION_REPOSITORY and ACTION_REF.

ci_actions_run() {
  local name=$1
  shift
  local action_path="${ACTION_PATH:?ACTION_PATH is the directory of this action}"
  local repo="${ACTION_REPOSITORY:-}"
  local ref="${ACTION_REF:-}"
  local tmp="${RUNNER_TEMP:-${TMPDIR:-/tmp}}/ci-actions-bin"

  if [ -n "${CI_ACTIONS_BIN:-}" ] && [ -x "$CI_ACTIONS_BIN" ]; then
    exec "$CI_ACTIONS_BIN" "$@"
  fi
  if command -v ci-actions >/dev/null 2>&1; then
    exec "$(command -v ci-actions)" "$@"
  fi

  mkdir -p "$tmp"

  if _ci_actions_fetch_release "$repo" "$ref" "$tmp"; then
    exec "$tmp/ci-actions" "$@"
  fi

  if command -v go >/dev/null 2>&1 && [ -f "$action_path/../go.mod" ]; then
    (cd "$action_path/.." && CGO_ENABLED=0 go build -trimpath -o "$tmp/ci-actions" ./cmd/ci-actions)
    exec "$tmp/ci-actions" "$@"
  fi

  echo "::error::${name} needs the ci-actions binary: this action is pinned at ${ref:-a local path}, which names no release archive, and Go is not on PATH to build it. Pin the commit of a release tag, or put Go on PATH."
  exit 1
}

_ci_actions_fetch_release() {
  local repo=$1 ref=$2 tmp=$3
  [[ "$ref" =~ ^[0-9a-f]{40}$ ]] && [ -n "$repo" ] || return 1
  command -v curl >/dev/null 2>&1 && command -v tar >/dev/null 2>&1 && command -v sha256sum >/dev/null 2>&1 || return 1

  local tag os arch name base
  tag=$(git ls-remote --tags "https://github.com/${repo}" 2>/dev/null \
    | sed -n "s|^${ref}[[:space:]]*refs/tags/\\(.*\\)^{}\$|\\1|p" | head -n1)
  [ -n "$tag" ] || return 1

  os=$(uname -s | tr '[:upper:]' '[:lower:]')
  case "$(uname -m)" in
    x86_64 | amd64) arch=amd64 ;;
    aarch64 | arm64) arch=arm64 ;;
    *) return 1 ;;
  esac
  name="ci-actions_${tag#v}_${os}_${arch}.tar.gz"
  base="https://github.com/${repo}/releases/download/${tag}"

  curl -fsSL --retry 2 -o "$tmp/$name" "$base/$name" || return 1
  curl -fsSL --retry 2 -o "$tmp/checksums.txt" "$base/checksums.txt" || return 1
  (cd "$tmp" && grep -F " $name" checksums.txt | sha256sum -c - >/dev/null) || {
    echo "::error::checksum mismatch for $name; refusing to run it"
    exit 1
  }
  tar -xzf "$tmp/$name" -C "$tmp" ci-actions
  return 0
}
