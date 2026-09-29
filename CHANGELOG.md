# Changelog

Every release of truvity/ci-actions, newest first. Pin the commit of a
tag (`git rev-parse vX.Y.Z^{commit}`), never the tag object and never
an untagged commit; `tagged-pins` refuses anything else.

## v1.2.0

- **`policy-conformance`**, a new action: checks the calling repository
  against the component contract's rules C1 to C12 (truvity/policy
  `docs/contracts/component.md`) and prints one line per rule.
  Report-only by default; `strict: true` fails the job. Self-check runs
  it against this repository, and `hack/policy-conformance-cases.sh`
  proves each rule fails on its own defect.
- `tagged-pins` judges `truvity/ci-cache` too, and searches the whole
  checkout rather than `.github` only, so a pin inside a composite action
  is seen.
- `setup-devbox` pins `truvity/ci-cache/setup` at the v0.2.0 release.
  v1.0.0 and v1.1.0 pinned an untagged commit.
- `caller-parity`: the `golangci-depguard` kit is on, arrays compare as
  sets, and `hack/policy-kit-current.sh` fails self-check when the kit
  drifts from its source in truvity/policy.
- This repository's `auto-release.yaml` is standalone instead of calling
  truvity/ci-workflows, which pins these actions; the two libraries no
  longer pin each other.
- `renovate.json` extends the estate preset, `github>truvity/ci-workflows`.
- README reworked around the per-action table; `cluster` has its own
  [README](cluster/README.md).

## v1.1.0

2026-09-26.

- **`cluster`**, a new action: one composite for both end-to-end tiers.
  `mode: kind` stands up truvity/policy's `hack/kind` box at a pinned
  release (policy v0.8.1 in self-check), with `background: true` and
  `mode: wait`; `mode: shared` refuses a fork pull request, then resolves
  the caller's kubeconfig, AWS config and registry login. Both export the
  same five variables.
- `caller-parity`: a kit can be a block inside a file (`kits/kits.yaml`)
  and can ship disabled. The `golangci-depguard` kit ships disabled.
- The `setup-devbox` / `truvity/ci-cache/setup` seam case reads the
  required inputs from ci-cache's `action.yaml` at the pinned SHA, and
  fails when it cannot fetch it.
- Licensed MIT.

## v1.0.0

2026-09-24. The composite actions truvity/ci-workflows is built from,
moved here from ci-workflows with their history, so a workflow change
and an action change stop needing three pull requests.

- `setup-devbox`: devbox, proto and the toolchain; CodeArtifact login;
  a GitHub App token for private-repository reads; the fleet caches,
  delegated to `truvity/ci-cache/setup`. `go-cache-server` is retired
  and warns when passed.
- `recipe`: one task-runner recipe, then a clean-tree check.
- `fleet-discover`: which repositories a fleet job touches, from an
  enrolment list, and whether each default branch is gated (a ruleset
  counts as a required check).
- `caller-parity`: compares a repository's shared caller workflows
  against the canonical kits, and reports.
- `devbox-parity`: refreshes devbox packages and aligns a Go
  repository's toolchain triple.
- `openbao-secrets`: a job reads its third-party secrets from OpenBao at
  run time.
- `setup-remote-builders`: the remote builders a cross-architecture
  image build needs.
- `public-runners`: refuses a public repository pointed at self-hosted
  runners.
- `tagged-pins`: refuses a pin into the shared CI library that names no
  release.
- `self-check.yaml` runs the `hack/` case harnesses under the `check`
  context, which is the whole merge gate.
