#!/usr/bin/env bash
# The body of the fleet-discover action.
#
# A file rather than an inline `run:` block so the rules can be exercised
# against a stub API (hack/discover-cases.sh) instead of only in a live
# fleet run — the required-check rule in particular, which decides
# whether a repository is processed at all and had been reading one of
# its two sources.
#
# Everything arrives through the environment, never spliced into script
# text: TOKEN, ESTATE, REQUIRE_CHECK, REQUIRE_FILE, FILTER, ENROLLED and
# API from the action's inputs, GITHUB_OUTPUT and GITHUB_STEP_SUMMARY
# from the runner. curl + jq only: the self-hosted image ships no gh.
set -euo pipefail

case "$ESTATE" in
  public | private | all) ;;
  *) echo "::error::estate must be public, private or all (got '$ESTATE')"; exit 1 ;;
esac

api() {
  curl -fsS --max-time 60 \
    -H "Authorization: Bearer $TOKEN" \
    -H "Accept: application/vnd.github+json" \
    -H "X-GitHub-Api-Version: 2022-11-28" \
    "$@"
}

# One request, kept whole: the body in $HTTP_BODY and the status in
# $HTTP_STATUS. `curl -f` cannot do this — it throws the body away and
# collapses every 4xx into one exit code, and the difference between
# "nothing gates this branch" and "this token may not look" is exactly
# what the required-check rule below has to tell apart.
http() {
  local raw
  HTTP_BODY=''
  HTTP_STATUS=000
  raw=$(curl -sS --max-time 60 -w $'\n%{http_code}' \
    -H "Authorization: Bearer $TOKEN" \
    -H "Accept: application/vnd.github+json" \
    -H "X-GitHub-Api-Version: 2022-11-28" \
    "$@") || return 0 # a transport failure leaves 000, i.e. unknown
  HTTP_STATUS=${raw##*$'\n'}
  HTTP_BODY=${raw%$'\n'*}
}

# Every repository the installation reaches, paginated, reduced to the
# four fields discovery reads AS EACH PAGE ARRIVES and collected in a
# file. Never through argv: a page of full repository objects is several
# megabytes, and passing it to jq as --argjson died with "Argument list
# too long" on the first real organisation.
pages=$(mktemp)
page=1
while :; do
  api "$API/installation/repositories?per_page=100&page=$page" \
    | jq -c '[.repositories[] | {full_name, visibility, archived, default_branch}]' >"$pages.page"
  cat "$pages.page" >>"$pages"
  [ "$(jq 'length' "$pages.page")" -lt 100 ] && break
  page=$((page + 1))
done
all=$(jq -cs 'add // []' "$pages")
rm -f "$pages" "$pages.page"

# THE REQUIRED-CHECK RULE READS TWO SOURCES.
#
# A branch can require a status check in two entirely separate ways, and
# a repository is gated when EITHER of them requires at least one:
#
#   rulesets   GET /repos/{owner}/{repo}/rules/branches/{branch} — the
#              EFFECTIVE rules for that branch: already merged across
#              every ruleset that applies to it, at repository and at
#              organisation level, with `evaluate` and `disabled` ones
#              left out. Needs Metadata: read, which every installation
#              token carries.
#   classic    branch protection. GET
#              /repos/{owner}/{repo}/branches/{branch}/protection first:
#              it is authoritative and viewer-independent, and its 404
#              says "Branch not protected" in so many words, which is a
#              definitive no. It needs Administration: read, which the
#              fleet App does not carry, so when it is not readable the
#              reader falls back to GraphQL's `refUpdateRule` — which
#              needs no permission beyond seeing the repository, but
#              reports the rule AS IT APPLIES TO THE VIEWER: a viewer
#              who may bypass it (`enforce_admins: false`) is told there
#              are no required contexts. `null` is no classic protection
#              at all.
#
# Reading classic protection alone skipped every repository that had
# moved its merge gate into a ruleset — silently, since a skip is the
# normal outcome for most of an installation.
#
# Each reader prints a COUNT or `unknown`. Unknown is not zero: a
# repository must never be dropped because a read failed.
ruleset_checks() { # $1 owner/name, $2 branch
  http "$API/repos/$1/rules/branches/$2?per_page=100"
  [ "$HTTP_STATUS" = 200 ] || { echo unknown; return 0; }
  jq -r 'if type == "array" then
           [ .[]
             | select(.type == "required_status_checks")
             | (.parameters.required_status_checks // []) | length
           ] | add // 0
         else "unknown" end' <<<"$HTTP_BODY" 2>/dev/null || echo unknown
}

classic_checks() { # $1 owner, $2 name, $3 branch
  local query
  http "$API/repos/$1/$2/branches/$3/protection"
  case "$HTTP_STATUS" in
    200)
      jq -r '(.required_status_checks.contexts // []) | length' <<<"$HTTP_BODY" 2>/dev/null || echo unknown
      return 0
      ;;
    404)
      # "Branch not protected" is the endpoint saying there is no
      # classic protection. A bare "Not Found" is this token being told
      # nothing, which is not the same answer — fall through.
      if jq -e '.message == "Branch not protected"' >/dev/null 2>&1 <<<"$HTTP_BODY"; then
        echo 0
        return 0
      fi
      ;;
  esac

  query=$(jq -n --arg o "$1" --arg r "$2" \
    '{query: "query($o:String!,$r:String!){repository(owner:$o,name:$r){defaultBranchRef{refUpdateRule{requiredStatusCheckContexts}}}}", variables: {o: $o, r: $r}}')
  http -X POST "$API/graphql" -d "$query"
  [ "$HTTP_STATUS" = 200 ] || { echo unknown; return 0; }
  # GraphQL answers 200 with an `errors` array, so the status is not the
  # whole story; a null repository is a read this token did not get.
  jq -r 'if ((.errors // []) | length) > 0 then "unknown"
         elif .data.repository == null then "unknown"
         elif (.data.repository.defaultBranchRef.refUpdateRule // null) == null then 0
         else (.data.repository.defaultBranchRef.refUpdateRule.requiredStatusCheckContexts // []) | length
         end' <<<"$HTTP_BODY" 2>/dev/null || echo unknown
}

describe() { # a count or `unknown`, as the summary says it
  case "$1" in
    unknown) echo "could not read" ;;
    0) echo none ;;
    *) echo "$1" ;;
  esac
}

# Sets GATE (yes | no | unknown) and GATE_DETAIL, which names BOTH
# sources so a skip line says what was actually consulted.
gate() { # $1 owner/name, $2 branch
  local rulesets classic
  rulesets=$(ruleset_checks "$1" "$2")
  classic=$(classic_checks "${1%/*}" "${1#*/}" "$2")
  GATE_DETAIL="rulesets: $(describe "$rulesets"), classic protection: $(describe "$classic")"
  if { [ "$rulesets" != unknown ] && [ "$rulesets" -ge 1 ]; } \
    || { [ "$classic" != unknown ] && [ "$classic" -ge 1 ]; }; then
    GATE=yes
  elif [ "$rulesets" = unknown ] || [ "$classic" = unknown ]; then
    GATE=unknown
  else
    GATE=no
  fi
}

# The four rules. Each one that trips is recorded with its reason, so the
# step summary says why a repository was NOT processed — "renovate has
# never opened a PR here" must never again be a silent baseline.
kept='[]'
skipped='[]'
undecided='[]'

enrolled=$(jq -c 'if type == "array" then . else [] end' <<<"${ENROLLED:-[]}" 2>/dev/null || echo '[]')
if [ -n "$ENROLLED" ] && [ "$(jq 'length' <<<"$enrolled")" -eq 0 ]; then
  echo "::error::repositories is not a non-empty JSON array of names: $ENROLLED"
  exit 1
fi

while IFS=$'\t' read -r full visibility archived default_branch; do
  reason=""
  if [ -n "$ENROLLED" ] && ! jq -e --arg n "${full#*/}" 'index($n)' <<<"$enrolled" >/dev/null; then
    reason="not enrolled"
  elif [ -n "$FILTER" ] && ! jq -en --arg f "$full" --arg re "$FILTER" '$f | test($re)' >/dev/null; then
    reason="does not match filter"
  elif [ "$archived" = "true" ]; then
    reason="archived"
  elif [ "$ESTATE" != "all" ] && [ "$visibility" != "$ESTATE" ]; then
    reason="visibility $visibility, estate $ESTATE"
  else
    if [ "$REQUIRE_CHECK" = "true" ]; then
      gate "$full" "$default_branch"
      case "$GATE" in
        no)
          reason="no required status check on $default_branch ($GATE_DETAIL)"
          ;;
        unknown)
          # Never a skip. A repository dropped because a read 403'd would
          # go unprocessed with nothing but a table row to say so, which
          # is the failure this rule exists to prevent, one level up.
          echo "::warning::$full: could not decide the required-check rule ($GATE_DETAIL) — processing it anyway"
          undecided=$(jq -c --arg r "$full" --arg d "$GATE_DETAIL" '. + [{repo: $r, detail: $d}]' <<<"$undecided")
          ;;
      esac
    fi
    if [ -z "$reason" ] && [ -n "$REQUIRE_FILE" ]; then
      if ! api -o /dev/null "$API/repos/$full/contents/$REQUIRE_FILE?ref=$default_branch" 2>/dev/null; then
        reason="no $REQUIRE_FILE"
      fi
    fi
  fi

  if [ -z "$reason" ]; then
    kept=$(jq -c --arg r "$full" '. + [$r]' <<<"$kept")
  else
    skipped=$(jq -c --arg r "$full" --arg why "$reason" '. + [{repo: $r, reason: $why}]' <<<"$skipped")
  fi
done < <(jq -r '.[] | [.full_name, .visibility, (.archived|tostring), .default_branch] | @tsv' <<<"$all")

kept=$(jq -c 'sort' <<<"$kept")
count=$(jq 'length' <<<"$kept")

# Enrolled but invisible to the App: not installed on it, renamed or
# deleted. Error annotations, one per repository, so the run page names
# every fault; the rest of the estate still runs.
missing=$(jq -c --argjson all "$(jq -c '[.[].full_name | split("/")[1]]' <<<"$all")" '[.[] | select(. as $n | $all | index($n) | not)] | sort' <<<"$enrolled")
for name in $(jq -r '.[]' <<<"$missing"); do
  echo "::error::$name is enrolled but the App cannot see it — install the App on it, or remove it from the list"
done

{
  echo "## fleet-discover — estate \`$ESTATE\`"
  echo
  echo "**$count** repositories to process out of $(jq 'length' <<<"$all") the App can see."
  echo
  if [ "$count" -gt 0 ]; then
    jq -r '.[] | "- " + .' <<<"$kept"
    echo
  fi
  if [ "$(jq 'length' <<<"$missing")" -gt 0 ]; then
    echo "**Enrolled but not reachable by the App:** $(jq -r 'join(", ")' <<<"$missing")"
    echo
  fi
  if [ "$(jq 'length' <<<"$undecided")" -gt 0 ]; then
    echo "**Required-check rule not decided** (kept regardless — a read failed, which is not the same as no check):"
    echo
    jq -r '.[] | "- " + .repo + " — " + .detail' <<<"$undecided"
    echo
  fi
  # "not enrolled" is the normal case for most of an installation once a
  # list is in use; listing each one would bury the rest.
  shown=$(jq -c '[.[] | select(.reason != "not enrolled")]' <<<"$skipped")
  if [ "$(jq 'length' <<<"$shown")" -gt 0 ]; then
    echo "| skipped | why |"
    echo "|---|---|"
    jq -r '.[] | "| " + .repo + " | " + .reason + " |"' <<<"$shown"
  fi
} | tee -a "$GITHUB_STEP_SUMMARY"

echo "repositories=$kept" >> "$GITHUB_OUTPUT"
echo "count=$count" >> "$GITHUB_OUTPUT"
echo "missing=$missing" >> "$GITHUB_OUTPUT"
