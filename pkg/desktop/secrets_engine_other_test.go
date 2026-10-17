//go:build !darwin && !linux && !windows

package desktop

import "testing"

func installNoSecretsEngine(*testing.T) {}
