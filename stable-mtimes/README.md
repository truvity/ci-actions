# stable-mtimes

Gives every tracked file of a checkout a deterministic modification time
derived from its content, so that Go's test cache can hit in CI.

```yaml
- uses: actions/checkout@<sha>
- uses: truvity/ci-actions/stable-mtimes@<commit of a tag> # vX.Y.Z
```

## Why

Go caches a test result keyed on, among other things, the **size and
modification time** of every file the test opened (`cmd/go/internal/test`,
`hashOpen`, which hashes the file's stat, not its content). It also refuses to
cache anything that touched a file modified within the last couple of seconds.
`actions/checkout` stamps every file with "now", so every test that reads a
repository file (a golden, a fixture, a schema) is a cache miss in CI, even
when the content is byte-identical to the last run and the result is sitting
in the shared `GOCACHEPROG` cache.

## What it does

In the working directory (default: the workspace root) it reads
`git ls-files -s -z` and, in-process (no subprocess per file):

- **regular files**: atime = mtime = `2000-01-01T00:00:00Z` + (the first 8
  bytes of the blob id as a big-endian unsigned integer, modulo
  20 * 365 * 24 * 3600) seconds. Same content, same time, on every runner and
  every branch; changed content, a different time.
- **symlinks**: the link's own time is set to the same function of the link
  blob (`utimensat` with `AT_SYMLINK_NOFOLLOW`). Where the platform cannot do
  that, the link is skipped and counted.
- **directories**: the root and every directory that contains tracked files
  are set to the constant `2000-01-01T00:00:00Z`. Go hashes a directory
  listing by its names, so a constant time is safe.
- **skipped**: gitlinks (submodules, mode 160000) and paths that do not exist
  on disk (sparse checkouts).

It prints one line, for example
`stable-mtimes: 9876 files, 12 symlinks, 640 directories set; 1 skipped, 0 warnings`.
A file it cannot touch is a warning, not a failure; only an unexpected error
(git cannot list the files) fails the step. Running it twice changes nothing.
It takes about a second for 10k files.

## Trade-off

Timestamps stop meaning "when this was checked out". A build tool that orders
work by timestamps (`make` and friends) may judge a committed generated file
newer than the source it was generated from, and skip regenerating it, when
the source changed. A repository whose CI relies on that ordering must not use
this action; `ci-workflows`' `check` workflow offers `stable-mtimes: false`
for it.

Git's own index caches stat data, so after this runs `git status` re-hashes
files once; the result is the same.

## Inputs

| input | default | meaning |
| -- | -- | -- |
| `working-directory` | workspace root | the checkout to rewrite |
