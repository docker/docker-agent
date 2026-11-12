package tuitest

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// liveFrames streams each captured frame to stderr. Use with `go test -v` for
// an approximate live view of what the harness sees.
var liveFrames = flag.Bool("tuitest.live", false, "stream captured TUI frames to stderr while tests run")

// dumpFrames writes captured frames under testing.TB.ArtifactDir.
// Use -artifacts to retain dumps after the test finishes.
var dumpFrames = flag.Bool("tuitest.frames", false, "dump captured TUI frames to the test artifact directory (use -artifacts to retain)")

// liveWriter is overridden by unit tests.
var liveWriter io.Writer = os.Stderr

var liveMu sync.Mutex

// frameSink receives captured frames for optional debugging side effects.
type frameSink interface {
	frame(frame string)
	err() error
}

type debugSink struct {
	mu       sync.Mutex
	live     bool
	dumpDir  string
	index    int
	firstErr error
}

func newDebugSink(tb testing.TB) frameSink {
	tb.Helper()

	if !*liveFrames && !*dumpFrames {
		return nil
	}

	s := &debugSink{live: *liveFrames}
	if *dumpFrames {
		// A test can create multiple drivers; keep their frame sequences separate.
		dir, err := os.MkdirTemp(tb.ArtifactDir(), "frames-") //nolint:usetesting // TempDir would discard dumps even with -artifacts.
		if err != nil {
			tb.Fatalf("tuitest: creating frame dump dir: %v", err)
		}
		s.dumpDir = dir
		tb.Logf("tuitest: dumping frames to %s", s.dumpDir)
	}
	return s
}

func (s *debugSink) frame(frame string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.firstErr != nil {
		return
	}

	s.index++
	if s.live {
		// Clear screen + home cursor, then render the current frame. This keeps
		// the output readable in an attached terminal while still being plain text
		// enough to inspect in `go test -v` logs.
		liveMu.Lock()
		_, err := fmt.Fprintf(liveWriter, "\x1b[2J\x1b[H--- tuitest frame %04d ---\n%s\n", s.index, frame)
		liveMu.Unlock()
		if err != nil {
			s.firstErr = err
			return
		}
	}
	if s.dumpDir != "" {
		path := filepath.Join(s.dumpDir, fmt.Sprintf("%04d.txt", s.index))
		if err := os.WriteFile(path, []byte(frame), 0o600); err != nil {
			s.firstErr = err
		}
	}
}

func (s *debugSink) err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.firstErr
}
