// Package configsize limits remotely loaded agent configurations.
package configsize

import (
	"fmt"
	"io"
)

const MaxBytes int64 = 32 << 20

var ErrTooLarge = fmt.Errorf("agent configuration exceeds %d MiB limit", MaxBytes>>20)

func Read(r io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, MaxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > MaxBytes {
		return nil, fmt.Errorf("%w (%d bytes maximum)", ErrTooLarge, MaxBytes)
	}
	return data, nil
}
