//go:build !darwin || !cgo || no_audio

package capture

import "context"

// Run returns ErrNotSupported when native audio capture is unavailable.
func (c *Capturer) Run(context.Context, string, AudioHandler, func()) error {
	return ErrNotSupported
}
