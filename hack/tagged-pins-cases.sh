#!/usr/bin/env bash
# tagged-pins' verdict, exercised against local git remotes.
#
# The guard decides whether a pull request may land, and both ways of
# being wrong are quiet. Too loose and a pin to whatever was on master
# that afternoon passes while the `# v3.1.0` beside it says otherwise --
# the exact defect the guard was written for. Too strict and it reds
# every repository over a pin that is fine, which is how a gate gets
# switched off.
#
# It went from one hard-coded library to a LIST when the actions moved to
# truvity/ci-actions (INF-971), and a list is where it can newly be wrong
# in ways the old shape could not: a second library whose pins are never
# looked at passes silently, and so does a name nobody spelled right.
#
# NO NETWORK, and no test-only seam in the script either. `git ls-remote`
# resolves the https URL the script builds through a url.<base>.insteadOf
# rule in a throwaway GIT_CONFIG_GLOBAL, so the thing under test is the
# unmodified command line a job would run.
set -uo pipefail

here="$(cd "$(dirname "$0")/.." && pwd)"
script="$here/tagged-pins/tagged-pins.sh"

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# Redirect github.com at the fake remotes, for this script's git calls
# and the ones it spawns. A file in $work, never the real ~/.gitconfig.
export GIT_CONFIG_GLOBAL="$work/gitconfig"
export GIT_CONFIG_SYSTEM=/dev/null
git config -f "$GIT_CONFIG_GLOBAL" "url.$work/remotes/.insteadOf" "https://github.com/"
git config -f "$GIT_CONFIG_GLOBAL" user.email ci@example.invalid
git config -f "$GIT_CONFIG_GLOBAL" user.name ci
git config -f "$GIT_CONFIG_GLOBAL" init.defaultBranch master
# file:// transports between repositories owned by another uid are
# refused by default, and a runner's HOME is not always the checkout's
# owner.
git config -f "$GIT_CONFIG_GLOBAL" --add safe.directory '*'

fail=0

ok() { printf 'ok    %s\n' "$1"; }
bad() { printf 'FAIL  %s\n' "$1"; fail=1; }

# A library remote holding one ANNOTATED tag and one commit after it.
# Echoes "<commit of the tag> <untagged commit>".
#
# Annotated on purpose: a release here is an annotated tag, and the tag
# OBJECT sha is the plausible-looking wrong answer that the script's
# `^{}` dereference exists to reject.
make_library() {
  local slug=$1 bare="$work/remotes/$1" src="$work/src/$1"

  git init -q --bare "$bare"
  mkdir -p "$src"
  git init -q "$src"

  echo one >"$src/a"
  git -C "$src" add -A
  git -C "$src" commit -qm one
  git -C "$src" tag -a v1.0.0 -m v1.0.0

  echo two >"$src/a"
  git -C "$src" add -A
  git -C "$src" commit -qm two

  git -C "$src" push -q "$bare" HEAD:master --tags

  echo "$(git -C "$src" rev-parse 'v1.0.0^{}') $(git -C "$src" rev-parse HEAD)"
}

# Runs the script over a workspace whose .github holds the given `uses:`
# lines. Echoes the log; returns the script's status.
run_case() {
  local libraries=$1
  shift
  run_case_at ".github/workflows/w.yaml" "$libraries" "$@"
}

# Same shape, but the fixture lands OUTSIDE .github -- at a path like
# setup-devbox/action.yaml, a composite action nested in THIS repository
# rather than a caller's workflow. The guard now searches the whole
# checkout for exactly this reason: ci-cache's own pin lives there, not
# under .github, and a search still scoped to .github would default
# ci-cache into the library list and then report "nothing to check" on
# every run, forever.
run_case_at() {
  local rel=$1 libraries=$2
  shift 2

  local ws="$work/ws"
  rm -rf "$ws"
  mkdir -p "$ws/$(dirname "$rel")"

  {
    echo "jobs:"
    echo "  x:"
    echo "    steps:"
    local line
    for line in "$@"; do
      echo "      - uses: $line"
    done
  } >"$ws/$rel"

  (cd "$ws" && LIBRARIES="$libraries" bash "$script") 2>&1
}

read -r WF_TAGGED WF_LOOSE < <(make_library truvity/ci-workflows)
read -r AC_TAGGED AC_LOOSE < <(make_library truvity/ci-actions)

# ── one library: the shape that already worked ──────────────────────────

log=$(run_case "truvity/ci-workflows" "truvity/ci-workflows/.github/workflows/check.yaml@${WF_TAGGED}")
if [ $? = 0 ]; then
  ok "a pin naming a release is accepted"
else
  bad "a pin naming a release is accepted: $log"
fi

log=$(run_case "truvity/ci-workflows" "truvity/ci-workflows/.github/workflows/check.yaml@${WF_LOOSE}")
if [ $? = 0 ]; then
  bad "a pin naming no release is refused: $log"
else
  ok "a pin naming no release is refused"
fi

case "$log" in
*UNTAGGED*) ok "the refusal names the offending pin" ;;
*) bad "the refusal names the offending pin: $log" ;;
esac

# ── two libraries: the new shape ────────────────────────────────────────
#
# The regression that matters. A loop that resolves one tag list, or that
# stops at the first library with pins, accepts the ci-actions pin below
# without ever looking at it -- and looks exactly like a pass.

log=$(run_case "truvity/ci-workflows truvity/ci-actions" \
  "truvity/ci-workflows/.github/workflows/check.yaml@${WF_TAGGED}" \
  "truvity/ci-actions/setup-devbox@${AC_LOOSE}")
if [ $? = 0 ]; then
  bad "an untagged pin in the SECOND library is refused: $log"
else
  ok "an untagged pin in the second library is refused"
fi

case "$log" in
*"truvity/ci-actions/setup-devbox"*) ok "the refusal says which library the pin is in" ;;
*) bad "the refusal says which library the pin is in: $log" ;;
esac

log=$(run_case "truvity/ci-workflows,truvity/ci-actions" \
  "truvity/ci-workflows/.github/workflows/check.yaml@${WF_TAGGED}" \
  "truvity/ci-actions/setup-devbox@${AC_TAGGED}")
if [ $? = 0 ]; then
  ok "commas separate libraries as well as spaces"
else
  bad "commas separate libraries as well as spaces: $log"
fi

# ── ci-cache, and a pin OUTSIDE .github ──────────────────────────────────
#
# ci-cache joined the default library list on 2026-09-29 because this
# repository's OWN setup-devbox/action.yaml carries a nested
# `truvity/ci-cache/setup` pin -- and that file is not under .github. A
# guard whose search stayed scoped to .github would add ci-cache to the
# list and then report "nothing to check" on every single run, which is
# indistinguishable from a guard that works until somebody reads the
# fine print. So the search covers the whole checkout, and this is the
# case that would have caught the regression if it had not.

read -r CC_TAGGED CC_LOOSE < <(make_library truvity/ci-cache)

log=$(run_case_at "setup-devbox/action.yaml" "truvity/ci-workflows truvity/ci-actions truvity/ci-cache" \
  "truvity/ci-workflows/.github/workflows/check.yaml@${WF_TAGGED}" \
  "truvity/ci-cache/setup@${CC_LOOSE}")
if [ $? = 0 ]; then
  bad "an untagged ci-cache pin OUTSIDE .github is refused: $log"
else
  ok "an untagged ci-cache pin outside .github is refused"
fi

case "$log" in
*"truvity/ci-cache/setup"*) ok "the refusal names the ci-cache pin" ;;
*) bad "the refusal names the ci-cache pin: $log" ;;
esac

log=$(run_case_at "setup-devbox/action.yaml" "truvity/ci-workflows truvity/ci-actions truvity/ci-cache" \
  "truvity/ci-workflows/.github/workflows/check.yaml@${WF_TAGGED}" \
  "truvity/ci-cache/setup@${CC_TAGGED}")
if [ $? = 0 ]; then
  ok "a tagged ci-cache pin outside .github is accepted"
else
  bad "a tagged ci-cache pin outside .github is accepted: $log"
fi

# ── nothing to check ────────────────────────────────────────────────────
#
# A repository that pins neither library passes, and says how many it
# found pins for. Without that line a MISSPELLED library name produces
# exactly the same quiet success as a repository with nothing to check,
# which is the failure this whole file is about.

log=$(run_case "truvity/ci-workflows truvity/ci-actions" \
  "actions/checkout@0000000000000000000000000000000000000000")
if [ $? = 0 ]; then
  ok "a repository pinning neither library passes"
else
  bad "a repository pinning neither library passes: $log"
fi

case "$log" in
*"0 of 2 libraries had pins"*) ok "it reports how many libraries it actually found pins for" ;;
*) bad "it reports how many libraries it actually found pins for: $log" ;;
esac

# A caller that leaves the `libraries` input blank gets the DEFAULT, not
# "check nothing". An action input is always set and usually empty, so the
# quiet-success failure mode here is an empty string being read as an
# empty list -- a gate that passes everything while looking healthy.
log=$(run_case "" \
  "truvity/ci-workflows/.github/workflows/check.yaml@${WF_LOOSE}")
if [ $? = 0 ]; then
  bad "a blank libraries input falls back to the default rather than checking nothing: $log"
else
  ok "a blank libraries input falls back to the default rather than checking nothing"
fi

echo
if [ "$fail" = 0 ]; then
  echo "all cases pass"
else
  echo "::error::tagged-pins cases failed"
fi

exit "$fail"
