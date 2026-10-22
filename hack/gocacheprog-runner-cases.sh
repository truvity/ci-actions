#!/usr/bin/env bash
# setup-cache/gocacheprog: the runner's own bucket is served by the runner's own
# identity, or, on a tier whose identity only reads (CI_RUNNER_CACHE_WRITES=release-credentials),
# by the job's own keys for writes. A stub plugin prints which AWS variables reach it.
set -uo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
tmp="$(mktemp -d)"; trap 'rm -rf "$tmp"' EXIT
cat > "$tmp/plugin" <<'PLUGIN'
#!/usr/bin/env bash
for v in AWS_ACCESS_KEY_ID AWS_SECRET_ACCESS_KEY AWS_SESSION_TOKEN AWS_PROFILE AWS_ROLE_ARN AWS_CONTAINER_CREDENTIALS_RELATIVE_URI AWS_CONFIG_FILE GOCACHE_MIN_SIZE; do
  printf '%s=%s\n' "$v" "${!v-<unset>}"
done
PLUGIN
chmod +x "$tmp/plugin"
fail=0
# case LABEL EXPECT_STRIPPED(yes|no) -- env...
case_() {
  local label="$1" want="$2"; shift 3
  local out
  out="$(env -i PATH="$PATH" HOME="$tmp" CI_CACHE_GO_PLUGIN="$tmp/plugin" AWS_ACCESS_KEY_ID=job-key AWS_SECRET_ACCESS_KEY=job-secret AWS_SESSION_TOKEN=job-token "$@" "$here/setup-cache/gocacheprog" 2>/dev/null)"
  local got=no
  grep -q '^AWS_ACCESS_KEY_ID=<unset>$' <<<"$out" && grep -q '^AWS_SESSION_TOKEN=<unset>$' <<<"$out" && got=yes
  if [ "$got" != "$want" ]; then echo "FAIL [$label]: job keys stripped=$got, want $want"; echo "$out"; fail=$((fail+1)); else echo "ok   [$label]"; fi
}
task=AWS_CONTAINER_CREDENTIALS_RELATIVE_URI=/v2/credentials/x
case_ "runner bucket + task identity: the plugin gets the task role"      yes -- CI_RUNNER_GOCACHE_S3_BUCKET=kernel GOCACHE_S3_BUCKET=kernel "$task"
case_ "runner bucket + pod identity (full URI)"                           yes -- CI_RUNNER_GOCACHE_S3_BUCKET=kernel GOCACHE_S3_BUCKET=kernel AWS_CONTAINER_CREDENTIALS_FULL_URI=http://169.254.170.23/v1/credentials
case_ "runner bucket but a job that names another bucket"                 no  -- CI_RUNNER_GOCACHE_S3_BUCKET=kernel GOCACHE_S3_BUCKET=other "$task"
case_ "runner bucket but no container identity"                           no  -- CI_RUNNER_GOCACHE_S3_BUCKET=kernel GOCACHE_S3_BUCKET=kernel
case_ "no runner bucket: the job's keys win as before"                    no  -- GOCACHE_S3_BUCKET=kernel "$task"

# detail LABEL KEYS(job|none) WANT_LINES... -- env...: the stub's output must contain every WANT line
# exactly, and stderr must mention "read-only" exactly when WANT_NOTE is yes.
detail() {
  local label="$1" keys="$2" note="$3"; shift 3
  local wants=() ; while [ "$1" != "--" ]; do wants+=("$1"); shift; done; shift
  local base=(PATH="$PATH" HOME="$tmp" CI_CACHE_GO_PLUGIN="$tmp/plugin")
  [ "$keys" = job ] && base+=(AWS_ACCESS_KEY_ID=job-key AWS_SECRET_ACCESS_KEY=job-secret AWS_SESSION_TOKEN=job-token)
  local out err
  out="$(env -i "${base[@]}" "$@" "$here/setup-cache/gocacheprog" 2>"$tmp/err")"; err="$(cat "$tmp/err")"
  local bad=0 w
  for w in "${wants[@]}"; do grep -qxF -- "$w" <<<"$out" || { echo "FAIL [$label]: missing line $w"; bad=1; }; done
  if [ "$note" = yes ] && ! grep -q 'read-only' <<<"$err"; then echo "FAIL [$label]: no read-only notice on stderr"; bad=1; fi
  if [ "$note" = yes ] && [ "$(grep -c 'read-only' <<<"$err")" != 1 ]; then echo "FAIL [$label]: notice not exactly once"; bad=1; fi
  if [ "$note" = no ] && grep -q 'read-only' <<<"$err"; then echo "FAIL [$label]: unexpected read-only notice"; bad=1; fi
  if [ "$bad" = 1 ]; then echo "$out"; fail=$((fail+1)); else echo "ok   [$label]"; fi
}
max=9223372036854775807
rel=CI_RUNNER_CACHE_WRITES=release-credentials
own=(CI_RUNNER_GOCACHE_S3_BUCKET=kernel GOCACHE_S3_BUCKET=kernel "$task")
detail "release-credentials runner + job keys: the plugin gets the job's keys, writes enabled" job no \
  AWS_ACCESS_KEY_ID=job-key AWS_SECRET_ACCESS_KEY=job-secret AWS_SESSION_TOKEN=job-token GOCACHE_MIN_SIZE='<unset>' -- "$rel" "${own[@]}"
detail "release-credentials runner + no job keys: task role, read only" none yes \
  AWS_ACCESS_KEY_ID='<unset>' AWS_SESSION_TOKEN='<unset>' AWS_CONTAINER_CREDENTIALS_RELATIVE_URI=/v2/credentials/x GOCACHE_MIN_SIZE="$max" -- "$rel" "${own[@]}"
detail "release-credentials runner + job profile only: task role, read only" none yes \
  AWS_PROFILE='<unset>' GOCACHE_MIN_SIZE="$max" -- "$rel" AWS_PROFILE=release "${own[@]}"
detail "release-credentials runner, other bucket: nothing changes" job no \
  AWS_ACCESS_KEY_ID=job-key GOCACHE_MIN_SIZE='<unset>' -- "$rel" CI_RUNNER_GOCACHE_S3_BUCKET=kernel GOCACHE_S3_BUCKET=other "$task"
detail "release-credentials runner, no container identity: nothing changes" job no \
  AWS_ACCESS_KEY_ID=job-key GOCACHE_MIN_SIZE='<unset>' -- "$rel" CI_RUNNER_GOCACHE_S3_BUCKET=kernel GOCACHE_S3_BUCKET=kernel
detail "unknown CI_RUNNER_CACHE_WRITES value: the preview behaviour (stripped, writes enabled)" job no \
  AWS_ACCESS_KEY_ID='<unset>' GOCACHE_MIN_SIZE='<unset>' -- CI_RUNNER_CACHE_WRITES=whatever "${own[@]}"
detail "no CI_RUNNER_CACHE_WRITES: the preview tier strips the job's keys, task role writes" job no \
  AWS_ACCESS_KEY_ID='<unset>' AWS_SESSION_TOKEN='<unset>' GOCACHE_MIN_SIZE='<unset>' -- "${own[@]}"
exit $((fail > 0))
