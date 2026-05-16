# Elicitation Testing Example

This example demonstrates how MCP (Model Context Protocol) elicitation works in docker-agent.
Elicitation allows MCP tools to request additional input from the user during tool execution.

## What is Elicitation?

Elicitation is an MCP feature that enables tools to interactively prompt users for information
that wasn't provided in the initial tool call. For example:
- Asking for confirmation before destructive actions
- Requesting missing required parameters
- Gathering additional details when needed

## Files in this Example

- `agent.yaml` - The agent configuration that uses the elicitation MCP server
- `server.py` - A Python MCP server that demonstrates various elicitation patterns
- `README.md` - This documentation

## Prerequisites

You need `uvx` (part of [uv](https://docs.astral.sh/uv/)) installed:

```bash
# Install uv (includes uvx)
curl -LsSf https://astral.sh/uv/install.sh | sh
```

The MCP SDK will be automatically installed when the server starts via `uvx`.

## Running the Example

```bash
# From the docker-agent root directory
$ docker agent run examples/elicitation/agent.yaml
```

## Elicitation Scenarios

The example MCP server provides several tools that demonstrate different elicitation patterns:

### 1. Simple Confirmation (`confirm_action`)
Shows a basic yes/no confirmation dialog before performing an action.

**Example prompt:** "Confirm that I want to delete the database"

### 2. Form Input (`create_user`)
Demonstrates a multi-field form with validation:
- Required string fields (username, email, role)
- Optional fields (bio)
- Email format validation
- Enum/choice fields (role selection)
- Boolean fields (active status)

**Example prompt:** "Create a new user"

### 3. Numeric Input (`configure_settings`)
Shows number fields with min/max validation. Supports presets.

**Example prompt:** "Configure settings with performance preset"

### 4. Boolean Toggles (`setup_preferences`)
Demonstrates multiple boolean fields with default values.

**Example prompt:** "Set up my preferences"

### 5. Enum Selection (`select_option`)
Shows multiple enum/dropdown fields for making selections.

**Example prompt:** "Help me select deployment options"

## How Elicitation Works

1. Agent decides to call an MCP tool
2. Tool execution starts
3. Tool sends an elicitation request with a message and JSON schema
4. docker-agent displays an interactive dialog based on the schema
5. User fills in the form and submits (or cancels)
6. Response is sent back to the tool
7. Tool continues execution with the provided data

## Schema Field Types Supported

- `string` - Text input (with optional format: email, uri, date, etc.)
- `integer` - Whole number input
- `number` - Decimal number input
- `boolean` - Yes/No toggle
- `enum` - Choice from predefined options (via `enum` array in schema)

## Validation Options

Fields can include validation constraints:
- `required` - Field must be filled (specified at schema level)
- `minLength`/`maxLength` - String length constraints
- `minimum`/`maximum` - Number range constraints
- `pattern` - Regex pattern matching
- `format` - Built-in formats (email, uri, date)
- `default` - Default value for the field

## Testing the Server Standalone

You can test the MCP server directly:

```bash
# Test initialization
echo '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"test","version":"1.0"}}}' | \
  uvx --with "mcp[cli]" python examples/elicitation/server.py
```

## Dialog Navigation

When an elicitation dialog appears in the TUI:
- **↑/↓** or **Tab/Shift+Tab** - Navigate between fields
- **Space** - Toggle boolean fields
- **Y/N** - Set boolean to Yes/No
- **Enter** - Submit the form
- **Esc** - Cancel the elicitation
