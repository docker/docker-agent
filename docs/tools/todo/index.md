---
title: "Todo Tool"
description: "Task list management for complex multi-step workflows."
keywords: docker agent, ai agents, tools, toolsets, todo tool
linkTitle: "Todo"
weight: 170
canonical: https://docs.docker.com/ai/docker-agent/tools/todo/
---

## Overview

The todo tool provides task list management. Agents can create, update, list, and track progress on tasks with status tracking (pending, in-progress, completed).

## Available Tools

| Tool           | Description                              |
| -------------- | ---------------------------------------- |
| `create_todo`  | Create a new task                        |
| `create_todos` | Create multiple tasks at once            |
| `update_todos` | Update status of one or more tasks       |
| `list_todos`   | List all current tasks with their status |

### Task Statuses

| Status        | Description                  |
| ------------- | ---------------------------- |
| `pending`     | Task has not been started    |
| `in-progress` | Task is currently being done |
| `completed`   | Task is finished             |

## Configuration

```yaml
toolsets:
  - type: todo
```

### Options

| Property | Type             | Default | Description                                                                                  |
| -------- | ---------------- | ------- | -------------------------------------------------------------------------------------------- |
| `shared` | boolean / `team` | `false` | Which agents see the same todo list: only the declaring agent, the whole process, or one team |

### Shared Todos

In multi-agent setups, enable shared todos so all agents can see and update the same task list:

```yaml
toolsets:
  - type: todo
    shared: team
```

| Value   | Todo list is shared by                                                                                              |
| ------- | ------------------------------------------------------------------------------------------------------------------- |
| `false` | nothing: each agent has its own list                                                                                |
| `team`  | the agents of one loaded team: root, its sub-agents and background agents. Separate team loads, such as TUI tabs, each get their own list |
| `true`  | every agent in the process, across team loads. In the TUI, all tabs read and write the same list                    |

Todos live in memory only, so they are lost when the process exits.

`team` is scoped to a team load, not to a session. Sessions that reuse one loaded team, as `serve a2a` and `serve mcp` do, share that team's list.

Use `team` to keep separate TUI tabs from sharing todos. Use `true` only if todos should cross team loads.
