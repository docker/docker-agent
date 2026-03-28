```
➜  docker-agent git:(main) ✗ tail -f ~/.cagent/cagent.debug.log | grep -v -i telemetry                                                                                                                 <region:us-east-1>
time=2026-03-28T15:12:16.143+01:00 level=DEBUG msg="Sending OAuth elicitation request to client"
time=2026-03-28T15:12:16.253+01:00 level=DEBUG msg="Starting unmanaged OAuth flow for server" url=https://mcp.slack.com/mcp
time=2026-03-28T15:12:16.480+01:00 level=DEBUG msg="Sending OAuth elicitation request to client"
time=2026-03-28T15:12:16.480+01:00 level=ERROR msg="Failed to initialize MCP client" error="failed to connect to MCP server: calling \"initialize\": sending \"initialize\": rejected by transport: Post \"https://mcp.slack.com/mcp\": OAuth flow failed: failed to send elicitation request: no elicitation handler configured"
time=2026-03-28T15:12:16.480+01:00 level=WARN msg="Toolset start failed; skipping" agent=root toolset=*mcp.Toolset error="failed to initialize MCP client: failed to connect to MCP server: calling \"initialize\": sending \"initialize\": rejected by transport: Post \"https://mcp.slack.com/mcp\": OAuth flow failed: failed to send elicitation request: no elicitation handler configured"
time=2026-03-28T15:12:16.480+01:00 level=DEBUG msg="Forwarding event to sidebar" event_type=*runtime.ToolsetInfoEvent
time=2026-03-28T15:12:16.483+01:00 level=DEBUG msg="Forwarding event to sidebar" event_type=*runtime.ToolsetInfoEvent

```

