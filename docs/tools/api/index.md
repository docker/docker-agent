---
title: "API Tool"
description: "Create custom tools that call HTTP APIs."
permalink: /tools/api/
---

# API Tool

_Create custom tools that call HTTP APIs._

## Overview

The API tool type lets you define custom tools that make HTTP requests to external APIs. This is useful for integrating agents with REST APIs, webhooks, or any HTTP-based service without writing code.

<div class="callout callout-info" markdown="1">
<div class="callout-title">When to Use
</div>

- Integrating with REST APIs that don't have an MCP server
- Simple HTTP operations (GET, POST)
- Quick prototyping before building a full MCP server

</div>

## Configuration

```yaml
agents:
  assistant:
    model: openai/gpt-4o
    description: Assistant with API access
    instruction: You can look up weather information.
    toolsets:
      - type: api
        api_config:
          name: get_weather
          method: GET
          endpoint: "https://api.weather.example/v1/current?city=${city}"
          instruction: Get current weather for a city
          args:
            city:
              type: string
              description: City name to get weather for
          required: ["city"]
          headers:
            Authorization: "Bearer ${env.WEATHER_API_KEY}"
```

## Properties

| Property        | Type   | Required | Description                                      |
| --------------- | ------ | -------- | ------------------------------------------------ |
| `name`          | string | ✓        | Tool name (how the agent references it)          |
| `method`        | string | ✓        | HTTP method: `GET` or `POST`                     |
| `endpoint`      | string | ✓        | URL endpoint (supports `${param}` interpolation) |
| `instruction`   | string | ✗        | Description shown to the agent                   |
| `args`          | object | ✗        | Parameter definitions (JSON Schema properties)   |
| `required`      | array  | ✗        | List of required parameter names                 |
| `headers`       | object | ✗        | HTTP headers to include                          |
| `output_schema` | object | ✗        | JSON Schema for the response (for documentation) |

## HTTP Methods

### GET Requests

For GET requests, parameters are interpolated into the URL:

```yaml
toolsets:
  - type: api
    api_config:
      name: search_users
      method: GET
      endpoint: "https://api.example.com/users?q=${query}&limit=${limit}"
      instruction: Search for users by name
      args:
        query:
          type: string
          description: Search query
        limit:
          type: integer
          description: Maximum results (default 10)
      required: ["query"]
```

### POST Requests

For POST requests, parameters are sent as JSON in the request body:

```yaml
toolsets:
  - type: api
    api_config:
      name: create_task
      method: POST
      endpoint: "https://api.example.com/tasks"
      instruction: Create a new task
      args:
        title:
          type: string
          description: Task title
        description:
          type: string
          description: Task description
        priority:
          type: string
          enum: ["low", "medium", "high"]
          description: Task priority
      required: ["title"]
      headers:
        Content-Type: "application/json"
        Authorization: "Bearer ${env.API_TOKEN}"
```

## URL Interpolation

Use `${param}` syntax to insert parameter values into URLs:

```yaml
endpoint: "https://api.example.com/users/${user_id}/posts/${post_id}"
```

Parameter values are URL-encoded automatically.

## Headers

Headers can include environment variables:

```yaml
headers:
  Authorization: "Bearer ${env.API_KEY}"
  X-Custom-Header: "static-value"
  Content-Type: "application/json"
```

## Output Schema

Optionally document the expected response format:

```yaml
toolsets:
  - type: api
    api_config:
      name: get_user
      method: GET
      endpoint: "https://api.example.com/users/${id}"
      instruction: Get user details by ID
      args:
        id:
          type: string
          description: User ID
      required: ["id"]
      output_schema:
        type: object
        properties:
          id:
            type: string
          name:
            type: string
          email:
            type: string
          created_at:
            type: string
```

## Example: GitHub API

```yaml
agents:
  github_assistant:
    model: openai/gpt-4o
    description: Assistant that can query GitHub
    instruction: You can look up GitHub repositories and users.
    toolsets:
      - type: api
        api_config:
          name: get_repo
          method: GET
          endpoint: "https://api.github.com/repos/${owner}/${repo}"
          instruction: Get information about a GitHub repository
          args:
            owner:
              type: string
              description: Repository owner (user or org)
            repo:
              type: string
              description: Repository name
          required: ["owner", "repo"]
          headers:
            Accept: "application/vnd.github.v3+json"
            Authorization: "Bearer ${env.GITHUB_TOKEN}"

      - type: api
        api_config:
          name: get_user
          method: GET
          endpoint: "https://api.github.com/users/${username}"
          instruction: Get information about a GitHub user
          args:
            username:
              type: string
              description: GitHub username
          required: ["username"]
          headers:
            Accept: "application/vnd.github.v3+json"
```

## Limitations

- Only supports GET and POST methods
- Response body is limited to 1MB
- 30 second timeout per request
- Only HTTP and HTTPS URLs are supported
- No support for file uploads or multipart forms

<div class="callout callout-tip" markdown="1">
<div class="callout-title">For Complex APIs
</div>
  <p>For APIs that need authentication flows, pagination, or complex request/response handling, consider using an MCP server instead. The API tool is best for simple, stateless HTTP operations.</p>
</div>

<div class="callout callout-warning" markdown="1">
<div class="callout-title">Security
</div>
  <p>API keys and tokens in headers are visible in debug logs. Use environment variables (<code>${env.VAR}</code>) rather than hardcoding secrets in configuration files.</p>
</div>
