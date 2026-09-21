#!/usr/bin/env bash
# The body of the caller-parity action.
#
# Two of the caller workflows every repository carries are the same file
# everywhere — `security.yaml` and `auto-release.yaml` differ only in
# their prose and in one staggered `cron:` minute. Nothing asserted it,
# so one repository lost its `push:` trigger and nobody noticed until it
# was read by eye. This compares each enrolled repository against the
# canonical copies in `kits/`, and REPORTS: no branch, no pull request,
# no rewrite. What a repository carries is still that repository's to
# change; what this does is make the change visible.
#
# A file rather than an inline `run:` block, for the same reason as
# fleet-discover's discover.sh: the normalisation below decides whether
# a difference is real, and a wrong answer is quiet either way — a
# comparison that is too strict is noise everyone learns to skip, one
# that is too loose reports parity that is not there. The shapes it has
# to survive are pinned by hack/caller-parity-cases.sh, which this
# repository's own CI runs against a stub API.
#
# Everything arrives through the environment, never spliced into script
# text: TOKEN, API, REPOSITORIES, KITS, FAIL_ON_DIFF from the action's
# inputs, GITHUB_OUTPUT and GITHUB_STEP_SUMMARY from the runner.
# curl + jq only: the self-hosted image ships no gh.
set -euo pipefail

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# WHAT IS COMPARED: SUBSTANCE, NOT BYTES.
#
# Each rule here is an exemption, and each one is deliberate:
#
#   comments      a repository explains itself in its own words. Four
#                 prose variants of security.yaml across eleven
#                 repositories were all the same workflow.
#   blank lines   follow the comments they separated.
#   cron          THE SCHEDULE IS STAGGERED PER REPOSITORY ON PURPOSE:
#                 repositories that tag in the same minute produce
#                 downstream pin pull requests that race each other's
#                 rebases. Comparing it would report every repository as
#                 differing and teach everyone to ignore the check.
#   this library's pinned ref
#                 renovate moves `uses: <owner>/<repo>/.github/workflows/
#                 <file>@<sha>` in each repository on its own schedule,
#                 so between a release here and renovate's sweep there
#                 the estate is legitimately spread across two pins. The
#                 pin has its own keeper; this check is about shape.
#                 Only reusable-workflow refs are exempt — a third-party
#                 action pin inside a caller IS compared.
#
# Comments are stripped textually, so a `#` inside a quoted scalar would
# be cut with them. No kit carries one; do not add one.
substance() {
  sed -e 's/[[:space:]]*#.*$//' \
    -e '/^[[:space:]]*$/d' \
    -e '/^[[:space:]]*-[[:space:]]*cron:/d' \
    -e 's#\(uses:[[:space:]]*[^@[:space:]]*/\.github/workflows/[^@[:space:]]*\)@[0-9a-f]\{40\}#\1@<pinned>#'
}

# One request, kept whole: the body in $HTTP_BODY, the status in
# $HTTP_STATUS. `curl -f` throws the body away and collapses every 4xx
# into one exit code, and "this repository does not carry the file" and
# "this token may not look" are the two answers that must not be
# confused.
http() {
  local raw
  HTTP_BODY=''
  HTTP_STATUS=000
  raw=$(curl -sS --max-time 60 -w $'\n%{http_code}' \
    -H "Authorization: Bearer $TOKEN" \
    -H "Accept: application/vnd.github+json" \
    -H "X-GitHub-Api-Version: 2022-11-28" \
    "$@") || return 0 # a transport failure leaves 000, i.e. unreadable
  HTTP_STATUS=${raw##*$'\n'}
  HTTP_BODY=${raw%$'\n'*}
}

kits=()
while read -r kit; do kits+=("$kit"); done < <(find "$KITS" -maxdepth 1 -name '*.yaml' | sort)
if [ "${#kits[@]}" -eq 0 ]; then
  echo "::error::no kit files in $KITS"
  exit 1
fi

repositories=$(jq -c 'if type == "array" then . else [] end' <<<"${REPOSITORIES:-[]}" 2>/dev/null || echo '[]')
if [ "$(jq 'length' <<<"$repositories")" -eq 0 ]; then
  echo "no repositories to check" | tee -a "$GITHUB_STEP_SUMMARY"
  {
    echo "differences=0"
    echo "absent=0"
  } >> "$GITHUB_OUTPUT"
  exit 0
fi

# Four states, and the difference between them is the point of the
# check: `same`, `differs`, `absent` (correct for a repository that
# releases nothing) and `unreadable` (a read that failed, which is never
# reported as either of the other three).
rows='[]'
differences=0
absent=0
unreadable=0

for repo in $(jq -r '.[]' <<<"$repositories"); do
  http "$API/repos/$repo"
  if [ "$HTTP_STATUS" != 200 ]; then
    echo "::warning::$repo: could not read the repository (HTTP $HTTP_STATUS) — not compared"
    for kit in "${kits[@]}"; do
      rows=$(jq -c --arg r "$repo" --arg f "$(basename "$kit")" \
        '. + [{repo: $r, file: $f, state: "unreadable"}]' <<<"$rows")
      unreadable=$((unreadable + 1))
    done
    continue
  fi
  branch=$(jq -r '.default_branch' <<<"$HTTP_BODY")

  for kit in "${kits[@]}"; do
    name=$(basename "$kit")
    state=unreadable
    # Kit file names are plain (`[a-z-]+.yaml`), so the path needs no
    # escaping; the ref is the default branch this repository reported.
    http "$API/repos/$repo/contents/.github/workflows/$name?ref=$branch"
    case "$HTTP_STATUS" in
      200)
        # The repository is in the installation and the token carries
        # contents: read, so a 404 here means the file is not there.
        jq -r '.content // empty' <<<"$HTTP_BODY" | tr -d '\n' | base64 -d >"$work/theirs" 2>/dev/null || : >"$work/theirs"
        substance <"$work/theirs" >"$work/theirs.substance"
        substance <"$kit" >"$work/ours.substance"
        if cmp -s "$work/ours.substance" "$work/theirs.substance"; then
          state=same
        else
          state=differs
          differences=$((differences + 1))
          diff -u --label "kits/$name (canonical)" --label "$repo .github/workflows/$name" \
            "$work/ours.substance" "$work/theirs.substance" >"$work/${repo//\//__}.$name.diff" || true
          echo "::warning::$repo: .github/workflows/$name differs from the canonical kit"
        fi
        ;;
      404)
        state=absent
        absent=$((absent + 1))
        ;;
      *)
        echo "::warning::$repo: could not read .github/workflows/$name (HTTP $HTTP_STATUS) — not compared"
        unreadable=$((unreadable + 1))
        ;;
    esac
    rows=$(jq -c --arg r "$repo" --arg f "$name" --arg s "$state" \
      '. + [{repo: $r, file: $f, state: $s}]' <<<"$rows")
  done
done

# Everything absent everywhere is not an estate that carries no callers;
# it is a token that cannot read file contents. Say so rather than
# reporting a clean-looking sweep of nothing.
if [ "$absent" = "$(jq 'length' <<<"$rows")" ]; then
  echo "::warning::every file is reported absent — check that the token carries contents: read"
fi

{
  echo "## caller-parity — the shared caller files"
  echo
  echo "Compared on SUBSTANCE: comment lines, blank lines, the \`cron:\` line and"
  echo "this library's pinned ref are not compared. The \`cron:\` minute is"
  echo "staggered per repository on purpose."
  echo
  echo "**$differences** differ, **$absent** absent, **$unreadable** could not be read,"
  echo "across $(jq 'length' <<<"$repositories") repositories and ${#kits[@]} files."
  echo
  printf '| repository |'
  for kit in "${kits[@]}"; do printf ' %s |' "$(basename "$kit")"; done
  printf '\n|---|'
  # `%s`, not the literal: bash's printf would read `---|` as options.
  for _ in "${kits[@]}"; do printf '%s' '---|'; done
  printf '\n'
  for repo in $(jq -r '.[]' <<<"$repositories"); do
    printf '| %s |' "$repo"
    for kit in "${kits[@]}"; do
      printf ' %s |' "$(jq -r --arg r "$repo" --arg f "$(basename "$kit")" \
        '(.[] | select(.repo == $r and .file == $f) | .state) // "-"' <<<"$rows")"
    done
    printf '\n'
  done
  echo
  if [ "$differences" -gt 0 ]; then
    echo "### What differs"
    echo
    echo "Normalised diffs. NOTHING IS REWRITTEN: bring the repository to the"
    echo "canonical copy by hand, or change the canonical copy for the estate."
    echo
    for f in "$work"/*.diff; do
      [ -e "$f" ] || break
      echo '```diff'
      cat "$f"
      echo '```'
      echo
    done
  fi
} | tee -a "$GITHUB_STEP_SUMMARY"

{
  echo "differences=$differences"
  echo "absent=$absent"
} >> "$GITHUB_OUTPUT"

if [ "$FAIL_ON_DIFF" = "true" ] && [ "$differences" -gt 0 ]; then
  echo "::error::$differences caller file(s) differ from the canonical kit"
  exit 1
fi
exit 0
