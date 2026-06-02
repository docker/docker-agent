//go:build darwin

package termfeatures

/*
#cgo LDFLAGS: -framework CoreGraphics
#include <CoreGraphics/CoreGraphics.h>

static int isShiftPressed() {
    CGEventFlags flags = CGEventSourceFlagsState(kCGEventSourceStateCombinedSessionState);
    return (flags & kCGEventFlagMaskShift) != 0;
}
*/
import "C"

// IsShiftPressed queries the macOS CoreGraphics event system to determine
// whether the Shift key is currently held down. This allows us to distinguish
// Shift+Enter from Enter in terminals that don't support the Kitty keyboard
// protocol (like macOS Terminal.app), since those terminals send the same byte
// for both key combinations.
func IsShiftPressed() bool {
	return C.isShiftPressed() != 0
}
