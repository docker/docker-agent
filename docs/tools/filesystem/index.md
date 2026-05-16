---
title: "Filesystem Tool"
description: "Read, write, list, search, and navigate files and directories."
permalink: /tools/filesystem/
---

# Filesystem Tool

_Read, write, list, search, and navigate files and directories._

## Overview

The filesystem tool gives agents the ability to explore codebases, read and edit files, create new files, search across files, and navigate directory structures. Paths are resolved relative to the working directory, though agents can also use absolute paths.

## Available Tools

| Tool                   | Description                                                               |
| ---------------------- | ------------------------------------------------------------------------- |
| `read_file`            | Read the complete contents of a file                                      |
| `read_multiple_files`  | Read several files in one call (more efficient than multiple `read_file`) |
| `write_file`           | Create or overwrite a file with new content                               |
| `edit_file`            | Make line-based edits (find-and-replace) in an existing file              |
| `list_directory`       | List files and directories at a given path                                |
| `directory_tree`       | Recursive tree view of a directory                                        |
| `create_directory`     | Create a new directory (creates parent directories as needed)             |
| `remove_directory`     | Remove an empty directory                                                 |
| `search_files_content` | Search for text or regex patterns across files                            |

## Configuration

```yaml
toolsets:
  - type: filesystem
```

### Options

| Property | Type | Default | Description |
| --- | --- | --- | --- |
| `ignore_vcs` | boolean | `true` | When `true` (default), `.git` directories and `.gitignore` patterns are excluded from listings and searches. Set to `false` to include them. |
| `post_edit` | array | `[]` | Commands to run after editing files matching a path pattern |
| `post_edit[].path` | string | — | Glob pattern for files (e.g., `*.go`, `src/**/*.ts`) |
| `post_edit[].cmd` | string | — | Command to run (use `${file}` for the edited file path) |
| `allow_list` | array | `[]` | Directories the tools may access. Empty = unrestricted (default). |
| `deny_list` | array | `[]` | Directories the tools must not access. Takes precedence over `allow_list`. |

### Path access control

By default the filesystem tools are unrestricted: relative paths resolve
from the working directory, but absolute paths and `..` traversals can
reach anywhere the agent process can. Configure `allow_list` and/or
`deny_list` to sandbox the toolset.

Entries in either list are expanded as follows:

- `"."` — the agent's working directory
- `"~"` or `"~/..."` — the user's home directory
- `"$VAR"` / `"${VAR}"` — environment variable expansion
- absolute paths — used as-is
- relative paths — anchored at the working directory

Symlinks are resolved before the containment check, so a symlink inside an
allowed root cannot be used to escape it. When an `allow_list` is set,
each entry is opened as a Go [`*os.Root`](https://pkg.go.dev/os#Root) so
that the kernel's rooted-lookup semantics also reject `..` and symlink
escapes at I/O time, not just at resolve time.

```yaml
toolsets:
  - type: filesystem
    # Restrict every operation to the working directory and the user's
    # home folder, then carve credentials out of the home folder.
    allow_list:
      - "."
      - "~"
    deny_list:
      - "~/.ssh"
      - "~/.aws"
```

When the path supplied by the agent is rejected, the tool returns a
structured error rather than performing any filesystem I/O. This makes the
restriction visible to the model so it can adjust its plan.

### Post-Edit Hooks

Automatically run formatting or other commands after file edits:

```yaml
toolsets:
  - type: filesystem
    ignore_vcs: false
    post_edit:
      - path: "*.go"
        cmd: "gofmt -w ${file}"
      - path: "*.ts"
        cmd: "prettier --write ${file}"
```

<div class="callout callout-tip" markdown="1">
<div class="callout-title">Tip
</div>
  <p>The filesystem tool resolves paths relative to the working directory. Agents can also use absolute paths.</p>
</div>
