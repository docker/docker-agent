package compaction

import (
	"math"
	"testing"
)

// TestShouldCompactNaNThreshold pins that a NaN threshold falls back to
// DefaultThreshold instead of underflowing int64(NaN) and reporting "compact"
// on any usage.
func TestShouldCompactNaNThreshold(t *testing.T) {
	t.Parallel()

	if ShouldCompact(1, 1, 1, 100000, math.NaN()) {
		t.Fatal("a NaN threshold must fall back to DefaultThreshold, not compact tiny usage")
	}
	if !ShouldCompact(95000, 0, 0, 100000, DefaultThreshold) {
		t.Fatal("usage above the default threshold must still compact")
	}
}
