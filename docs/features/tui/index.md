---
title: "Terminal UI (TUI)"
description: "docker-agent's default interface is a rich, interactive terminal UI with file attachments, themes, session management, and more."
permalink: /features/tui/
---

# Terminal UI (TUI)

_docker-agent's default interface is a rich, interactive terminal UI with file attachments, themes, session management, and more._

<div class="demo-container">
  <img src="{{ '/demo.gif' | relative_url }}" alt="docker-agent TUI in action showing an interactive agent session" loading="lazy">
</div>

## Launching the TUI

```bash
# Launch with a config
$ docker agent run agent.yaml

# Start with an initial message
$ docker agent run agent.yaml "Help me refactor this code"

# Auto-approve all tool calls
$ docker agent run agent.yaml --yolo

# Enable debug logging
$ docker agent run agent.yaml --debug
```

## Slash Commands

Type `/` during a session to see available commands, or press <kbd>Ctrl</kbd>+<kbd>K</kbd> for the command palette:

| Command            | Description                                                                          |
| ------------------ | ------------------------------------------------------------------------------------ |
| `/new`             | Start a new conversation                                                             |
| `/clear`           | Clear the current conversation (keep session, drop messages)                         |
| `/compact`         | Summarize and compact the conversation history                                       |
| `/fork`            | Fork the current session into a new branch                                           |
| `/copy`            | Copy the entire conversation to clipboard                                            |
| `/copy-last`       | Copy only the last assistant message to clipboard                                    |
| `/undo`            | Restore file changes from the latest snapshot (only when snapshots are enabled)      |
| `/snapshots`       | List captured snapshots (only when snapshots are enabled)                            |
| `/export`          | Export the session as HTML                                                           |
| `/sessions`        | Browse and load past sessions                                                        |
| `/model`           | Change the model for the current agent                                               |
| `/theme`           | Change the color theme                                                               |
| `/yolo`            | Toggle automatic tool call approval                                                  |
| `/title`           | Set or regenerate session title                                                      |
| `/attach`          | Attach a file to your message                                                        |
| `/shell`           | Open a shell                                                                         |
| `/star`            | Star/unstar the current session                                                      |
| `/cost`            | Show cost breakdown for this session                                                 |
| `/eval`            | Create an evaluation report                                                          |
| `/pause`           | Pause/resume the runtime loop after the current request                              |
| `/tools`           | Show every toolset (with lifecycle state) and the tools they expose                  |
| `/toolset-restart` | Force a supervisor-driven reconnect of the named toolset (`/toolset-restart <name>`) |
| `/permissions`     | Inspect and edit tool permission rules                                               |
| `/split-diff`      | Toggle split-diff view for file edits                                                |
| `/speak`           | Voice input via system speech-to-text (macOS only)                                   |
| `/exit`            | Exit the application (aliases: `/quit`, `/q`)                                        |

### Thinking and Tool Details

Reasoning/thinking blocks are collapsed by default. When collapsed, the TUI shows a short preview and compact tool summaries. Expand a block to see the full thinking content and the real tool renderers, including detailed tool output such as file edit diffs.

To start new sessions with thinking/tool blocks expanded by default, set `expand_thinking` in your user config:

```yaml
# ~/.config/cagent/config.yaml
settings:
  expand_thinking: true
```

Set it to `false` or omit it to keep the default collapsed behavior.

### Snapshots, `/undo`, and `/snapshots`

Enable shadow-git snapshots globally in `~/.config/cagent/config.yaml`:

```yaml
settings:
  snapshot: true
```

When enabled, docker-agent records filesystem snapshots at turn boundaries. The TUI exposes two slash commands that operate on those snapshots:

- **`/undo`** restores files from the most recent snapshot (one step back).
- **`/snapshots`** opens a dialog showing how many snapshots have been captured and the number of files in each one. Use <kbd>↑</kbd>/<kbd>↓</kbd> (or <kbd>j</kbd>/<kbd>k</kbd>) to highlight an entry, then press <kbd>r</kbd> to reset the workspace to that point. Pick `<original>` to revert every snapshot and bring the workspace back to its pre-agent state. <kbd>Esc</kbd> closes the dialog without changing anything.

Neither command removes messages from the session transcript — they only touch files on disk. Both commands (and the matching command-palette entries) are hidden when snapshots are turned off. Omit `snapshot` or set it to `false` to leave automatic snapshots off; agents can still configure snapshot hooks manually.

## File Attachments

Attach file contents to your messages using the `@` trigger:

1. Type `@` to open the file completion menu
2. Start typing to filter files (respects `.gitignore`)
3. Select a file to insert the reference

```bash
# In the chat input:
Explain what the code in @pkg/agent/agent.go does
```

The agent receives the full file contents in a structured `&lt;attachments&gt;` block, while the UI shows just the reference.

## Runtime Model Switching

Change the AI model during a session with `/model` or <kbd>Ctrl</kbd>+<kbd>M</kbd>:

1. Press <kbd>Ctrl</kbd>+<kbd>M</kbd> or type `/model`
2. Select from config models or type a custom `provider/model`
3. The model switch is saved with the session and restored on reload

<div class="callout callout-tip" markdown="1">
<div class="callout-title">Tip
</div>
  <p>Use model switching to try a more capable model for complex tasks, or a cheaper one for simple queries — without modifying your YAML config.</p>

</div>

## Editable Messages

Edit any previous user message to branch the conversation. Click on a past message to modify it — the agent will re-process from that point, while the original session history is preserved. This is great for exploring alternative approaches without losing your work.

## Session Management

docker-agent automatically saves your sessions. Use `/sessions` to browse past conversations:

- **Browse** past sessions with search and filtering
- **Star** important sessions with `/star`
- **Branch** conversations by editing any previous user message — preserving the original session history
- **Resume** sessions with `docker agent run config.yaml --session &lt;id&gt;`
- **Relative refs**: `--session -1` for the last session, `-2` for the one before

### Session Title Editing

Customize session titles to make them more meaningful and easier to find. By default, docker-agent auto-generates titles based on your first message, but you can override or regenerate them at any time.

**Using the `/title` command:**

```bash
/title                     # Regenerate title using AI (based on recent messages)
/title My Custom Title     # Set a specific title
```

**Using the sidebar:**

1. Click the pencil icon (✎) next to the session title in the sidebar
2. Type your new title
3. Press <kbd>Enter</kbd> to save, or <kbd>Escape</kbd> to cancel

<div class="callout callout-info" markdown="1">
<div class="callout-title">Note
</div>
  <p>Manually set titles are preserved and won’t be overwritten by auto-generation. Title changes are persisted immediately to the session.</p>

</div>

## Keyboard Shortcuts

| Shortcut   | Action                                          |
| ---------- | ----------------------------------------------- |
| Ctrl+K     | Open command palette                            |
| Ctrl+M     | Switch model                                    |
| Ctrl+R     | Reverse history search (search previous inputs) |
| Ctrl+G     | Cancel reverse history search                   |
| Ctrl+S     | Cycle to next agent in the team                 |
| Ctrl+1 – 9 | Switch directly to agent _N_ in the team list   |
| Ctrl+T     | Open a new tab (additional agent session)       |
| Ctrl+W     | Close the current tab                           |
| Ctrl+N     | Next tab                                        |
| Ctrl+P     | Previous tab                                    |
| Ctrl+B     | Toggle the sidebar (full-UI mode only)          |
| Ctrl+Y     | Toggle YOLO mode (auto-approve tool calls)      |
| Ctrl+O     | Toggle hide tool results                        |
| Ctrl+Z     | Suspend TUI to background (resume with `fg`)    |
| Ctrl+X     | Clear queued messages                           |
| Escape     | Cancel current operation                        |
| Enter      | Send message (or newline with Shift+Enter)      |
| Up/Down    | Navigate message history                        |

Press <kbd>Ctrl</kbd>+<kbd>H</kbd> to view the complete list of all available keyboard shortcuts.

## History Search

Press <kbd>Ctrl</kbd>+<kbd>R</kbd> to enter incremental history search mode. Start typing to filter through your previous inputs. Press <kbd>Enter</kbd> to select a match, or <kbd>Escape</kbd> to cancel.

## Theming

Customize the TUI appearance with built-in or custom themes:

```bash
# Switch themes interactively
/theme
```

### Built-in Themes

`default`, `catppuccin-latte`, `catppuccin-mocha`, `dracula`, `gruvbox-dark`, `gruvbox-light`, `nord`, `one-dark`, `solarized-dark`, `tokyo-night`

### Custom Themes

Create theme files in `~/.cagent/themes/` as YAML. Theme files are **partial overrides** — you only need to specify the colors you want to change. Any omitted keys fall back to the built-in default theme values.

```yaml
# ~/.cagent/themes/my-theme.yaml
name: "My Custom Theme"

colors:
  # Backgrounds
  background: "#1a1a2e"
  background_alt: "#16213e"

  # Text colors
  text_bright: "#ffffff"
  text_primary: "#e8e8e8"
  text_secondary: "#b0b0b0"
  text_muted: "#707070"

  # Accent colors
  accent: "#4fc3f7"
  brand: "#1d96f3"

  # Status colors
  success: "#4caf50"
  error: "#f44336"
  warning: "#ff9800"
  info: "#00bcd4"

# Optional: Customize syntax highlighting colors
chroma:
  comment: "#6a9955"
  keyword: "#569cd6"
  literal_string: "#ce9178"

# Optional: Customize markdown rendering colors
markdown:
  heading: "#4fc3f7"
  link: "#569cd6"
  code: "#ce9178"
```

### Applying Themes

**In user config** (`~/.config/cagent/config.yaml`):

```yaml
settings:
  theme: my-theme # References ~/.cagent/themes/my-theme.yaml
```

**At runtime:** Use the `/theme` command to open the theme picker and select from available themes. Your selection is saved globally in `~/.config/cagent/config.yaml` under `settings.theme` and persists across sessions.

<div class="callout callout-tip" markdown="1">
<div class="callout-title">Hot Reload
</div>
  <p>Custom themes auto-reload when you save changes to the file — no restart needed. This makes it easy to tweak colors in real-time.</p>

</div>

<div class="callout callout-warning" markdown="1">
<div class="callout-title">Partial overrides
</div>
  <p>All user themes are applied on top of the <code>default</code> theme. If you want to customize a built-in theme (e.g., <code>dracula</code>), copy its full YAML from the <a href="https://github.com/docker/docker-agent/tree/main/pkg/tui/styles/themes">built-in themes on GitHub</a> into <code>~/.cagent/themes/</code> and edit the copy. Otherwise, omitted values will use <code>default</code> colors, not the original theme's colors.</p>

</div>

## Tool Permissions

When an agent calls a tool, docker-agent shows a confirmation dialog by default. You can:

- **Approve once** — Allow this specific call
- **Always allow** — Permanently approve this tool/command for the session
- **Deny** — Reject the tool call

**Granular permissions:** The permission system supports pattern-based matching. When you “Always allow” a specific tool command, only that exact pattern is auto-approved — other commands from the same tool still require confirmation. This lets you auto-approve safe, read-only operations while maintaining control over destructive ones.

<div class="callout callout-tip" markdown="1">
<div class="callout-title">YOLO mode
</div>
  <p>Use <code>--yolo</code> or the <code>/yolo</code> command to auto-approve all tool calls. You can also toggle this mid-session. For aliases, set <code>--yolo</code> when creating the alias: <code>docker agent alias add fast agentcatalog/coder --yolo</code>.</p>

</div>
