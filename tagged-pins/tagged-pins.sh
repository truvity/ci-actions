#!/usr/bin/env bash
# Every `uses: <library>/...@<sha>` under .github must name the COMMIT OF A
# TAG, for each shared CI library named in $LIBRARIES.
#
# A pin to an untagged commit is a pin to whatever was on master that
# afternoon: it names no release, so it cannot be diffed against one, the
# `# v3.1.0` beside it is a wish rather than a fact, and renovate has
# nothing to bump it from. The failure is quiet -- such a pin works
# perfectly until somebody asks what version a repository is on.
#
# Runs from the root of the repository being judged: the CALLER's checkout
# in the shared `check`, this library's own in self-check.
set -euo pipefail

# One library or several. The split of the actions out of ci-workflows
# (INF-971) made this a list rather than a constant: a repository can now
# pin the workflows, the actions, or both, and a guard that knows only one
# of the two names is a guard that passes silently over the other half.
#
# Commas as well as whitespace, because an action input is a string and
# `a,b` is the shape somebody will write.
# Defaulted here as well as in action.yaml so the script is runnable on
# its own -- by the case harness, and by anyone debugging a red gate in a
# checkout rather than in a job.
LIBRARIES="${LIBRARIES:-truvity/ci-workflows truvity/ci-actions}"

read -r -a libraries <<<"${LIBRARIES//,/ }"

fail=0
seen=0

for library in "${libraries[@]}"; do
  mapfile -t refs < <(grep -rhoE "${library}/[^@[:space:]\"']+@[0-9a-f]{40}" .github 2>/dev/null | sort -u)

  if [ ${#refs[@]} -eq 0 ]; then
    echo "no ${library} pins under .github — nothing to check"
    continue
  fi

  seen=$((seen + 1))

  # Every tag's COMMIT, straight from the remote: no API, no token, no jq.
  # `^{}` is the dereferenced commit of an annotated tag, which is what a
  # `uses:` pin must name — the tag OBJECT sha looks just as plausible and
  # resolves to nothing.
  tags=$(git ls-remote --tags "https://github.com/${library}" \
         | sed -n 's|^\([0-9a-f]\{40\}\)[[:space:]]*refs/tags/\(.*\)^{}$|\1 \2|p')
  newest=$(cut -d' ' -f2 <<<"$tags" | sort -V | tail -1)

  for ref in "${refs[@]}"; do
    sha=${ref##*@}
    what=${ref%@*}
    what=${what#"${library}/"}

    tag=$(awk -v s="$sha" '$1 == s { print $2; exit }' <<<"$tags")

    if [ -n "$tag" ]; then
      printf 'ok        %-46s %.12s  %s\n' "${library}/${what}" "$sha" "$tag"
      continue
    fi

    fail=1
    printf 'UNTAGGED  %-46s %.12s  names no release; newest is %s\n' "${library}/${what}" "$sha" "$newest"
  done
done

if [ "$fail" != 0 ]; then
  echo "::error::A pinned commit is not a release. Pin the commit of a tag (git rev-parse <tag>^{}), with the version in a trailing comment."
  exit 1
fi

# Said out loud, because "nothing to check" printed once per library and
# then silence is indistinguishable from a guard that matched nothing
# because the name it was given is wrong.
echo "tagged-pins: ${seen} of ${#libraries[@]} libraries had pins under .github, all naming releases"
