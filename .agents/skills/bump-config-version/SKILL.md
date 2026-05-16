---
name: bump-config-version
description: Freeze the current `pkg/config/latest` package as a numbered, immutable `pkg/config/vN`, then bump `latest` to the next version so new features can land on the work-in-progress schema
---

# Bump Agent Config Version

The agent config schema is **versioned**. Each numbered package
(`pkg/config/v0`, `pkg/config/v1`, …, `pkg/config/vN`) is **frozen** and
must never change. New features always land on `pkg/config/latest`, which
holds the next, unreleased version.

This skill freezes whatever currently lives in `pkg/config/latest` as
`pkg/config/vN`, then re-opens `latest` for `vN+1` work. Run it whenever a
released version of the schema needs to be locked down before the next round
of breaking changes.

**Note**: Throughout this skill, `N` represents the current version number (e.g., `9`), 
and `vN` represents the version package name (e.g., `v9`). Replace these placeholders 
with the actual numbers when executing commands.

## 1. Determine the Current and Next Version

Read the version constant:

```sh
grep '^const Version' pkg/config/latest/types.go
```

It looks like `const Version = "N"`. Call this number `N`. The new latest
version will be `N+1`.

Sanity-check that `pkg/config/vN` does **not** already exist:

```sh
test ! -d pkg/config/vN && echo "ok"
```

If it exists, the freeze has already happened — stop and ask the user.

## 2. Create the Frozen `vN` Package

Copy every Go file from `latest/` into the new directory. This includes:
- All implementation files (`types.go`, `parse.go`, `auth.go`, `lifecycle.go`, `model_ref.go`, `validate.go`)
- All test files (`*_test.go`)

The new directory is created on the fly by `cp`:

```sh
cp -R pkg/config/latest pkg/config/vN
```

Rewrite the package name in all Go files (including tests) under the new directory:

```sh
# macOS / BSD sed requires -i '' (empty backup extension)
find pkg/config/vN -name '*.go' -exec sed -i '' 's/^package latest$/package vN/' {} +
```

Do **not** touch the `previous` import in `pkg/config/vN/parse.go`. It
already points at `vN-1` and that is exactly what the frozen `vN` upgrader
needs in order to migrate `vN-1 → vN`.

## 3. Re-open `latest` for `vN+1`

Three edits, all in `pkg/config/latest/`:

1. Bump the version constant in `types.go`:
   ```go
   const Version = "N+1"
   ```

2. Point the `previous` import in `parse.go` at the just-frozen package:
   ```go
   previous "github.com/docker/docker-agent/pkg/config/vN"
   ```

3. **If** `latest/parse.go`'s `upgradeIfNeeded` contains migration logic
   beyond the bare `types.CloneThroughJSON(old, &config)` (e.g. field
   renames or shape rewrites that converted `vN-1 → N`), that migration is
   already correctly preserved in `pkg/config/vN/parse.go` thanks to step 2.
   Reset `latest/parse.go`'s upgrader to the no-op `CloneThroughJSON` body
   so it starts fresh for `vN → vN+1`. If `latest/parse.go` was already a
   no-op, leave it alone.

## 4. Register the New Frozen Package

Edit `pkg/config/versions.go` to import and register `vN`. Both
modifications are inserted just before the `latest` lines so registrations
stay in numerical order:

```go
import (
    // ...
    vN_minus_1 "github.com/docker/docker-agent/pkg/config/vN-1"
    vN         "github.com/docker/docker-agent/pkg/config/vN"   // ← new
)

func versions() (...) {
    // ...
    vN_minus_1.Register(parsers, &upgraders)
    vN.Register(parsers, &upgraders)        // ← new
    latest.Register(parsers, &upgraders)
    // ...
}
```

## 5. Update `agent-schema.json`

The JSON schema is hand-maintained but tracked alongside the Go types.
Three small edits:

1. Update the top-level `description` field:
   ```json
   "description": "Configuration schema for Docker Agent vN+1"
   ```

2. Append `"N+1"` to the `enum` array under the top-level `version`
   property.

3. Append `"N+1"` to the `examples` array under the same property.

Keep the existing entries — older versions remain valid input.

After editing, validate the JSON is well-formed:

```sh
jq empty agent-schema.json
```

## 6. Validate

Run the project's standard validation chain. All must pass:

```sh
task build
task test
task lint
```

Pay special attention to:

- `pkg/config/schema_test.go` (the schema/Go-types matcher) — failures here
  usually mean step 5 was missed or `latest` types changed shape relative
  to what got frozen into `vN`.
- Any `pkg/config/*_test.go` that hard-codes `version: "N"` in YAML
  fixtures may need to be retargeted to `"N+1"` if the assertion is about
  the latest schema.

If anything fails, fix it and re-run until everything is green.

## 7. Commit

Match the convention used by previous freeze commits in the repo
(`Freeze v7`, `freeze config v8 and start v9 as latest`):

```sh
git add -A
git commit -m "freeze config vN and start vN+1 as latest" -m "" -m "Assisted-By: docker-agent"
```

## 8. Summary

Report back to the user with:

- The version that was frozen (`vN`).
- The new latest version (`vN+1`).
- The list of files created under `pkg/config/vN/`.
- The files that were modified (`pkg/config/latest/types.go`,
  `pkg/config/latest/parse.go`, `pkg/config/versions.go`,
  `agent-schema.json`).
- Confirmation that `task build`, `task test`, and `task lint` all
  succeeded.
