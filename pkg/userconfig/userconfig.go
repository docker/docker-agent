// Package userconfig provides user-level configuration for docker agent.
// This configuration is stored in ~/.config/cagent/config.yaml and contains
// user preferences like aliases.
package userconfig

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"maps"
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
	// Yolo enables auto-approve mode for all tool calls. Legacy alias for
	// Safety: autonomous; when both are set, Safety wins.
	Yolo bool `yaml:"yolo,omitempty" json:"yolo,omitempty"`
	// Safety is the default safety mode applied when the alias is run and
	// no explicit --safety/--yolo flag was passed: strict, balanced,
	// restricted, or autonomous. Wins over the legacy Yolo flag.
	Safety latest.SafetyMode `yaml:"safety,omitempty" json:"safety,omitempty"`
	// Model overrides the agent's model (format: [agent=]provider/model)
	Model string `yaml:"model,omitempty" json:"model,omitempty"`
	// HideToolResults hides tool call results in the TUI
	HideToolResults bool `yaml:"hide_tool_results,omitempty" json:"hide_tool_results,omitempty"`
	// Sandbox runs the agent inside a Docker sandbox by default.
	Sandbox bool `yaml:"sandbox,omitempty" json:"sandbox,omitempty"`
}

// HasOptions returns true if the alias has any runtime options set
func (a *Alias) HasOptions() bool {
	return a != nil && (a.Yolo || a.Safety != "" || a.Model != "" || a.HideToolResults || a.Sandbox)
}

// GetSafety returns the alias's safety-mode default: Safety when set,
// otherwise autonomous when the legacy Yolo flag is set, otherwise empty.
func (a *Alias) GetSafety() latest.SafetyMode {
	if a == nil {
		return ""
	}
	if a.Safety != "" {
		return a.Safety
	}
	if a.Yolo {
		return latest.SafetyModeAutonomous
	}
	return ""
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
	// RenderImages displays images returned by tools in supported terminals.
	// Defaults to true when not set.
	RenderImages *bool `yaml:"render_images,omitempty"`
	// Theme is the default theme reference (e.g., "dark", "light")
	// Theme files are loaded from ~/.cagent/themes/<theme>.yaml
	// The special value "auto" follows the terminal's light/dark background,
	// resolving to ThemeDark or ThemeLight.
	Theme string `yaml:"theme,omitempty"`
	// ThemeDark is the theme applied when Theme is "auto" and the terminal
	// background is dark. Defaults to "default".
	ThemeDark string `yaml:"theme_dark,omitempty"`
	// ThemeLight is the theme applied when Theme is "auto" and the terminal
	// background is light. Defaults to "default-light".
	ThemeLight string `yaml:"theme_light,omitempty"`
	// YOLO enables auto-approve mode for all tool calls globally. Legacy
	// alias for Safety: autonomous; when both are set, Safety wins.
	YOLO bool `yaml:"YOLO,omitempty"`
	// Safety is the global default safety mode applied when no explicit
	// --safety/--yolo flag and no alias safety option was given: strict,
	// balanced, restricted, or autonomous. Wins over the legacy YOLO flag.
	Safety latest.SafetyMode `yaml:"safety,omitempty"`
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
	// CacheStablePrompts keeps changing trusted context out of the frozen system
	// prefix and appends chronological updates instead. Defaults to false.
	CacheStablePrompts *bool `yaml:"cache_stable_prompts,omitempty"`
	// WarnOnCacheMiss shows a warning when a model call after the first one in
	// a session reports no cached input tokens. Defaults to false.
	WarnOnCacheMiss *bool `yaml:"warn_on_cache_miss,omitempty"`
	// Permissions defines global permission patterns applied across all sessions
	// and agents. These act as user-wide defaults; session-level and agent-level
	// permissions override them.
	Permissions *latest.PermissionsConfig `yaml:"permissions,omitempty"`
	// Hooks defines global hooks applied to every agent. These are additive with
	// agent-config and CLI hooks.
	Hooks *latest.HooksConfig `yaml:"hooks,omitempty"`
	// Keybindings lets users remap TUI keyboard shortcuts. Each entry maps an
	// action name to one or more key combinations (Bubbles key format, e.g.
	// "ctrl+q", "f2"). Unknown actions, malformed keys, and conflicts are
	// ignored with a logged warning so a bad entry never breaks the TUI.
	Keybindings []Keybinding `yaml:"keybindings,omitempty"`
	// Layout customizes the TUI chat layout (sidebar position and which
	// sidebar sections are visible). Managed via the /settings command.
	Layout *LayoutSettings `yaml:"layout,omitempty"`
	// BusySendMode controls what happens to messages sent while the agent is
	// working: "steer" (default) injects them into the ongoing stream,
	// "queue" holds them until the current turn ends. Managed via /settings.
	BusySendMode string `yaml:"busy_send_mode,omitempty"`
	// InterruptConfirmation controls how Esc interrupts a running stream:
	// "always" (default) shows a confirmation dialog, "double-tap" requires
	// pressing Esc twice within 1 second, "none" interrupts immediately.
	InterruptConfirmation string `yaml:"interrupt_confirmation,omitempty"`
	// Extra preserves settings keys this version does not know about (e.g.
	// written by a newer docker-agent) across a load/save round trip.
	Extra map[string]any `yaml:",inline"`
}

// LayoutSettings customizes the TUI chat layout. The zero value is the
// default layout: sidebar on the right with every section visible and
// normal spacing between sections.
type LayoutSettings struct {
	// SidebarPosition places the session info sidebar: "right" (default),
	// "left", "top", or "bottom".
	SidebarPosition string `yaml:"sidebar_position,omitempty"`
	// SectionSpacing controls the blank space between sidebar sections:
	// "normal" (default), "compact", or "relaxed".
	SectionSpacing string `yaml:"section_spacing,omitempty"`
	// SidebarInfoMode controls how the sidebar's Agents section renders each
	// agent: "compact" (default, two lines per agent) or "detailed"
	// (mini-cards with labeled effort/context/cost metrics).
	SidebarInfoMode string `yaml:"sidebar_info_mode,omitempty"`
	// ActiveAgentsOnly filters the sidebar's Agents section to agents active
	// in the current session instead of the whole configured team.
	ActiveAgentsOnly bool `yaml:"active_agents_only,omitempty"`
	// HideSessionPath hides the working directory (session path) line in the sidebar.
	HideSessionPath bool `yaml:"hide_session_path,omitempty"`
	// HideUsage hides the token usage section in the sidebar.
	HideUsage bool `yaml:"hide_usage,omitempty"`
	// HideAgents hides the agents section in the sidebar.
	HideAgents bool `yaml:"hide_agents,omitempty"`
	// HideTools hides the tools section in the sidebar.
	HideTools bool `yaml:"hide_tools,omitempty"`
	// HideTodos hides the todo list section in the sidebar.
	HideTodos bool `yaml:"hide_todos,omitempty"`
}

// GetLayout returns the layout settings, falling back to defaults when unset.
func (s *Settings) GetLayout() LayoutSettings {
	if s == nil || s.Layout == nil {
		return LayoutSettings{}
	}
	return *s.Layout
}

// GetBusySendMode returns the raw busy-send mode ("steer", "queue", or ""
// when unset; unknown values are normalized by the TUI layer).
func (s *Settings) GetBusySendMode() string {
	if s == nil {
		return ""
	}
	return s.BusySendMode
}

// Keybinding maps a single TUI action to the key combinations that trigger it.
type Keybinding struct {
	// Action is the identifier of the action to remap (e.g. "quit",
	// "editor_newline"). See pkg/tui/core for the list of valid actions.
	Action string `yaml:"action"`
	// Keys is the list of key combinations bound to the action, in Bubbles
	// key format (e.g. "ctrl+q", "shift+enter", "f2").
	Keys []string `yaml:"keys"`
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

// GetRenderImages returns whether tool result images are displayed, defaulting to true.
func (s *Settings) GetRenderImages() bool {
	if s == nil || s.RenderImages == nil {
		return true
	}
	return *s.RenderImages
}

// GetRestoreTabs returns whether previously open tabs are restored on
// launch, defaulting to false.
func (s *Settings) GetRestoreTabs() bool {
	if s == nil || s.RestoreTabs == nil {
		return false
	}
	return *s.RestoreTabs
}

// GetInterruptConfirmation returns the configured interrupt confirmation mode.
// Returns "always" if unset.
func (s *Settings) GetInterruptConfirmation() string {
	if s == nil || s.InterruptConfirmation == "" {
		return "always"
	}
	return s.InterruptConfirmation
}

// SnapshotsEnabled returns whether global snapshot auto-injection is enabled.
func (s *Settings) SnapshotsEnabled() bool {
	return s != nil && s.Snapshot != nil && *s.Snapshot
}

// GetSafety returns the global safety-mode default: Safety when set,
// otherwise autonomous when the legacy YOLO flag is set, otherwise empty.
func (s *Settings) GetSafety() latest.SafetyMode {
	if s == nil {
		return ""
	}
	if s.Safety != "" {
		return s.Safety
	}
	if s.YOLO {
		return latest.SafetyModeAutonomous
	}
	return ""
}

// CacheStablePromptsEnabled reports whether chronological instruction updates are enabled.
func (s *Settings) CacheStablePromptsEnabled() bool {
	return s != nil && s.CacheStablePrompts != nil && *s.CacheStablePrompts
}

// CacheMissWarningsEnabled reports whether cache-miss notifications are enabled.
func (s *Settings) CacheMissWarningsEnabled() bool {
	return s != nil && s.WarnOnCacheMiss != nil && *s.WarnOnCacheMiss
}

// GlobalHooks returns the user-level hooks config, if configured. Invalid
// hooks are skipped with a warning, mirroring hook drop-ins: a bad entry
// must never reach the runtime, but the section is kept as loaded so a
// later save does not destroy the user's data.
func (s *Settings) GlobalHooks() *latest.HooksConfig {
	if s == nil || s.Hooks == nil {
		return nil
	}
	if err := s.Hooks.Validate(); err != nil {
		slog.Warn("Ignoring invalid global hooks from user config", "path", Path(), "error", err)
		return nil
	}
	return s.Hooks
}

// BoardProject is a repository the board can create cards against.
type BoardProject struct {
	// Name is the display name shown on cards.
	Name string `yaml:"name"`
	// Path is the repository's local path.
	Path string `yaml:"path"`
	// Agent is the agent ref launched for the project's cards (defaults to
	// the built-in root agent when empty).
	Agent string `yaml:"agent,omitempty"`
}

// BoardColumn is one column of the board's pipeline. When a card moves
// forward into a column, the column's prompt is sent to the card's agent.
type BoardColumn struct {
	ID     string `yaml:"id"`
	Name   string `yaml:"name"`
	Emoji  string `yaml:"emoji,omitempty"`
	Prompt string `yaml:"prompt,omitempty"`
}

// Board configures the `docker agent board` Kanban TUI.
type Board struct {
	// Projects are the repositories cards can be created against.
	Projects []BoardProject `yaml:"projects,omitempty"`
	// Columns overrides the default pipeline (Dev → Review → Push → Done).
	// Leave empty to keep the defaults.
	Columns []BoardColumn `yaml:"columns,omitempty"`
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
	// Providers maps user-defined provider names to reusable provider
	// configurations (e.g. custom OpenAI-compatible endpoints registered via
	// `docker agent setup`). They use the same shape as the `providers`
	// section of an agent file and are merged into every loaded agent config;
	// definitions in the agent file win on name conflicts.
	Providers map[string]latest.ProviderConfig `yaml:"providers,omitempty"`
	// Settings contains global user settings
	Settings *Settings `yaml:"settings,omitempty"`
	// Board configures the `docker agent board` Kanban TUI.
	Board *Board `yaml:"board,omitempty"`
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
	// Extra preserves top-level keys this version does not know about (e.g.
	// written by a newer docker-agent) across a load/save round trip.
	Extra map[string]any `yaml:",inline"`

	// comments preserves the YAML comments read from the config file so
	// hand-written notes survive a load/save round trip.
	comments yaml.CommentMap
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
	config := &Config{Aliases: make(map[string]*Alias), comments: yaml.CommentMap{}}

	data, err := os.ReadFile(configPath)
	if err != nil {
		if os.IsNotExist(err) {
			return config, nil
		}
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	if err := yaml.UnmarshalWithOptions(data, config, yaml.CommentToMap(config.comments)); err != nil {
		return nil, fmt.Errorf("failed to parse config file: %w", err)
	}

	if config.Aliases == nil {
		config.Aliases = make(map[string]*Alias)
	}

	if config.Version != "" && config.Version != CurrentVersion {
		slog.Warn("User config file has an unexpected version; treating it as "+CurrentVersion,
			"path", configPath, "version", config.Version)
	}

	if err := config.validateSafety(); err != nil {
		return nil, fmt.Errorf("invalid config file %s: %w", configPath, err)
	}

	return config, nil
}

// validateSafety rejects non-canonical safety modes in settings and
// aliases. YAML parsing is lenient about unknown values, so this is the
// only place a typo like `safety: yolo` gets a clear error instead of
// silently behaving as "unset".
func (c *Config) validateSafety() error {
	if c.Settings != nil {
		if err := c.Settings.Safety.Validate(); err != nil {
			return fmt.Errorf("settings.safety: %w", err)
		}
	}
	for name, alias := range c.Aliases {
		if alias == nil {
			continue
		}
		if err := alias.Safety.Validate(); err != nil {
			return fmt.Errorf("aliases.%s.safety: %w", name, err)
		}
	}
	return nil
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

// Save saves the configuration to the config file.
//
// Save alone does not guard against concurrent writers: another process
// saving between this config's load and this call would be overwritten.
// Callers doing a load-mutate-save cycle should prefer [Update], which
// serializes the whole cycle behind a file lock.
func (c *Config) Save() error {
	return c.saveTo(Path())
}

func (c *Config) saveTo(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("failed to create config directory: %w", err)
	}

	// The mutex keeps the marshaled snapshot consistent when another
	// goroutine mutates the config (e.g. SetAlias) during the save.
	c.mu.Lock()
	defer c.mu.Unlock()

	// Ensure version is always set to current version when saving
	c.Version = CurrentVersion

	data, err := c.marshal()
	if err != nil {
		return fmt.Errorf("failed to marshal config: %w", err)
	}

	// The config may contain a credential helper command, so restrict it
	// to the user.
	return atomicfile.Write(path, bytes.NewReader(data), 0o600)
}

// marshal serializes the config, re-attaching the comments captured at load
// time. A comment whose node no longer exists could fail the marshal, so a
// comment-related failure falls back to plain serialization: losing comments
// beats failing the save.
func (c *Config) marshal() ([]byte, error) {
	if len(c.comments) > 0 {
		data, err := yaml.MarshalWithOptions(c, yaml.WithComment(c.comments))
		if err == nil {
			return data, nil
		}
		slog.Warn("Failed to preserve config comments; saving without them", "error", err)
	}
	return yaml.Marshal(c)
}

// Update atomically applies mutate to the freshest on-disk configuration and
// saves the result. An advisory file lock serializes the whole
// load-mutate-save cycle against other docker-agent processes, so concurrent
// writers cannot overwrite each other's changes. Returning an error from
// mutate aborts the update and leaves the file untouched. When the lock
// cannot be acquired the update proceeds unlocked (best effort) rather than
// failing the save.
func Update(mutate func(*Config) error) error {
	if release, err := acquireFileLock(Path() + ".lock"); err == nil {
		defer release()
	} else {
		slog.Warn("Proceeding without config file lock", "error", err)
	}

	cfg, err := Load()
	if err != nil {
		return err
	}
	if err := mutate(cfg); err != nil {
		return err
	}
	return cfg.Save()
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
	if err := alias.Safety.Validate(); err != nil {
		return fmt.Errorf("safety: %w", err)
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

// GetProviders returns a copy of the user-defined provider configurations.
//
// This method is safe for concurrent use; the returned map is detached from
// the config so callers can read it without holding the lock.
func (c *Config) GetProviders() map[string]latest.ProviderConfig {
	c.mu.Lock()
	defer c.mu.Unlock()

	if len(c.Providers) == 0 {
		return nil
	}
	return maps.Clone(c.Providers)
}

// SetProvider creates or updates a user-defined provider configuration.
//
// This method is safe for concurrent use. Writes to the Providers map are
// protected by a mutex to avoid concurrent map write panics when providers
// are modified from multiple goroutines.
func (c *Config) SetProvider(name string, provider latest.ProviderConfig) error {
	if strings.TrimSpace(name) == "" {
		return errors.New("provider name cannot be empty")
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.Providers == nil {
		c.Providers = make(map[string]latest.ProviderConfig)
	}
	c.Providers[name] = provider
	return nil
}

// GetSettings returns the global settings with defaults applied.
func (c *Config) GetSettings() *Settings {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.Settings == nil {
		return &Settings{RestoreTabs: new(false)}
	}
	if c.Settings.RestoreTabs == nil {
		c.Settings.RestoreTabs = new(false)
	}
	return c.Settings
}

// Get returns the global user settings from the config file.
// Returns default settings if the config file doesn't exist, has no
// settings, or cannot be read: a broken config must never take the caller
// down, but it is logged so the ignored settings are not a silent mystery.
func Get() *Settings {
	cfg, err := Load()
	if err != nil {
		slog.Warn("Failed to load user config; using default settings", "path", Path(), "error", err)
		return (&Config{}).GetSettings()
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
