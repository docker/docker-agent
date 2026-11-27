package animation

import (
	"slices"
	"time"

	tea "charm.land/bubbletea/v2"
)

type hoverItem struct {
	motion   Transition
	from, to float64
}

func (h *hoverItem) fraction() float64 {
	return h.from + (h.to-h.from)*h.motion.Value()
}

// HoverFade tracks independent fade-in/fade-out hover transitions keyed by K.
// A nil runtime makes all operations safe no-ops.
type HoverFade[K comparable] struct {
	ar       *Runtime
	duration time.Duration
	easing   EasingFunc
	items    map[K]*hoverItem
}

// NewHoverFade creates a HoverFade bound to ar with the given duration and easing.
func NewHoverFade[K comparable](ar *Runtime, duration time.Duration, easing EasingFunc) HoverFade[K] {
	return HoverFade[K]{
		ar:       ar,
		duration: duration,
		easing:   easing,
	}
}

// Set fades the given target keys toward 1 and all other tracked keys toward 0.
// Zero-valued keys in targets are ignored.
func (h *HoverFade[K]) Set(targets ...K) tea.Cmd {
	if h.ar == nil {
		return nil
	}
	var zero K
	var cmds []tea.Cmd
	for key, item := range h.items {
		if !slices.Contains(targets, key) && item.to != 0 {
			item.from, item.to = item.fraction(), 0
			if cmd := item.motion.Start(h.duration, h.easing); cmd != nil {
				cmds = append(cmds, cmd)
			}
		}
	}
	for _, key := range targets {
		if key == zero {
			continue
		}
		if h.items == nil {
			h.items = make(map[K]*hoverItem)
		}
		item := h.items[key]
		if item == nil {
			item = &hoverItem{motion: h.ar.Transition()}
			h.items[key] = item
		}
		if item.to != 1 {
			item.from, item.to = item.fraction(), 1
			if cmd := item.motion.Start(h.duration, h.easing); cmd != nil {
				cmds = append(cmds, cmd)
			}
		}
	}
	return tea.Batch(cmds...)
}

// Fraction returns the current interpolated hover value in [0, 1] for key.
func (h *HoverFade[K]) Fraction(key K) float64 {
	if item := h.items[key]; item != nil {
		return item.fraction()
	}
	return 0
}

// Len reports how many keys are currently hovered or fading out.
func (h *HoverFade[K]) Len() int { return len(h.items) }

// Tick advances all running hover transitions and prunes keys that finished
// fading to 0. Returns true if any transition advanced.
func (h *HoverFade[K]) Tick(tick TickMsg) bool {
	changed := false
	for key, item := range h.items {
		if item.motion.Running() {
			item.motion.Tick()
			tick.MarkDirty()
			changed = true
		}
		if !item.motion.Running() && item.to == 0 {
			delete(h.items, key)
		}
	}
	return changed
}

// Clear immediately cancels all hover transitions and resets tracked keys.
// Returns true if any keys were present.
func (h *HoverFade[K]) Clear() bool {
	had := len(h.items) > 0
	for _, item := range h.items {
		item.motion.Cancel()
	}
	clear(h.items)
	return had
}
