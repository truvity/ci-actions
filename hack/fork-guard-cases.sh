#!/usr/bin/env bash
# cluster's fork refusal, exercised against fake event payloads. mode:
# shared hands out this repository's kubeconfig, aws.ini and (optionally)
# an ECR login, so the one thing that must never be true is a fork pull
# request reaching it — and the only way to be sure the refusal fires is
# to run the actual guard script against the actual event shapes GitHub
# sends, not to read it and agree it looks right.
#
# No network, no token: $GITHUB_EVENT_PATH points at a file this script
# writes itself.
set -uo pipefail

here="$(cd "$(dirname "$0")/.." && pwd)"
script="$here/cluster/fork-guard.sh"

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

fail=0
ok()  { printf 'ok    %s\n' "$1"; }
bad() { printf 'FAIL  %s\n' "$1"; fail=1; }

run_case() {
  local payload=$1
  printf '%s' "$payload" >"$work/event.json"
  GITHUB_EVENT_PATH="$work/event.json" bash "$script" 2>&1
}

# ── the refusal itself ───────────────────────────────────────────────────

log=$(run_case '{"pull_request":{"head":{"repo":{"fork":true}}}}')
if [ $? = 0 ]; then
  bad "a fork pull request is refused: $log"
else
  ok "a fork pull request is refused"
fi

case "$log" in
*"refuses a fork pull request"*) ok "the refusal explains why" ;;
*) bad "the refusal explains why: $log" ;;
esac

# ── what must NOT be refused ─────────────────────────────────────────────

log=$(run_case '{"pull_request":{"head":{"repo":{"fork":false}}}}')
if [ $? = 0 ]; then
  ok "a same-repository pull request proceeds"
else
  bad "a same-repository pull request proceeds: $log"
fi

# `fork` absent entirely (some payload shapes omit it rather than send
# false) must not be read as "unknown, refuse" — the `// false` default in
# the guard's jq filter is what this pins down.
log=$(run_case '{"pull_request":{"head":{"repo":{}}}}')
if [ $? = 0 ]; then
  ok "a pull request payload with no fork field proceeds"
else
  bad "a pull request payload with no fork field proceeds: $log"
fi

# A push event carries no .pull_request at all.
log=$(run_case '{"ref":"refs/heads/master","commits":[]}')
if [ $? = 0 ]; then
  ok "a push event (no pull_request key) proceeds"
else
  bad "a push event (no pull_request key) proceeds: $log"
fi

# No event payload at all (GITHUB_EVENT_PATH unset or missing): the guard
# must not crash a caller that runs mode: shared from an event this
# library was not tested against — it is not a pull request, so it is not
# a fork pull request.
log=$(GITHUB_EVENT_PATH="$work/does-not-exist.json" bash "$script" 2>&1)
if [ $? = 0 ]; then
  ok "a missing event payload proceeds rather than crashing"
else
  bad "a missing event payload proceeds rather than crashing: $log"
fi

echo
if [ "$fail" = 0 ]; then
  echo "all cases pass"
else
  echo "::error::fork-guard cases failed"
fi

exit "$fail"
