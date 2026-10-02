#!/usr/bin/env bash
# TEMPORARY, removed before merge: the shell public-runners (read from the
# base the pull request branched from) against the Go port, on the same
# fixtures, comparing stdout+stderr and the exit status byte for byte.
set -uo pipefail
here="$(cd "$(dirname "$0")/.." && pwd)"
base="${PARITY_BASE:-origin/master}"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
git -C "$here" show "$base:public-runners/public-runners.sh" >"$work/old.sh" || { echo "::error::cannot read the old script at $base"; exit 2; }
(cd "$here" && CGO_ENABLED=0 go build -o "$work/ci-actions" ./cmd/ci-actions) || exit 2
# `gh` that cannot answer, so an unset visibility is "unknown" in both.
mkdir -p "$work/bin"
printf '#!/bin/sh\nexit 1\n' >"$work/bin/gh"
chmod +x "$work/bin/gh"

fail=0
n=0
case_() { # name runners visibility [repo]
  n=$((n + 1))
  local runners=$2 vis=$3 repo=${4:-acme/app}
  old=$(env -i PATH="$work/bin:$PATH" RUNNERS="$runners" VISIBILITY="$vis" GITHUB_REPOSITORY="$repo" GH_TOKEN=x bash "$work/old.sh" 2>&1; echo "rc=$?")
  new=$(env -i PATH="$work/bin:$PATH" RUNNERS="$runners" VISIBILITY="$vis" GITHUB_REPOSITORY="$repo" GH_TOKEN=x GITHUB_API_URL=http://127.0.0.1:1 "$work/ci-actions" public-runners 2>&1; echo "rc=$?")
  if [ "$old" = "$new" ]; then
    printf 'same  %s\n' "$1"
  else
    printf 'DIFF  %s\n--- old\n%s\n--- new\n%s\n' "$1" "$old" "$new"
    fail=1
  fi
}
case_ "private, any runner" "tier-big" private
case_ "internal" "x" internal
case_ "public hosted" "ubuntu-latest" public
case_ "public, three platforms" $'ubuntu-24.04 windows-2022\nmacos-14' public
case_ "public, none" "" public
case_ "public, blanks" $'  \t ubuntu-latest   ' public
case_ "public, self-hosted" "self-hosted" public
case_ "public, estate tier among hosted" "ubuntu-latest tier-small ubuntu-latest" public
case_ "public, larger runner" "linux-big" public
case_ "public, bare prefix" "ubuntu" public
case_ "unknown visibility" "ubuntu-latest" ""
case_ "dash and dollar in the repository name" "ubuntu-latest" public 'a-b/c_d.e'
echo "$n cases"
exit $fail
