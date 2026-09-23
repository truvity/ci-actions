#!/usr/bin/env bash
# A PUBLIC repository's jobs run on GitHub-hosted runners. Always.
#
# GitHub's own guidance is blunt about why: "self-hosted runners should
# almost never be used for public repositories, because any user can open
# pull requests against the repository and compromise the environment". A
# fork's pull request receives no secrets and no id-token, so the usual
# reasoning stops there -- but the RUNNER is the exposure, not the token.
# On a self-hosted pool the job inherits whatever the runner's own identity
# can reach, and it writes the shared build caches that TRUSTED jobs read
# afterwards. A poisoned cache entry is a supply-chain compromise that no
# amount of ephemerality undoes, because the persistence is in the cache,
# not the machine.
#
# The runner group's "allow public repositories" setting is the hard stop
# and stays off. This is the second lock: a public repository that asks for
# a self-hosted tier -- by copying a private repository's caller, which is
# exactly how it would happen -- is told so in seconds, on a hosted runner,
# before anything of the estate's has run.
#
# What a public repository does instead, when it genuinely needs the
# estate's infrastructure: a PRIVATE repository checks it out at a pinned
# version and runs that suite itself. The result travels with the pin.
set -euo pipefail

visibility=${VISIBILITY:-}

# The event payload carries the visibility for every webhook this library
# is called from, but not for every event that exists. Asking the API
# costs one request and makes the guard independent of the payload shape;
# `contents: read`, which every caller has, is enough to read it.
if [ -z "$visibility" ]; then
  visibility=$(gh api "repos/${GITHUB_REPOSITORY}" --jq .visibility 2>/dev/null || true)
fi

if [ -z "$visibility" ]; then
  echo "::error::cannot tell whether ${GITHUB_REPOSITORY} is public; refusing to guess. Pass visibility: \${{ github.event.repository.visibility }}."
  exit 1
fi

if [ "$visibility" != "public" ]; then
  echo "${GITHUB_REPOSITORY} is ${visibility} — any runner is allowed"
  exit 0
fi

# Hosted labels are the three platforms GitHub runs. Everything else is
# somebody's own machine, including this estate's tiers and GitHub's own
# larger runners -- which are billed even for a public repository, so
# refusing them here is right too.
fail=0
for label in $RUNNERS; do
  [ -n "$label" ] || continue
  case "$label" in
    ubuntu-*|windows-*|macos-*)
      echo "ok        $label"
      ;;
    *)
      echo "SELF-HOSTED  $label"
      fail=1
      ;;
  esac
done

if [ "$fail" != 0 ]; then
  echo "::error::${GITHUB_REPOSITORY} is public, so its jobs run on GitHub-hosted runners only. A fork's code must never execute on the estate's own infrastructure. Drop the runner inputs and take the hosted default; if the work genuinely needs the estate, run it from a private repository that checks this one out at a pinned version."
  exit 1
fi

echo "public repository, hosted runners only — checked"
