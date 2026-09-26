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
| [`cluster`](cluster/) | Stands up the end-to-end test cluster — a disposable kind box or the estate's shared cluster — behind one identical set of outputs |

## `cluster`: one end-to-end cluster, two tiers

Both a public repository and a private one run the same integration suite
against a real cluster; only *which* cluster differs. A public
repository's pull requests come from forks, so it gets a disposable kind
box, stood up from nothing, on the runner itself. A private repository's
own CI identity can be trusted with the estate's shared development
cluster, so it uses that instead. `cluster/` is the seam: whichever tier
runs, the rest of the job reads the same five things and does not know
which one it is on.

```yaml
- uses: truvity/ci-actions/cluster@<sha> # vX.Y.Z
  with:
    mode: kind                 # or: shared
    policy-version: v0.8.0     # kind only
    namespace: e2e
# ... build, install, migrate, test — reading $KUBECONFIG, $SNAPSHOT_REGISTRY,
# $GEMAAL_TIER, $GEMAAL_NAMESPACE, $GEMAAL_RELEASE either way
```

**`mode: kind`.** Fetches [truvity/policy](https://github.com/truvity/policy)'s
`hack/kind/` box — its kind cluster, CloudNativePG, NATS with JetStream,
Gateway API CRDs, an S3 stand-in and a local image registry — at the
exact release tag `policy-version` names, so the box stays owned by
truvity/policy and a change to it ships through policy's own release
rather than through this repository. It needs kind, kubectl and helm,
which this action installs itself at pinned versions — never the
caller's devbox: mode: kind exists precisely so a repository with no
devbox at all can run the suite, and policy's own devbox pins these
three to "latest" anyway, so there is no version contract to inherit
from it. The kubeconfig it exports is an explicit file under
`$RUNNER_TEMP`, never the default `~/.kube/config` — a job's other
kubectl or helm calls (mode: shared, elsewhere in the same workflow)
must never be repointed by this one running.

**`SNAPSHOT_REGISTRY` is the box's own registry, not something this
action stands up.** hack/kind/up.sh already runs kind's [documented local
registry recipe](https://kind.sigs.k8s.io/docs/user/local-registry/) on
`localhost:5001` (`hack/kind/versions.env`'s `REGISTRY_PORT`), wires it
into every node's containerd, and hack/kind/verify.sh already proves a
push and a pull through it. The box is the one owner of what a kind lane
provides, so this action does not create a second registry or any other
infrastructure the box did not ask for — it only checks the box's own
claim after `up.sh` returns, and **fails loudly, naming the pinned
version,** if that particular release does not publish one on 5001. A
version this action's `SNAPSHOT_REGISTRY` contract does not hold for is a
version to not pin, not a version to work around.

**`background: true` / `mode: wait`.** Standing the box up costs a few
minutes even on a hosted runner with nothing cached. Passing
`background: true` starts it and returns immediately — the outputs are
already set, because they are paths and fixed strings known before the
cluster exists, not anything the box computes — so the same job can build
its images while the box comes up. A later step calls this action again
with `mode: wait` (no other inputs — it finds the earlier launch by
`state-dir`, which defaults to a fixed path under `$RUNNER_TEMP` for the
whole job) to block until the box is ready and re-emit the same outputs.
Skipping `wait` and using the cluster immediately after a `background: true`
launch races the box.

**`mode: shared`.** **Refuses a fork pull request outright**, before
resolving a kubeconfig, reading an `aws.ini` or attempting an ECR login —
checked against `$GITHUB_EVENT_PATH` directly, not a caller-supplied
input, so it cannot be defeated by passing the wrong value. Then resolves
`kubeconfig` and `aws-config-file` (repo-relative, the same files a laptop
uses — their exec plugin/credential process exchanges this job's GitHub
token for a cluster/AWS credential), proves `kubectl auth whoami` reports
`expected-identity` (three tries, stderr kept — a wrong identity fails
before anything is built, not as a mysterious RBAC error later), and logs
into `ecr-registries` if given. `SNAPSHOT_REGISTRY` is the first registry
[`amazon-ecr-login`](https://github.com/aws-actions/amazon-ecr-login)
actually logged into. Never call this action twice in parallel within a
job in this mode: each identity exchange spends a one-use token.

**This mode assumes a toolchain, deliberately.** Unlike mode: kind, this
action installs nothing for mode: shared — `kubectl` (and the exec plugin
a repo-relative kubeconfig names, e.g. `accessctl`) must already be on
`PATH`, the same way truvity/ci-workflows' `integration.yaml` runs it
today, normally from the caller's own devbox via `setup-devbox`. A
private repository's own CI identity is who mode: shared trusts, and that
identity's devbox is part of what it trusts.

`GEMAAL_TIER` is `kind` or `shared` (a fixed string, not left unset):
[gemaal's `harness.DetectTier`](https://github.com/truvity/gemaal)
(`pkg/harness/tier.go`, v0.24.0) reads `$GEMAAL_TIER` "when set (any
value — `TierKind` is the only one the harness treats specially today)",
so any non-`kind` value — `shared` included — falls back to exactly the
shared-cluster behaviour every existing caller already gets from leaving
it unset. `GEMAAL_RELEASE` defaults to `<job>-r<run id>-a<run attempt>`
in both modes, matching truvity/ci-workflows' `integration.yaml`, so
parallel jobs and re-runs never collide over a release name.

No organisation particulars live in this action, in either mode: account
ids, hostnames, cluster names and namespaces all arrive as inputs.

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
hack/fork-guard-cases.sh     cluster's fork refusal, against fake event payloads
hack/leak-canary.sh          rule 2, mechanically
```

They need no network, no token and no cluster; each builds its own stub
and throws it away. `self-check.yaml` runs all of them under the `check`
context, which is **the whole merge gate** on this repository — a case
that is not run there is a case nobody runs.

`cluster`'s `mode: kind` is the one exception: proving it needs a real
kind cluster and a container runtime, which `check` deliberately does not
carry. `self-check.yaml`'s separate `cluster-kind` job runs it end to end
— a real release tarball, a real cluster, the background/wait pattern —
but is not part of `check` and is not required.

## Merging

`check` green is the gate. No approving review, and no bypass actor for
anyone: there is no approval left to bypass, and the one thing a bypass
could still skip is `check` itself. Renovate opens a pull request like
anyone else and GitHub's auto-merge finishes it when the context goes
green.
