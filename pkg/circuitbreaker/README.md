# Circuit Breaker Package

## Overview

The `circuitbreaker` package provides a production-grade implementation of the Circuit Breaker pattern for fault tolerance and resilience in docker-agent. It's essential for managing interactions with external services that may be unreliable or experience transient failures.

## Why Circuit Breakers?

Docker-agent depends on numerous external services:
- **LLM Providers** (OpenAI, Anthropic, Gemini, etc.) — API rate limits, outages, latency spikes
- **MCP Servers** — Tool invocations, potential timeouts or crashes  
- **HTTP Tools** — Web scraping, API calls, external integrations
- **Remote Agents** — Network delays, connection drops

Without circuit breakers, failures cascade:
1. Agent retries failing requests → more load
2. Clients wait for timeouts → slower response times
3. Resource exhaustion → more failures → cascade continues

Circuit breakers **fast-fail** when a service is known to be down, reducing unnecessary load and improving user experience.

## Features

- **Three States**: Closed (normal), Open (failing fast), Half-Open (testing recovery)
- **Configurable Thresholds**: Customize when to open and how many successes to close
- **Concurrency Safe**: Thread-safe for use in goroutines
- **Timeout-based Recovery**: Automatically attempt recovery after a timeout
- **Statistics**: Track failures, successes, and state transitions
- **Context Support**: Respects context cancellation for graceful shutdown

## Usage

### Basic Usage

```go
package main

import (
	"context"
	"log"
	
	"github.com/docker/docker-agent/pkg/circuitbreaker"
)

func main() {
	// Create a circuit breaker with default configuration
	cb := circuitbreaker.New(circuitbreaker.DefaultConfig())
	
	// Execute a function through the circuit breaker
	err := cb.Execute(context.Background(), func(ctx context.Context) error {
		// Call external service
		return callExternalAPI(ctx)
	})
	
	if err == circuitbreaker.ErrCircuitOpen {
		log.Println("Service is down, try again later")
	} else if err != nil {
		log.Printf("API call failed: %v\n", err)
	}
}

func callExternalAPI(ctx context.Context) error {
	// Your API call here
	return nil
}
```

### Custom Configuration

```go
config := circuitbreaker.Config{
	FailureThreshold: 5,           // Open after 5 failures
	SuccessThreshold: 2,           // Close after 2 successes
	Timeout:          30 * time.Second,  // Try recovery after 30s
	MaxRequests:      3,            // Allow 3 concurrent test requests
}

cb := circuitbreaker.New(config)
```

### Monitoring State

```go
// Check current state
state := cb.State()
if state == circuitbreaker.StateOpen {
	log.Println("Circuit is open - service is down")
}

// Get detailed statistics
stats := cb.Stats()
fmt.Printf("State: %s, Failures: %d, Successes: %d\n",
	stats.State, stats.FailureCount, stats.SuccessCount)
```

### Multiple Circuit Breakers

Use separate circuit breakers for different services:

```go
openaiCB := circuitbreaker.New(circuitbreaker.DefaultConfig())
anthropicCB := circuitbreaker.New(circuitbreaker.DefaultConfig())
geminiCB := circuitbreaker.New(circuitbreaker.DefaultConfig())

// Each provider's circuit breaker is independent
openaiCB.Execute(ctx, func(ctx context.Context) error {
	return callOpenAI(ctx)
})

anthropicCB.Execute(ctx, func(ctx context.Context) error {
	return callAnthropic(ctx)
})
```

## State Diagram

```
          +-------+
          | Closed|  Normal operation
          +---+---+
              |
         [5 failures]
              |
              v
          +-------+
          | Open  |  Fast fail mode
          +---+---+
              |
         [30s timeout]
              |
              v
        +----------+
        |Half-Open|  Testing recovery
        +----+-----+
             |
         [2 successes]  OR  [1 failure]
             |               |
             v               v
          Closed <---------- Open
```

## Configuration Recommendations

### For Slow Services (MCP servers, webhooks)
```go
Config{
	FailureThreshold: 2,
	SuccessThreshold: 1,
	Timeout: 60 * time.Second,
	MaxRequests: 2,
}
```

### For Fast APIs (LLM providers with good SLA)
```go
Config{
	FailureThreshold: 5,
	SuccessThreshold: 2,
	Timeout: 30 * time.Second,
	MaxRequests: 3,
}
```

### For Experimental Features (lower tolerance)
```go
Config{
	FailureThreshold: 3,
	SuccessThreshold: 3,
	Timeout: 120 * time.Second,
	MaxRequests: 1,
}
```

## Error Handling

The circuit breaker returns specific errors:

- `ErrCircuitOpen` — Circuit is open, service is known to be down
- `ErrTooManyRequests` — Too many concurrent requests in half-open state
- Original error — From the wrapped function

```go
err := cb.Execute(ctx, fn)
if err == circuitbreaker.ErrCircuitOpen {
	// Use fallback or retry with backoff
} else if err == circuitbreaker.ErrTooManyRequests {
	// Circuit is testing recovery, wait and retry
} else {
	// Actual error from fn
}
```

## Integration Examples

### With MCP Tool Calls
```go
cb := circuitbreaker.New(defaultConfig)

toolResult, err := cb.Execute(ctx, func(ctx context.Context) error {
	return mcpServer.InvokeTool(ctx, toolName, args)
})
```

### With HTTP Clients
```go
cb := circuitbreaker.New(defaultConfig)

err := cb.Execute(ctx, func(ctx context.Context) error {
	resp, err := httpClient.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	
	if resp.StatusCode >= 500 {
		return fmt.Errorf("server error: %d", resp.StatusCode)
	}
	return nil
})
```

### With LLM Calls
```go
cb := circuitbreaker.New(defaultConfig)

completion, err := cb.Execute(ctx, func(ctx context.Context) error {
	return llmClient.CreateCompletion(ctx, request)
})
```

## Best Practices

1. **Use separate circuit breakers per service** — Failures in one service shouldn't affect others
2. **Combine with backoff** — Use with `pkg/backoff` for retry logic
3. **Monitor state transitions** — Log when circuits open/close for observability
4. **Set appropriate timeouts** — Match your service's typical recovery time
5. **Handle ErrCircuitOpen** — Provide fallbacks or graceful degradation
6. **Test failure scenarios** — Verify your circuit breaker helps during outages

## Testing

Circuit breakers are tested with:
- State transitions (closed → open → half-open → closed)
- Concurrent access patterns
- Context cancellation
- Failure and success counting
- Timeout-based recovery
- Capacity limits in half-open state

Run tests:
```bash
go test ./pkg/circuitbreaker -v
```

## Performance Considerations

- **Minimal overhead**: Simple atomic operations for state checks
- **Thread-safe**: Uses sync.RWMutex for safe concurrent access
- **Context-aware**: Respects cancellation for efficient cleanup

## Future Enhancements

Potential improvements:
- Metrics export (Prometheus format)
- Custom state change callbacks
- Adaptive configuration based on error rates
- Circuit breaker group management
- Event logging and tracing integration
