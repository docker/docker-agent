package tools

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"maps"
	"reflect"
	"slices"
)

// Dispose permanently releases a toolset graph, including construction-time
// resources that never started. Call it only after tool execution has ended.
// Shared children are disposed once; composites' children, not their Stop
// methods, own the leaf cleanup. Composites may implement io.Closer for their
// own additional resources.
func Dispose(ctx context.Context, toolsets ...ToolSet) error {
	var errs []error
	for _, group := range disposalGroups(toolsets) {
		errs = append(errs, disposeGroup(ctx, group))
	}
	return errors.Join(errs...)
}

type disposalGroup struct {
	roots []ToolSet
	nodes map[ToolSet]struct{}
}

// Only roots sharing nodes must wait for each other's lifecycle operations.
func disposalGroups(toolsets []ToolSet) []disposalGroup {
	var groups []disposalGroup
	for _, root := range toolsets {
		group := disposalGroup{roots: []ToolSet{root}, nodes: make(map[ToolSet]struct{})}
		Walk(root, func(ts ToolSet) bool {
			return !seenToolSet(ts, group.nodes)
		})
		for i, existing := range slices.Backward(groups) {
			shared := false
			for node := range existing.nodes {
				if _, shared = group.nodes[node]; shared {
					break
				}
			}
			if shared {
				group.roots = append(existing.roots, group.roots...)
				maps.Copy(group.nodes, existing.nodes)
				groups = slices.Delete(groups, i, i+1)
			}
		}
		groups = append(groups, group)
	}
	return groups
}

func disposeGroup(ctx context.Context, group disposalGroup) error {
	var owners []*StartableToolSet
	seen := make(map[ToolSet]struct{})
	for _, root := range group.roots {
		Walk(root, func(ts ToolSet) bool {
			if seenToolSet(ts, seen) {
				return false
			}
			if owner, ok := ts.(*StartableToolSet); ok {
				owners = append(owners, owner)
			}
			return true
		})
	}
	// Close admission before waiting: Tools may lazily create resources too.
	pending := make([]<-chan struct{}, 0, len(owners))
	for _, owner := range owners {
		if done := owner.beginDisposal(); done != nil {
			pending = append(pending, done)
		}
	}
	for _, done := range pending {
		select {
		case <-done:
			continue
		default:
		}
		select {
		case <-done:
		case <-ctx.Done():
			deferDisposal(ctx, group)
			return ctx.Err()
		}
	}
	// Shared leaves must not be closed while any of their owners is starting.
	if err := lockDisposalOwners(ctx, owners); err != nil {
		deferDisposal(ctx, group)
		return err
	}
	defer func() {
		for _, owner := range slices.Backward(owners) {
			owner.unlock()
		}
	}()

	d := disposal{seen: make(map[ToolSet]struct{}), running: make(map[ToolSet]struct{}), stopped: make(map[ToolSet]struct{})}
	for _, ts := range group.roots {
		d.collectStops(ts, false)
	}
	var errs []error
	for _, ts := range group.roots {
		errs = append(errs, d.dispose(ctx, ts, false))
	}
	return errors.Join(errs...)
}

func deferDisposal(ctx context.Context, group disposalGroup) {
	go func() {
		if err := disposeGroup(context.WithoutCancel(ctx), group); err != nil {
			slog.WarnContext(ctx, "Failed to dispose toolsets after pending operation", "error", err)
		}
	}()
}

func (s *StartableToolSet) beginDisposal() <-chan struct{} {
	s.listingMu.Lock()
	defer s.listingMu.Unlock()
	s.disposing = true
	if s.listings != 0 {
		return s.listingsDone
	}
	return nil
}

func (s *StartableToolSet) disposalRequested() bool {
	s.listingMu.Lock()
	defer s.listingMu.Unlock()
	return s.disposing
}

// Release earlier locks before waiting: Start can hold an outer owner while
// waiting for an inner one, and concurrent disposals may visit different roots.
func lockDisposalOwners(ctx context.Context, owners []*StartableToolSet) error {
	for {
		locked := 0
		for locked < len(owners) && owners[locked].mu.TryLock() {
			locked++
		}
		if locked == len(owners) {
			return nil
		}
		// A pending parent Stop may acquire a child lock. Defer that
		// handshake until disposal owns (and has released) the whole graph.
		for _, owner := range owners[:locked] {
			owner.mu.Unlock()
		}
		if err := owners[locked].mu.LockContext(ctx); err != nil {
			return err
		}
		owners[locked].unlock()
	}
}

type disposal struct {
	seen    map[ToolSet]struct{}
	running map[ToolSet]struct{}
	stopped map[ToolSet]struct{}
}

// Aggregate every owner's requirement before cleanup deduplicates shared leaves.
func (d *disposal) collectStops(ts ToolSet, stopped bool) {
	if ts == nil {
		return
	}
	if owner, ok := ts.(*StartableToolSet); ok && owner != nil {
		if owner.disposed {
			return
		}
		stopped = owner.stopped
	}
	seen := d.running
	if stopped {
		seen = d.stopped
	}
	if seenToolSet(ts, seen) {
		return
	}
	if wrapper, ok := ts.(Unwrapper); ok {
		d.collectStops(wrapper.Unwrap(), stopped)
	}
	if composite, ok := ts.(Composite); ok {
		for _, child := range composite.Children() {
			d.collectStops(child, stopped)
		}
	}
}

func (d *disposal) dispose(ctx context.Context, ts ToolSet, stopped bool) error {
	if ts == nil || seenToolSet(ts, d.seen) {
		return nil
	}
	if owner, ok := ts.(*StartableToolSet); ok {
		if owner.disposed {
			return owner.disposeErr
		}
		owner.disposed = true
		if owner.run != nil {
			owner.run.stop()
			owner.run = nil
		}
		owner.disposeErr = errors.Join(owner.stopErr, d.dispose(ctx, owner.ToolSet, owner.stopped))
		owner.started = false
		return owner.disposeErr
	}

	var errs []error
	wrapper, wraps := ts.(Unwrapper)
	if wraps {
		errs = append(errs, d.dispose(ctx, wrapper.Unwrap(), stopped))
	}
	composite, combines := ts.(Composite)
	if combines {
		for _, child := range composite.Children() {
			errs = append(errs, d.dispose(ctx, child, stopped))
		}
	}
	if !wraps && !combines {
		mustStop := !stopped
		if reflect.ValueOf(ts).Comparable() {
			_, mustStop = d.running[ts]
		}
		if startable, ok := ts.(Startable); ok && mustStop {
			errs = append(errs, startable.Stop(ctx))
		}
	}
	if closer, ok := ts.(io.Closer); ok {
		errs = append(errs, closer.Close())
	}
	return errors.Join(errs...)
}

// Dispose is final cleanup, unlike Stop which permits a subsequent Start.
func (s *StartableToolSet) Dispose(ctx context.Context) error {
	return Dispose(ctx, s)
}
