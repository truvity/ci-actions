#!/usr/bin/env bash
# The no-privilege contract, enforced: no action file and no script in this
# repository may call sudo.
#
# These actions run on runners under the Pod Security `restricted` profile
# (uid 1001, no_new_privs, all capabilities dropped), where sudo can never
# work. GitHub-hosted runners do have passwordless sudo, and we no longer
# use it: an action that needs it works on one and breaks on the other,
# which is exactly how setup-devbox broke every pinned caller.
#
# Scans everything but prose (*.md) -- a changelog may SAY the word -- and
# hack/restricted-sim/, which installs sudo on purpose as the control that
# proves the simulation fails because of no_new_privs and nothing else.
#
# The word is matched as a whole word, in comments too: a commented-out
# call is one edit from a live one.
#
#   hack/no-escalation-cases.sh            scan this repository, then self-test
#   hack/no-escalation-cases.sh --scan DIR scan DIR only (exit 1 on a hit)
set -uo pipefail

here="$(cd "$(dirname "$0")/.." && pwd)"
word='su''do' # spelled in two pieces so this file does not trip itself

scan() {
  local root=$1
  grep -rnIw "$word" "$root" \
    --exclude-dir=.git --exclude-dir=restricted-sim --exclude-dir=node_modules \
    --exclude='*.md' --exclude=no-escalation-cases.sh
}

if [ "${1:-}" = "--scan" ]; then
  if scan "$2"; then exit 1; fi
  exit 0
fi

fail=0
ok() { printf 'ok    %s\n' "$1"; }
bad() { printf 'FAIL  %s\n' "$1"; fail=1; }

if hits=$(scan "$here"); then
  bad "this repository is free of $word"
  printf '%s\n' "$hits"
else
  ok "this repository is free of $word"
fi

# A scanner that finds nothing anywhere cannot be told apart from one that
# looks at nothing: plant the call in each shape it must catch.
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
mkdir -p "$work/act" "$work/hack/restricted-sim"
printf 'runs:\n  steps:\n    - shell: bash\n      run: %s ln -sf /bin/bash /bin/sh\n' "$word" >"$work/act/action.yaml"
printf '#!/usr/bin/env bash\n# %s apt-get update\n' "$word" >"$work/act/x.sh"
printf 'recipe:\n    %s true\n' "$word" >"$work/Justfile"
printf 'FROM x\nRUN %s true\n' "$word" >"$work/hack/restricted-sim/Dockerfile"
printf 'we removed %s\n' "$word" >"$work/CHANGELOG.md"

for f in act/action.yaml act/x.sh Justfile; do
  if scan "$work" | grep -q "$f"; then ok "a $word call in $f is found"; else bad "a $word call in $f is found"; fi
done
if scan "$work" | grep -q 'restricted-sim\|CHANGELOG'; then
  bad "prose and the simulation's control image are exempt"
else
  ok "prose and the simulation's control image are exempt"
fi
printf 'pseudo-%sx and pseudocode\n' "$word" >"$work/act/ok.sh"
if scan "$work" | grep -q 'act/ok.sh'; then bad "only the whole word matches"; else ok "only the whole word matches"; fi

echo
if [ "$fail" = 0 ]; then echo "all cases pass"; else echo "::error::the no-privilege contract is broken: no $word in action files or scripts"; fi
exit "$fail"
