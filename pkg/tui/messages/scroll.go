package messages

// WheelCoalescedMsg aggregates mouse wheel deltas to reduce render storms.
// Delta is positive for wheel down and negative for wheel up.
type WheelCoalescedMsg struct {
	Delta int
	X     int
	Y     int
}
