package mcp

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCancellableParent_RoundTrip(t *testing.T) {
	parent, cancel := context.WithCancel(t.Context())
	defer cancel()

	ctx := withCancellableParent(context.WithoutCancel(parent), parent)
	got := cancellableParentFromContext(ctx)
	assert.Same(t, parent, got)
}

func TestCancellableParent_AbsentReturnsNil(t *testing.T) {
	got := cancellableParentFromContext(t.Context())
	assert.Nil(t, got)
}

func TestCancellableParent_NilParentIsNoOp(t *testing.T) {
	base := t.Context()
	got := withCancellableParent(base, nil)
	// No key attached, so cancellableParentFromContext returns nil --
	// the helper is a no-op when handed a nil parent.
	assert.Nil(t, cancellableParentFromContext(got))
}

// TestCancellableParent_DetachedCtxStaysIndependent verifies the
// invariant the helper preserves: even though the parent is reachable
// via the returned ctx as a value, the returned ctx itself does NOT
// inherit the parent's cancellation. That's exactly what makes the
// helper useful in clientConnector.Connect -- code that just calls
// ctx.Done() sees the detached ctx, code that opts in by reading
// cancellableParentFromContext can additionally observe the parent.
func TestCancellableParent_DetachedCtxStaysIndependent(t *testing.T) {
	parent, parentCancel := context.WithCancel(t.Context())
	defer parentCancel()

	detached := context.WithoutCancel(parent)
	ctx := withCancellableParent(detached, parent)

	parentCancel()

	// Local ctx is unaffected by parent cancellation (because of
	// WithoutCancel applied before withCancellableParent).
	select {
	case <-ctx.Done():
		t.Fatal("detached ctx must not be cancelled when its parent-by-value is cancelled")
	default:
	}

	// Parent reachable via value IS cancelled.
	got := cancellableParentFromContext(ctx)
	assert.NotNil(t, got)
	select {
	case <-got.Done():
		// expected
	default:
		t.Fatal("parent ctx retrieved from value should be cancelled")
	}
}
