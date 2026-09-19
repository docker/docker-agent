// Package circuitbreaker provides examples of using the circuit breaker.
package circuitbreaker

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"time"
)

// ExampleLLMAPICall demonstrates using a circuit breaker for LLM API calls.
// This is a common pattern in docker-agent when calling external LLM providers.
func ExampleLLMAPICall() {
	config := Config{
		FailureThreshold: 5,      // Open after 5 consecutive failures
		SuccessThreshold: 2,      // Close after 2 consecutive successes in half-open
		Timeout:          30 * time.Second, // Try recovery after 30s
		MaxRequests:      3,      // Allow up to 3 concurrent test requests in half-open
	}

	cb := New(config)

	// Simulate repeated API calls
	for i := 0; i < 10; i++ {
		err := cb.Execute(context.Background(), func(ctx context.Context) error {
			// In real usage, this would call an external LLM API
			return callLLMAPI(ctx)
		})

		if err != nil {
			fmt.Printf("Request %d failed: %v\n", i, err)
		} else {
			fmt.Printf("Request %d succeeded\n", i)
		}

		time.Sleep(100 * time.Millisecond)
	}
}

// ExampleMCPServerResilience demonstrates using a circuit breaker for MCP server calls.
// MCP (Model Context Protocol) servers may be unreliable, and circuit breakers
// prevent excessive retry storms.
func ExampleMCPServerResilience() {
	config := DefaultConfig()
	mcpCircuitBreaker := New(config)

	ctx := context.Background()

	// Simulate calling an MCP tool
	err := mcpCircuitBreaker.Execute(ctx, func(ctx context.Context) error {
		// In real usage, this would invoke an MCP tool through a server
		return invokeMCPTool(ctx)
	})

	if err != nil {
		log.Printf("MCP tool invocation failed: %v\n", err)
	}
}

// ExampleHTTPClientWithCircuitBreaker shows integrating circuit breaker
// with the HTTP client for external service calls.
func ExampleHTTPClientWithCircuitBreaker(url string) {
	config := Config{
		FailureThreshold: 3,
		SuccessThreshold: 2,
		Timeout:          60 * time.Second,
		MaxRequests:      5,
	}

	cb := New(config)
	client := &http.Client{Timeout: 10 * time.Second}

	err := cb.Execute(context.Background(), func(ctx context.Context) error {
		resp, err := client.Get(url)
		if err != nil {
			return err
		}
		defer resp.Body.Close()

		if resp.StatusCode >= 500 {
			return fmt.Errorf("server error: %d", resp.StatusCode)
		}

		return nil
	})

	if err != nil {
		log.Printf("HTTP request failed: %v\n", err)
	}
}

// ExampleMultipleCircuitBreakers shows how different external services
// can have independent circuit breakers.
func ExampleMultipleCircuitBreakers() {
	// Separate circuit breakers for different LLM providers
	openaiCB := New(DefaultConfig())
	anthropicCB := New(DefaultConfig())
	geminCB := New(DefaultConfig())

	// Each provider circuit breaker tracks failures independently
	ctx := context.Background()

	// OpenAI call
	openaiCB.Execute(ctx, func(ctx context.Context) error {
		return callLLMAPI(ctx)
	})

	// Anthropic call
	anthropicCB.Execute(ctx, func(ctx context.Context) error {
		return callLLMAPI(ctx)
	})

	// Gemini call
	geminCB.Execute(ctx, func(ctx context.Context) error {
		return callLLMAPI(ctx)
	})
}

// callLLMAPI is a mock function representing an external LLM API call.
func callLLMAPI(ctx context.Context) error {
	// Simulate API call
	return nil
}

// invokeMCPTool is a mock function representing an MCP tool invocation.
func invokeMCPTool(ctx context.Context) error {
	// Simulate MCP tool call
	return nil
}
