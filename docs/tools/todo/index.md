---
title: "Todo Tool"
description: "Task list management for complex multi-step workflows."
keywords: docker agent, ai agents, tools, toolsets, todo tool
linkTitle: "Todo"
weight: 170
canonical: https://docs.docker.com/ai/docker-agent/tools/todo/
---

_Task list management for complex multi-step workflows._

## Overview

The todo tool provides task list management. Agents can create, update, list, and track progress on tasks with status tracking (pending, in-progress, completed). Useful for complex multi-step workflows where the agent needs to stay organized and ensure all steps are completed.

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

| Value            | Todo list is shared by                                                                                       |
| ---------------- | ------------------------------------------------------------------------------------------------------------ |
| `false`          | nothing: each agent has its own list                                                                          |
| `team`           | the agents of one team: root, its sub-agents and background agents. Each session or tab has its own list     |
| `true`           | every agent in the process, across sessions. In the TUI, all tabs read and write the same list                |

Todos live in memory only, so they are lost when the process exits.

Use `team` unless you want todos to cross session boundaries.
