---
title: "Think Tool"
description: "Step-by-step reasoning scratchpad for planning and decision-making."
permalink: /tools/think/
---

# Think Tool

_Step-by-step reasoning scratchpad for planning and decision-making._

## Overview

The think tool is a reasoning scratchpad that lets agents think step-by-step before acting. The agent can write its thoughts without producing visible output to the user — ideal for planning complex tasks, breaking down problems, and reasoning through multi-step solutions.

This is a lightweight tool with no side effects. It is most useful for models that lack built-in reasoning or thinking capabilities (e.g., smaller or older models). For models that already support native thinking — such as Claude with extended thinking, OpenAI o-series, or Gemini with a thinking budget — this tool is unnecessary since the model can reason internally.

## Configuration

```yaml
toolsets:
  - type: think
```

No configuration options.

<div class="callout callout-tip" markdown="1">
<div class="callout-title">When to use
</div>
  <p>Use the think tool with models that don't have native reasoning capabilities. If your model already supports a <a href="{{ '/configuration/models/#thinking-budget' | relative_url }}">thinking budget</a>, you likely don't need this tool.</p>
</div>
