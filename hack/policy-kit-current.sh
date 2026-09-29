#!/usr/bin/env bash
# Is caller-parity/kits/golangci-depguard.yaml still the file it claims to
# be a copy of?
#
# golangci-lint v2 cannot extend a configuration from a URL, so the import
# bans are a hand copy — in every consuming repository, and in this
# action's own canonical copy for caller-parity, one level up. A hand copy
# with nothing keeping it in step has the half-life the parity check
# exists to close everywhere else; this closes it here, against the
# source, rather than leaving "does this still match policy" to whoever
# next compares the two by eye.
#
# The pin lives beside the kit, in kits.yaml's `source:` field
# (`owner/repo@tag`) — not a constant in this script, so bumping the pin
# is the one-line edit `hack/caller-parity-cases.sh` already expects kit
# changes to be. No token: truvity/policy is public, and this reads the
# tagged blob over HTTPS the same way tagged-pins reads tags over git —
# no API, no auth, nothing to exchange.
#
#   hack/policy-kit-current.sh
set -euo pipefail

root="$(cd "$(dirname "$0")/.." && pwd)"
kit="$root/caller-parity/kits/golangci-depguard.yaml"
manifest="$root/caller-parity/kits/kits.yaml"

source=$(yq -r '.["golangci-depguard.yaml"].source // ""' "$manifest")
[ -n "$source" ] || { echo "::error::kits.yaml has no source: for golangci-depguard.yaml"; exit 1; }

repo=${source%@*}
tag=${source##*@}
[ -n "$repo" ] && [ -n "$tag" ] && [ "$repo" != "$tag" ] || {
  echo "::error::kits.yaml's source ($source) is not owner/repo@tag"
  exit 1
}

upstream=$(mktemp)
trap 'rm -f "$upstream"' EXIT

url="https://raw.githubusercontent.com/${repo}/${tag}/lint/golangci-depguard.yaml"
if ! curl -fsSL --max-time 30 "$url" -o "$upstream"; then
  echo "::error::could not read $url — is the tag right, and is lint/golangci-depguard.yaml still there?"
  exit 1
fi

if cmp -s "$kit" "$upstream"; then
  echo "policy-kit-current: caller-parity/kits/golangci-depguard.yaml matches ${source} verbatim"
  exit 0
fi

echo "::error::caller-parity/kits/golangci-depguard.yaml has drifted from ${source} — update the kit, or bump the pin if ${repo} moved on purpose"
diff -u --label "${source}:lint/golangci-depguard.yaml" --label "caller-parity/kits/golangci-depguard.yaml" \
  "$upstream" "$kit" || true
exit 1
