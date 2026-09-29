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
#
# curl, jq and yq; no gh, because the self-hosted image ships none. yq is
# needed only for the kit manifest and for kits compared as a subtree, and
# this job runs on a GitHub-hosted runner, where it is present — the same
# reason the enrolment step upstream already uses it.
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

# One subtree of a YAML file, as canonical JSON: keys sorted, ARRAYS
# sorted, comments gone, indentation irrelevant. What a copied BLOCK has
# to keep is its data — the depguard `deny:` list is a set of bans, not a
# sequence, and a repository that pasted it back with its entries in a
# different order still has the same bans. `jq -S` alone sorts only
# object keys; a list's own order survives it, so `walk` sorts every
# array too before the two sides are compared.
#
# A path that matches nothing prints `null`, which differs from any kit —
# a repository that dropped the block has fallen behind it.
subtree() { # $1 file, $2 jq-style path
  yq -o=json -I=0 "$2" "$1" 2>/dev/null \
    | jq -S 'walk(if type == "array" then sort else . end)' 2>/dev/null \
    || echo 'null'
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

# WHERE A KIT LIVES, AND HOW MUCH OF IT IS SHARED.
#
# Most kits are a whole file at `.github/workflows/<name>`, which is what
# a caller workflow is. A lint configuration is not: the shared thing is a
# BLOCK inside a file the repository also fills with settings that are
# legitimately its own, and comparing the whole file would report every
# repository as differing over things nobody else has an opinion about.
#
# kits/kits.yaml says so, per kit, and says nothing about the ones that
# are the default shape. It is not itself a kit, and is skipped by name.
manifest="$KITS/kits.yaml"
if [ -f "$manifest" ]; then
  # Loudly, not by falling back to the defaults: a missing yq would turn
  # every manifest entry into "a whole file at .github/workflows/<name>",
  # which for a lint kit means reading a path that is not there and
  # reporting the whole estate absent.
  command -v yq >/dev/null || { echo "::error::$manifest needs yq, which is not on PATH"; exit 1; }
else
  manifest=""
fi

# setting <kit file name> <key> <default>
#
# The name and the key go through the environment, not into the
# expression: a kit file name is a path fragment and splicing one into a
# yq program is how a filename becomes code.
#
# `// ""` is NOT used, deliberately. yq's alternative operator treats
# `false` as absent, so `enabled: false` would read as unset and every
# kit turned off would be compared anyway — silently, which is the only
# way a lever can be worse than not having one. `null` is the absent
# answer and nothing else is.
setting() {
  local value='null'
  if [ -n "$manifest" ]; then
    value=$(KIT="$1" FIELD="$2" yq -r '.[strenv(KIT)][strenv(FIELD)]' "$manifest" 2>/dev/null || echo 'null')
  fi
  [ "$value" != "null" ] && { printf '%s' "$value"; return; }
  printf '%s' "$3"
}

# kit_paths <kit file name> <default path> — the candidate paths to try,
# one per line, in order.
#
# `path:` used to be one scalar. golangci-lint v2 reads either
# `.golangci.yaml` or `.golangci.yml`, and a repository that happened to
# write the short extension read as `absent` for no reason the kit
# should care about — so `path:` may now be a list, tried in order until
# one answers 200. `[] + x` is yq's own flattening: it turns a bare
# scalar into a one-element list and leaves a list alone, so the same
# expression serves both shapes and the kits with no manifest entry at
# all (nothing to flatten, falls straight to the default below).
kit_paths() {
  local raw=""
  if [ -n "$manifest" ]; then
    raw=$(KIT="$1" yq -r '([] + .[strenv(KIT)].path)[]' "$manifest" 2>/dev/null || true)
  fi
  if [ -z "$raw" ]; then
    printf '%s\n' "$2"
  else
    printf '%s\n' "$raw"
  fi
}

# exempt_reason <kit file name> <repo owner/name> — the documented reason
# a repository is not compared against this kit, or empty when it is not
# exempt.
#
# Keyed on the repository's own name, not `owner/name`: kits.yaml is
# read once for every estate this action runs against, and a repository
# does not change name when it changes owner. A false is never the
# intended reason here (unlike `enabled`), so the plain `//` default is
# fine.
exempt_reason() {
  [ -z "$manifest" ] && return 0
  local short="${2##*/}"
  KIT="$1" NAME="$short" yq -r '.[strenv(KIT)].exempt[strenv(NAME)] // ""' "$manifest" 2>/dev/null || true
}

kits=()
skipped=()
while read -r kit; do
  name=$(basename "$kit")
  [ "$name" = "kits.yaml" ] && continue
  if [ "$(setting "$name" enabled true)" != "true" ]; then
    skipped+=("$name")
    continue
  fi
  kits+=("$kit")
done < <(find "$KITS" -maxdepth 1 -name '*.yaml' | sort)
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

# Six states, and the difference between them is the point of the
# check: `same`, `differs`, `absent` (a repository that carries none of
# the file — wrong for a Go repository, correct for one that releases
# nothing), `unreadable` (a read that failed, which is never reported as
# any of the others), `n/a` (a repository this kit's `applies_if` says
# to skip — an "absent" go.mod would otherwise read as an absent kit
# file, which is a different claim) and `exempt (<reason>)` (a
# repository named in the kit's `exempt:` map, documented and not
# compared at all).
rows='[]'
differences=0
absent=0
unreadable=0
napplicable=0
nexempt=0

# applies_cache["<repo>|<applies_if path>"] — yes/no/unreadable, so a
# kit's `applies_if` file is read at most once per repository even when
# more than one kit asks about the same file.
declare -A applies_cache=()

# applies_state <repo> <applies_if path> — whether that repository
# carries the file `applies_if` names, cached per repository. Reuses
# `http`/`$branch` exactly as the per-kit reads below do; the cache key
# does not include the branch because a repository has exactly one
# default branch for the lifetime of this loop.
applies_state() {
  local key="$1|$2"
  if [ -z "${applies_cache[$key]+x}" ]; then
    http "$API/repos/$1/contents/$2?ref=$branch"
    case "$HTTP_STATUS" in
      200) applies_cache[$key]=yes ;;
      404) applies_cache[$key]=no ;;
      *) applies_cache[$key]=unreadable ;;
    esac
  fi
  printf '%s' "${applies_cache[$key]}"
}

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
    skip=false

    # DOCUMENTED EXEMPTIONS. `kits.yaml`'s `exempt:` map names a
    # repository and says why, in prose that ends up in the table:
    # `exempt (<reason>)`. Checked first, and it costs no read of its
    # own — a repository this kit has no opinion about is not worth
    # spending the file read on.
    reason=$(exempt_reason "$name" "$repo")
    if [ -n "$reason" ]; then
      state="exempt ($reason)"
      skip=true
      nexempt=$((nexempt + 1))
    fi

    # ONLY CHECK REPOSITORIES THIS KIT APPLIES TO. `applies_if` names a
    # file at the repository's ROOT — for golangci-depguard.yaml, the
    # root `go.mod` that makes a repository a Go repository at all. Kept
    # to the root deliberately: a multi-module repository with no root
    # go.mod would need a tree search to tell "no Go here" from "Go, just
    # not at the root", and this check has no reason to guess which — it
    # reports `n/a` for that shape too, same as a repository with no Go
    # in it. A repository whose module lives one level down can carry
    # the lint block anyway; nothing here stops it, this check simply
    # will not notice.
    if ! $skip; then
      applies_if=$(setting "$name" applies_if "")
      if [ -n "$applies_if" ]; then
        case "$(applies_state "$repo" "$applies_if")" in
          no)
            state="n/a"
            skip=true
            napplicable=$((napplicable + 1))
            ;;
          unreadable)
            echo "::warning::$repo: could not read $applies_if — not compared"
            state=unreadable
            unreadable=$((unreadable + 1))
            skip=true
            ;;
        esac
      fi
    fi

    if ! $skip; then
      compare=$(setting "$name" compare "")
      within=$(setting "$name" within ".")
      # `path:` may be one scalar or a list (golangci-depguard.yaml's is
      # both `.golangci.yaml` and `.golangci.yml` — golangci-lint v2
      # accepts either extension, and a repository that chose the short
      # one is not "absent"). Tried in order; the first 200 wins. A 404
      # on every candidate is absent; a non-404 failure on any of them,
      # with no 200 among them, is unreadable — reported for whichever
      # candidate hit it first.
      mapfile -t candidate_paths < <(kit_paths "$name" ".github/workflows/$name")
      found=false
      found_path=""
      err_status=""
      err_path=""
      for candidate in "${candidate_paths[@]}"; do
        # Kit file names are plain (`[a-z-]+.yaml`) and so are the paths
        # in the manifest; the ref is the default branch this repository
        # reported.
        http "$API/repos/$repo/contents/$candidate?ref=$branch"
        if [ "$HTTP_STATUS" = 200 ]; then
          found=true
          found_path="$candidate"
          break
        elif [ "$HTTP_STATUS" != 404 ] && [ -z "$err_status" ]; then
          err_status="$HTTP_STATUS"
          err_path="$candidate"
        fi
      done

      if $found; then
        path="$found_path"
        # The repository is in the installation and the token carries
        # contents: read, so a 404 here means the file is not there.
        jq -r '.content // empty' <<<"$HTTP_BODY" | tr -d '\n' | base64 -d >"$work/theirs" 2>/dev/null || : >"$work/theirs"
        if [ -n "$compare" ]; then
          # A BLOCK inside a larger file, compared as data rather than as
          # text: canonical JSON, keys sorted. Comments and key order are
          # not what a copied block has to keep, and the alternative --
          # normalising the text -- would report an indentation change
          # that the linter cannot see.
          #
          # A subtree that is not there reads as `null`, which differs
          # from the kit. That is the answer: a repository that dropped
          # the block has fallen behind it, and it is not "absent",
          # because the file it belongs in is right there.
          subtree "$work/theirs" "$compare" >"$work/theirs.substance"
          subtree "$kit" "$within" >"$work/ours.substance"
        else
          substance <"$work/theirs" >"$work/theirs.substance"
          substance <"$kit" >"$work/ours.substance"
        fi
        if cmp -s "$work/ours.substance" "$work/theirs.substance"; then
          state=same
        else
          state=differs
          differences=$((differences + 1))
          diff -u --label "kits/$name (canonical)" --label "$repo $path" \
            "$work/ours.substance" "$work/theirs.substance" >"$work/${repo//\//__}.$name.diff" || true
          echo "::warning::$repo: $path differs from the canonical kit"
        fi
      elif [ -n "$err_status" ]; then
        echo "::warning::$repo: could not read $err_path (HTTP $err_status) — not compared"
        state=unreadable
        unreadable=$((unreadable + 1))
      else
        state=absent
        absent=$((absent + 1))
      fi
    fi

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
  echo "**$differences** differ, **$absent** absent, **$napplicable** n/a, **$nexempt** exempt,"
  echo "**$unreadable** could not be read, across $(jq 'length' <<<"$repositories") repositories"
  echo "and ${#kits[@]} files."
  echo
  if [ "${#skipped[@]}" -gt 0 ]; then
    echo "Kits turned off in \`kits/kits.yaml\` and NOT compared: ${skipped[*]}."
    echo
  fi
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
