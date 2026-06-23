// Package userconfig provides user-level configuration for docker agent.
// This configuration is stored in ~/.config/cagent/config.yaml and contains
// user preferences like aliases.
package userconfig

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/goccy/go-yaml"

	"github.com/docker/docker-agent/pkg/atomicfile"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/paths"
)

// Alias represents an alias configuration with optional runtime settings
type Alias struct {
	// Path is the agent file path or OCI reference
	Path string `yaml:"path" json:"path"`
	// Yolo enables auto-approve mode for all tool calls
	Yolo bool `yaml:"yolo,omitempty" json:"yolo,omitempty"`
	// Model overrides the agent's model (format: [agent=]provider/model)
	Model string `yaml:"model,omitempty" json:"model,omitempty"`
	// HideToolResults hides tool call results in the TUI
	HideToolResults bool `yaml:"hide_tool_results,omitempty" json:"hide_tool_results,omitempty"`
	// Sandbox runs the agent inside a Docker sandbox by default.
	Sandbox bool `yaml:"sandbox,omitempty" json:"sandbox,omitempty"`
}

// HasOptions returns true if the alias has any runtime options set
func (a *Alias) HasOptions() bool {
	return a != nil && (a.Yolo || a.Model != "" || a.HideToolResults || a.Sandbox)
}

// Settings represents global user settings
type Settings struct {
	// HideToolResults hides tool call results in the TUI by default
	HideToolResults bool `yaml:"hide_tool_results,omitempty"`
	// ExpandThinking expands reasoning/tool blocks in the TUI by default.
	// Defaults to false when not set.
	ExpandThinking *bool `yaml:"expand_thinking,omitempty"`
	// SplitDiffView enables side-by-side split diff rendering for file edits.
	// Defaults to true when not set.
	SplitDiffView *bool `yaml:"split_diff_view,omitempty"`
	// Theme is the default theme reference (e.g., "dark", "light")
	// Theme files are loaded from ~/.cagent/themes/<theme>.yaml
	Theme string `yaml:"theme,omitempty"`
	// YOLO enables auto-approve mode for all tool calls globally
	YOLO bool `yaml:"YOLO,omitempty"`
	// Lean makes the simplified TUI with minimal chrome the default UI.
	Lean bool `yaml:"lean,omitempty"`
	// TabTitleMaxLength is the maximum display length for tab titles in the TUI.
	// Titles longer than this are truncated with an ellipsis. Defaults to 20.
	TabTitleMaxLength int `yaml:"tab_title_max_length,omitempty"`
	// RestoreTabs restores previously open tabs when launching the TUI.
	// Defaults to false when not set (user must explicitly opt-in).
	RestoreTabs *bool `yaml:"restore_tabs,omitempty"`
	// Sound enables playing notification sounds on task success or failure.
	// Defaults to false (user must explicitly opt-in).
	Sound bool `yaml:"sound,omitempty"`
	// SoundThreshold is the minimum duration in seconds a task must run
	// before a success sound is played. Defaults to 5 seconds.
	SoundThreshold int `yaml:"sound_threshold,omitempty"`
	// Snapshot enables automatic shadow-git snapshots globally when true.
	Snapshot *bool `yaml:"snapshot,omitempty"`
	// Permissions defines global permission patterns applied across all sessions
	// and agents. These act as user-wide defaults; session-level and agent-level
	// permissions override them.
	Permissions *latest.PermissionsConfig `yaml:"permissions,omitempty"`
}

// DefaultTabTitleMaxLength is the default maximum tab title length when not configured.
const DefaultTabTitleMaxLength = 20

// DefaultSoundThreshold is the default duration threshold for sound notifications.
const DefaultSoundThreshold = 10

// GetTabTitleMaxLength returns the configured tab title max length, falling back to the default.
func (s *Settings) GetTabTitleMaxLength() int {
	if s == nil || s.TabTitleMaxLength <= 0 {
		return DefaultTabTitleMaxLength
	}
	return s.TabTitleMaxLength
}

// GetSound returns whether sound notifications are enabled, defaulting to false.
func (s *Settings) GetSound() bool {
	if s == nil {
		return false
	}
	return s.Sound
}

// GetSoundThreshold returns the minimum duration for sound notifications, defaulting to 10s.
func (s *Settings) GetSoundThreshold() int {
	if s == nil || s.SoundThreshold <= 0 {
		return DefaultSoundThreshold
	}
	return s.SoundThreshold
}

// GetExpandThinking returns whether reasoning/tool blocks are expanded by default.
func (s *Settings) GetExpandThinking() bool {
	if s == nil || s.ExpandThinking == nil {
		return false
	}
	return *s.ExpandThinking
}

// GetSplitDiffView returns whether split diff view is enabled, defaulting to true.
func (s *Settings) GetSplitDiffView() bool {
	if s == nil || s.SplitDiffView == nil {
		return true
	}
	return *s.SplitDiffView
}

// SnapshotsEnabled returns whether global snapshot auto-injection is enabled.
func (s *Settings) SnapshotsEnabled() bool {
	return s != nil && s.Snapshot != nil && *s.Snapshot
}

// CredentialHelper contains configuration for a credential helper command
// that retrieves Docker credentials (DOCKER_TOKEN) from an external source.
type CredentialHelper struct {
	// Command is the CLI command to execute to retrieve the Docker token.
	// The command should output the token on stdout.
	Command string   `yaml:"command,omitempty"`
	Args    []string `yaml:"args,omitempty"`
}

// CurrentVersion is the current version of the user config format
const CurrentVersion = "v1"

// Config represents the user-level docker agent configuration
type Config struct {
	// mu protects concurrent access to the Aliases map.
	// Config methods may be called from parallel tests or goroutines.
	mu sync.Mutex

	// Version is the config format version
	Version string `yaml:"version,omitempty"`
	// ModelsGateway is the default models gateway URL
	ModelsGateway string `yaml:"models_gateway,omitempty"`
	// DefaultModel is the default model to use when model is set to "auto".
	// Supports both shorthand ("provider/model") and full model definition.
	DefaultModel *latest.FlexibleModelConfig `yaml:"default_model,omitempty"`
	// Aliases maps alias names to alias configurations
	Aliases map[string]*Alias `yaml:"aliases,omitempty"`
	// Settings contains global user settings
	Settings *Settings `yaml:"settings,omitempty"`
	// CredentialHelper configures an external command to retrieve Docker credentials
	CredentialHelper *CredentialHelper `yaml:"credential_helper,omitempty"`
	// SandboxAllowlist is the persistent list of hosts the user has
	// taught docker-agent to open in the sandbox proxy on every run
	// (in addition to the gateway, the kit-resolved tool install
	// hosts, and the agent-declared runtime.network_allowlist).
	// Managed via `docker agent sandbox allow/deny/list`. Each entry
	// is a hostname with an optional ":port" suffix; commas and
	// whitespace are rejected at write time.
	SandboxAllowlist []string `yaml:"sandbox_allowlist,omitempty"`
}

// Path returns the path to the config file
func Path() string {
	return filepath.Join(paths.GetConfigDir(), "config.yaml")
}

// legacyAliasesPath returns the path to the legacy aliases.yaml file
func legacyAliasesPath() string {
	return filepath.Join(paths.GetConfigDir(), "aliases.yaml")
}

// Load loads the user configuration from the config file.
// If the config file doesn't exist but a legacy aliases.yaml does,
// the aliases are migrated to the new config file.
func Load() (*Config, error) {
	return loadFrom(Path(), legacyAliasesPath())
}

func loadFrom(configPath, legacyPath string) (*Config, error) {
	config, err := readConfig(configPath)
	if err != nil {
		return nil, err
	}

	// Try migrating from legacy file if no aliases exist yet
	if len(config.Aliases) == 0 && config.migrateFromLegacy(legacyPath) {
		if err := config.saveTo(configPath); err != nil {
			return nil, fmt.Errorf("failed to save migrated config: %w", err)
		}
	}

	return config, nil
}

// readConfig reads and parses the config file, returning an empty config if file doesn't exist.
func readConfig(configPath string) (*Config, error) {
	config := &Config{Aliases: make(map[string]*Alias)}

	data, err := os.ReadFile(configPath)
	if err != nil {
		if os.IsNotExist(err) {
			return config, nil
		}
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	if err := yaml.Unmarshal(data, config); err != nil {
		return nil, fmt.Errorf("failed to parse config file: %w", err)
	}

	if config.Aliases == nil {
		config.Aliases = make(map[string]*Alias)
	}

	return config, nil
}

// migrateFromLegacy migrates aliases from the legacy aliases.yaml file.
// Returns true if any aliases were migrated.
// After successful migration, the legacy file is deleted.
func (c *Config) migrateFromLegacy(legacyPath string) bool {
	if legacyPath == "" {
		return false
	}

	data, err := os.ReadFile(legacyPath)
	if err != nil {
		return false
	}

	var legacy map[string]string
	if err := yaml.Unmarshal(data, &legacy); err != nil {
		slog.Warn("Failed to parse legacy aliases file", "path", legacyPath, "error", err)
		return false
	}

	if len(legacy) == 0 {
		return false
	}

	// Protect concurrent writes to the Aliases map while migrating
	// legacy aliases. This avoids concurrent map write panics if
	// the config is accessed by multiple goroutines.
	c.mu.Lock()
	defer c.mu.Unlock()

	for name, path := range legacy {
		c.Aliases[name] = &Alias{Path: path}
	}

	slog.Info("Migrated aliases from legacy file", "path", legacyPath, "count", len(legacy))

	if err := os.Remove(legacyPath); err != nil {
		slog.Warn("Failed to remove legacy aliases file", "path", legacyPath, "error", err)
	}

	return true
}

// Save saves the configuration to the config file
func (c *Config) Save() error {
	return c.saveTo(Path())
}

func (c *Config) saveTo(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("failed to create config directory: %w", err)
	}

	// Ensure version is always set to current version when saving
	c.Version = CurrentVersion

	data, err := yaml.Marshal(c)
	if err != nil {
		return fmt.Errorf("failed to marshal config: %w", err)
	}

	// The config may contain a credential helper command, so restrict it
	// to the user.
	return atomicfile.Write(path, bytes.NewReader(data), 0o600)
}

// GetAlias retrieves the alias configuration for a given name.
//
// This method is safe for concurrent use. Reads from the Aliases map
// are protected by a mutex to avoid concurrent read/write panics when
// aliases are accessed while being modified.
func (c *Config) GetAlias(name string) (*Alias, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	alias, ok := c.Aliases[name]
	return alias, ok
}

// validAliasNameRegex matches valid alias names: alphanumeric characters, hyphens, and underscores.
// Must start with an alphanumeric character.
var validAliasNameRegex = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]*$`)

// ValidateAliasName checks if an alias name is valid.
// Valid names must:
// - Not be empty
// - Start with an alphanumeric character
// - Contain only alphanumeric characters, hyphens, and underscores
// - Not contain path separators or special characters
func ValidateAliasName(name string) error {
	if name == "" {
		return errors.New("alias name cannot be empty")
	}
	if !validAliasNameRegex.MatchString(name) {
		return fmt.Errorf("invalid alias name %q: must start with a letter or digit and contain only letters, digits, hyphens, and underscores", name)
	}
	return nil
}

// SetAlias creates or updates an alias.
// Returns an error if the alias name or alias configuration is invalid.
//
// This method is safe for concurrent use. Writes to the Aliases map
// are protected by a mutex to avoid concurrent map write panics when
// aliases are modified from multiple goroutines.
func (c *Config) SetAlias(name string, alias *Alias) error {
	if err := ValidateAliasName(name); err != nil {
		return err
	}
	if alias == nil || alias.Path == "" {
		return errors.New("agent path cannot be empty")
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	c.Aliases[name] = alias
	return nil
}

// DeleteAlias removes an alias by name.
// It returns true if the alias existed.
//
// This method is safe for concurrent use. Access to the Aliases map
// is protected by a mutex to prevent concurrent map read/write panics
// when called from parallel tests or goroutines.
func (c *Config) DeleteAlias(name string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	if _, exists := c.Aliases[name]; exists {
		delete(c.Aliases, name)
		return true
	}
	return false
}

// GetSettings returns the global settings with defaults applied.
func (c *Config) GetSettings() *Settings {
	if c.Settings == nil {
		return &Settings{RestoreTabs: new(false)}
	}
	if c.Settings.RestoreTabs == nil {
		c.Settings.RestoreTabs = new(false)
	}
	return c.Settings
}

// Get returns the global user settings from the config file.
// Returns an empty Settings if the config file doesn't exist or has no settings.
func Get() *Settings {
	cfg, err := Load()
	if err != nil {
		return &Settings{}
	}
	return cfg.GetSettings()
}

// AddSandboxHosts appends host(s) to SandboxAllowlist, preserving
// insertion order and skipping duplicates. Each entry is trimmed of
// surrounding whitespace; commas and embedded whitespace are
// rejected because the sandbox network policy joins entries with
// commas downstream and a single value containing one of those
// would silently smuggle several distinct rules into the engine.
//
// All entries are validated before any mutation: a malformed value
// in the batch leaves c.SandboxAllowlist unchanged so callers that
// reuse the *Config after a failed call still observe a consistent
// in-memory view.
//
// Returns the list of hosts that were actually added (i.e. not
// already present), so callers can report "already allowed" without
// re-walking the slice.
func (c *Config) AddSandboxHosts(hosts ...string) ([]string, error) {
	cleaned := make([]string, 0, len(hosts))
	for _, h := range hosts {
		h = strings.TrimSpace(h)
		if h == "" {
			continue
		}
		if strings.ContainsAny(h, ", \t") {
			return nil, fmt.Errorf("refusing to allowlist host %q: contains comma or whitespace", h)
		}
		cleaned = append(cleaned, h)
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	existing := make(map[string]struct{}, len(c.SandboxAllowlist))
	for _, h := range c.SandboxAllowlist {
		existing[h] = struct{}{}
	}

	var added []string
	for _, h := range cleaned {
		if _, ok := existing[h]; ok {
			continue
		}
		existing[h] = struct{}{}
		c.SandboxAllowlist = append(c.SandboxAllowlist, h)
		added = append(added, h)
	}
	return added, nil
}

// RemoveSandboxHost drops host from SandboxAllowlist. Returns true
// when the host was present.
func (c *Config) RemoveSandboxHost(host string) bool {
	host = strings.TrimSpace(host)
	if host == "" {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for i, h := range c.SandboxAllowlist {
		if h == host {
			c.SandboxAllowlist = append(c.SandboxAllowlist[:i], c.SandboxAllowlist[i+1:]...)
			return true
		}
	}
	return false
}
