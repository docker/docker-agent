package runtime

import "time"

// defaultEventChannelCapacity is the buffer size used for every Event
// channel the runtime hands back to a caller. Sized large enough that a
// reasonable producer (the run loop, a stream forwarder) can stay ahead of
// a typical consumer (the TUI, an API server) without blocking, but small
// enough to keep memory pressure bounded when consumers are slow.
//
// All event-channel constructors in pkg/runtime go through this constant
// so the buffer size is set in exactly one place.
const defaultEventChannelCapacity = 128

// defaultMaxOverflowCompactions caps the number of consecutive
// context-overflow auto-compactions that the run loop will attempt before
// giving up and surfacing the error to the caller. The runtime's
// compaction routine cannot guarantee that the resulting transcript will
// fit inside the model's context window (very large recent messages may
// still exceed it), so this bound prevents an infinite retry loop.
//
// Production uses 1 (one compaction attempt per stream); tests can change
// this via WithMaxOverflowCompactions to verify both the "compaction
// succeeded" and "compaction exhausted" branches without having to drive
// many real overflows.
const defaultMaxOverflowCompactions = 1

// toolsChangedTimeout bounds how long a single MCP-tool-change refresh
// may take. The handler is invoked outside any RunStream goroutine (it's
// a notification from a server), so a slow or stuck server cannot be
// allowed to wedge the caller indefinitely.
const toolsChangedTimeout = 5 * time.Second
