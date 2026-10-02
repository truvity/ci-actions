#!/usr/bin/env bash
# TEMPORARY, removed before merge: the shell policy-conformance against the Go
# port. The shell version's own cases script (every rule broken one at a time,
# the exemptions, the skip and strict inputs, the empty repository) is run from
# a scratch tree in which policy-conformance.sh is a shim: each time the cases
# call the action, the shim runs the OLD script and the NEW binary in the same
# directory with the same environment and compares stdout+stderr, the exit
# status and the step summary byte for byte, then hands the old results back so
# the cases' own assertions run too. After that, three real mirror charts
# (CRD charts republished from upstream) are judged by both, intact and with
# each way C1 can fail them.
set -uo pipefail
here="$(cd "$(dirname "$0")/.." && pwd)"
base="${PARITY_BASE:-origin/master}"
T=$(mktemp -d)
trap '[ -n "${KEEP:-}" ] || rm -rf "$T"' EXIT
mkdir -p "$T/hack" "$T/policy-conformance"
git -C "$here" show "$base:hack/policy-conformance-cases.sh" >"$T/hack/policy-conformance-cases.sh" || exit 2
git -C "$here" show "$base:policy-conformance/policy-conformance.sh" >"$T/policy-conformance/old.sh" || exit 2
(cd "$here" && CGO_ENABLED=0 go build -o "$T/ci-actions" ./cmd/ci-actions) || exit 2
chmod +x "$T/hack/policy-conformance-cases.sh"

cat >"$T/policy-conformance/policy-conformance.sh" <<'SHIM'
#!/usr/bin/env bash
here="$(cd "$(dirname "$0")" && pwd)"
tmp=$(mktemp -d)
export LC_ALL=C
real_summary=${GITHUB_STEP_SUMMARY:-}
: >"$tmp/s1"; : >"$tmp/s2"
if [ -n "$real_summary" ]; then S1="$tmp/s1"; S2="$tmp/s2"; else S1=""; S2=""; fi
GITHUB_STEP_SUMMARY=$S1 bash "$here/old.sh" 2>&1 | grep -v '^awk: .*warning:' >"$tmp/l1"; rc1=${PIPESTATUS[0]}
GITHUB_STEP_SUMMARY=$S2 "$PARITY_BIN" policy-conformance >"$tmp/l2" 2>&1; rc2=$?
n=$(($(cat "$PARITY_COUNT" 2>/dev/null || echo 0) + 1)); echo "$n" >"$PARITY_COUNT"
same=1
[ "$rc1" = "$rc2" ] || same=0
cmp -s "$tmp/l1" "$tmp/l2" || same=0
cmp -s "$tmp/s1" "$tmp/s2" || same=0
if [ $same = 0 ]; then
  { echo "invocation $n DIFFERS in $(pwd) (old exit $rc1, new exit $rc2)"; diff "$tmp/l1" "$tmp/l2" | head -20; diff "$tmp/s1" "$tmp/s2" | head -10; } >>"$PARITY_LOG"
  echo x >>"$PARITY_FAILED"
fi
[ -z "$real_summary" ] || cat "$tmp/s1" >>"$real_summary"
cat "$tmp/l1"
rm -rf "$tmp"
exit "$rc1"
SHIM
chmod +x "$T/policy-conformance/policy-conformance.sh"
: >"$T/failed"; : >"$T/log"
export PARITY_BIN="$T/ci-actions" PARITY_COUNT="$T/count" PARITY_FAILED="$T/failed" PARITY_LOG="$T/log"
bash "$T/hack/policy-conformance-cases.sh" 2>&1 | tail -2
cases_rc=${PIPESTATUS[0]}

# ---- real mirror charts ----------------------------------------------------
export GIT_AUTHOR_NAME=p GIT_AUTHOR_EMAIL=p@example.invalid GIT_COMMITTER_NAME=p GIT_COMMITTER_EMAIL=p@example.invalid
export GITHUB_REPOSITORY=example/charts-repo
mirror_chart() { # name upstream version
  cat <<EOC
apiVersion: v2
name: $1
description: >-
  The upstream CustomResourceDefinitions of $2 at the version pinned in
  crdctl.yaml, mirrored verbatim. CRDs only; takes no values.
type: application
# A MIRROR chart (component contract C1): the version is the UPSTREAM
# version it mirrors, declared by the annotation below.
version: $3
appVersion: "$3"
annotations:
  truvity.io/mirror: "$2@$3"
home: https://example.invalid/$1
sources:
  - https://example.invalid/$1
EOC
}
mirror_repo() { # dir ; charts given as name:upstream:version ...
  local d=$1; shift
  rm -rf "$d"; mkdir -p "$d/hack" "$d/.github/workflows"
  local spec name up ver
  for spec in "$@"; do
    IFS=: read -r name up ver <<<"$spec"
    mkdir -p "$d/charts/$name"
    mirror_chart "$name" "$up" "$ver" >"$d/charts/$name/Chart.yaml"
    echo '{}' >"$d/charts/$name/values.schema.json"
    mkdir -p "$d/tests/golden/$name" "$d/tests/invalid/$name"
    echo a >"$d/tests/golden/$name/a.yaml"; echo b >"$d/tests/invalid/$name/b.yaml"
  done
  (cd "$d" && printf '#!/bin/sh\n' >hack/leak-canary.sh && printf 'check:\n    ./hack/leak-canary.sh\n' >Justfile \
    && printf '# Changelog\n\n## v0.1.0\n' >CHANGELOG.md && echo '{"packages":["just@1.40.0"]}' >devbox.json \
    && echo '{"$schema":"x","extends":["github>truvity/ci-workflows"]}' >renovate.json \
    && { for h in "Who it is for" "The model" "Install and a worked example" "Consumers" "Neighbours" "Documentation" "The rule that makes this repository public" "Status" "Development" "Releasing" "Licence"; do printf '\n## %s\n\ntext\n' "$h"; done; } >README.md \
    && printf 'MIT License\n' >LICENSE && git init -q -b master . && git add -A && git commit -q -m init && git tag -a v0.1.0 -m v0.1.0)
}
compare_dir() { # name dir
  local out1 out2 rc1 rc2
  out1=$(cd "$2" && git add -A && LC_ALL=C bash "$T/policy-conformance/old.sh" 2>&1 | grep -v '^awk: .*warning:'; exit "${PIPESTATUS[0]}"); rc1=$?
  out2=$(cd "$2" && LC_ALL=C "$T/ci-actions" policy-conformance 2>&1); rc2=$?
  n=$(($(cat "$PARITY_COUNT") + 1)); echo "$n" >"$PARITY_COUNT"
  if [ "$out1" = "$out2" ] && [ "$rc1" = "$rc2" ]; then echo "same  $1"; else
    echo "DIFF  $1"; diff <(echo "$out1") <(echo "$out2") | head -10; echo x >>"$PARITY_FAILED"; fi
}
cnpg="barman-cloud-crds:cloudnative-pg/plugin-barman-cloud:0.13.0"
k8s1="cilium-crds:cilium/cilium:1.20.1"
k8s2="volume-snapshot-crds:kubernetes-csi/external-snapshotter:8.6.0"
mirror_repo "$T/m-cnpg" "$cnpg";            compare_dir "mirror chart (cnpg's barman-cloud-crds)" "$T/m-cnpg"
mirror_repo "$T/m-k8s" "$k8s1" "$k8s2";     compare_dir "mirror charts (k8s's cilium-crds and volume-snapshot-crds)" "$T/m-k8s"
mirror_repo "$T/m-bump" "$k8s1";            sed -i 's/^version: 1.20.1/version: 1.20.2/' "$T/m-bump/charts/cilium-crds/Chart.yaml"; compare_dir "mirror chart, version drifted" "$T/m-bump"
mirror_repo "$T/m-app" "$k8s2";             sed -i 's/^appVersion: .*/appVersion: "8.5.0"/' "$T/m-app/charts/volume-snapshot-crds/Chart.yaml"; compare_dir "mirror chart, appVersion drifted" "$T/m-app"
mirror_repo "$T/m-bad" "$cnpg";             sed -i 's|barman-cloud@0.13.0|0.13.0|; s|cloudnative-pg/plugin-barman-cloud@0.13.0|0.13.0|' "$T/m-bad/charts/barman-cloud-crds/Chart.yaml"; compare_dir "mirror annotation malformed" "$T/m-bad"
mirror_repo "$T/m-mixed" "$cnpg" "$k8s1";   mkdir -p "$T/m-mixed/charts/plain/"; printf 'apiVersion: v2\nname: plain\nversion: 0.0.0\n' >"$T/m-mixed/charts/plain/Chart.yaml"; echo '{}' >"$T/m-mixed/charts/plain/values.schema.json"; compare_dir "mirror and plain charts side by side" "$T/m-mixed"

cat "$T/log"
count=$(cat "$PARITY_COUNT")
echo "$count invocations compared, $(wc -l <"$T/failed") differed"
[ "$cases_rc" = 0 ] && [ ! -s "$T/failed" ] && [ "$count" -ge 20 ]
