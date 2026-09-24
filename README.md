# ci-actions

The composite actions
[truvity/ci-workflows](https://github.com/truvity/ci-workflows) is built
from. They lived there until 2026-09-24; they are the same actions, with
the same history, moved.

## Why they are not in ci-workflows any more

A workflow cannot pin the commit that introduces an action in its **own**
repository. That commit does not exist until the merge, and rebase-merge
rewrites it anyway. ci-workflows carried two gates for exactly that
self-reference — one asking whether a pin was older than the action, one
asking whether it named a release — and together they made every action
change a three-step dance: land the action, tag a release, land a second
pull request moving the pin. In between, `master` was red.

Across repositories none of that exists. ci-workflows pins
`truvity/ci-actions@<sha>` the way it pins any third party, in one
commit, and the release gate goes back to meaning what it was written
for.

## Two rules, the same two ci-workflows has

**1. Pin by commit SHA, never by tag or branch.** Every repository in
both organizations executes this code, so a compromise here reaches the
whole estate. Tags move; commit SHAs do not. Pin the commit **of a tag** —
`tagged-pins` below refuses anything else.

```yaml
uses: truvity/ci-actions/setup-devbox@<40-char-sha> # vX.Y.Z
```

**2. Mechanism only.** Account ids, role ARNs, registry hostnames, bucket
names, cluster names and internal DNS are **caller inputs or org
variables** — never content here. This repository is public, and public
history cannot be unpublished. `hack/leak-canary.sh` enforces it in CI.

Public for the same reason ci-workflows is: a private repository's
actions cannot be used across an organization boundary, and *internal*
visibility needs an Enterprise plan neither organization has. Public is
what lets `trust-form` consume these directly — no mirror, no sync job,
no drift check. It is safe because of rule 2.

## The actions

| action | what it does |
| -- | -- |
| [`setup-devbox`](setup-devbox/) | Bootstraps devbox, proto and the toolchain, logs into CodeArtifact, and wires the fleet caches by delegating to [`truvity/ci-cache/setup`](https://github.com/truvity/ci-cache) |
| [`recipe`](recipe/) | Runs one task-runner recipe and checks the working tree afterwards |
| [`fleet-discover`](fleet-discover/) | Decides which repositories a fleet job touches, and whether each one's default branch is gated |
| [`caller-parity`](caller-parity/) | Compares a repository's shared caller workflow against the canonical copy in [`caller-parity/kits/`](caller-parity/kits/) |
| [`devbox-parity`](devbox-parity/) | Refreshes devbox packages and keeps a Go repository's toolchain triple aligned |
| [`openbao-secrets`](openbao-secrets/) | Reads a job's third-party secrets from OpenBao at run time instead of copying them into every repository |
| [`setup-remote-builders`](setup-remote-builders/) | Attaches the remote builders a cross-architecture image build needs |
| [`public-runners`](public-runners/) | Refuses a public repository that has been pointed at self-hosted runners |
| [`tagged-pins`](tagged-pins/) | Refuses a pin into the shared CI library that names no release |

## Docs

The prose that explains *when* to reach for these — the estate lifecycle,
the fleet model, the devbox contract, the secret plane — stays with the
workflows that orchestrate them:

- [estate-lifecycle.md](https://github.com/truvity/ci-workflows/blob/master/docs/estate-lifecycle.md) — a repository from birth to autopilot
- [fleet.md](https://github.com/truvity/ci-workflows/blob/master/docs/fleet.md) — one job per estate, and `caller-parity`'s comparison
- [devbox.md](https://github.com/truvity/ci-workflows/blob/master/docs/devbox.md) — preparing `devbox.json` for both faces
- [devbox-update.md](https://github.com/truvity/ci-workflows/blob/master/docs/devbox-update.md) — the toolchain triple's one owner
- [secrets.md](https://github.com/truvity/ci-workflows/blob/master/docs/secrets.md) — a job's third-party secrets, read at run time

## Testing

There is no unit test for a composite action that is not "run its step
bodies and read what they do". That is what `hack/` is:

```
hack/discover-cases.sh       fleet-discover's required-check rule, against a stub API
hack/caller-parity-cases.sh  what counts as a DIFFERENCE from the canonical caller
hack/cache-env-cases.sh      what setup-devbox writes into GITHUB_ENV per cache shape
hack/tagged-pins-cases.sh    the pin guard, against local git remotes
hack/leak-canary.sh          rule 2, mechanically
```

They need no network, no token and no cluster; each builds its own stub
and throws it away. `self-check.yaml` runs all of them under the `check`
context, which is **the whole merge gate** on this repository — a case
that is not run there is a case nobody runs.

## Merging

`check` green is the gate. No approving review, and no bypass actor for
anyone: there is no approval left to bypass, and the one thing a bypass
could still skip is `check` itself. Renovate opens a pull request like
anyone else and GitHub's auto-merge finishes it when the context goes
green.
