package runtime

import (
	"errors"
	"strings"
	"sync/atomic"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/harness"
)

// harnessFactory is empty until a driver is registered (the CLI and
// loaderdefaults.Opts register pkg/codingharness). The indirection keeps the
// harness CLI drivers out of pkg/runtime's import graph for embedders that
// never run harness agents.
var harnessFactory atomic.Pointer[harness.Factory]

// RegisterHarness installs the factory used to run `harness:` agents:
//
//	runtime.RegisterHarness(codingharness.Factory)
//
// Prefer [WithHarnessFactory] to configure independent runtimes.
func RegisterHarness(factory harness.Factory) {
	harnessFactory.Store(&factory)
}

// ErrHarnessNotRegistered is returned when an agent declares a harness but no
// driver was registered with RegisterHarness.
var ErrHarnessNotRegistered = errors.New("harness agents need a driver: call runtime.RegisterHarness(codingharness.Factory)")

// WithHarnessFactory selects the harness driver for this runtime only.
// Passing nil disables harnesses, even when a global driver is registered.
// Without this option, the runtime uses [RegisterHarness]'s global driver.
func WithHarnessFactory(factory harness.Factory) Opt {
	return func(r *LocalRuntime) {
		r.harnessFactory = &factory
	}
}

func (r *LocalRuntime) newHarnessProvider(cfg *latest.HarnessConfig) (harness.Provider, error) {
	factory := r.harnessFactory
	if factory == nil {
		factory = harnessFactory.Load()
	}
	if factory == nil || *factory == nil {
		return nil, ErrHarnessNotRegistered
	}
	return (*factory)(cfg)
}

// harnessLabel is the model-slot label shown for a harness agent, e.g.
// "claude-code/opus".
func harnessLabel(cfg *latest.HarnessConfig) string {
	if cfg == nil {
		return ""
	}
	model := strings.TrimSpace(cfg.Model)
	if model == "" {
		return cfg.Type
	}
	return cfg.Type + "/" + model
}
