package telemetry

import (
	"cmp"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"runtime"

	"github.com/docker/docker-agent/pkg/userid"
)

// getSystemInfo collects system information for events
func getSystemInfo() (osName, osVersion, osLanguage string) {
	osInfo := runtime.GOOS
	osLang := cmp.Or(os.Getenv("LANG"), "en-US")
	return osInfo, "", osLang
}

func GetTelemetryEnabled() bool {
	// Disable telemetry when running in tests to prevent HTTP calls
	if flag.Lookup("test.v") != nil {
		return false
	}
	return getTelemetryEnabledFromEnv()
}

// getTelemetryEnabledFromEnv checks only the environment variable,
// without the test detection bypass. This allows testing the env var logic.
func getTelemetryEnabledFromEnv() bool {
	if env := os.Getenv("TELEMETRY_ENABLED"); env != "" {
		// Only disable if explicitly set to "false"
		return env != "false"
	}
	// Default to true (telemetry enabled)
	return true
}

// getUserUUID returns the persistent UUID identifying this cagent
// installation, generating and persisting one on first use.
//
// It delegates to [userid.Get], which is also used by the HTTP
// transport so the same identifier appears as the `user_uuid`
// telemetry property and as the `X-Cagent-Id` header on gateway-bound
// requests.
func getUserUUID() string {
	return userid.Get()
}

// structToMap converts a struct to map[string]any using JSON marshaling
// This automatically handles all fields and respects JSON tags (including omitempty)
func structToMap(v any) (map[string]any, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal struct: %w", err)
	}

	var result map[string]any
	err = json.Unmarshal(data, &result)
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal to map: %w", err)
	}

	return result, nil
}

// CommandInfo represents the parsed command information
type CommandInfo struct {
	Action string
	Args   []string
	Flags  []string
}
