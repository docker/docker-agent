//go:build js

package filesystem

import "context"

// runPostEditCommands is a no-op under js/wasm (no os/exec available).
func runPostEditCommands(_ context.Context, _ []PostEditConfig, _ string) error {
	return nil
}
