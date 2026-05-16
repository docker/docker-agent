---
title: "AWS Bedrock"
description: "Access Claude, Nova, Llama, and more through AWS infrastructure with enterprise-grade security and compliance."
permalink: /providers/bedrock/
---

# AWS Bedrock

_Access Claude, Nova, Llama, and more through AWS infrastructure with enterprise-grade security and compliance._

## Prerequisites

- AWS account with Bedrock enabled in your region
- Model access granted in the [Bedrock Console](https://console.aws.amazon.com/bedrock/) (some models require approval)
- AWS credentials configured (see authentication below)

## Configuration

```yaml
models:
  bedrock-claude:
    provider: amazon-bedrock
    model: global.anthropic.claude-sonnet-4-5-20250929-v1:0
    max_tokens: 64000
    provider_opts:
      region: us-east-1
```

## Authentication

### Option 1: Bedrock API Key (Simplest)

```bash
export AWS_BEARER_TOKEN_BEDROCK="your-key"
```

```yaml
models:
  bedrock:
    provider: amazon-bedrock
    model: global.anthropic.claude-sonnet-4-5-20250929-v1:0
    token_key: AWS_BEARER_TOKEN_BEDROCK # env var name
    provider_opts:
      region: us-east-1
```

### Option 2: AWS Credentials (Default)

Uses the standard AWS SDK credential chain: env vars → shared credentials → config → IAM roles.

```yaml
models:
  bedrock:
    provider: amazon-bedrock
    model: global.anthropic.claude-sonnet-4-5-20250929-v1:0
    provider_opts:
      profile: my-aws-profile
      region: us-east-1
```

### With IAM Role Assumption

```yaml
models:
  bedrock:
    provider: amazon-bedrock
    model: global.anthropic.claude-sonnet-4-5-20250929-v1:0
    provider_opts:
      role_arn: "arn:aws:iam::123456789012:role/BedrockAccessRole"
      external_id: "my-external-id"
```

## Provider Options

| Option                   | Type   | Default                | Description                          |
| ------------------------ | ------ | ---------------------- | ------------------------------------ |
| `region`                 | string | us-east-1              | AWS region                           |
| `profile`                | string | —                      | AWS profile name                     |
| `role_arn`               | string | —                      | IAM role ARN for assume role         |
| `role_session_name`      | string | docker-agent-bedrock-session | Session name for assumed role        |
| `external_id`            | string | —                      | External ID for role assumption      |
| `endpoint_url`           | string | —                      | Custom endpoint (VPC/testing)        |
| `interleaved_thinking`   | bool   | true                   | Reasoning during tool calls (Claude) |
| `disable_prompt_caching` | bool   | false                  | Disable automatic prompt caching     |

## Inference Profiles

Use inference profile prefixes for optimal routing:

| Prefix    | Routes To                                |
| --------- | ---------------------------------------- |
| `global.` | All commercial AWS regions (recommended) |
| `us.`     | US regions only                          |
| `eu.`     | EU regions only (GDPR compliance)        |
| `apac.`   | Asia Pacific regions only                |

<div class="callout callout-tip" markdown="1">
<div class="callout-title">Inference profiles
</div>
  <p>Use <code>global.</code> prefix on model IDs for automatic cross-region routing. Use <code>eu.</code> prefix for GDPR compliance.</p>

</div>

## Prompt Caching

Automatically enabled for supported models to reduce latency and costs. System prompts, tool definitions, and recent messages are cached with a 5-minute TTL.

```bash
# Disable if needed
provider_opts:
  disable_prompt_caching: true
```
