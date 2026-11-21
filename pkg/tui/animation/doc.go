// Package animation defines the timing for repo-owned visual effects in the
// full-screen TUI, lean TUI, and board. Ticks ask the UI to sample; they do not
// determine how far an animation has moved. TickMsg.Frame is legacy only.
//
// Use Transition for runtime-owned slides and fades and ProgressAt for a
// custom fade or sweep. Both clamp elapsed progress and accept the same easing
// functions. Use TimedStep or Spinner.FrameAt for discrete glyphs or cells;
// divide elapsed time by the intended step duration without easing.
//
// Full-screen and board runtimes sample at 14 Hz (about 71 ms). A change
// appears on the next sample, not necessarily on its exact time boundary. A
// delayed sample goes straight to the position its elapsed time calls for;
// it does not replay missed frames. Runtime clocks exclude paused and idle
// intervals. The lean TUI samples elapsed time while its busy UI is active
// and keeps its independent 100 ms repaint wake-up. An effect tied to a
// pre-existing wall deadline (such as the reasoning-tool fade) can instead
// use elapsed wall time so expiry semantics do not change.
//
// Timers are not inherently animations. Selection autoscroll repeats an input
// action; clipboard or board flashes, notifications, tour delays, and agent
// transfer deadlines trigger state changes; tmux visibility, program status,
// and resize timers poll external state. The evaluation progress display also
// polls state: the bar is derived from completed evaluations, not refreshes.
// These timers, and dependency-owned effects such as cursor blinking, are not
// governed by the animation clock.
package animation
