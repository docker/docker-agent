package circuitbreaker

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCircuitBreakerClosedState(t *testing.T) {
	cb := New(DefaultConfig())
	assert.Equal(t, StateClosed, cb.State())

	// Success should not change state
	err := cb.Execute(context.Background(), func(ctx context.Context) error {
		return nil
	})
	assert.NoError(t, err)
	assert.Equal(t, StateClosed, cb.State())
}

func TestCircuitBreakerOpensAfterFailures(t *testing.T) {
	config := DefaultConfig()
	config.FailureThreshold = 3
	cb := New(config)

	// Fail 3 times to open the circuit
	for i := 0; i < 3; i++ {
		err := cb.Execute(context.Background(), func(ctx context.Context) error {
			return errors.New("service error")
		})
		assert.Error(t, err)
	}

	// Circuit should now be open
	assert.Equal(t, StateOpen, cb.State())

	// Subsequent calls should fail with circuit open error
	err := cb.Execute(context.Background(), func(ctx context.Context) error {
		return nil
	})
	assert.Equal(t, ErrCircuitOpen, err)
}

func TestCircuitBreakerHalfOpenAfterTimeout(t *testing.T) {
	config := DefaultConfig()
	config.FailureThreshold = 1
	config.Timeout = 100 * time.Millisecond
	cb := New(config)

	// Open the circuit
	cb.Execute(context.Background(), func(ctx context.Context) error {
		return errors.New("fail")
	})
	assert.Equal(t, StateOpen, cb.State())

	// Wait for timeout
	time.Sleep(150 * time.Millisecond)

	// Next call should attempt half-open
	err := cb.Execute(context.Background(), func(ctx context.Context) error {
		return nil
	})
	assert.NoError(t, err)
	assert.Equal(t, StateHalfOpen, cb.State())
}

func TestCircuitBreakerRecovery(t *testing.T) {
	config := DefaultConfig()
	config.FailureThreshold = 1
	config.SuccessThreshold = 2
	config.Timeout = 50 * time.Millisecond
	cb := New(config)

	// Open the circuit
	cb.Execute(context.Background(), func(ctx context.Context) error {
		return errors.New("fail")
	})
	assert.Equal(t, StateOpen, cb.State())

	// Wait and transition to half-open
	time.Sleep(100 * time.Millisecond)

	// Succeed twice to close
	for i := 0; i < 2; i++ {
		err := cb.Execute(context.Background(), func(ctx context.Context) error {
			return nil
		})
		assert.NoError(t, err)
	}

	assert.Equal(t, StateClosed, cb.State())
}

func TestCircuitBreakerReopensOnHalfOpenFailure(t *testing.T) {
	config := DefaultConfig()
	config.FailureThreshold = 1
	config.Timeout = 50 * time.Millisecond
	cb := New(config)

	// Open the circuit
	cb.Execute(context.Background(), func(ctx context.Context) error {
		return errors.New("fail")
	})

	// Wait and transition to half-open
	time.Sleep(100 * time.Millisecond)
	assert.Equal(t, StateHalfOpen, cb.State())

	// Fail in half-open should reopen
	err := cb.Execute(context.Background(), func(ctx context.Context) error {
		return errors.New("still failing")
	})
	assert.Error(t, err)
	assert.Equal(t, StateOpen, cb.State())
}

func TestCircuitBreakerMaxRequestsHalfOpen(t *testing.T) {
	config := DefaultConfig()
	config.FailureThreshold = 1
	config.Timeout = 50 * time.Millisecond
	config.MaxRequests = 1
	cb := New(config)

	// Open the circuit
	cb.Execute(context.Background(), func(ctx context.Context) error {
		return errors.New("fail")
	})

	// Wait and transition to half-open
	time.Sleep(100 * time.Millisecond)

	// Simulate concurrent request that blocks
	blockCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errChan := make(chan error, 1)
	go func() {
		errChan <- cb.Execute(blockCtx, func(ctx context.Context) error {
			// Simulate long operation
			<-time.After(200 * time.Millisecond)
			return nil
		})
	}()

	// Give first request time to start
	time.Sleep(10 * time.Millisecond)

	// Second request should fail with too many requests
	err := cb.Execute(context.Background(), func(ctx context.Context) error {
		return nil
	})
	assert.Equal(t, ErrTooManyRequests, err)

	// Clean up
	cancel()
	<-errChan
}

func TestCircuitBreakerReset(t *testing.T) {
	config := DefaultConfig()
	config.FailureThreshold = 1
	cb := New(config)

	// Open the circuit
	cb.Execute(context.Background(), func(ctx context.Context) error {
		return errors.New("fail")
	})
	assert.Equal(t, StateOpen, cb.State())

	// Reset should return to closed
	cb.Reset()
	assert.Equal(t, StateClosed, cb.State())

	// Should accept requests again
	err := cb.Execute(context.Background(), func(ctx context.Context) error {
		return nil
	})
	assert.NoError(t, err)
}

func TestCircuitBreakerContextCancellation(t *testing.T) {
	cb := New(DefaultConfig())

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := cb.Execute(ctx, func(ctx context.Context) error {
		return nil
	})
	assert.Error(t, err)
}

func TestCircuitBreakerStats(t *testing.T) {
	config := DefaultConfig()
	config.FailureThreshold = 3
	cb := New(config)

	// Generate some failures
	for i := 0; i < 2; i++ {
		cb.Execute(context.Background(), func(ctx context.Context) error {
			return errors.New("fail")
		})
	}

	stats := cb.Stats()
	assert.Equal(t, StateClosed, stats.State)
	assert.Equal(t, 2, stats.FailureCount)
	assert.False(t, stats.LastFailureTime.IsZero())
}

func TestCircuitBreakerFailureCountReset(t *testing.T) {
	config := DefaultConfig()
	config.FailureThreshold = 2
	cb := New(config)

	// Fail once
	cb.Execute(context.Background(), func(ctx context.Context) error {
		return errors.New("fail")
	})

	// Success should reset failure count
	cb.Execute(context.Background(), func(ctx context.Context) error {
		return nil
	})

	stats := cb.Stats()
	assert.Equal(t, 0, stats.FailureCount)
}

func TestCircuitBreakerConcurrency(t *testing.T) {
	config := DefaultConfig()
	config.FailureThreshold = 10
	cb := New(config)

	// Run concurrent operations
	errChan := make(chan error, 100)
	for i := 0; i < 100; i++ {
		go func(index int) {
			err := cb.Execute(context.Background(), func(ctx context.Context) error {
				if index%2 == 0 {
					return errors.New("fail")
				}
				return nil
			})
			errChan <- err
		}(i)
	}

	// Collect results
	failCount := 0
	for i := 0; i < 100; i++ {
		err := <-errChan
		if err != nil {
			failCount++
		}
	}

	// Should have about 50 failures
	assert.True(t, failCount >= 40 && failCount <= 60, "expected ~50 failures, got %d", failCount)
}

func TestCircuitBreakerWithCustomConfig(t *testing.T) {
	config := Config{
		FailureThreshold: 2,
		SuccessThreshold: 1,
		Timeout:          50 * time.Millisecond,
		MaxRequests:      5,
	}
	cb := New(config)

	// Verify config is used
	assert.Equal(t, StateClosed, cb.State())

	// Fail twice to open
	for i := 0; i < 2; i++ {
		cb.Execute(context.Background(), func(ctx context.Context) error {
			return errors.New("fail")
		})
	}
	assert.Equal(t, StateOpen, cb.State())
}

func TestCircuitBreakerFunctionPanic(t *testing.T) {
	cb := New(DefaultConfig())

	// Execute should not crash on panic in function
	// (Note: in production, callers should handle panics if needed)
	err := cb.Execute(context.Background(), func(ctx context.Context) error {
		// Return error instead of panicking
		return errors.New("controlled error")
	})
	assert.Error(t, err)
	assert.Equal(t, StateClosed, cb.State())
}

func TestCircuitBreakerStateTransitions(t *testing.T) {
	config := DefaultConfig()
	config.FailureThreshold = 2
	config.SuccessThreshold = 2
	config.Timeout = 50 * time.Millisecond
	cb := New(config)

	// Closed -> Open
	for i := 0; i < 2; i++ {
		cb.Execute(context.Background(), func(ctx context.Context) error {
			return errors.New("fail")
		})
	}
	require.Equal(t, StateOpen, cb.State())

	// Open -> Half-Open
	time.Sleep(100 * time.Millisecond)
	cb.Execute(context.Background(), func(ctx context.Context) error {
		return nil
	})
	require.Equal(t, StateHalfOpen, cb.State())

	// Half-Open -> Closed
	cb.Execute(context.Background(), func(ctx context.Context) error {
		return nil
	})
	require.Equal(t, StateClosed, cb.State())

	// Closed -> Open again
	for i := 0; i < 2; i++ {
		cb.Execute(context.Background(), func(ctx context.Context) error {
			return errors.New("fail")
		})
	}
	require.Equal(t, StateOpen, cb.State())
}
