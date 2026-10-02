#!/usr/bin/env bash
# setup-devbox's privilege-free devbox install, one branch at a time, against
# a local directory standing in for the release page (no network, no root).
#
# What must hold: the binary the release published is installed into
# $RUNNER_TEMP/bin and put on PATH; an archive that does not match the
# release's checksums is NEVER installed; and nothing here pipes a download
# into a shell or reaches for privilege.
set -uo pipefail

here="$(cd "$(dirname "$0")/.." && pwd)"
script="$here/setup-devbox/install-devbox.sh"

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
fail=0
ok() { printf 'ok    %s\n' "$1"; }
bad() { printf 'FAIL  %s\n' "$1"; fail=1; }
has() { case "$1" in *"$2"*) return 0 ;; *) return 1 ;; esac; }

case "$(uname -s)" in Linux) os=linux ;; Darwin) os=darwin ;; *) echo "skip: unsupported OS"; exit 0 ;; esac
case "$(uname -m)" in x86_64 | amd64) arch=amd64 ;; aarch64 | arm64) arch=arm64 ;; *) echo "skip: unsupported arch"; exit 0 ;; esac

# A release: devbox_<v>_<os>_<arch>.tar.gz holding one `devbox`, and a
# checksums.txt in the release's own format.
make_release() { # $1 version
  local v=$1 d="$work/rel/download/$1" name="devbox_$1_${os}_${arch}.tar.gz"
  mkdir -p "$d" "$work/pack"
  printf '#!/bin/sh\necho "devbox %s (stand-in)"\n' "$v" >"$work/pack/devbox"
  chmod +x "$work/pack/devbox"
  tar -czf "$d/$name" -C "$work/pack" devbox
  (cd "$d" && sha256sum "$name" >checksums.txt && echo "$(printf 'f%.0s' {1..64})  devbox_$1_linux_riscv64.tar.gz" >>checksums.txt)
}
make_release 1.2.3

run() { # extra env... ; fresh RUNNER_TEMP each time
  rm -rf "$work/temp" "$work/gh_path"
  mkdir -p "$work/temp"
  : >"$work/gh_path"
  env -i PATH="$PATH" HOME="$work" RUNNER_TEMP="$work/temp" GITHUB_PATH="$work/gh_path" \
    DEVBOX_RELEASE_BASE="file://$work/rel" "$@" bash "$script" 2>&1
}

log=$(run DEVBOX_VERSION=1.2.3)
rc=$?
if [ $rc = 0 ] && [ -x "$work/temp/bin/devbox" ] && [ "$("$work/temp/bin/devbox")" = "devbox 1.2.3 (stand-in)" ]; then
  ok "the published binary is installed into RUNNER_TEMP/bin, executable"
else
  bad "the published binary is installed into RUNNER_TEMP/bin (rc=$rc): $log"
fi
[ "$(cat "$work/gh_path")" = "$work/temp/bin" ] && ok "its directory is put on PATH for the next steps" || bad "its directory is put on PATH: $(cat "$work/gh_path")"
has "$log" "devbox 1.2.3 installed to $work/temp/bin/devbox (sha256 " && has "$log" "verified, no privilege)" \
  && ok "it says what it installed, where, and that the checksum was verified" || bad "it says what it installed: $log"
[ -z "$(ls -A "$work/temp" | grep -v '^bin$')" ] && ok "its download directory is cleaned up" || bad "its download directory is cleaned up: $(ls -A "$work/temp")"

# A corrupted archive: same name, different bytes. NEVER installed.
cp -r "$work/rel" "$work/rel-good"
printf 'tampered' >>"$work/rel/download/1.2.3/devbox_1.2.3_${os}_${arch}.tar.gz"
log=$(run DEVBOX_VERSION=1.2.3)
rc=$?
if [ $rc != 0 ] && [ ! -e "$work/temp/bin/devbox" ] && [ "$(grep -c '^::error::' <<<"$log")" = 1 ] \
  && has "$log" "::error::checksum mismatch for devbox_1.2.3_${os}_${arch}.tar.gz; refusing to install it"; then
  ok "an archive that does not match checksums.txt is refused, and nothing is installed"
else
  bad "an archive that does not match checksums.txt is refused (rc=$rc): $log"
fi
rm -rf "$work/rel" && mv "$work/rel-good" "$work/rel"

# checksums.txt that does not list the asset at all.
grep -v "devbox_1.2.3_${os}_${arch}" "$work/rel/download/1.2.3/checksums.txt" >"$work/cs" && mv "$work/cs" "$work/rel/download/1.2.3/checksums.txt"
log=$(run DEVBOX_VERSION=1.2.3)
rc=$?
[ $rc != 0 ] && has "$log" "checksums.txt lists no devbox_1.2.3_${os}_${arch}.tar.gz; refusing to install it" && [ ! -e "$work/temp/bin/devbox" ] \
  && ok "an asset the checksums do not list is refused" || bad "an asset the checksums do not list is refused (rc=$rc): $log"
make_release 1.2.3

# A version that does not exist.
log=$(run DEVBOX_VERSION=9.9.9)
rc=$?
[ $rc != 0 ] && has "$log" "::error::could not download file://$work/rel/download/9.9.9/devbox_9.9.9_${os}_${arch}.tar.gz" \
  && ok "a release that does not exist fails with one ::error::" || bad "a release that does not exist fails (rc=$rc): $log"

# An architecture with no release binary.
log=$(run DEVBOX_VERSION=1.2.3 DEVBOX_UNAME_M=riscv64)
rc=$?
[ $rc != 0 ] && has "$log" "::error::cannot install devbox for" && ok "an unsupported architecture is named" || bad "an unsupported architecture is named (rc=$rc): $log"

# `latest` is resolved by the releases page's redirect: the final URL names the tag.
mkdir -p "$work/stub"
cat >"$work/stub/curl" <<'STUB'
#!/usr/bin/env bash
# the one request `latest` makes: the releases page redirecting to a tag
case "$*" in
  *"-w %{url_effective}"*) printf '%s' "${STUB_FINAL:-https://github.com/jetify-com/devbox/releases/tag/1.2.3}"; exit 0 ;;
esac
exec "$REAL_CURL" "$@"
STUB
chmod +x "$work/stub/curl"
real_curl=$(command -v curl)
log=$(run PATH="$work/stub:$PATH" REAL_CURL="$real_curl")
rc=$?
[ $rc = 0 ] && has "$log" "devbox 1.2.3 installed" && ok "latest is resolved to the tag the releases page redirects to" || bad "latest is resolved (rc=$rc): $log"
log=$(run PATH="$work/stub:$PATH" REAL_CURL="$real_curl" STUB_FINAL=https://github.com/jetify-com/devbox/releases/latest)
rc=$?
[ $rc != 0 ] && has "$log" "::error::the latest devbox release" && ok "a redirect that names no tag is an error" || bad "a redirect that names no tag is an error (rc=$rc): $log"

# Nothing here pipes a download into a shell or reaches for privilege.
if grep -nE 'get\.jetify\.com|\|[[:space:]]*(ba)?sh\b' "$here/setup-devbox/action.yaml" "$here/setup-devbox/install-devbox.sh" | grep -v '^[^:]*:[0-9]*:[[:space:]]*#'; then
  bad "setup-devbox pipes no download into a shell"
else
  ok "setup-devbox pipes no download into a shell"
fi

echo
if [ "$fail" = 0 ]; then echo "all cases pass"; else echo "::error::install-devbox cases failed"; fi
exit "$fail"
