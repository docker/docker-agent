package a2a

import (
	"encoding/json"
	"errors"
	"iter"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	adka2a "google.golang.org/adk/v2/server/adka2a/v2"
)

func sourceOf(events ...a2a.Event) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		for _, event := range events {
			if !yield(event, nil) {
				return
			}
		}
	}
}

func collect(t *testing.T, events iter.Seq2[a2a.Event, error]) []a2a.Event {
	t.Helper()
	var collected []a2a.Event
	for event, err := range events {
		require.NoError(t, err)
		collected = append(collected, event)
	}
	return collected
}

func TestFixArtifactEvents_NilParts(t *testing.T) {
	t.Parallel()

	// Create an artifact update event with nil Parts
	source := sourceOf(&a2a.TaskArtifactUpdateEvent{
		ContextID: "test-context",
		TaskID:    "test-task",
		Append:    true,
		Artifact: &a2a.Artifact{
			ID:    "test-artifact",
			Parts: nil,
		},
	})

	events := collect(t, fixArtifactEvents(source))
	require.Len(t, events, 1)

	fixed := events[0].(*a2a.TaskArtifactUpdateEvent)
	assert.NotNil(t, fixed.Artifact.Parts)
	assert.Empty(t, fixed.Artifact.Parts)

	// Verify it serializes correctly
	data, err := json.Marshal(fixed.Artifact)
	require.NoError(t, err)

	assert.JSONEq(t, `{"artifactId":"test-artifact","parts":[]}`, string(data))
}

func TestFixArtifactEvents_WithParts(t *testing.T) {
	t.Parallel()

	// Create an artifact update event with actual parts
	source := sourceOf(&a2a.TaskArtifactUpdateEvent{
		ContextID: "test-context",
		TaskID:    "test-task",
		Append:    true,
		Artifact: &a2a.Artifact{
			ID:    "test-artifact",
			Parts: a2a.ContentParts{a2a.NewTextPart("Hello")},
		},
	})

	events := collect(t, fixArtifactEvents(source))
	require.Len(t, events, 1)

	// Verify the event was yielded unchanged
	fixed := events[0].(*a2a.TaskArtifactUpdateEvent)
	require.Len(t, fixed.Artifact.Parts, 1)
	assert.Equal(t, "Hello", fixed.Artifact.Parts[0].Text())
}

func TestFixArtifactEvents_NonArtifactEvent(t *testing.T) {
	t.Parallel()

	// Create a different type of event
	execCtx := &a2asrv.ExecutorContext{
		TaskID:    "test-task",
		ContextID: "test-context",
	}
	event := a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateCompleted, nil)

	events := collect(t, fixArtifactEvents(sourceOf(event)))

	// Verify the event was yielded unchanged
	require.Len(t, events, 1)
	assert.Same(t, event, events[0])
}

func TestFixArtifactEvents_ErrorPassthrough(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("agent run failed")
	source := func(yield func(a2a.Event, error) bool) {
		yield(nil, wantErr)
	}

	var errs []error
	for event, err := range fixArtifactEvents(source) {
		assert.Nil(t, event)
		errs = append(errs, err)
	}
	require.Len(t, errs, 1)
	require.ErrorIs(t, errs[0], wantErr)
}

func TestFixArtifactEvents_StopsSourceWhenConsumerStops(t *testing.T) {
	t.Parallel()

	execCtx := &a2asrv.ExecutorContext{
		TaskID:    "test-task",
		ContextID: "test-context",
	}
	yielded := 0
	source := func(yield func(a2a.Event, error) bool) {
		for {
			yielded++
			if !yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateWorking, nil), nil) {
				return
			}
		}
	}

	for range fixArtifactEvents(source) {
		break
	}

	assert.Equal(t, 1, yielded)
}

func TestExecutorWrapper_Cancel(t *testing.T) {
	t.Parallel()

	wrapper := newExecutorWrapper(adka2a.ExecutorConfig{})
	execCtx := &a2asrv.ExecutorContext{
		TaskID:    "test-task",
		ContextID: "test-context",
	}

	events := collect(t, wrapper.Cancel(t.Context(), execCtx))
	require.Len(t, events, 1)

	statusEvent, ok := events[0].(*a2a.TaskStatusUpdateEvent)
	require.True(t, ok)
	assert.Equal(t, a2a.TaskStateCanceled, statusEvent.Status.State)
}
