#!/usr/bin/env bash
# Thin wrapper: find the ci-actions binary and run `tagged-pins` with it.
# The rule itself lives in cmd/ci-actions (internal/taggedpins); this file
# only decides WHICH binary, in this order:
#
#   1. $CI_ACTIONS_BIN, or `ci-actions` already on PATH
#   2. the release archive of the tag this action is pinned at: consumers
#      pin the COMMIT of a tag, so the commit resolves back to the tag and
#      to its goreleaser archive, verified against checksums.txt
#   3. built from this checkout with Go, when Go is on PATH (a commit that
#      is not a release, a local `./tagged-pins`, a developer's machine)
#
# No privilege is needed at any step: everything lands in $RUNNER_TEMP.
set -euo pipefail

action_path="${ACTION_PATH:?ACTION_PATH is the directory of this action}"
repo="${ACTION_REPOSITORY:-}"
ref="${ACTION_REF:-}"
tmp="${RUNNER_TEMP:-${TMPDIR:-/tmp}}/ci-actions-bin"

run_bin() {
  exec "$1" tagged-pins
}

if [ -n "${CI_ACTIONS_BIN:-}" ] && [ -x "$CI_ACTIONS_BIN" ]; then
  run_bin "$CI_ACTIONS_BIN"
fi
if command -v ci-actions >/dev/null 2>&1; then
  run_bin "$(command -v ci-actions)"
fi

mkdir -p "$tmp"

fetch_release() {
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

if fetch_release; then
  run_bin "$tmp/ci-actions"
fi

if command -v go >/dev/null 2>&1 && [ -f "$action_path/../go.mod" ]; then
  (cd "$action_path/.." && CGO_ENABLED=0 go build -trimpath -o "$tmp/ci-actions" ./cmd/ci-actions)
  run_bin "$tmp/ci-actions"
fi

echo "::error::tagged-pins needs the ci-actions binary: this action is pinned at ${ref:-a local path}, which names no release archive, and Go is not on PATH to build it. Pin the commit of a release tag, or put Go on PATH."
exit 1
