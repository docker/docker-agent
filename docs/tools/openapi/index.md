---
title: "OpenAPI Tool"
description: "Automatically generate tools from an OpenAPI specification."
permalink: /tools/openapi/
---

# OpenAPI Tool

_Automatically generate tools from an OpenAPI specification._

## Overview

The OpenAPI tool fetches an OpenAPI 3.x specification from a URL and creates one tool per API operation. Each endpoint's parameters, request body, and description are translated into a callable tool that the agent can invoke directly.

## Configuration

```yaml
toolsets:
  - type: openapi
    url: "https://petstore3.swagger.io/api/v3/openapi.json"
```

### With custom headers

Pass custom headers to every HTTP request made by the generated tools (for example, for authentication):

```yaml
toolsets:
  - type: openapi
    url: "https://api.example.com/openapi.json"
    headers:
      Authorization: "Bearer ${env.API_TOKEN}"
      X-Custom-Header: "my-value"
```

## Properties

| Property  | Type              | Required | Description                                                                 |
| --------- | ----------------- | -------- | --------------------------------------------------------------------------- |
| `url`     | string            | ✓        | URL of the OpenAPI specification (JSON format)                              |
| `headers` | map[string]string |          | Custom HTTP headers sent with every request. Values support `${env.VAR}` and `${headers.NAME}` placeholders. |

## How it works

1. The spec is fetched from the configured `url` at startup.
2. Each operation (GET, POST, PUT, …) becomes a separate tool named after its `operationId` (or `method_path` when no `operationId` is set).
3. Path and query parameters are exposed as tool parameters. Request body properties are prefixed with `body_`.
4. Read-only operations (GET, HEAD, OPTIONS) are annotated accordingly.
5. Responses are returned as text; errors include the HTTP status code.

## Limits

- The OpenAPI spec must be **10 MB or less**.
- Individual API responses are truncated at **1 MB**.

## Example

See the full [Pet Store example](https://github.com/docker/docker-agent/blob/main/examples/openapi-petstore.yaml) for a working agent configuration.
