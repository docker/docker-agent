#!/usr/bin/env python3
"""
MCP Server demonstrating elicitation capabilities.

This server provides tools that use MCP elicitation to request additional
information from the user during tool execution.

Usage with docker-agent:
    docker agent run examples/elicitation/agent.yaml

Usage standalone:
    uvx --with "mcp[cli]" mcp run examples/elicitation/server.py
"""

import json
from mcp.server.fastmcp import FastMCP

# Create the MCP server
mcp = FastMCP("elicitation-demo")


@mcp.tool()
async def confirm_action(action: str) -> str:
    """
    Perform an action that requires user confirmation.
    Use this when you need to confirm something with the user.
    
    Args:
        action: The action to confirm
    """
    ctx = mcp.get_context()
    
    # Request confirmation from the user
    result = await ctx.session.elicit(
        message=f"Are you sure you want to proceed with: {action}?",
        requestedSchema={
            "type": "object",
            "properties": {
                "confirmed": {
                    "type": "boolean",
                    "description": "Confirm this action",
                    "default": False,
                },
                "reason": {
                    "type": "string",
                    "description": "Optional reason for your decision",
                },
            },
        },
    )

    if result.action == "accept":
        content = result.content or {}
        if content.get("confirmed", False):
            reason = content.get("reason", "")
            reason_text = f" Reason: {reason}" if reason else ""
            return f"✅ Action confirmed: {action}.{reason_text}"
        else:
            return f"❌ Action declined: {action}"
    else:
        return f"⚠️ Confirmation cancelled for: {action}"


@mcp.tool()
async def create_user() -> str:
    """
    Create a new user with interactive form input.
    Demonstrates multi-field elicitation with validation.
    """
    ctx = mcp.get_context()
    
    result = await ctx.session.elicit(
        message="Please provide the new user details:",
        requestedSchema={
            "type": "object",
            "properties": {
                "username": {
                    "type": "string",
                    "description": "Username (3-20 characters)",
                    "minLength": 3,
                    "maxLength": 20,
                },
                "email": {
                    "type": "string",
                    "description": "Email address",
                    "format": "email",
                },
                "role": {
                    "type": "string",
                    "description": "User role",
                    "enum": ["admin", "editor", "viewer"],
                },
                "bio": {
                    "type": "string",
                    "description": "Short bio (optional)",
                },
                "active": {
                    "type": "boolean",
                    "description": "Account active",
                    "default": True,
                },
            },
            "required": ["username", "email", "role"],
        },
    )

    if result.action == "accept":
        content = result.content or {}
        user_info = json.dumps(content, indent=2)
        return f"✅ User created successfully!\n\nUser details:\n{user_info}"
    else:
        return "❌ User creation cancelled."


@mcp.tool()
async def configure_settings(preset: str = "default") -> str:
    """
    Configure numeric settings with validation.
    Demonstrates number field elicitation.
    
    Args:
        preset: Optional preset name to start from (default, performance, or reliable)
    """
    ctx = mcp.get_context()
    
    # Default values based on preset
    defaults = {
        "default": {"max_connections": 10, "timeout": 30, "retry_count": 3},
        "performance": {"max_connections": 100, "timeout": 5, "retry_count": 1},
        "reliable": {"max_connections": 5, "timeout": 60, "retry_count": 5},
    }.get(preset, {"max_connections": 10, "timeout": 30, "retry_count": 3})

    result = await ctx.session.elicit(
        message=f"Configure settings (preset: {preset}):",
        requestedSchema={
            "type": "object",
            "properties": {
                "max_connections": {
                    "type": "integer",
                    "description": "Maximum concurrent connections (1-100)",
                    "minimum": 1,
                    "maximum": 100,
                    "default": defaults["max_connections"],
                },
                "timeout": {
                    "type": "number",
                    "description": "Request timeout in seconds (1-300)",
                    "minimum": 1,
                    "maximum": 300,
                    "default": defaults["timeout"],
                },
                "retry_count": {
                    "type": "integer",
                    "description": "Number of retries (0-10)",
                    "minimum": 0,
                    "maximum": 10,
                    "default": defaults["retry_count"],
                },
            },
            "required": ["max_connections", "timeout"],
        },
    )

    if result.action == "accept":
        content = result.content or {}
        settings_info = json.dumps(content, indent=2)
        return f"✅ Settings configured!\n\nConfiguration:\n{settings_info}"
    else:
        return "❌ Settings configuration cancelled."


@mcp.tool()
async def setup_preferences() -> str:
    """
    Set up user preferences with boolean toggles.
    Demonstrates boolean field elicitation.
    """
    ctx = mcp.get_context()
    
    result = await ctx.session.elicit(
        message="Set up your preferences:",
        requestedSchema={
            "type": "object",
            "properties": {
                "dark_mode": {
                    "type": "boolean",
                    "description": "Enable dark mode theme",
                    "default": False,
                },
                "notifications": {
                    "type": "boolean",
                    "description": "Enable notifications",
                    "default": True,
                },
                "auto_save": {
                    "type": "boolean",
                    "description": "Auto-save documents",
                    "default": True,
                },
                "telemetry": {
                    "type": "boolean",
                    "description": "Share anonymous usage data",
                    "default": False,
                },
            },
        },
    )

    if result.action == "accept":
        content = result.content or {}
        prefs_info = json.dumps(content, indent=2)
        return f"✅ Preferences saved!\n\nYour preferences:\n{prefs_info}"
    else:
        return "❌ Preferences setup cancelled."


@mcp.tool()
async def select_option() -> str:
    """
    Select from a list of options.
    Demonstrates enum field elicitation.
    """
    ctx = mcp.get_context()
    
    result = await ctx.session.elicit(
        message="Make your selections:",
        requestedSchema={
            "type": "object",
            "properties": {
                "environment": {
                    "type": "string",
                    "description": "Deployment environment",
                    "enum": ["development", "staging", "production"],
                },
                "region": {
                    "type": "string",
                    "description": "Server region",
                    "enum": ["us-east", "us-west", "eu-west", "ap-south"],
                },
                "tier": {
                    "type": "string",
                    "description": "Service tier",
                    "enum": ["free", "starter", "professional", "enterprise"],
                },
            },
            "required": ["environment", "region"],
        },
    )

    if result.action == "accept":
        content = result.content or {}
        selection_info = json.dumps(content, indent=2)
        return f"✅ Selection confirmed!\n\nYour choices:\n{selection_info}"
    else:
        return "❌ Selection cancelled."


@mcp.tool()
async def visit_documentation(topic: str = "getting-started") -> str:
    """
    Open documentation in the browser.
    Demonstrates URL-based elicitation where the user visits an external page.
    
    Args:
        topic: Documentation topic (getting-started, api-reference, tutorials, faq)
    """
    import uuid
    
    ctx = mcp.get_context()
    
    # Map topics to documentation URLs
    docs_urls = {
        "getting-started": "https://docs.example.com/getting-started",
        "api-reference": "https://docs.example.com/api",
        "tutorials": "https://docs.example.com/tutorials",
        "faq": "https://docs.example.com/faq",
    }
    
    url = docs_urls.get(topic, docs_urls["getting-started"])
    elicitation_id = str(uuid.uuid4())
    
    result = await ctx.session.elicit_url(
        message=f"Please visit the {topic} documentation and confirm when you're done reading:",
        url=url,
        elicitation_id=elicitation_id,
    )

    if result.action == "accept":
        return f"✅ Great! You've reviewed the {topic} documentation at:\n{url}\n\nLet me know if you have any questions!"
    else:
        return f"❌ Documentation review cancelled. The {topic} docs are available at {url} when you're ready."


if __name__ == "__main__":
    mcp.run()
