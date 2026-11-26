---
title: "State Directories"
description: "Where Docker Agent keeps its config, data and cache on Linux, macOS and Windows: overrides, XDG variables, OS-native defaults, and why an existing ~/.cagent install stays where it is."
keywords: docker agent, ai agents, configuration, xdg, directories, data directory, config directory, cache directory
linkTitle: "State Directories"
weight: 115
canonical: https://docs.docker.com/ai/docker-agent/configuration/directories/
---

Docker Agent keeps its files in three directories. Other pages refer to them as `<config-dir>`, `<data-dir>` and `<cache-dir>`.

| Directory | Holds |
| --- | --- |
| Config | `config.yaml` (user settings and aliases), the anonymous user id, MCP OAuth tokens, sandbox tokens |
| Data | `session.db`, prompt `history`, `cagent.debug.log`, installed tools, themes, memory, plans, worktrees, snapshots |
| Cache | Data that can be rebuilt, such as the model catalog and remote skills |

## Where Each Directory Lives

Each directory is resolved on its own, in this order:

1. **An explicit override.** See [Overriding a Directory](#overriding-a-directory).
2. **The XDG or OS-native location, if Docker Agent already adopted it.**
3. **The legacy location, if it holds any file.**
4. **The XDG or OS-native location.** `$XDG_CONFIG_HOME/cagent`, `$XDG_DATA_HOME/cagent` or `$XDG_CACHE_HOME/cagent` when the variable is set to an absolute path, on every platform. Otherwise the OS-native default in the table below.

| | Linux | macOS | Windows | Legacy location |
| --- | --- | --- | --- | --- |
| Config | `~/.config/cagent` | `~/Library/Application Support/cagent` | `%AppData%\cagent` | `~/.config/cagent` |
| Data | `~/.local/share/cagent` | `~/Library/Application Support/cagent` | `%LocalAppData%\cagent` | `~/.cagent` |
| Cache | `~/.cache/cagent` | `~/Library/Caches/cagent` | `%LocalAppData%\cagent` | Same as the OS-native default |

A relative `XDG_*_HOME` value is ignored, as the XDG Base Directory Specification requires.

## Existing Installations Stay in Place

Docker Agent never moves, copies, merges or deletes your files. If the legacy location of a directory holds anything at all, Docker Agent keeps using it.

- **Any file counts.** A single old `cagent.debug.log`, a `.DS_Store` or an empty subdirectory keeps you on the legacy location. Docker Agent does not guess which files matter.
- **Each directory is decided separately.** Your data can stay in `~/.cagent` while a config directory that never held anything moves to the new location.
- **A legacy location wins over an XDG variable.** Setting `XDG_DATA_HOME` does not move a `~/.cagent` that still holds files.

To switch to the new location, quit Docker Agent and move the directory yourself. On Linux, for example, when `~/.local/share/cagent` does not exist yet:

```bash
$ mv ~/.cagent ~/.local/share/cagent
```

## Adopted Locations

The first time Docker Agent uses an XDG or OS-native location instead of a legacy one, it writes an empty marker file there: `.cagent-root-config`, `.cagent-root-data` or `.cagent-root-cache`. From then on that location wins, even if an older version recreates `~/.cagent`. Leave the marker files in place.

## Changing an XDG Variable Later

Docker Agent resolves the directories on every start. If you change `XDG_DATA_HOME` after you have sessions, the data directory becomes a new, empty one and your sessions stay behind in the old location.

When the OS-native location holds state and a variable now points elsewhere, Docker Agent prints a warning the first time. It cannot detect state left under a previous value of the variable. Move the directory yourself, or unset the variable, to get your state back.

## Downgrading

Versions that predate this layout only read `~/.cagent` and `~/.config/cagent`. After a downgrade they do not see sessions or settings stored in the new locations, and they create `~/.cagent` again. A newer version then keeps using the adopted location and ignores the recreated `~/.cagent`. Run with `--debug` to see this reported in the debug log.

## Overriding a Directory

| Directory | Flag | Environment variable |
| --- | --- | --- |
| Config | `--config-dir <path>` | `DOCKER_AGENT_CONFIG_DIR` (legacy: `CAGENT_CONFIG_DIR`) |
| Data | `--data-dir <path>` | `DOCKER_AGENT_DATA_DIR` |
| Cache | `--cache-dir <path>` | None |

A flag takes precedence over its environment variable. An override is used as given: no marker is written and no legacy location is consulted.

`DOCKER_AGENT_TOOLS_DIR` moves only the installed tools, which otherwise live in `<data-dir>/tools/`.

## Special Cases

> [!WARNING]
> **Windows: the config directory roams**
>
> `%AppData%` is the roaming profile folder, so the config directory can follow you between machines on a domain. It holds MCP OAuth tokens and sandbox tokens. To keep them on one machine, set `DOCKER_AGENT_CONFIG_DIR` once instead of passing `--config-dir` on every run:
>
> ```powershell
> [Environment]::SetEnvironmentVariable('DOCKER_AGENT_CONFIG_DIR', "$env:LOCALAPPDATA\cagent-config", 'User')
> ```
>
> Tokens already stored in the roaming directory do not follow; sign in again, or move the files yourself.

- **Read-only home directory.** Common in containers and CI. Docker Agent still runs, but anything that writes state fails. Point `--data-dir`, `--config-dir` and `--cache-dir` at writable paths.
- **No home directory.** Docker Agent falls back to `.cagent`, `.cagent-config` and `.cagent-cache` under the system temporary directory. That directory can be shared with other users, so set the overrides instead.
- **A legacy location that cannot be read.** Docker Agent keeps using it and prints a warning, so that a permissions problem never hides your data. This happens when `~/.cagent` is owned by another user, for example after running Docker Agent with `sudo`. Fix the ownership to clear the warning.
