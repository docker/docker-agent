// Package circuitbreaker provides a Circuit Breaker pattern implementation
// for fault tolerance and resilience in external service calls.
//
// The circuit breaker prevents cascading failures by:
// - Fast-failing when a service is known to be down (Open state)
// - Allowing recovery attempts (Half-Open state)
// - Tracking failure rates and success metrics (Closed state)
//
// This is essential for docker-agent's interactions with LLM providers,
// MCP servers, HTTP tools, and other external dependencies.
package circuitbreaker

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// State represents the current state of the circuit breaker.
type State string

const (
	StateClosed   State = "closed"   // Normal operation
	StateOpen     State = "open"     // Failing, rejecting requests
	StateHalfOpen State = "half-open" // Testing recovery
)

// Config holds circuit breaker configuration.
type Config struct {
	// FailureThreshold is the number of consecutive failures before opening.
	FailureThreshold int
	// SuccessThreshold is the number of consecutive successes in half-open state to close.
	SuccessThreshold int
	// Timeout is how long to stay in open state before trying half-open.
	Timeout time.Duration
	// MaxRequests is max concurrent requests allowed in half-open state.
	MaxRequests int
}

// DefaultConfig returns sensible defaults for circuit breaker configuration.
func DefaultConfig() Config {
	return Config{
		FailureThreshold: 5,
		SuccessThreshold: 2,
		Timeout:          30 * time.Second,
		MaxRequests:      1,
	}
}

// CircuitBreaker provides fault tolerance for external service calls.
type CircuitBreaker struct {
	mu              sync.RWMutex
	state           State
	failureCount    int
	successCount    int
	lastFailureTime time.Time
	halfOpenCount   int

	config Config
}

// New creates a new circuit breaker with the given configuration.
func New(config Config) *CircuitBreaker {
	return &CircuitBreaker{
		state:  StateClosed,
		config: config,
	}
}

// Execute runs the given function through the circuit breaker.
// Returns an error if the circuit is open or the function fails.
func (cb *CircuitBreaker) Execute(ctx context.Context, fn func(context.Context) error) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("context already cancelled: %w", err)
	}

	cb.mu.Lock()
	state := cb.state
	if state == StateOpen && time.Since(cb.lastFailureTime) < cb.config.Timeout {
		cb.mu.Unlock()
		return ErrCircuitOpen
	}

	// Transition from open to half-open
	if state == StateOpen {
		cb.state = StateHalfOpen
		cb.halfOpenCount = 0
		cb.successCount = 0
		state = StateHalfOpen
	}

	// Check half-open capacity
	if state == StateHalfOpen {
		if cb.halfOpenCount >= cb.config.MaxRequests {
			cb.mu.Unlock()
			return ErrTooManyRequests
		}
		cb.halfOpenCount++
	}

	cb.mu.Unlock()

	// Execute the function
	err := fn(ctx)

	cb.mu.Lock()
	defer cb.mu.Unlock()

	if state == StateHalfOpen {
		cb.halfOpenCount--
	}

	if err != nil {
		cb.recordFailure(state)
		return err
	}

	cb.recordSuccess(state)
	return nil
}

// recordFailure handles failure tracking and state transitions.
func (cb *CircuitBreaker) recordFailure(previousState State) {
	cb.lastFailureTime = time.Now()

	switch previousState {
	case StateClosed:
		cb.failureCount++
		cb.successCount = 0
		if cb.failureCount >= cb.config.FailureThreshold {
			cb.state = StateOpen
		}
	case StateHalfOpen:
		// Any failure in half-open returns to open
		cb.state = StateOpen
		cb.failureCount = 0
		cb.successCount = 0
	}
}

// recordSuccess handles success tracking and state transitions.
func (cb *CircuitBreaker) recordSuccess(previousState State) {
	cb.failureCount = 0

	if previousState == StateClosed {
		return
	}

	if previousState == StateHalfOpen {
		cb.successCount++
		if cb.successCount >= cb.config.SuccessThreshold {
			cb.state = StateClosed
			cb.successCount = 0
		}
	}
}

// State returns the current state of the circuit breaker.
func (cb *CircuitBreaker) State() State {
	cb.mu.RLock()
	defer cb.mu.RUnlock()
	return cb.state
}

// Reset resets the circuit breaker to closed state.
func (cb *CircuitBreaker) Reset() {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.state = StateClosed
	cb.failureCount = 0
	cb.successCount = 0
	cb.halfOpenCount = 0
	cb.lastFailureTime = time.Time{}
}

// Stats returns the current statistics of the circuit breaker.
func (cb *CircuitBreaker) Stats() Stats {
	cb.mu.RLock()
	defer cb.mu.RUnlock()
	return Stats{
		State:           cb.state,
		FailureCount:    cb.failureCount,
		SuccessCount:    cb.successCount,
		HalfOpenCount:   cb.halfOpenCount,
		LastFailureTime: cb.lastFailureTime,
	}
}

// Stats contains circuit breaker statistics.
type Stats struct {
	State           State
	FailureCount    int
	SuccessCount    int
	HalfOpenCount   int
	LastFailureTime time.Time
}

var (
	// ErrCircuitOpen is returned when the circuit is open.
	ErrCircuitOpen = errors.New("circuit breaker is open")
	// ErrTooManyRequests is returned when too many requests are in-flight in half-open state.
	ErrTooManyRequests = errors.New("too many requests in half-open state")
)
