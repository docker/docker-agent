//go:build !darwin

package termfeatures

// IsShiftPressed returns false on non-macOS platforms. The CoreGraphics-based
// modifier detection is only available on macOS.
func IsShiftPressed() bool {
	return false
}
