# cluster

One end-to-end test cluster, two tiers, behind one set of outputs.

Both a public repository and a private one run the same integration
suite against a real cluster; only *which* cluster differs. A public
repository's pull requests come from forks, so it gets a disposable kind
box, stood up from nothing, on the runner itself. A private repository's
own CI identity can be trusted with a shared development cluster, so it
uses that instead. This action is the seam: whichever tier runs, the rest
of the job reads the same five variables and does not know which one it
is on.

```yaml
- uses: truvity/ci-actions/cluster@f0c56df8cb20d13c6ebd350f3e5f62d6dfbd4e08 # v1.1.0
  with:
    mode: kind                 # or: shared
    policy-version: v0.8.1     # kind only
    namespace: e2e
# ... build, install, migrate, test, reading $KUBECONFIG, $SNAPSHOT_REGISTRY,
# $GEMAAL_TIER, $GEMAAL_NAMESPACE, $GEMAAL_RELEASE either way
```

truvity/ci-workflows' `integration.yaml` picks the tier from the
calling repository's visibility and calls this action on its kind tier
(`mode: kind` with `background: true`, then `mode: wait`). Its shared
tier still runs its own inline steps, so `mode: shared` has no caller in
ci-workflows today.

## Which mode is neutral

**`mode: kind` is the neutral one.** It needs nothing from any estate: a
hosted runner, a container runtime and a public release of
truvity/policy. Any repository, in any organisation, can use it.

**`mode: shared` assumes the Truvity shape.** It expects:

- a repo-relative kubeconfig whose exec plugin is `accessctl` (from
  [truvity/access-roster](https://github.com/truvity/access-roster)),
  exchanging the job's GitHub token for a cluster credential;
- a repo-relative `aws.ini` whose credential process does the same for
  AWS;
- container registries on Amazon ECR, logged into with
  `aws-actions/amazon-ecr-login`.

None of those particulars live in this action (account ids, hostnames,
cluster names and namespaces all arrive as inputs), but the mechanism is
that one. An estate with a different credential path uses `mode: kind`,
or its own step in place of this action.

## Inputs

| input | mode | meaning |
| -- | -- | -- |
| `mode` | all | `kind`, `shared`, or `wait` (block on an earlier `background: true` launch) |
| `policy-version` | kind | truvity/policy release tag whose `hack/kind/` box to run; required |
| `namespace` | kind, shared | namespace the caller installs into; default `e2e` |
| `release` | kind, shared | release name; default `<job>-r<run id>-a<run attempt>` |
| `background` | kind | `true` returns immediately; call again with `mode: wait` |
| `state-dir` | kind, wait | where a background launch keeps its state; the default is fixed for the job |
| `expected-identity` | shared | what `kubectl auth whoami` must report; required |
| `kubeconfig` | shared | repo-relative kubeconfig; required |
| `aws-config-file` | shared | repo-relative `aws.ini`; required |
| `ecr-registries` | shared | comma-separated registry account ids to log into; empty skips |

Outputs, each also exported as an environment variable: `kubeconfig`
(`KUBECONFIG`), `snapshot-registry` (`SNAPSHOT_REGISTRY`), `gemaal-tier`
(`GEMAAL_TIER`), `gemaal-namespace` (`GEMAAL_NAMESPACE`),
`gemaal-release` (`GEMAAL_RELEASE`).

## `mode: kind`

Fetches [truvity/policy](https://github.com/truvity/policy)'s
`hack/kind/` box (its kind cluster, CloudNativePG, NATS with JetStream,
Gateway API CRDs, an S3 stand-in and a local image registry) at the
exact release tag `policy-version` names, so the box stays owned by
truvity/policy and a change to it ships through policy's own release
rather than through this repository. It needs kind, kubectl and helm,
which this action installs itself at pinned versions, never from the
caller's devbox: `mode: kind` exists so a repository with no devbox at
all can run the suite, and policy's own devbox pins these three to
"latest", so there is no version contract to inherit from it. The
kubeconfig it exports is an explicit file under `$RUNNER_TEMP`, never
`~/.kube/config`, so a job's other kubectl or helm calls are never
repointed by this one running.

**`SNAPSHOT_REGISTRY` is the box's own registry, not something this
action stands up.** `hack/kind/up.sh` already runs kind's [documented
local registry recipe](https://kind.sigs.k8s.io/docs/user/local-registry/)
on `localhost:5001` (`hack/kind/versions.env`'s `REGISTRY_PORT`), wires
it into every node's containerd, and `hack/kind/verify.sh` proves a push
and a pull through it. This action creates no second registry; it checks
the box's claim after `up.sh` returns and **fails loudly, naming the
pinned version,** if that release does not publish one on 5001. A
version that does not hold the contract is a version not to pin.

**`background: true` / `mode: wait`.** Standing the box up costs a few
minutes even on a hosted runner. `background: true` starts it and returns
immediately; the outputs are already set, because they are paths and
fixed strings known before the cluster exists, so the same job can build
its images while the box comes up. A later step calls this action again
with `mode: wait` (no other inputs: it finds the launch by `state-dir`)
to block until the box is ready and re-emit the same outputs. Using the
cluster without `wait` races the box.

## `mode: shared`

**Refuses a fork pull request outright**, before resolving a kubeconfig,
reading an `aws.ini` or attempting a registry login. The check reads
`$GITHUB_EVENT_PATH` directly, not a caller-supplied input, so it cannot
be defeated by passing the wrong value. Then it resolves `kubeconfig`
and `aws-config-file` (repo-relative, the same files a laptop uses),
proves `kubectl auth whoami` reports `expected-identity` (three tries,
stderr kept, so a wrong identity fails before anything is built rather
than as an RBAC error later), and logs into `ecr-registries` if given.
`SNAPSHOT_REGISTRY` is the first registry
[`amazon-ecr-login`](https://github.com/aws-actions/amazon-ecr-login)
logged into. Never call this action twice in parallel within a job in
this mode: each identity exchange spends a one-use token.

**This mode installs nothing.** `kubectl`, and the exec plugin the
kubeconfig names, must already be on `PATH`, normally from the caller's
own devbox via `setup-devbox`. A private repository's own CI identity is
what this mode trusts, and that identity's devbox is part of what it
trusts.

## The variables

`GEMAAL_TIER` is `kind` or `shared`, never unset.
[gemaal's `harness.DetectTier`](https://github.com/truvity/gemaal)
(`pkg/harness/tier.go`) treats `kind` specially and every other value as
the shared-cluster behaviour, so `shared` is what a caller that left it
unset already got. `GEMAAL_RELEASE` defaults to
`<job>-r<run id>-a<run attempt>` in both modes, so parallel jobs and
re-runs never collide over a release name.

## Tests

The Go table tests (`internal/cluster`, inside `check`) run the fork
refusal against fake event payloads, and every step against a fake box,
`docker`, `kubectl` and filesystem. `self-check.yaml`'s separate `cluster-kind` job
stands up a real box from a pinned policy release, with the
background/wait pattern, and proves the exported `KUBECONFIG` and the
registry answer. It needs network and a container runtime and takes
minutes, so it is not part of `check` and is not required.
