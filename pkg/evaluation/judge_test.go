package evaluation

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewJudge(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name                string
		concurrency         int
		expectedConcurrency int
	}{
		{
			name:                "concurrency 0 defaults to 1",
			concurrency:         0,
			expectedConcurrency: 1,
		},
		{
			name:                "custom concurrency",
			concurrency:         5,
			expectedConcurrency: 5,
		},
		{
			name:                "negative concurrency defaults to 1",
			concurrency:         -3,
			expectedConcurrency: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			judge := NewJudge(nil, tt.concurrency)
			assert.Equal(t, tt.expectedConcurrency, judge.concurrency)
		})
	}
}

func TestJudge_CheckRelevance_EmptyCriteria(t *testing.T) {
	t.Parallel()

	judge := NewJudge(nil, 1)
	results, err := judge.CheckRelevance(t.Context(), "some response", nil)

	assert.Empty(t, results)
	assert.NoError(t, err)
}

func TestJudge_CheckRelevance_ContextCanceled(t *testing.T) {
	t.Parallel()

	judge := NewJudge(nil, 2)

	ctx, cancel := context.WithCancel(t.Context())
	cancel() // Cancel immediately

	criteria := []string{"criterion1", "criterion2", "criterion3"}
	results, err := judge.CheckRelevance(ctx, "some response", criteria)

	// All should have errors due to context cancellation
	assert.Len(t, results, len(criteria))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "context cancelled")
}

type judgeBackendFunc func(context.Context, string, string) (Judgment, error)

func (f judgeBackendFunc) Check(ctx context.Context, transcript, criterion string) (Judgment, error) {
	return f(ctx, transcript, criterion)
}

func TestJudgeBackendValidation(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name    string
		verdict Judgment
		err     error
		wantErr string
	}{
		{"pass", Judgment{Passed: true}, nil, ""},
		{"fail", Judgment{}, nil, "expected the test criterion to pass"},
		{"error", Judgment{}, context.DeadlineExceeded, "judge model validation failed"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			backend := judgeBackendFunc(func(_ context.Context, transcript, criterion string) (Judgment, error) {
				assert.Equal(t, "The sky is blue.", transcript)
				assert.Equal(t, "The response mentions a color.", criterion)
				return tt.verdict, tt.err
			})
			err := NewJudgeWithBackend(backend, 1).Validate(t.Context())
			if tt.wantErr == "" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, tt.wantErr)
				if tt.err != nil {
					require.ErrorIs(t, err, tt.err)
				}
			}
		})
	}
}

func TestJudgeBackendConcurrencyAndOrder(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	entered := make(chan string, 2)
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	backend := judgeBackendFunc(func(ctx context.Context, transcript, criterion string) (Judgment, error) {
		assert.Equal(t, "transcript", transcript)
		entered <- criterion
		select {
		case <-release:
		case <-ctx.Done():
			return Judgment{}, ctx.Err()
		}
		return Judgment{Passed: criterion == "first", Reason: criterion, Probability: new(0.75)}, nil
	})
	judge := NewJudgeWithBackend(backend, 2)
	done := make(chan struct{})
	var results []RelevanceResult
	var err error
	go func() {
		defer close(done)
		results, err = judge.CheckRelevance(ctx, "transcript", []string{"first", "second"})
	}()
	for range 2 {
		select {
		case <-entered:
		case <-ctx.Done():
			t.Fatal("backend checks did not run concurrently")
		}
	}
	unblock()
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("judge checks did not finish")
	}
	require.NoError(t, err)
	require.Len(t, results, 2)
	assert.Equal(t, "first", results[0].Criterion)
	assert.True(t, results[0].Passed)
	assert.Equal(t, "second", results[1].Criterion)
	assert.False(t, results[1].Passed)
	assert.Equal(t, "second", results[1].Reason)
	require.NotNil(t, results[1].Probability)
	assert.InDelta(t, 0.75, *results[1].Probability, 1e-9)
}
