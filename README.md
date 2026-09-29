# ci-actions

The composite actions [truvity/ci-workflows](https://github.com/truvity/ci-workflows)
is built from: bootstrapping a runner, running a recipe, guarding pins
and runners, discovering and comparing the fleet, standing up an
end-to-end cluster, and checking a repository against the component
contract.

## What ships

| action | what it does | key inputs | used by (ci-workflows) |
| -- | -- | -- | -- |
| [`setup-devbox`](setup-devbox/action.yaml) | Installs devbox, proto and the toolchain, logs into CodeArtifact, mints a GitHub App token for private-module reads, and wires the caches by delegating to [`truvity/ci-cache/setup`](https://github.com/truvity/ci-cache) | `go-cache-bucket`, `go-cache-region`, `goproxy`, `go-private`, `module-app-client-id`, `aws-config-file`, `codeartifact-domain` | `check`, `integration`, `release-public`, `release-private`, `parity-fleet` |
| [`recipe`](recipe/action.yaml) | Runs one task-runner recipe inside devbox, then fails if the working tree changed | `recipe` (required), `command` (`just`) | `check` |
| [`public-runners`](public-runners/action.yaml) | Refuses a public repository that asks for self-hosted runners | `runners` (required), `visibility` | `check`, `integration`, `release-public` |
| [`tagged-pins`](tagged-pins/action.yaml) | Refuses a pin into the shared CI libraries that is not the commit of a tag | `libraries` (ci-workflows, ci-actions, ci-cache) | `check` |
| [`policy-conformance`](policy-conformance/README.md) | Checks a repository against the component contract, rules C1 to C12, one line per rule | `strict` (`false`), `skip`, `reason` | `check`, opt-in |
| [`cluster`](cluster/README.md) | Stands up the end-to-end cluster (a disposable kind box, or a shared cluster) behind one set of outputs | `mode` (required), `policy-version`, `namespace`, `background` | `integration` (kind tier) |
| [`setup-remote-builders`](setup-remote-builders/action.yaml) | Attaches the remote BuildKit builders a cross-architecture image build needs | `remote-builders` (required) | `integration` |
| [`openbao-secrets`](openbao-secrets/action.yaml) | Reads a job's third-party secrets from OpenBao at run time | `issuer`, `address`, `path` (required), `keys` | `release-private` |
| [`fleet-discover`](fleet-discover/action.yaml) | Decides which repositories a fleet job touches, and whether each default branch is gated | `token` (required), `estate`, `require-check`, `repositories` | `renovate-fleet`, `parity-fleet` |
| [`caller-parity`](caller-parity/action.yaml) | Compares each repository's shared caller workflows (and the depguard block) against the kits in [`caller-parity/kits/`](caller-parity/kits/), and reports | `token`, `repositories` (required), `fail-on-diff` (`false`) | `parity-fleet` |
| [`devbox-parity`](devbox-parity/action.yaml) | Refreshes devbox packages and aligns a Go repository's toolchain triple, opening a pull request | `token` (required), `mode`, `module-dirs` | `parity-fleet` |

Every input is described in full in its `action.yaml`.

## Who it is for

Mostly for truvity/ci-workflows, which is the only thing a repository in
either organisation pins: its reusable workflows call these actions, so
a repository gets them without naming this repository at all. Pin an
action here directly when you are writing a workflow that
ci-workflows does not provide, or when you maintain ci-workflows itself.

## The model

The actions lived in ci-workflows until 2026-09-24. A workflow cannot
pin the commit that introduces an action in its **own** repository: that
commit does not exist until the merge, and rebase-merge rewrites it. So
every action change was a three-step dance (land the action, tag,
land a second pull request moving the pin) with `master` red in between.
Across repositories that problem does not exist: ci-workflows pins
`truvity/ci-actions/<action>@<sha>` the way it pins any third party.

**Pin the commit of a tag, never a tag or a branch.** Every repository in
both organisations executes this code, so a compromise here reaches the
whole estate. Tags move; commit SHAs do not. Take the commit, not the
annotated tag object (`git rev-parse vX.Y.Z^{commit}`); a `uses:` at a
tag object resolves to nothing. `tagged-pins` refuses anything that is
not the commit of a tag.

## Install and a worked example

Nothing to install: GitHub fetches an action from its `uses:` line. The
smallest useful job, on a public repository:

```yaml
jobs:
  guard:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7.0.1
      - uses: truvity/ci-actions/public-runners@73a8e54d28025275f2b2750ae4bfc56f008b36cb # v1.3.0
        with:
          runners: ubuntu-latest
          visibility: ${{ github.event.repository.visibility }}
      - uses: truvity/ci-actions/tagged-pins@73a8e54d28025275f2b2750ae4bfc56f008b36cb # v1.3.0
```

The first action prints `ok ubuntu-latest` and `public repository,
hosted runners only — checked`; the second prints one `ok` or `UNTAGGED`
line per pin into the shared CI libraries and fails on an `UNTAGGED`
one. A repository that calls ci-workflows' `check.yaml` already runs
both.

## Consumers

| consumer | what it uses |
| -- | -- |
| [truvity/ci-workflows](https://github.com/truvity/ci-workflows) | every action, from the workflows in the table above; and through them, every repository that calls ci-workflows |
| [truvity/policy](https://github.com/truvity/policy) | `cluster` (`mode: kind`), through ci-workflows' `integration.yaml` on its kind tier |

## Neighbours

- **ci-workflows → ci-actions → ci-cache; ci-plane hosts the runners.**
  [ci-workflows](https://github.com/truvity/ci-workflows) is the only
  thing a caller pins; this repository holds the composite steps;
  [ci-cache](https://github.com/truvity/ci-cache) owns cache wiring (its
  `setup` action, which `setup-devbox` calls) and the cache server;
  [ci-plane](https://github.com/truvity/ci-plane) is where work executes
  (runner and nix-worker images, `arc-runners`, `ci-builders`).
- **[policy](https://github.com/truvity/policy)**: the component contract
  `policy-conformance` checks, and the `hack/kind/` box `cluster` runs at
  a pinned release.
- **[access-roster](https://github.com/truvity/access-roster)**: its
  action mints this repository's tagging token in `auto-release.yaml`,
  and its `accessctl` is the exec plugin `cluster`'s `mode: shared`
  expects.

## Documentation

- [`cluster/README.md`](cluster/README.md): both tiers, which one is
  neutral, and the five variables.
- [`policy-conformance/README.md`](policy-conformance/README.md): each
  rule, what it reads, and how to skip one.
- Each action's `action.yaml`: every input, with its reason.
- [CHANGELOG.md](CHANGELOG.md): every release.
- When to reach for these, in ci-workflows: the
  [estate lifecycle](https://github.com/truvity/ci-workflows/blob/master/docs/estate-lifecycle.md),
  the [fleet model](https://github.com/truvity/ci-workflows/blob/master/docs/fleet.md),
  the [devbox contract](https://github.com/truvity/ci-workflows/blob/master/docs/devbox.md)
  and [its update job](https://github.com/truvity/ci-workflows/blob/master/docs/devbox-update.md),
  and the [secret plane](https://github.com/truvity/ci-workflows/blob/master/docs/secrets.md).

## The rule that makes this repository public

**Mechanism only.** Account ids, role ARNs, registry hostnames, bucket
names, cluster names and internal DNS are caller inputs or organisation
variables, never content here. Public history cannot be unpublished, so
`hack/leak-canary.sh` enforces the rule in CI.

Public because a private repository's actions cannot be used across an
organisation boundary, and *internal* visibility needs an Enterprise
plan neither organisation has. Public is what lets the second
organisation consume these directly: no mirror, no sync job, no drift
check.

## Status

Four tags: v1.0.0 (2026-09-24), v1.1.0 (2026-09-26), v1.2.0 and v1.3.0
(both 2026-09-29). v1.2.0 and v1.3.0 have a GitHub release each,
v1.3.0 marked "Latest"; v1.0.0 and v1.1.0 do not. ci-workflows pins
v1.3.0 for every action. `policy-conformance` and `setup-devbox`'s move
to a tagged ci-cache pin, both once listed here as unreleased work,
shipped in v1.2.0; see [CHANGELOG.md](CHANGELOG.md) for what shipped
since.

## Development

`just check` runs what CI runs on every pull request. Set up your
environment with `direnv allow`, or run `devbox run just <recipe>` to
call a recipe without direnv.

There is no unit test for a composite action that is not "run its step
bodies and read what they do". That is what `hack/` is, with recipes for
each:

- `just lint` — actionlint over every workflow and composite.
- `just discover` — fleet-discover's required-check rule, against a stub API.
- `just parity` — what counts as a DIFFERENCE from the canonical caller.
- `just kit` — the golangci-depguard kit against truvity/policy.
- `just cache-env` — what setup-devbox writes into GITHUB_ENV, per cache shape.
- `just pins-cases` — the pin guard, against local git remotes.
- `just conformance-cases` — policy-conformance rule tests against fixture repositories.
- `just fork-guard` — cluster's fork refusal, against fake event payloads.
- `just pins` — verify all pins point to release tags.
- `just runners` — verify the repository uses public runners.
- `just conformance` — run policy-conformance against this repository.
- `just leak-canary` — scan for secrets and sensitive data.
- `just check` — run all recipes (the merge gate).

All but `kit` need no network and no token. `self-check.yaml` runs every
one of them plus `policy-conformance` against this repository, under the
`check` context. `check` is the whole merge gate: no approving review,
and no bypass. Renovate opens a pull request like anyone else and
GitHub's auto-merge finishes it when `check` goes green.

`cluster`'s `mode: kind` needs a real kind cluster and a container
runtime, so `self-check.yaml`'s separate `cluster-kind` job proves it
end to end, outside `check` and not required.

## Releasing

A release is an annotated `vX.Y.Z` tag on `master`; add its heading to
[CHANGELOG.md](CHANGELOG.md) in the same change that is tagged.
[`auto-release.yaml`](.github/workflows/auto-release.yaml) cuts the
next patch tag each Monday when `master` has moved, and at once for a
merged pull request labelled `security`, but only while
`vars.AUTO_RELEASE` is `true` and `vars.ACCESS_ROSTER_ISSUER` is set.
Every run so far has been skipped on that guard, so all four tags were
cut by hand; v1.2.0 and v1.3.0 also got a GitHub release by hand, v1.0.0
and v1.1.0 did not. Consumers pin the tag's commit either way.

`auto-release.yaml` is standalone rather than a call into ci-workflows'
shared workflow, because ci-workflows pins these actions and the two
libraries would otherwise pin each other.

## Licence

MIT, as [LICENSE](LICENSE); the same text as ci-cache and ci-plane.
