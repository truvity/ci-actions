# ci-actions development gate
# Recipes mirror what CI runs on every pull request

default: check

# Lint GitHub Actions workflows and composites with actionlint
lint:
    #!/usr/bin/env bash
    set -euo pipefail
    if ! command -v actionlint &>/dev/null; then
        tmpdir=$(mktemp -d)
        trap "rm -rf $tmpdir" EXIT
        curl -fsSL -o "$tmpdir/actionlint.tgz" \
          https://github.com/rhysd/actionlint/releases/download/v1.7.12/actionlint_1.7.12_linux_amd64.tar.gz
        tar xzf "$tmpdir/actionlint.tgz" -C "$tmpdir" actionlint
        "$tmpdir/actionlint" -color
    else
        actionlint -color
    fi

# Test policy kit currency
kit:
    go run ./cmd/ci-actions repo-check policy-kit

# setup-devbox's cache delegation holds (reads ci-cache's action at the pinned sha)
cache-env:
    go run ./cmd/ci-actions repo-check cache-seam

# Go vet and table tests: tagged-pins against local remotes, fleet pins, fleet-discover, caller-parity, openbao-secrets, cluster, policy-conformance (against fixture repositories) and setup-devbox (preflight, the devbox install, tokens, CodeArtifact, guards) against fakes
go-test:
    go vet ./...
    go test ./...

# Test the tagged-pins wrapper that finds the binary
pins-wrapper:
    @./hack/tagged-pins-wrapper-cases.sh

# token-exchange's step body, run as written against a stub curl and kubectl
token-exchange:
    @./hack/token-exchange-cases.sh

# Fail if any action file or script escalates privilege
no-escalation:
    go run ./cmd/ci-actions repo-check no-escalation

# Run the step scripts as a restricted runner (needs docker and yq)
restricted-sim:
    @./hack/restricted-sim.sh --control
    @./hack/restricted-sim.sh --self-test
    @./hack/restricted-sim.sh

# Known vulnerabilities in the Go CLI. NOT part of `check`: security.yaml
# runs it on its own schedule so a new advisory cannot redden the merge gate.
vuln:
    go run golang.org/x/vuln/cmd/govulncheck@v1.7.0 ./...

# Verify all pins point to release tags
pins:
    #!/usr/bin/env bash
    set -euo pipefail
    LIBRARIES="truvity/ci-workflows truvity/ci-actions truvity/ci-cache"
    
    fail=0
    seen=0
    
    for library in $LIBRARIES; do
      mapfile -t refs < <(grep -rhoE --exclude-dir=.git "${library}/[^@[:space:]\"']+@[0-9a-f]{40}" . 2>/dev/null | sort -u)
    
      if [ ${#refs[@]} -eq 0 ]; then
        echo "no ${library} pins in this checkout — nothing to check"
        continue
      fi
    
      seen=$((seen + 1))
    
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
      echo "ERROR: A pinned commit is not a release. Pin the commit of a tag."
      exit 1
    fi
    
    echo "tagged-pins: ${seen} of 3 libraries had pins in this checkout, all naming releases"

# Verify public runners are used
runners:
    #!/usr/bin/env bash
    set -euo pipefail
    visibility="public"
    
    if [ "$visibility" != "public" ]; then
      echo "not a public repository — any runner is allowed"
      exit 0
    fi
    
    fail=0
    for label in ubuntu-latest; do
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
      echo "ERROR: public repository, hosted runners only"
      exit 1
    fi
    
    echo "public repository, hosted runners only — checked"

# Run policy-conformance against this repository
conformance:
    #!/usr/bin/env bash
    set -euo pipefail
    # Fetch tags for C5 check
    git fetch --tags origin 2>/dev/null || true

    # Run policy-conformance (the Go CLI's command) against this repo
    STRICT=false \
    SKIP="C10" \
    REASON="standalone security.yaml; a pin into ci-workflows would make the libraries pin each other" \
    RENOVATE_PRESET="github>truvity/ci-workflows" \
    DEFAULT_BRANCH="master" \
    go run ./cmd/ci-actions policy-conformance

# Scan for secrets and sensitive data
leak-canary:
    @./hack/leak-canary.sh

# Run all checks (the merge gate)
check: lint kit cache-env go-test pins-wrapper token-exchange no-escalation pins runners conformance leak-canary
    @echo "✓ All checks passed"
