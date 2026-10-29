# s3-cache

`actions/cache` against the CI cache bucket the runner already reaches for
its Go build cache, instead of GitHub's cache. Whether it applies is decided
by what is configured (a `bucket`, then credentials the runner can use), never
by the kind of runner.

GitHub's cache is blob storage on the internet: on a self-hosted runner every restore
is downloaded through the NAT, every save uploaded through it. The bucket is
reached through an S3 gateway endpoint, so the same bytes cost nothing on the
NAT and stay inside the region.

```yaml
- uses: truvity/ci-actions/s3-cache/restore@<commit of a tag> # vX.Y.Z
  id: cache
  with:
    path: ~/.cache/golangci-lint
    key: golangci-${{ runner.os }}-${{ hashFiles('go.sum') }}
    restore-keys: |
      golangci-${{ runner.os }}-
    bucket: ${{ vars.CI_GOCACHE_S3_BUCKET }}   # the caller's own variable
    region: ${{ vars.CI_GOCACHE_S3_REGION }}

# ... the work ...

- uses: truvity/ci-actions/s3-cache/save@<commit of a tag> # vX.Y.Z
  if: always() && steps.cache.outputs.cache-hit != 'true'
  with:
    path: ~/.cache/golangci-lint
    key: golangci-${{ runner.os }}-${{ hashFiles('go.sum') }}
    bucket: ${{ vars.CI_GOCACHE_S3_BUCKET }}
    region: ${{ vars.CI_GOCACHE_S3_REGION }}
```

## What it does

- Same contract as `actions/cache`: the exact `key` first, then each
  `restore-keys` entry as a prefix (the NEWEST cache under the first prefix
  that has any), a saved key is immutable, a miss is not a failure.
- The payload is a tar stream compressed with zstd, streamed both ways (no
  temporary file), at
  `<prefix>/<repository>/<hash of the paths>/<key>.tar.zst` in the bucket.
  `prefix` defaults to `gha-cache`. The bucket's lifecycle rule ages objects
  out; nothing here deletes.
- With no `bucket`, or when the runner has no usable credentials (found out
  in one bounded lookup, 20 s at most), the same step runs the pinned
  `actions/cache/restore` / `actions/cache/save`. One workflow serves every
  kind of runner: a GitHub-hosted runner is simply one that is passed no
  bucket, or has no credentials for it.
- **Fail-open**: a bucket error is a `::warning::` and a miss, never a failed
  job.
- Credentials are the runner's default AWS chain (pod identity, or the
  profile its config names). The identity needs list/get/put on the prefix.
- On a runner that exports `CI_RUNNER_CACHE_WRITES=release-credentials` (its own
  identity only reads the bucket), restore uses the runner's identity only, and
  save uses the job's AWS keys (a role the job assumed). With no keys the save
  is skipped: a `::notice::`, output `skipped=true`, and GitHub's cache is not
  used in its place. The key check is a hint; the bucket's policy is the fence.

## Limits

- `path` is a list of files or directories, one per line. `~` works; globs do
  not.
- Entries are stored by absolute path, so a cache restores only where it was
  saved from (the same runner image and home directory), and only entries
  under the configured paths are ever extracted.
- Symlinks are not cached: they are left out when saving and never created
  when restoring (a tool rebuilds a link far more cheaply than a cache can
  vouch for one).
- Unlike GitHub's cache there is no scoping by branch: any job that can write
  the prefix can write any key. That is the trust model of the shared build
  cache next to it, not a stronger one.
- Hit/miss, size and time are printed by the step; `backend` says which store
  served it.
