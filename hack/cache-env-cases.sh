#!/usr/bin/env bash
# What `setup-devbox` does about caches, now that it does not do the caching.
#
# The wiring moved to truvity/ci-cache's own `setup` action, which is tested
# there by executing its step bodies (hack/setup-cases.sh, 21 cases). What
# stays here is the SEAM: that this composite delegates, that it hands over
# every input the cache needs, that the retired input is not silently
# ignored, and that the one local step still runs.
#
# The seam is worth its own cases because it is where the two repositories
# can disagree without either being wrong on its own -- an input added there
# and not passed here is a cache that quietly does less, which is exactly
# the failure mode this whole line of work was chasing.
set -uo pipefail

# The C locale, for the same reason the executing harness pins it: anything
# compared as text must not depend on where the machine thinks it is.
export LC_ALL=C

here="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
action="$here/setup-devbox/action.yaml"
[ -f "$action" ] || { echo "no action at $action" >&2; exit 2; }

command -v yq >/dev/null || { echo "yq (mikefarah) is required: ubuntu-latest ships it, devbox provides yq-go" >&2; exit 2; }

fail=0
checked=0

step_field() { yq -r ".runs.steps[] | select(.name == \"$1\") | $2" "$action"; }

# --- the delegation itself ------------------------------------------------
#
# Pinned by SHA, not by tag or branch. A moving ref here would let the cache
# wiring of every repository in both organizations change without a single
# pin moving, which is the property this repository exists to prevent.
checked=$((checked + 1))
uses="$(step_field "Wire the fleet caches" ".uses")"
case "$uses" in
    truvity/ci-cache/setup@[0-9a-f]*)
        sha="${uses##*@}"
        if [ "${#sha}" -ne 40 ]; then
            echo "FAIL [pin]: ci-cache/setup is pinned to \"$sha\", which is not a full 40-character SHA"
            fail=$((fail + 1))
        fi
        ;;
    *)
        echo "FAIL [pin]: the cache step does not use a SHA-pinned truvity/ci-cache/setup; it uses \"$uses\""
        fail=$((fail + 1))
        ;;
esac

# --- every input the cache needs crosses the seam -------------------------
#
# DERIVED from the downstream action at the SHA this one pins, not from a
# list kept here. The list was kept here until 2026-09-24, with a comment
# arguing that deriving it "could not notice when the action downstream
# grows an input this one does not pass" -- which had it exactly backwards.
# Deriving from THIS file would be circular; reading the OTHER repository
# is the only thing that can notice.
#
# It cost something to learn. ci-cache/setup declares nine inputs and this
# action passed five. `bazel-remote` and `npm-registry` were never passed,
# so the moon and yarn checks could not fire in any job in the estate --
# silently, because an unset value makes those branches no-ops. It surfaced
# only when bar's first run on the new pin printed no moon line at all.
#
# Fetched at the PINNED sha, so this asks about the version actually in
# use rather than whatever ci-cache's master says today.
checked=$((checked + 1))

pin="$(step_field "Wire the fleet caches" ".uses")"
sha="${pin##*@}"
case "$sha" in
    [0-9a-f]*) ;;
    *) echo "FAIL [inputs]: cannot read a sha out of \"$pin\""; fail=$((fail + 1)); sha="" ;;
esac

if [ -n "$sha" ]; then
    downstream="$(mktemp)"
    url="https://raw.githubusercontent.com/truvity/ci-cache/${sha}/setup/action.yaml"

    # A guard that passes when it could not look is the failure mode this
    # whole file exists to prevent, so a fetch failure is a FAILURE and not
    # a skip.
    if ! curl -fsSL --max-time 20 -o "$downstream" "$url"; then
        echo "FAIL [inputs]: could not fetch $url — the seam went unchecked"
        fail=$((fail + 1))
    else
        # Declared THERE but deliberately not passed here, each with the
        # reason. Anything not on this list must cross, or this fails.
        #
        #   languages       setup-devbox does not second-guess detection;
        #                   the action reads the tree, which is the whole
        #                   point of it owning detection.
        #   client-version  the action pins the clients that match its own
        #                   release; a caller choosing them would defeat
        #                   versioning the bundle together.
        #   bazel-remote    NOT WIRED YET, and the reason is not this
        #   npm-registry    action's: the estate has no org variable naming
        #                   either host, so there is nothing to pass. The
        #                   values live in each repository's own
        #                   .moon/workspace.yml and .yarnrc.yml today.
        #                   Tracked in the cache bundle work; remove them from
        #                   this list the moment a variable exists, and the
        #                   guard will then insist they cross.
        exempt=" languages client-version bazel-remote npm-registry "

        for to in $(yq -r '.inputs | keys | .[]' "$downstream"); do
            case "$exempt" in *" $to "*) continue ;; esac

            got="$(step_field "Wire the fleet caches" ".with.\"$to\"")"
            case "$got" in
                *"inputs."*) ;;
                *)
                    echo "FAIL [inputs]: ci-cache/setup declares \"$to\" and this action does not pass it"
                    echo "               (wired to \"$got\"); pass it, or exempt it with a reason"
                    fail=$((fail + 1))
                    ;;
            esac
        done
    fi

    rm -f "$downstream"
fi

# --- the retired input is refused loudly, not ignored quietly -------------
#
# go-cache-server pointed at a cache server that was measured out of the Go
# path. Dropping it silently would repeat the fault that started all this: a
# variable that named something gone, a fall-through that cost four
# milliseconds, and days before anyone noticed.
checked=$((checked + 1))
cond="$(step_field "Warn on the retired cache-server input" ".if")"
case "$cond" in
    *"go-cache-server != ''"*) ;;
    *)
        echo "FAIL [retired]: the warning step's condition is \"$cond\"; it must fire when go-cache-server is set"
        fail=$((fail + 1))
        ;;
esac

checked=$((checked + 1))
d="$(mktemp -d)"
step_field "Warn on the retired cache-server input" ".run" > "$d/step.sh"
: > "$d/env"
env -i PATH="/usr/bin:/bin" HOME="$d" GITHUB_ENV="$d/env" bash "$d/step.sh" > "$d/out" 2>&1
rc=$?
if [ "$rc" -ne 0 ]; then
    echo "FAIL [retired]: the warning step exited $rc; telling a caller something must not fail its job"
    fail=$((fail + 1))
fi
if ! grep -q "::warning::" "$d/out"; then
    echo "FAIL [retired]: the step emitted no ::warning::, so a caller would never learn the input is dead"
    fail=$((fail + 1))
fi
if [ -s "$d/env" ]; then
    echo "FAIL [retired]: the warning step wrote GITHUB_ENV; it must only speak"
    fail=$((fail + 1))
fi
rm -rf "$d"

# --- nothing here wires a cache any more ----------------------------------
#
# If a `run:` step in this composite starts writing GOCACHEPROG again, two
# places decide the same thing and the last one in file order wins -- which
# is not a decision anybody made.
checked=$((checked + 1))
if yq -r '.runs.steps[] | select(.run) | .run' "$action" | grep -q "GOCACHEPROG"; then
    echo "FAIL [ownership]: a run: step in setup-devbox writes GOCACHEPROG; that belongs to ci-cache/setup now"
    fail=$((fail + 1))
fi

# --- the local step that stays --------------------------------------------
#
# devbox re-applies devbox.json's env block over the job environment, so a
# GOPROXY pinned there silently beats whatever the cache wiring set. That
# check is about devbox, not about caches, so it did not move.
checked=$((checked + 1))
if [ "$(step_field "Guard GOPROXY against devbox.json" ".name")" != "Guard GOPROXY against devbox.json" ]; then
    echo "FAIL [guard]: the devbox.json GOPROXY guard is gone; it is about devbox, not about caches"
    fail=$((fail + 1))
fi

if [ "$checked" -eq 0 ]; then
    echo "NOTHING CHECKED — the harness found no cases, which is a failure of the harness"
    exit 1
fi

if [ "$fail" -ne 0 ]; then
    echo "$fail case(s) failed of $checked"
    exit 1
fi

echo "cache seam holds ($checked cases checked)"
