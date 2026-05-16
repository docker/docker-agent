package animation

import tea "charm.land/bubbletea/v2"

// Subscription represents a component's subscription to animation ticks.
// It encapsulates the registration/unregistration lifecycle, making it
// easier to manage animation state correctly.
//
// Usage:
//
//	type MyComponent struct {
//	    animSub animation.Subscription
//	}
//
//	func (m *MyComponent) Init() tea.Cmd {
//	    return m.animSub.Start()
//	}
//
//	func (m *MyComponent) Cleanup() {
//	    m.animSub.Stop()
//	}
type Subscription struct {
	active bool
}

// Start activates the subscription if not already active.
// Returns a command to start the tick if this is the first subscription.
// Safe to call multiple times - only the first call registers.
func (s *Subscription) Start() tea.Cmd {
	if s.active {
		return nil
	}
	s.active = true
	return StartTickIfFirst()
}

// Stop deactivates the subscription if currently active.
// Safe to call multiple times - only the first call unregisters.
func (s *Subscription) Stop() {
	if !s.active {
		return
	}
	s.active = false
	Unregister()
}

// IsActive returns whether the subscription is currently active.
func (s *Subscription) IsActive() bool {
	return s.active
}

// Reset returns a new inactive subscription.
// Useful when recreating a component that needs fresh animation state.
func (s *Subscription) Reset() Subscription {
	s.Stop()
	return Subscription{}
}
