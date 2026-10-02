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

  # A job calls several steps of the same action (cluster has five), and
  # each is a separate process: a binary already resolved for this pin is
  # reused, never fetched twice. Keyed by the pinned commit, so two pins of
  # this library in one job never share a binary.
  local bin rc=0
  bin=$(_ci_actions_fetch_release "$repo" "$ref" "$tmp") || rc=$?
  [ "$rc" = 0 ] && exec "$bin" "$@"
  # 2 is a checksum mismatch: never fall through to another binary after it.
  [ "$rc" = 2 ] && exit 1

  if command -v go >/dev/null 2>&1 && [ -f "$action_path/../go.mod" ]; then
    bin="$tmp/ci-actions"
    if [[ "$ref" =~ ^[0-9a-f]{40}$ ]]; then
      bin="$tmp/build-$ref/ci-actions"
      mkdir -p "$tmp/build-$ref"
    fi
    if [ ! -x "$bin" ] || [ "$bin" = "$tmp/ci-actions" ]; then
      (cd "$action_path/.." && CGO_ENABLED=0 go build -trimpath -o "$bin" ./cmd/ci-actions)
    fi
    exec "$bin" "$@"
  fi

  echo "::error::${name} needs the ci-actions binary: this action is pinned at ${ref:-a local path}, which names no release archive, and Go is not on PATH to build it. Pin the commit of a release tag, or put Go on PATH."
  exit 1
}

# Prints the path of the verified binary, or fails. Everything else goes to
# stderr: stdout is the answer.
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

  local dir="$tmp/$ref"
  if [ -x "$dir/ci-actions" ]; then
    printf '%s' "$dir/ci-actions"
    return 0
  fi
  mkdir -p "$dir"
  curl -fsSL --retry 2 -o "$dir/$name" "$base/$name" || return 1
  curl -fsSL --retry 2 -o "$dir/checksums.txt" "$base/checksums.txt" || return 1
  (cd "$dir" && grep -F " $name" checksums.txt | sha256sum -c - >/dev/null) || {
    echo "::error::checksum mismatch for $name; refusing to run it" >&2
    return 2
  }
  tar -xzf "$dir/$name" -C "$dir" ci-actions
  printf '%s' "$dir/ci-actions"
  return 0
}
