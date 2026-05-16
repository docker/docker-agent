---
title: "Remote MCP Servers"
description: "Connect docker-agent to cloud services via remote MCP servers with built-in OAuth authentication."
permalink: /features/remote-mcp/
---

# Remote MCP Servers

_Connect docker-agent to cloud services via remote MCP servers with built-in OAuth authentication._

## Overview

Docker Agent supports connecting to remote MCP servers over **SSE** (Server-Sent Events) and **Streamable HTTP** transports. Many popular services offer MCP endpoints with OAuth — docker-agent handles the authentication flow automatically.

```yaml
toolsets:
  - type: mcp
    remote:
      url: "https://mcp.linear.app/sse"
      transport_type: "sse"
```

<div class="callout callout-tip" markdown="1">
<div class="callout-title">OAuth flow
</div>
  <p>When you connect to a remote MCP server that requires OAuth, docker-agent opens your browser automatically for authentication. Tokens are cached for subsequent sessions.</p>

</div>

## Configuration

```yaml
toolsets:
  - type: mcp
    remote:
      url: "https://mcp.example.com/sse"
      transport_type: "sse" # or "streamable"
      headers:
        Authorization: "Bearer token" # optional: static auth
```

For full configuration details, see the [Tool Config]({{ '/configuration/tools/' | relative_url }}) page.

### OAuth for servers without Dynamic Client Registration

Most remote MCP servers that require OAuth support [Dynamic Client Registration (RFC 7591)]({{ 'https://datatracker.ietf.org/doc/html/rfc7591' }}) — no configuration is needed, docker-agent handles the flow for you.

For servers that do **not** support DCR, provide explicit OAuth credentials with the `oauth:` block:

```yaml
toolsets:
  - type: mcp
    remote:
      url: "https://mcp.example.com/mcp"
      transport_type: "streamable"
      oauth:
        clientId: "my-app-client-id"
        clientSecret: "my-app-client-secret" # optional (public clients may omit)
        callbackPort: 8765                   # optional; picks a free port otherwise
        scopes:                              # optional; server-specific
          - read
          - write
```

| Field          | Type            | Required | Description                                                                                      |
| -------------- | --------------- | -------- | ------------------------------------------------------------------------------------------------ |
| `clientId`     | string          | ✓        | OAuth client ID registered with the remote MCP server.                                           |
| `clientSecret` | string          | ✗        | OAuth client secret. Omit for public clients using PKCE.                                         |
| `callbackPort` | integer         | ✗        | Local port to receive the OAuth redirect. If omitted, docker-agent picks a random free port.    |
| `scopes`       | array[string]   | ✗        | Scopes to request during the authorization step. Values are server-specific.                     |
| `callbackRedirectURL` | string   | ✗        | Custom OAuth redirect URI. Useful when the auth server requires HTTPS or a pre-registered URL. The literal placeholder `${callbackPort}` is replaced with the actual local callback port. See below.            |

Secrets should be stored in a credential helper or environment variable rather than committed — see [Secrets]({{ '/guides/secrets/' | relative_url }}) for interpolation patterns.

### Custom redirect URI (`callbackRedirectURL`)

Some authorization servers require the OAuth `redirect_uri` to be HTTPS or to match a URL that was pre-registered during app creation — neither of which plays nicely with a locally-bound loopback address such as `http://127.0.0.1:8765/callback`.

To work around this, set `callbackRedirectURL` to a public URL that redirects back to the local callback server. The literal placeholder `${callbackPort}` is substituted with the actual port the local callback server is listening on (either `callbackPort` when set, or the randomly-assigned port otherwise).

```yaml
toolsets:
  - type: mcp
    remote:
      url: "https://mcp.example.com/mcp"
      transport_type: "streamable"
      oauth:
        clientId: "my-app-client-id"
        callbackPort: 8765
        # Advertise this URL to the authorization server. The external
        # service at redirect.example.com is expected to 302-redirect the
        # browser to http://127.0.0.1:8765/callback preserving the query
        # string (code, state, …).
        callbackRedirectURL: "https://redirect.example.com/cb?port=${callbackPort}"
```

The local callback server still listens on the loopback interface on `callbackPort`; only the `redirect_uri` advertised to the authorization server changes.

**Validation rules:**

- The URL must be absolute (scheme + host) once `${callbackPort}` has been substituted.
- Only `http` and `https` schemes are accepted.
- `http` is only allowed when the host is a loopback address (`127.0.0.1`, `::1`, `localhost`); any other host must use `https` to avoid exposing the authorization `code` on the wire (RFC 8252 §7.3).

## Project Management &amp; Collaboration

| Service    | URL                                | Transport | Description                           |
| ---------- | ---------------------------------- | --------- | ------------------------------------- |
| Asana      | `https://mcp.asana.com/sse`        | sse       | Task and project management           |
| Atlassian  | `https://mcp.atlassian.com/v1/sse` | sse       | Jira, Confluence integration          |
| Linear     | `https://mcp.linear.app/sse`       | sse       | Issue tracking and project management |
| Monday.com | `https://mcp.monday.com/sse`       | sse       | Work management platform              |
| Intercom   | `https://mcp.intercom.com/sse`     | sse       | Customer communication platform       |

## Development &amp; Infrastructure

| Service                  | URL                                            | Transport  | Description                       |
| ------------------------ | ---------------------------------------------- | ---------- | --------------------------------- |
| GitHub                   | `https://api.githubcopilot.com/mcp`            | sse        | Version control and collaboration |
| Buildkite                | `https://mcp.buildkite.com/mcp`                | streamable | CI/CD platform                    |
| Netlify                  | `https://netlify-mcp.netlify.app/mcp`          | streamable | Web hosting and deployment        |
| Vercel                   | `https://mcp.vercel.com/`                      | sse        | Web deployment platform           |
| Cloudflare Bindings      | `https://bindings.mcp.cloudflare.com/sse`      | sse        | Edge computing resources          |
| Cloudflare Observability | `https://observability.mcp.cloudflare.com/sse` | sse        | Monitoring and analytics          |
| Grafbase                 | `https://api.grafbase.com/mcp`                 | streamable | GraphQL backend platform          |
| Neon                     | `https://mcp.neon.tech/sse`                    | sse        | Serverless Postgres database      |
| Prisma                   | `https://mcp.prisma.io/mcp`                    | streamable | Database ORM and toolkit          |
| Sentry                   | `https://mcp.sentry.dev/sse`                   | sse        | Error tracking and monitoring     |

## Content &amp; Media

| Service    | URL                                               | Transport  | Description                       |
| ---------- | ------------------------------------------------- | ---------- | --------------------------------- |
| Canva      | `https://mcp.canva.com/mcp`                       | streamable | Design and graphics platform      |
| Cloudinary | `https://asset-management.mcp.cloudinary.com/sse` | sse        | Media management and optimization |
| InVideo    | `https://mcp.invideo.io/sse`                      | sse        | Video creation platform           |
| Webflow    | `https://mcp.webflow.com/sse`                     | sse        | Website builder and CMS           |
| Wix        | `https://mcp.wix.com/sse`                         | sse        | Website builder platform          |
| Notion     | `https://mcp.notion.com/sse`                      | sse        | Documentation and knowledge base  |

## Communication &amp; Voice

| Service     | URL                                 | Transport  | Description                 |
| ----------- | ----------------------------------- | ---------- | --------------------------- |
| Fireflies   | `https://api.fireflies.ai/mcp`      | streamable | Meeting transcription       |
| Listenetic  | `https://mcp.listenetic.com/v1/mcp` | streamable | Audio intelligence platform |
| Carbonvoice | `https://mcp.carbonvoice.app`       | sse        | Voice communication tools   |
| Telnyx      | `https://api.telnyx.com/v2/mcp`     | streamable | Communications platform     |
| Dialer      | `https://getdialer.app/sse`         | sse        | Phone communication tools   |

## Storage &amp; File Management

| Service | URL                                 | Transport | Description              |
| ------- | ----------------------------------- | --------- | ------------------------ |
| Box     | `https://mcp.box.com`               | sse       | Cloud content management |
| Egnyte  | `https://mcp-server.egnyte.com/sse` | sse       | Enterprise file sharing  |

## Business &amp; Finance

| Service       | URL                                       | Transport  | Description                |
| ------------- | ----------------------------------------- | ---------- | -------------------------- |
| PayPal        | `https://mcp.paypal.com/sse`              | sse        | Payment processing         |
| Plaid         | `https://api.dashboard.plaid.com/mcp/sse` | sse        | Financial data integration |
| Square        | `https://mcp.squareup.com/sse`            | sse        | Payment processing         |
| Close         | `https://mcp.close.com/mcp`               | streamable | CRM platform               |
| Dodo Payments | `https://mcp.dodopayments.com/sse`        | sse        | Payment processing         |

## Analytics &amp; Data

| Service     | URL                                     | Transport  | Description                    |
| ----------- | --------------------------------------- | ---------- | ------------------------------ |
| ThoughtSpot | `https://agent.thoughtspot.app/mcp`     | streamable | Analytics and BI platform      |
| Meta Ads    | `https://mcp.pipeboard.co/meta-ads-mcp` | streamable | Facebook advertising analytics |

## Utilities &amp; Tools

| Service       | URL                                | Transport  | Description                     |
| ------------- | ---------------------------------- | ---------- | ------------------------------- |
| Apify         | `https://mcp.apify.com`            | sse        | Web scraping and automation     |
| SimpleScraper | `https://mcp.simplescraper.io/mcp` | streamable | Web scraping tool               |
| GlobalPing    | `https://mcp.globalping.dev/sse`   | sse        | Network diagnostics             |
| Jam           | `https://mcp.jam.dev/mcp`          | streamable | Bug reporting and collaboration |

## Example: Multi-Service Agent

Combine multiple remote MCP servers in a single agent:

```yaml
agents:
  root:
    model: anthropic/claude-sonnet-4-5
    instruction: |
      You help manage projects and deployments.
    toolsets:
      - type: mcp
        remote:
          url: "https://mcp.linear.app/sse"
          transport_type: "sse"
        instruction: Use Linear for issue tracking.
      - type: mcp
        remote:
          url: "https://api.githubcopilot.com/mcp"
          transport_type: "sse"
        instruction: Use GitHub for code and PRs.
      - type: mcp
        remote:
          url: "https://mcp.vercel.com/"
          transport_type: "sse"
        instruction: Use Vercel for deployments.
```

<div class="callout callout-info" markdown="1">
<div class="callout-title">Growing list
</div>
  <p>This list is updated as more services add MCP support. If a service you use isn't listed, check their documentation — many providers are adding MCP endpoints regularly.</p>

</div>
