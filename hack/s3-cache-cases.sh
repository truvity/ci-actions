#!/usr/bin/env bash
# The s3-cache wrapper: which step it hands the binary, that a missing binary
# is a hand-off to GitHub's cache and never a failed job, and that the actions
# declare the fallback. The cache logic itself is tested in Go
# (internal/s3cache) against an in-memory bucket.
set -uo pipefail

here="$(cd "$(dirname "$0")/.." && pwd)"
wrapper="$here/s3-cache/run.sh"

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
fail=0
ok() { printf 'ok    %s\n' "$1"; }
bad() { printf 'FAIL  %s\n' "$1"; fail=1; }
has() { case "$1" in *"$2"*) return 0 ;; *) return 1 ;; esac; }

mkdir -p "$work/stubdir" "$work/tmp" "$work/min"
cat >"$work/stubdir/ci-actions" <<'STUB'
#!/usr/bin/env bash
echo "stub ran: $*"
exit "${STUB_EXIT:-0}"
STUB
chmod +x "$work/stubdir/ci-actions"
for t in bash mkdir dirname; do ln -s "$(command -v "$t")" "$work/min/$t"; done

run() { env -i HOME="$work" RUNNER_TEMP="$work/tmp" GITHUB_OUTPUT="$work/out" ACTION_PATH="$here/s3-cache/$1" "${@:2}" bash "$wrapper" "$1" 2>&1; }

: >"$work/out"
log=$(run restore PATH="$work/min" CI_ACTIONS_BIN="$work/stubdir/ci-actions")
rc=$?
[ $rc = 0 ] && has "$log" "stub ran: s3-cache restore" \
  && ok "restore runs the binary as 's3-cache restore'" \
  || bad "restore runs the binary as 's3-cache restore' (rc=$rc): $log"

log=$(run save PATH="$work/min" CI_ACTIONS_BIN="$work/stubdir/ci-actions")
has "$log" "stub ran: s3-cache save" && ok "save runs the binary as 's3-cache save'" \
  || bad "save runs the binary as 's3-cache save': $log"

: >"$work/out"
log=$(run restore PATH="$work/min")
rc=$?
if [ $rc = 0 ] && has "$log" "::warning::s3-cache: the ci-actions binary is unavailable" && grep -qx 'fallback=true' "$work/out"; then
  ok "with no binary the step still exits 0 and asks for GitHub's cache"
else
  bad "with no binary the step still exits 0 and asks for GitHub's cache (rc=$rc): $log / $(cat "$work/out")"
fi

for a in restore save; do
  f="$here/s3-cache/$a/action.yaml"
  if grep -q "steps.s3.outputs.fallback == 'true'" "$f" && ! grep -q "runner.environment" "$f" \
    && grep -q "uses: actions/cache/$a@[0-9a-f]\{40\}" "$f"; then
    ok "$a: falls back to a pinned actions/cache/$a without a bucket or without credentials, and never asks the runner type"
  else
    bad "$a: fallback to a pinned actions/cache/$a is not wired as expected (or the runner type is consulted)"
  fi
done

exit "$fail"
