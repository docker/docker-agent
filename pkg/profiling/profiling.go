// Package profiling provides helpers for CPU and memory profiling.
package profiling

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"runtime/pprof"
	"slices"
)

// Stop is a function returned by Start that stops profiling and flushes
// any buffered data. It must be called (typically via defer) when the
// profiled section of code completes.
type Stop func() error

// Start begins CPU and/or memory profiling based on the provided file
// paths. Pass an empty string to skip the corresponding profile.
// The returned Stop function must be called to finalise the profiles.
func Start(cpuProfile, memProfile string) (Stop, error) {
	var closers []func() error

	if cpuProfile != "" {
		f, err := os.OpenFile(cpuProfile, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			return noop, fmt.Errorf("failed to create CPU profile: %w", err)
		}
		if err := pprof.StartCPUProfile(f); err != nil {
			f.Close()
			return noop, fmt.Errorf("failed to start CPU profile: %w", err)
		}
		closers = append(closers, func() error {
			pprof.StopCPUProfile()
			return f.Close()
		})
	}

	if memProfile != "" {
		closers = append(closers, func() error {
			f, err := os.OpenFile(memProfile, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
			if err != nil {
				return fmt.Errorf("failed to create memory profile: %w", err)
			}
			defer f.Close()
			runtime.GC()
			if err := pprof.WriteHeapProfile(f); err != nil {
				return fmt.Errorf("failed to write memory profile: %w", err)
			}
			return nil
		})
	}

	return func() error {
		// Run in reverse order so CPU profile is stopped before mem profile is written.
		var errs []error
		for _, closeFn := range slices.Backward(closers) {
			if err := closeFn(); err != nil {
				errs = append(errs, err)
			}
		}
		return errors.Join(errs...)
	}, nil
}

func noop() error { return nil }
