// Package subscription provides patterns for external event sources in the TUI.
//
// In the Elm Architecture, subscriptions declare interest in external events
// (time, WebSocket messages, etc.). This package provides similar patterns
// for Go/Bubble Tea, making external event sources explicit and manageable.
//
// # Pattern Overview
//
// A subscription converts an external event source (channel, timer, etc.)
// into a tea.Cmd that returns tea.Msg values. The TUI's Update function
// then processes these messages like any other.
//
// # Example Usage
//
//	// In your model
//	type model struct {
//	    eventCh chan ExternalEvent
//	}
//
//	// Create a listener that converts channel events to messages
//	func (m *model) listenForEvents() tea.Cmd {
//	    return subscription.FromChannel(m.eventCh, func(e ExternalEvent) tea.Msg {
//	        return MyEventMsg{Event: e}
//	    })
//	}
//
//	// In Init, start listening
//	func (m *model) Init() tea.Cmd {
//	    return m.listenForEvents()
//	}
//
//	// In Update, handle the message and re-subscribe
//	func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
//	    switch msg := msg.(type) {
//	    case MyEventMsg:
//	        // Handle the event
//	        // Re-subscribe to continue listening
//	        return m, m.listenForEvents()
//	    }
//	}
package subscription

import tea "charm.land/bubbletea/v2"

// FromChannel creates a tea.Cmd that waits for a value from the channel
// and converts it to a tea.Msg using the provided function.
//
// The returned Cmd blocks until a value is received or the channel is closed.
// When the channel is closed, it returns nil (no message).
//
// To continue listening after receiving a message, call this function again
// in your Update handler (re-subscription pattern).
func FromChannel[T any](ch <-chan T, toMsg func(T) tea.Msg) tea.Cmd {
	return func() tea.Msg {
		val, ok := <-ch
		if !ok {
			return nil // Channel closed
		}
		return toMsg(val)
	}
}

// FromChannelWithClose is like FromChannel but also calls onClose when the
// channel is closed, allowing cleanup or final messages.
func FromChannelWithClose[T any](ch <-chan T, toMsg func(T) tea.Msg, onClose func() tea.Msg) tea.Cmd {
	return func() tea.Msg {
		val, ok := <-ch
		if !ok {
			if onClose != nil {
				return onClose()
			}
			return nil
		}
		return toMsg(val)
	}
}

// ChannelSubscription wraps a channel with helper methods for the
// re-subscription pattern common in Bubble Tea.
type ChannelSubscription[T any] struct {
	ch    <-chan T
	toMsg func(T) tea.Msg
}

// NewChannelSubscription creates a subscription for the given channel.
func NewChannelSubscription[T any](ch <-chan T, toMsg func(T) tea.Msg) *ChannelSubscription[T] {
	return &ChannelSubscription[T]{ch: ch, toMsg: toMsg}
}

// Listen returns a Cmd that waits for the next value from the channel.
// Call this in Init() to start listening, and again in Update() after
// handling each message to continue listening.
func (s *ChannelSubscription[T]) Listen() tea.Cmd {
	return FromChannel(s.ch, s.toMsg)
}
