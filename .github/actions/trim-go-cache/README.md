# Trim Go build caches

`setup-go` registers this action after `actions/cache`. Its post step therefore
runs first: it removes unused inherited entries before the successful job saves
its cache. PRs remain restore-only. Module caches are unchanged.

Go keeps unused build entries for five days, which accumulates obsolete package
variants across commits. Before the job, `mark` ages inherited entries by two
hours. Go refreshes last-use mtimes at most hourly, so this forces cache hits to
be marked. After the job, `prune` retains touched or newly written entries.

The directory must be private to the job and idle during mark/prune. Entries are
opaque: no assumptions about archive contents or package names. Invalid names
and symlinks are ignored. A missed entry only causes a later cache miss. Bump
`build-cache-version` in `setup-go` to reset the build cache independently of the
module cache.

Test with `node --test .github/actions/trim-go-cache/go-cache.test.mjs`.
