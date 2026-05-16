---
name: bump-go-dependencies
description: Update direct Go module dependencies one by one, validating each bump with tests and linter, committing individually, and producing a summary table for a PR description
---

# Bump Direct Go Dependencies

When asked to update or bump Go dependencies, follow this procedure.

## 1. List Outdated Direct Dependencies

Run the following to get a list of direct dependencies that have newer versions available:

```sh
go list -m -u -json all 2>/dev/null | jq -r 'select(.Indirect != true and .Update != null) | "\(.Path) \(.Version) \(.Update.Path) \(.Update.Version)"'
```

This produces lines of the form:

```
module/path current_version update_path new_version
```

If the command produces no output, all direct dependencies are already up to date. Inform the user and stop.

## 2. Update Each Dependency One by One

For **each** outdated dependency, perform the following steps in order:

### a. Upgrade

```sh
go get <module_path>@<new_version>
```

### b. Tidy

```sh
go mod tidy
```

### c. Validate

Run the linter and the tests:

```sh
task lint
task test
```

### d. Decide

- **If both pass**: stage and commit the changes:
  ```sh
  git add -A
  git commit -m "bump <module_path> from <old_version> to <new_version>" -m "" -m "Assisted-By: docker-agent"
  ```
  Record the dependency as **bumped** in your tracking table.

- **If either fails**: revert all changes and move on:
  ```sh
  git checkout -- .
  ```
  Record the dependency as **skipped** in your tracking table, noting the reason (lint failure, test failure, or both).

## 3. Produce a Summary Table

After processing every dependency, output a **copy-pastable** Markdown table inside a fenced code block.
The table must list every dependency that was considered, with columns for the module path, old version, new version, and status.
Don't use emojis, just plain markdown.

Example:

~~~
```markdown
| Module | From | To | Status |
|--------|------|----|--------|
| github.com/example/foo | v1.2.0 | v1.3.0 | bumped |
| github.com/example/bar | v0.4.1 | v0.5.0 | skipped — test failure |
| golang.org/x/text | v0.21.0 | v0.22.0 | bumped |
```
~~~

This table is meant to be pasted directly into a pull-request description.
