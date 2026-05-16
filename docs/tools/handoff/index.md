---
title: "Handoff Tool"
description: "Hand off the active conversation to another local agent defined in the same config."
permalink: /tools/handoff/
---

# Handoff Tool

_Hand off the active conversation to another local agent defined in the same config._

## Overview

The `handoff` tool lets an agent transfer control of the **current conversation** to another agent in the **same config file**. Unlike [`transfer_task`]({{ '/tools/transfer-task/' | relative_url }}), which delegates a sub-task and collects the result, `handoff` rewires the session so the receiving agent continues the conversation directly with the user.

This is the core mechanism for **handoffs routing** — a pattern where a router agent classifies the user's request and hands it off to a specialist, which then owns the rest of the session.

<div class="callout callout-info" markdown="1">
<div class="callout-title">Local only
</div>
  <p>The <code>handoff</code> tool only targets agents declared in the <strong>same</strong> config file by their local name. It does <strong>not</strong> open network connections. To delegate to a remote agent over the network, use the <a href="{{ '/tools/a2a/' | relative_url }}">A2A toolset</a> instead.</p>

</div>

## Configuration

The tool is enabled implicitly when an agent declares a non-empty `handoffs:` list. You do **not** add `- type: handoff` under `toolsets:` — it is not a toolset type.

```yaml
agents:
  router:
    model: openai/gpt-4o
    description: Routes questions to the right specialist
    instruction: |
      Classify the user's question and hand off to the most appropriate
      specialist. If unsure, ask a clarifying question first.
    handoffs: [billing, support]

  billing:
    model: openai/gpt-4o
    description: Billing specialist
    instruction: Answer billing questions.

  support:
    model: openai/gpt-4o
    description: Technical support specialist
    instruction: Help with technical issues.
```

The router agent automatically gets a `handoff` tool it can call to switch the conversation to `billing` or `support`.

## Tool Interface

The `handoff` tool takes a single parameter:

| Parameter | Type   | Required | Description                                                       |
| --------- | ------ | -------- | ----------------------------------------------------------------- |
| `agent`   | string | ✓        | The local name of the agent to hand off the conversation to.      |

Only names listed in the current agent's `handoffs:` field are valid targets.

<div class="callout callout-tip" markdown="1">
<div class="callout-title">See also
</div>
  <p>For sub-task delegation (caller stays in control, waits for the result), see <a href="{{ '/tools/transfer-task/' | relative_url }}">Transfer Task</a>. For remote agent connections over the network, see the <a href="{{ '/tools/a2a/' | relative_url }}">A2A toolset</a>. For the broader pattern, see <a href="{{ '/concepts/multi-agent/#handoffs-routing' | relative_url }}">Handoffs Routing</a>.</p>

</div>
