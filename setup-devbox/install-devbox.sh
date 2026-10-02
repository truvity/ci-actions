#!/usr/bin/env bash
# Install the devbox release binary into $RUNNER_TEMP/bin, with no privilege.
#
# Replaces `curl https://get.jetify.com/devbox | bash`, which is a script
# somebody else can change between two runs, run unverified as the job, and
# which installs into /usr/local/bin through root. This fetches ONE named
# release asset, checks it against the release's own checksums.txt, unpacks
# the single `devbox` binary it holds into a directory the job owns, and puts
# that directory on PATH for the steps that follow. It runs only when devbox
# is not already on the runner (a runner image that bakes it skips this).
#
#   DEVBOX_VERSION        a release tag such as 0.18.4, or `latest` (the default),
#                         which the releases page redirects to the newest tag
#   RUNNER_TEMP           where the binary lands, under bin/ (required)
#   GITHUB_PATH           the runner's PATH file, appended to when set
#
# The release location can be redirected for a test:
#   DEVBOX_RELEASE_BASE   default https://github.com/jetify-com/devbox/releases
#   DEVBOX_UNAME_M        the machine name to pick the asset for, default `uname -m`
set -euo pipefail

version="${DEVBOX_VERSION:-latest}"
base="${DEVBOX_RELEASE_BASE:-https://github.com/jetify-com/devbox/releases}"
tmp="${RUNNER_TEMP:?RUNNER_TEMP is not set}"
dest="$tmp/bin"

case "$(uname -s)" in
  Linux) os=linux ;;
  Darwin) os=darwin ;;
  *) echo "::error::cannot install devbox for $(uname -s): no release binary for it"; exit 1 ;;
esac
case "${DEVBOX_UNAME_M:-$(uname -m)}" in
  x86_64 | amd64) arch=amd64 ;;
  aarch64 | arm64) arch=arm64 ;;
  *) echo "::error::cannot install devbox for $(uname -m): no release binary for it"; exit 1 ;;
esac

if [ "$version" = latest ]; then
  # The releases page redirects `latest` to the newest tag; the final URL
  # names it. No API call, so no token and no rate limit.
  final=$(curl -fsSL --retry 2 --max-time 60 -o /dev/null -w '%{url_effective}' "$base/latest") \
    || { echo "::error::could not look up the latest devbox release at $base/latest"; exit 1; }
  version=${final##*/}
  [ -n "$version" ] && [ "$version" != latest ] \
    || { echo "::error::the latest devbox release at $base/latest named no tag ($final)"; exit 1; }
fi

name="devbox_${version}_${os}_${arch}.tar.gz"
url="$base/download/$version"

work=$(mktemp -d "$tmp/devbox-install.XXXXXX")
trap 'rm -rf "$work"' EXIT

curl -fsSL --retry 2 --max-time 300 -o "$work/$name" "$url/$name" \
  || { echo "::error::could not download $url/$name"; exit 1; }
curl -fsSL --retry 2 --max-time 60 -o "$work/checksums.txt" "$url/checksums.txt" \
  || { echo "::error::could not download $url/checksums.txt"; exit 1; }

# The checksum comes from the same release page as the archive, so it proves
# the download is the file the release published (no truncated or swapped
# transfer), not that the release itself is trustworthy: pin DEVBOX_VERSION,
# as the setup-devbox input does, to make the choice of release a reviewed one.
expected=$(awk -v n="$name" '$2 == n || $2 == "*" n { print $1; exit }' "$work/checksums.txt")
[ -n "$expected" ] || { echo "::error::checksums.txt lists no $name; refusing to install it"; exit 1; }
actual=$(sha256sum "$work/$name" | awk '{ print $1 }')
if [ "$expected" != "$actual" ]; then
  echo "::error::checksum mismatch for $name; refusing to install it"
  exit 1
fi

mkdir -p "$dest"
tar -xzf "$work/$name" -C "$work" devbox
install -m 0755 "$work/devbox" "$dest/devbox"

if [ -n "${GITHUB_PATH:-}" ]; then
  echo "$dest" >>"$GITHUB_PATH"
fi
echo "devbox $version installed to $dest/devbox (sha256 $actual verified, no privilege)"
