#!/usr/bin/env bash
# TEMPORARY, removed before merge: the shell caller-parity against the Go
# port, driven by the shell version's own cases (its stub API and fixtures:
# the identical, comment-only, cron, pin, dropped-trigger, added-input,
# added-block, absent, 403, gone, kit-off, and the whole depguard-block set).
#
# The old hack/caller-parity-cases.sh is run from a scratch tree whose
# caller-parity.sh is a shim: every time the cases invoke the action, the
# shim runs the OLD script and the NEW binary on the same environment and
# compares stdout+stderr, the step summary, the outputs file and the exit
# status, then hands the old results back to the cases script, so its own
# assertions still run too.
set -uo pipefail
here="$(cd "$(dirname "$0")/.." && pwd)"
base="${PARITY_BASE:-origin/master}"
T=$(mktemp -d)
trap 'rm -rf "$T"' EXIT
mkdir -p "$T/hack" "$T/caller-parity"
git -C "$here" show "$base:hack/caller-parity-cases.sh" >"$T/hack/caller-parity-cases.sh" || { echo "::error::cannot read the old cases at $base"; exit 2; }
git -C "$here" show "$base:caller-parity/caller-parity.sh" >"$T/caller-parity/old.sh" || { echo "::error::cannot read the old script at $base"; exit 2; }
cp -r "$here/caller-parity/kits" "$T/caller-parity/kits"
chmod +x "$T/hack/caller-parity-cases.sh"
(cd "$here" && CGO_ENABLED=0 go build -o "$T/ci-actions" ./cmd/ci-actions) || exit 2

cat >"$T/caller-parity/caller-parity.sh" <<'SHIM'
#!/usr/bin/env bash
here="$(cd "$(dirname "$0")" && pwd)"
tmp=$(mktemp -d)
export LC_ALL=C
: >"$tmp/o1"; : >"$tmp/s1"; : >"$tmp/o2"; : >"$tmp/s2"
GITHUB_OUTPUT="$tmp/o1" GITHUB_STEP_SUMMARY="$tmp/s1" bash "$here/old.sh" >"$tmp/l1" 2>&1
rc1=$?
GITHUB_OUTPUT="$tmp/o2" GITHUB_STEP_SUMMARY="$tmp/s2" "$PARITY_BIN" caller-parity >"$tmp/l2" 2>&1
rc2=$?
n=$(($(cat "$PARITY_COUNT" 2>/dev/null || echo 0) + 1)); echo "$n" >"$PARITY_COUNT"
same=1
[ "$rc1" = "$rc2" ] || same=0
for p in l s o; do cmp -s "$tmp/${p}1" "$tmp/${p}2" || same=0; done
if [ $same = 1 ]; then
  echo "invocation $n same (exit $rc1)" >>"$PARITY_LOG"
else
  {
    echo "invocation $n DIFFERS (old exit $rc1, new exit $rc2)"
    for p in l s o; do diff "$tmp/${p}1" "$tmp/${p}2" | head -30; done
  } >>"$PARITY_LOG"
  echo x >>"$PARITY_FAILED"
fi
# hand the old results back, so the cases' own assertions run on them
cat "$tmp/o1" >>"$GITHUB_OUTPUT"
cat "$tmp/s1" >>"$GITHUB_STEP_SUMMARY"
cat "$tmp/l1"
rm -rf "$tmp"
exit "$rc1"
SHIM
chmod +x "$T/caller-parity/caller-parity.sh"
: >"$T/failed"
: >"$T/log"
export PARITY_BIN="$T/ci-actions" PARITY_COUNT="$T/count" PARITY_FAILED="$T/failed" PARITY_LOG="$T/log"
bash "$T/hack/caller-parity-cases.sh" 2>&1 | tail -3
rc=${PIPESTATUS[0]}
cat "$T/log"
echo "$(cat "$T/count" 2>/dev/null || echo 0) invocations compared, $(wc -l <"$T/failed") differed"
[ "$rc" = 0 ] && [ ! -s "$T/failed" ] && [ "$(cat "$T/count" 2>/dev/null || echo 0)" -ge 4 ]
