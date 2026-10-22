#!/usr/bin/env bash
# setup-cache/gocacheprog: the runner's own bucket is served by the runner's own
# identity. A stub plugin prints which AWS variables reach it.
set -uo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
tmp="$(mktemp -d)"; trap 'rm -rf "$tmp"' EXIT
cat > "$tmp/plugin" <<'PLUGIN'
#!/usr/bin/env bash
for v in AWS_ACCESS_KEY_ID AWS_SECRET_ACCESS_KEY AWS_SESSION_TOKEN AWS_PROFILE AWS_ROLE_ARN AWS_CONTAINER_CREDENTIALS_RELATIVE_URI AWS_CONFIG_FILE; do
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
exit $((fail > 0))
