package styles

import (
	"embed"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/textinput"
	"charm.land/lipgloss/v2"
	"github.com/goccy/go-yaml"

	"github.com/docker/docker-agent/pkg/paths"
	"github.com/docker/docker-agent/pkg/userconfig"
)

//go:embed themes/*.yaml
var builtinThemes embed.FS

// themeCacheEntry holds a cached theme with metadata for invalidation.
type themeCacheEntry struct {
	theme   *Theme
	modTime time.Time // For user themes: file modTime; for built-in: zero value
	path    string    // For user themes: file path; for built-in: empty
}

var (
	themeCache   = make(map[string]*themeCacheEntry)
	themeCacheMu sync.RWMutex

	// builtinRefsCache caches the list of built-in theme refs (they never change at runtime)
	builtinRefsCache   []string
	builtinRefsCacheOK bool
	builtinRefsCacheMu sync.Mutex
)

// InvalidateThemeCache clears the theme cache for a specific ref, or all if ref is empty.
// This is primarily for testing; the cache is mtime-aware so it auto-invalidates on file changes.
func InvalidateThemeCache(ref string) {
	themeCacheMu.Lock()
	defer themeCacheMu.Unlock()
	if ref == "" {
		themeCache = make(map[string]*themeCacheEntry)
	} else {
		delete(themeCache, ref)
	}
}

// DefaultThemeRef is the reference for the built-in default theme.
const DefaultThemeRef = "default"

// ThemesDir returns the directory where user themes are stored.
func ThemesDir() string {
	return filepath.Join(paths.GetDataDir(), "themes")
}

// Theme represents a complete color theme for the TUI.
// All fields are optional; unset fields use the built-in defaults.
type Theme struct {
	Version  int           `yaml:"version,omitempty"`
	Name     string        `yaml:"name,omitempty"`
	Ref      string        `yaml:"-"` // Set by loader, not from YAML
	Colors   ThemeColors   `yaml:"colors,omitempty"`
	Chroma   ChromaColors  `yaml:"chroma,omitempty"`
	Markdown MarkdownTheme `yaml:"markdown,omitempty"`
}

// ThemeColors contains all color definitions for the TUI.
// Use hex color strings (e.g., "#7AA2F7") or ANSI color numbers (e.g., "39").
type ThemeColors struct {
	// Text colors
	TextBright    string `yaml:"text_bright,omitempty"`    // Bright/emphasized text
	TextPrimary   string `yaml:"text_primary,omitempty"`   // Primary text
	TextSecondary string `yaml:"text_secondary,omitempty"` // Secondary text
	TextMuted     string `yaml:"text_muted,omitempty"`     // Muted/subtle text
	TextFaint     string `yaml:"text_faint,omitempty"`     // Very faint text/decorations

	// Accent colors
	Accent      string `yaml:"accent,omitempty"`       // Primary accent color
	AccentMuted string `yaml:"accent_muted,omitempty"` // Muted accent color

	// Background colors
	Background    string `yaml:"background,omitempty"`     // Main background
	BackgroundAlt string `yaml:"background_alt,omitempty"` // Alternate background (cards, panels)

	// Border colors
	BorderSecondary string `yaml:"border_secondary,omitempty"`

	// Status colors
	Success   string `yaml:"success,omitempty"`   // Success/positive state
	Error     string `yaml:"error,omitempty"`     // Error/negative state
	Warning   string `yaml:"warning,omitempty"`   // Warning state
	Info      string `yaml:"info,omitempty"`      // Info/neutral state
	Highlight string `yaml:"highlight,omitempty"` // Highlighted elements

	// Brand colors
	Brand   string `yaml:"brand,omitempty"`    // Primary brand color
	BrandBg string `yaml:"brand_bg,omitempty"` // Brand background

	// Error-specific colors
	ErrorStrong string `yaml:"error_strong,omitempty"` // Strong error emphasis
	ErrorDark   string `yaml:"error_dark,omitempty"`   // Dark error background

	// Spinner colors
	SpinnerDim       string `yaml:"spinner_dim,omitempty"`
	SpinnerBright    string `yaml:"spinner_bright,omitempty"`
	SpinnerBrightest string `yaml:"spinner_brightest,omitempty"`

	// Diff colors
	DiffAddBg    string `yaml:"diff_add_bg,omitempty"`
	DiffRemoveBg string `yaml:"diff_remove_bg,omitempty"`

	// UI element colors
	LineNumber      string `yaml:"line_number,omitempty"`
	Separator       string `yaml:"separator,omitempty"`
	Selected        string `yaml:"selected,omitempty"`
	SelectedFg      string `yaml:"selected_fg,omitempty"` // Text on selected/brand backgrounds
	SuggestionGhost string `yaml:"suggestion_ghost,omitempty"`
	TabBg           string `yaml:"tab_bg,omitempty"`
	TabActiveBg     string `yaml:"tab_active_bg,omitempty"`   // Active/focused tab background
	TabActiveFg     string `yaml:"tab_active_fg,omitempty"`   // Active/focused tab text
	TabInactiveFg   string `yaml:"tab_inactive_fg,omitempty"` // Inactive/unfocused tab text
	TabBorder       string `yaml:"tab_border,omitempty"`      // Tab left/right border color
	Placeholder     string `yaml:"placeholder,omitempty"`

	// Badge colors
	BadgeAccent  string `yaml:"badge_accent,omitempty"`  // Accent badge (e.g., purple highlights)
	BadgeInfo    string `yaml:"badge_info,omitempty"`    // Info badge (e.g., cyan)
	BadgeSuccess string `yaml:"badge_success,omitempty"` // Success badge (e.g., green)

	// Agent colors
	AgentHues []float64 `yaml:"agent_hues,omitempty"` // Hue values (0-360) for agent color generation
}

// ChromaColors contains syntax highlighting colors (for code blocks).
type ChromaColors struct {
	ErrorFg             string `yaml:"error_fg,omitempty"`
	ErrorBg             string `yaml:"error_bg,omitempty"`
	Success             string `yaml:"success,omitempty"`
	Comment             string `yaml:"comment,omitempty"`
	CommentPreproc      string `yaml:"comment_preproc,omitempty"`
	Keyword             string `yaml:"keyword,omitempty"`
	KeywordReserved     string `yaml:"keyword_reserved,omitempty"`
	KeywordNamespace    string `yaml:"keyword_namespace,omitempty"`
	KeywordType         string `yaml:"keyword_type,omitempty"`
	Operator            string `yaml:"operator,omitempty"`
	Punctuation         string `yaml:"punctuation,omitempty"`
	NameBuiltin         string `yaml:"name_builtin,omitempty"`
	NameTag             string `yaml:"name_tag,omitempty"`
	NameAttribute       string `yaml:"name_attribute,omitempty"`
	NameDecorator       string `yaml:"name_decorator,omitempty"`
	LiteralNumber       string `yaml:"literal_number,omitempty"`
	LiteralString       string `yaml:"literal_string,omitempty"`
	LiteralStringEscape string `yaml:"literal_string_escape,omitempty"`
	GenericDeleted      string `yaml:"generic_deleted,omitempty"`
	GenericSubheading   string `yaml:"generic_subheading,omitempty"`
	Background          string `yaml:"background,omitempty"`
}

// MarkdownTheme contains markdown-specific color overrides.
type MarkdownTheme struct {
	Heading    string `yaml:"heading,omitempty"`
	Link       string `yaml:"link,omitempty"`
	Strong     string `yaml:"strong,omitempty"`
	Code       string `yaml:"code,omitempty"`
	CodeBg     string `yaml:"code_bg,omitempty"`
	Blockquote string `yaml:"blockquote,omitempty"`
	List       string `yaml:"list,omitempty"`
	HR         string `yaml:"hr,omitempty"`
}

// cachedDefaultTheme holds the parsed default.yaml theme (loaded once).
var (
	cachedDefaultTheme   *Theme
	cachedDefaultThemeMu sync.Mutex
)

// DefaultTheme returns the built-in default theme loaded from embedded default.yaml.
// The result is cached internally, but a copy is returned to prevent callers from
// accidentally modifying the cached theme.
func DefaultTheme() *Theme {
	cachedDefaultThemeMu.Lock()
	defer cachedDefaultThemeMu.Unlock()

	if cachedDefaultTheme == nil {
		// Load default.yaml from embedded files
		data, err := builtinThemes.ReadFile("themes/default.yaml")
		if err != nil {
			// This should never happen - default.yaml is embedded at compile time
			panic(fmt.Sprintf("failed to read embedded default.yaml: %v", err))
		}

		var theme Theme
		if err := yaml.Unmarshal(data, &theme); err != nil {
			panic(fmt.Sprintf("failed to parse embedded default.yaml: %v", err))
		}

		theme.Ref = DefaultThemeRef
		cachedDefaultTheme = &theme
	}

	// Return a copy to prevent callers from modifying the cached theme
	themeCopy := *cachedDefaultTheme
	return &themeCopy
}

// UserThemePrefix is used to distinguish user themes from built-in themes
// when they have the same base name. A ref like "user:nord" refers to the
// user's custom nord theme, while "nord" refers to the built-in.
const UserThemePrefix = "user:"

// ListThemeRefs returns the list of available theme references.
// It includes all built-in themes (including "default") and user themes from ~/.cagent/themes/.
// User themes with names matching built-in themes are prefixed with "user:" to distinguish them.
// The "default" theme is always listed first for UX purposes.
func ListThemeRefs() ([]string, error) {
	// Track built-in refs to detect conflicts with user themes
	builtinSet := make(map[string]bool)

	// Start with default theme (listed first for UX)
	refs := []string{DefaultThemeRef}
	builtinSet[DefaultThemeRef] = true

	// Add built-in themes from embedded files (default.yaml will be skipped since already added)
	builtinRefs, err := listBuiltinThemeRefs()
	if err != nil {
		return nil, fmt.Errorf("listing built-in themes: %w", err)
	}
	for _, ref := range builtinRefs {
		if !builtinSet[ref] {
			refs = append(refs, ref)
			builtinSet[ref] = true
		}
	}

	// Add user themes from data directory
	// If a user theme has the same name as a built-in, prefix it with "user:"
	userRefs, err := listUserThemeRefs()
	if err != nil {
		return nil, fmt.Errorf("listing user themes: %w", err)
	}
	for _, ref := range userRefs {
		if builtinSet[ref] {
			// User theme has same name as built-in - use prefixed ref
			refs = append(refs, UserThemePrefix+ref)
		} else {
			refs = append(refs, ref)
		}
	}

	return refs, nil
}

// listBuiltinThemeRefs returns the list of built-in theme references from embedded files.
// Results are cached since built-in themes never change at runtime.
func listBuiltinThemeRefs() ([]string, error) {
	builtinRefsCacheMu.Lock()
	defer builtinRefsCacheMu.Unlock()

	if builtinRefsCacheOK {
		return builtinRefsCache, nil
	}

	var refs []string

	entries, err := builtinThemes.ReadDir("themes")
	if err != nil {
		return nil, fmt.Errorf("reading embedded themes directory: %w", err)
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		// Accept .yaml and .yml files
		if before, ok := strings.CutSuffix(name, ".yaml"); ok {
			refs = append(refs, before)
		} else if before, ok := strings.CutSuffix(name, ".yml"); ok {
			refs = append(refs, before)
		}
	}

	builtinRefsCache = refs
	builtinRefsCacheOK = true
	return refs, nil
}

// listUserThemeRefs returns the list of user theme references from ~/.cagent/themes/.
func listUserThemeRefs() ([]string, error) {
	return listThemeRefsFrom(ThemesDir())
}

// UserThemeExists returns true if a user theme file exists for the given ref
// in the user themes directory (typically ~/.cagent/themes/).
//
// This handles the "user:" prefix - "user:nord" checks for ~/.cagent/themes/nord.yaml.
func UserThemeExists(ref string) bool {
	if ref == "" {
		return false
	}

	// Strip user: prefix if present
	baseRef := strings.TrimPrefix(ref, UserThemePrefix)

	if err := validateThemeRef(baseRef); err != nil {
		return false
	}

	dir := ThemesDir()

	// Try .yaml first, then .yml
	if _, err := os.Stat(filepath.Join(dir, baseRef+".yaml")); err == nil {
		return true
	}
	if _, err := os.Stat(filepath.Join(dir, baseRef+".yml")); err == nil {
		return true
	}

	return false
}

// SaveThemeToUserConfig persists the theme reference to the user config file.
// If themeRef equals DefaultThemeRef, the setting is cleared (empty string).
func SaveThemeToUserConfig(themeRef string) error {
	cfg, err := userconfig.Load()
	if err != nil {
		return fmt.Errorf("loading user config: %w", err)
	}

	if cfg.Settings == nil {
		cfg.Settings = &userconfig.Settings{}
	}

	// Clear the setting if using the default theme
	if themeRef == DefaultThemeRef {
		cfg.Settings.Theme = ""
	} else {
		cfg.Settings.Theme = themeRef
	}

	if err := cfg.Save(); err != nil {
		return fmt.Errorf("saving user config: %w", err)
	}

	return nil
}

// GetPersistedThemeRef returns the theme reference persisted in user config.
// Returns DefaultThemeRef if no theme is set or if loading fails.
func GetPersistedThemeRef() string {
	cfg, err := userconfig.Load()
	if err != nil {
		return DefaultThemeRef
	}

	if cfg.Settings == nil || cfg.Settings.Theme == "" {
		return DefaultThemeRef
	}

	return cfg.Settings.Theme
}

// listThemeRefsFrom lists theme refs from a specific directory (for testing).
// It only returns theme refs found in the directory, without adding any defaults.
func listThemeRefsFrom(dir string) ([]string, error) {
	var refs []string

	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return refs, nil
		}
		return nil, fmt.Errorf("reading themes directory: %w", err)
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		// Accept .yaml and .yml files
		if before, ok := strings.CutSuffix(name, ".yaml"); ok {
			refs = append(refs, before)
		} else if before, ok := strings.CutSuffix(name, ".yml"); ok {
			refs = append(refs, before)
		}
	}

	return refs, nil
}

// LoadTheme loads a theme by reference with mtime-aware caching.
// If ref is "" (empty), this is an error - caller should resolve to DefaultThemeRef first.
//
// Refs starting with "user:" (e.g., "user:nord") explicitly load from user themes directory.
// Other refs load built-in themes first, falling back to user themes if no built-in exists.
//
// The cache is mtime-aware: user themes are re-parsed only when the file's modTime changes.
// If a user theme file exists but fails to parse, an error is returned (no silent fallback).
func LoadTheme(ref string) (*Theme, error) {
	// Empty ref means "use default theme" - caller should resolve this to DefaultThemeRef
	if ref == "" {
		return nil, fmt.Errorf("cannot load theme with empty ref; use %q instead", DefaultThemeRef)
	}

	// Check if this is an explicit user theme reference (user:name)
	forceUserTheme := strings.HasPrefix(ref, UserThemePrefix)
	baseRef := ref
	if forceUserTheme {
		baseRef = strings.TrimPrefix(ref, UserThemePrefix)
	}

	// Validate the base ref - reject path traversal attempts
	if err := validateThemeRef(baseRef); err != nil {
		return nil, err
	}

	// Determine if this should load from built-in or user themes
	isBuiltin := !forceUserTheme && IsBuiltinTheme(baseRef)

	// For user themes, check if file exists and get modTime
	var userThemePath string
	var userModTime time.Time
	if !isBuiltin {
		userThemePath, userModTime = getUserThemeFileInfo(baseRef)
	}

	// Check the cache (use the full ref as cache key to distinguish user:nord from nord)
	themeCacheMu.RLock()
	cached, hasCached := themeCache[ref]
	themeCacheMu.RUnlock()

	if hasCached {
		if isBuiltin {
			// Built-in themes don't change at runtime, cache is always valid
			return cached.theme, nil
		}
		// User theme: check if modTime matches
		if cached.path == userThemePath && cached.modTime.Equal(userModTime) {
			return cached.theme, nil
		}
		// modTime changed or path changed, need to reload
	}

	// Load and cache the theme
	var theme *Theme
	var err error
	var entry *themeCacheEntry

	switch {
	case isBuiltin:
		// Load built-in theme from embedded files
		theme, err = loadBuiltinTheme(baseRef)
		if err != nil {
			return nil, err
		}
		entry = &themeCacheEntry{
			theme:   theme,
			modTime: time.Time{}, // Zero time for built-in themes
			path:    "",          // Empty path for built-in themes
		}
	case userThemePath != "":
		// User theme file exists - load it
		theme, err = loadThemeFrom(baseRef, ThemesDir())
		if err != nil {
			return nil, err
		}
		entry = &themeCacheEntry{
			theme:   theme,
			modTime: userModTime,
			path:    userThemePath,
		}
	default:
		// Not a built-in and no user theme file exists
		return nil, fmt.Errorf("theme %q not found", ref)
	}

	// Store in cache (use full ref as key)
	themeCacheMu.Lock()
	themeCache[ref] = entry
	themeCacheMu.Unlock()

	return theme, nil
}

// getUserThemeFileInfo returns the path and modTime of a user theme file if it exists.
// Returns empty path and zero time if the file doesn't exist.
func getUserThemeFileInfo(ref string) (path string, modTime time.Time) {
	dir := ThemesDir()

	// Try .yaml first, then .yml
	yamlPath := filepath.Join(dir, ref+".yaml")
	if info, err := os.Stat(yamlPath); err == nil {
		return yamlPath, info.ModTime()
	}

	ymlPath := filepath.Join(dir, ref+".yml")
	if info, err := os.Stat(ymlPath); err == nil {
		return ymlPath, info.ModTime()
	}

	return "", time.Time{}
}

// validateThemeRef validates a theme reference to prevent path traversal attacks.
func validateThemeRef(ref string) error {
	if ref == "" || ref == DefaultThemeRef {
		return nil // These are valid sentinel values
	}
	if strings.Contains(ref, "/") || strings.Contains(ref, "\\") || strings.Contains(ref, "..") {
		return fmt.Errorf("invalid theme ref %q: must not contain path separators or traversal", ref)
	}
	return nil
}

// loadBuiltinTheme loads a built-in theme from embedded files.
func loadBuiltinTheme(ref string) (*Theme, error) {
	base := DefaultTheme()

	// Try .yaml first, then .yml
	var data []byte
	var err error

	yamlPath := "themes/" + ref + ".yaml"
	ymlPath := "themes/" + ref + ".yml"

	data, err = builtinThemes.ReadFile(yamlPath)
	if err != nil {
		data, err = builtinThemes.ReadFile(ymlPath)
	}
	if err != nil {
		return nil, fmt.Errorf("built-in theme %q not found", ref)
	}

	var override Theme
	if err := yaml.Unmarshal(data, &override); err != nil {
		return nil, fmt.Errorf("parsing built-in theme %q: %w", ref, err)
	}

	// Merge override onto base
	merged := mergeTheme(base, &override)
	merged.Ref = ref
	if merged.Name == "" {
		merged.Name = ref
	}

	return merged, nil
}

// IsBuiltinTheme returns true if the given theme reference is a built-in theme.
// Refs prefixed with "user:" are always considered user themes, not built-in.
func IsBuiltinTheme(ref string) bool {
	// User-prefixed refs are explicitly user themes
	if strings.HasPrefix(ref, UserThemePrefix) {
		return false
	}

	if ref == DefaultThemeRef {
		return true
	}

	builtinRefs, err := listBuiltinThemeRefs()
	if err != nil {
		return false
	}

	return slices.Contains(builtinRefs, ref)
}

// loadThemeFrom loads a theme from a specific directory (for testing).
// Returns an error if the theme file is not found.
func loadThemeFrom(ref, dir string) (*Theme, error) {
	base := DefaultTheme()

	// Try .yaml first, then .yml
	var data []byte
	var err error

	yamlPath := filepath.Join(dir, ref+".yaml")
	ymlPath := filepath.Join(dir, ref+".yml")

	data, err = os.ReadFile(yamlPath)
	if os.IsNotExist(err) {
		data, err = os.ReadFile(ymlPath)
	}
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("theme %q not found in %s", ref, dir)
		}
		return nil, fmt.Errorf("reading theme file: %w", err)
	}

	var override Theme
	if err := yaml.Unmarshal(data, &override); err != nil {
		return nil, fmt.Errorf("parsing theme %q: %w", ref, err)
	}

	// Merge override onto base
	merged := mergeTheme(base, &override)
	merged.Ref = ref
	if merged.Name == "" {
		merged.Name = ref
	}

	return merged, nil
}

// mergeTheme merges override onto base, returning a new theme.
// Only non-empty fields in override replace base values.
func mergeTheme(base, override *Theme) *Theme {
	result := *base

	if override.Version != 0 {
		result.Version = override.Version
	}
	if override.Name != "" {
		result.Name = override.Name
	}

	// Merge colors
	result.Colors = mergeColors(base.Colors, override.Colors)
	result.Chroma = mergeChromaColors(base.Chroma, override.Chroma)
	result.Markdown = mergeMarkdownTheme(base.Markdown, override.Markdown)

	return &result
}

func mergeColors(base, override ThemeColors) ThemeColors {
	result := base
	// Text colors
	if override.TextBright != "" {
		result.TextBright = override.TextBright
	}
	if override.TextPrimary != "" {
		result.TextPrimary = override.TextPrimary
	}
	if override.TextSecondary != "" {
		result.TextSecondary = override.TextSecondary
	}
	if override.TextMuted != "" {
		result.TextMuted = override.TextMuted
	}
	if override.TextFaint != "" {
		result.TextFaint = override.TextFaint
	}
	// Accent colors
	if override.Accent != "" {
		result.Accent = override.Accent
	}
	if override.AccentMuted != "" {
		result.AccentMuted = override.AccentMuted
	}
	// Background colors
	if override.Background != "" {
		result.Background = override.Background
	}
	if override.BackgroundAlt != "" {
		result.BackgroundAlt = override.BackgroundAlt
	}
	// Border colors
	if override.BorderSecondary != "" {
		result.BorderSecondary = override.BorderSecondary
	}
	// Status colors
	if override.Success != "" {
		result.Success = override.Success
	}
	if override.Error != "" {
		result.Error = override.Error
	}
	if override.Warning != "" {
		result.Warning = override.Warning
	}
	if override.Info != "" {
		result.Info = override.Info
	}
	if override.Highlight != "" {
		result.Highlight = override.Highlight
	}
	// Brand colors
	if override.Brand != "" {
		result.Brand = override.Brand
	}
	if override.BrandBg != "" {
		result.BrandBg = override.BrandBg
	}
	// Error colors
	if override.ErrorStrong != "" {
		result.ErrorStrong = override.ErrorStrong
	}
	if override.ErrorDark != "" {
		result.ErrorDark = override.ErrorDark
	}
	// Spinner colors
	if override.SpinnerDim != "" {
		result.SpinnerDim = override.SpinnerDim
	}
	if override.SpinnerBright != "" {
		result.SpinnerBright = override.SpinnerBright
	}
	if override.SpinnerBrightest != "" {
		result.SpinnerBrightest = override.SpinnerBrightest
	}
	// Diff colors
	if override.DiffAddBg != "" {
		result.DiffAddBg = override.DiffAddBg
	}
	if override.DiffRemoveBg != "" {
		result.DiffRemoveBg = override.DiffRemoveBg
	}
	// UI element colors
	if override.LineNumber != "" {
		result.LineNumber = override.LineNumber
	}
	if override.Separator != "" {
		result.Separator = override.Separator
	}
	if override.Selected != "" {
		result.Selected = override.Selected
	}
	if override.SelectedFg != "" {
		result.SelectedFg = override.SelectedFg
	}
	if override.SuggestionGhost != "" {
		result.SuggestionGhost = override.SuggestionGhost
	}
	if override.TabBg != "" {
		result.TabBg = override.TabBg
	}
	if override.TabActiveBg != "" {
		result.TabActiveBg = override.TabActiveBg
	}
	if override.TabActiveFg != "" {
		result.TabActiveFg = override.TabActiveFg
	}
	if override.TabInactiveFg != "" {
		result.TabInactiveFg = override.TabInactiveFg
	}
	if override.TabBorder != "" {
		result.TabBorder = override.TabBorder
	}
	if override.Placeholder != "" {
		result.Placeholder = override.Placeholder
	}
	// Badge colors
	if override.BadgeAccent != "" {
		result.BadgeAccent = override.BadgeAccent
	}
	if override.BadgeInfo != "" {
		result.BadgeInfo = override.BadgeInfo
	}
	if override.BadgeSuccess != "" {
		result.BadgeSuccess = override.BadgeSuccess
	}
	// Agent colors
	if len(override.AgentHues) > 0 {
		result.AgentHues = override.AgentHues
	}
	return result
}

func mergeChromaColors(base, override ChromaColors) ChromaColors {
	result := base
	if override.ErrorFg != "" {
		result.ErrorFg = override.ErrorFg
	}
	if override.ErrorBg != "" {
		result.ErrorBg = override.ErrorBg
	}
	if override.Success != "" {
		result.Success = override.Success
	}
	if override.Comment != "" {
		result.Comment = override.Comment
	}
	if override.CommentPreproc != "" {
		result.CommentPreproc = override.CommentPreproc
	}
	if override.Keyword != "" {
		result.Keyword = override.Keyword
	}
	if override.KeywordReserved != "" {
		result.KeywordReserved = override.KeywordReserved
	}
	if override.KeywordNamespace != "" {
		result.KeywordNamespace = override.KeywordNamespace
	}
	if override.KeywordType != "" {
		result.KeywordType = override.KeywordType
	}
	if override.Operator != "" {
		result.Operator = override.Operator
	}
	if override.Punctuation != "" {
		result.Punctuation = override.Punctuation
	}
	if override.NameBuiltin != "" {
		result.NameBuiltin = override.NameBuiltin
	}
	if override.NameTag != "" {
		result.NameTag = override.NameTag
	}
	if override.NameAttribute != "" {
		result.NameAttribute = override.NameAttribute
	}
	if override.NameDecorator != "" {
		result.NameDecorator = override.NameDecorator
	}
	if override.LiteralNumber != "" {
		result.LiteralNumber = override.LiteralNumber
	}
	if override.LiteralString != "" {
		result.LiteralString = override.LiteralString
	}
	if override.LiteralStringEscape != "" {
		result.LiteralStringEscape = override.LiteralStringEscape
	}
	if override.GenericDeleted != "" {
		result.GenericDeleted = override.GenericDeleted
	}
	if override.GenericSubheading != "" {
		result.GenericSubheading = override.GenericSubheading
	}
	if override.Background != "" {
		result.Background = override.Background
	}
	return result
}

func mergeMarkdownTheme(base, override MarkdownTheme) MarkdownTheme {
	result := base
	if override.Heading != "" {
		result.Heading = override.Heading
	}
	if override.Link != "" {
		result.Link = override.Link
	}
	if override.Strong != "" {
		result.Strong = override.Strong
	}
	if override.Code != "" {
		result.Code = override.Code
	}
	if override.CodeBg != "" {
		result.CodeBg = override.CodeBg
	}
	if override.Blockquote != "" {
		result.Blockquote = override.Blockquote
	}
	if override.List != "" {
		result.List = override.List
	}
	if override.HR != "" {
		result.HR = override.HR
	}
	return result
}

// currentTheme stores the currently applied theme for reference.
var currentTheme atomic.Pointer[Theme]

// CurrentTheme returns the currently applied theme, or the default if none applied.
func CurrentTheme() *Theme {
	t := currentTheme.Load()
	if t == nil {
		return DefaultTheme()
	}
	return t
}

// ApplyTheme applies the given theme to all style variables.
// This updates all exported color and style variables in the styles package.
// After calling this, send ThemeChangedMsg to invalidate all TUI caches.
func ApplyTheme(theme *Theme) {
	if theme == nil {
		theme = DefaultTheme()
	}

	// Store current theme
	currentTheme.Store(theme)

	// Update color variables
	c := theme.Colors
	// Background colors
	Background = lipgloss.Color(c.Background)
	BackgroundAlt = lipgloss.Color(c.BackgroundAlt)
	// Text colors
	White = lipgloss.Color(c.SelectedFg)
	TextPrimary = lipgloss.Color(c.TextPrimary)
	TextSecondary = lipgloss.Color(c.TextSecondary)
	TextMuted = lipgloss.Color(c.AccentMuted)
	TextMutedGray = lipgloss.Color(c.TextMuted)
	// Accent & brand colors
	Accent = lipgloss.Color(c.Accent)
	MobyBlue = lipgloss.Color(c.Brand)
	// Status colors
	Success = lipgloss.Color(c.Success)
	Error = lipgloss.Color(c.Error)
	Warning = lipgloss.Color(c.Warning)
	Info = lipgloss.Color(c.Info)
	Highlight = lipgloss.Color(c.Highlight)
	// Border colors
	BorderPrimary = lipgloss.Color(c.Accent)
	BorderSecondary = lipgloss.Color(c.BorderSecondary)
	BorderMuted = lipgloss.Color(c.BackgroundAlt)
	BorderWarning = lipgloss.Color(c.Warning)
	// Diff colors
	DiffAddBg = lipgloss.Color(c.DiffAddBg)
	DiffRemoveBg = lipgloss.Color(c.DiffRemoveBg)
	DiffAddFg = lipgloss.Color(c.Success)
	DiffRemoveFg = lipgloss.Color(c.Error)
	// UI element colors
	LineNumber = lipgloss.Color(c.LineNumber)
	Separator = lipgloss.Color(c.Separator)
	Selected = lipgloss.Color(c.Selected)
	SelectedFg = lipgloss.Color(c.TextPrimary)
	PlaceholderColor = lipgloss.Color(c.Placeholder)
	// Badge colors
	AgentBadgeBg = MobyBlue
	AgentBadgeFg = lipgloss.Color(bestForegroundHex(
		c.Brand,
		c.TextBright,
		c.Background,
		"#000000",
		"#ffffff",
	))
	BadgePurple = lipgloss.Color(c.BadgeAccent)
	BadgeCyan = lipgloss.Color(c.BadgeInfo)
	BadgeGreen = lipgloss.Color(c.BadgeSuccess)
	// Error colors
	ErrorStrong = lipgloss.Color(c.ErrorStrong)
	ErrorDark = lipgloss.Color(c.ErrorDark)
	// Other UI colors
	FadedGray = lipgloss.Color(c.TextFaint)
	TabBg = lipgloss.Color(c.TabBg)
	TabPrimaryFg = lipgloss.Color(c.TextMuted)
	TabAccentFg = lipgloss.Color(c.Highlight)
	TabActiveBg = lipgloss.Color(c.TabActiveBg)
	TabActiveFg = lipgloss.Color(c.TabActiveFg)
	TabInactiveFg = lipgloss.Color(c.TabInactiveFg)
	TabBorder = lipgloss.Color(c.TabBorder)

	// Rebuild all derived styles
	rebuildStyles()

	// Rebuild cached agent color styles with new theme contrast values
	InvalidateAgentColorCache()

	// Clear style sequence cache (used by RenderComposite)
	clearStyleSeqCache()
}

// rebuildStyles rebuilds all derived lipgloss.Style variables from the current color values.
func rebuildStyles() {
	// Base styles
	BaseStyle = NoStyle.Foreground(TextPrimary)
	AppStyle = BaseStyle.Padding(0, AppPadding, 0, AppPadding)

	// Text styles
	HighlightWhiteStyle = BaseStyle.Foreground(White).Bold(true)
	MutedStyle = BaseStyle.Foreground(TextMutedGray)
	SecondaryStyle = BaseStyle.Foreground(TextSecondary)
	BoldStyle = BaseStyle.Bold(true)
	FadingStyle = NoStyle.Foreground(FadedGray)

	// Status styles
	SuccessStyle = BaseStyle.Foreground(Success)
	ErrorStyle = BaseStyle.Foreground(Error)
	WarningStyle = BaseStyle.Foreground(Warning)
	InfoStyle = BaseStyle.Foreground(Info)
	ActiveStyle = BaseStyle.Foreground(Success)
	ToBeDoneStyle = BaseStyle.Foreground(TextPrimary)
	InProgressStyle = BaseStyle.Foreground(Highlight)
	CompletedStyle = BaseStyle.Foreground(TextMutedGray)

	// Layout styles
	CenterStyle = BaseStyle.Align(lipgloss.Center, lipgloss.Center)

	// Border/message styles
	BaseMessageStyle = BaseStyle.
		Padding(1, 1).
		BorderLeft(true).
		BorderStyle(lipgloss.HiddenBorder()).
		BorderForeground(BorderPrimary)

	UserMessageStyle = BaseMessageStyle.
		BorderStyle(lipgloss.ThickBorder()).
		BorderForeground(BorderPrimary).
		Foreground(TextPrimary).
		Background(BackgroundAlt).
		Bold(true)

	AssistantMessageStyle = BaseMessageStyle.Padding(0, 1)

	WelcomeMessageStyle = BaseMessageStyle.
		BorderStyle(lipgloss.DoubleBorder()).
		Bold(true)

	ErrorMessageStyle = BaseMessageStyle.
		BorderStyle(lipgloss.ThickBorder()).
		Foreground(Error)

	SelectedMessageStyle = AssistantMessageStyle.
		BorderStyle(lipgloss.NormalBorder()).
		BorderForeground(Success)

	SelectedUserMessageStyle = UserMessageStyle.
		BorderStyle(lipgloss.ThickBorder()).
		BorderForeground(Success)

	// Dialog styles
	DialogStyle = BaseStyle.
		Border(lipgloss.RoundedBorder()).
		BorderForeground(BorderSecondary).
		Foreground(TextPrimary).
		Padding(1, 2).
		Align(lipgloss.Left)

	DialogWarningStyle = BaseStyle.
		Border(lipgloss.RoundedBorder()).
		BorderForeground(BorderWarning).
		Foreground(TextPrimary).
		Padding(1, 2).
		Align(lipgloss.Left)

	DialogTitleStyle = BaseStyle.
		Bold(true).
		Foreground(TextSecondary).
		Align(lipgloss.Center)

	DialogTitleWarningStyle = BaseStyle.
		Bold(true).
		Foreground(Warning).
		Align(lipgloss.Center)

	DialogTitleInfoStyle = BaseStyle.
		Bold(true).
		Foreground(Info).
		Align(lipgloss.Center)

	DialogContentStyle = BaseStyle.Foreground(TextPrimary)

	DialogSeparatorStyle = BaseStyle.Foreground(BorderMuted)

	DialogQuestionStyle = BaseStyle.
		Bold(true).
		Foreground(TextPrimary).
		Align(lipgloss.Center)

	DialogOptionsStyle = BaseStyle.
		Foreground(TextMuted).
		Align(lipgloss.Center)

	DialogHelpStyle = BaseStyle.
		Foreground(TextMuted).
		Italic(true)

	TabTitleStyle = BaseStyle.Foreground(TabPrimaryFg)
	TabPrimaryStyle = BaseStyle.Foreground(TextPrimary)
	TabStyle = TabPrimaryStyle.Padding(1, 0)
	TabAccentStyle = BaseStyle.Foreground(TabAccentFg)

	// Command palette styles
	PaletteCategoryStyle = BaseStyle.
		Bold(true).
		Foreground(White).
		MarginTop(1)

	PaletteUnselectedActionStyle = BaseStyle.
		Foreground(TextPrimary).
		Bold(true)

	PaletteSelectedActionStyle = PaletteUnselectedActionStyle.
		Background(MobyBlue).
		Foreground(White)

	PaletteUnselectedDescStyle = BaseStyle.Foreground(TextSecondary)

	PaletteSelectedDescStyle = PaletteUnselectedDescStyle.
		Background(MobyBlue).
		Foreground(White)

	// Badge styles
	BadgeAlloyStyle = BaseStyle.Foreground(BadgePurple)
	BadgeDefaultStyle = BaseStyle.Foreground(BadgeCyan)
	BadgeCurrentStyle = BaseStyle.Foreground(BadgeGreen)

	// Star styles
	StarredStyle = BaseStyle.Foreground(Success)
	UnstarredStyle = BaseStyle.Foreground(TextMuted)

	// Diff styles
	DiffAddStyle = BaseStyle.Background(DiffAddBg).Foreground(DiffAddFg)
	DiffRemoveStyle = BaseStyle.Background(DiffRemoveBg).Foreground(DiffRemoveFg)
	DiffUnchangedStyle = BaseStyle.Background(BackgroundAlt)

	// Syntax highlighting styles
	LineNumberStyle = BaseStyle.Foreground(LineNumber).Background(BackgroundAlt)
	SeparatorStyle = BaseStyle.Foreground(Separator).Background(BackgroundAlt)

	// Tool call styles
	ToolMessageStyle = BaseStyle.Foreground(TextMutedGray)
	ToolErrorMessageStyle = BaseStyle.Foreground(ErrorStrong)
	ToolName = ToolMessageStyle.Foreground(TextMutedGray).Padding(0, 1)
	ToolNameError = ToolName.
		Foreground(ErrorStrong).
		Background(ErrorDark)
	ToolNameDim = ToolMessageStyle.Foreground(TextMutedGray).Italic(true)
	ToolDescription = ToolMessageStyle.Foreground(TextPrimary)
	ToolCompletedIcon = BaseStyle.MarginLeft(2).Foreground(TextMutedGray)
	ToolErrorIcon = ToolCompletedIcon.Background(ErrorStrong)
	ToolPendingIcon = ToolCompletedIcon.Background(Warning)
	ToolCallArgs = ToolMessageStyle.Padding(0, 0, 0, 2)
	ToolCallResult = ToolMessageStyle.Padding(0, 0, 0, 2)

	// Input styles
	InputStyle = textarea.Styles{
		Focused: textarea.StyleState{
			Base:        BaseStyle,
			Placeholder: BaseStyle.Foreground(PlaceholderColor),
		},
		Blurred: textarea.StyleState{
			Base:        BaseStyle,
			Placeholder: BaseStyle.Foreground(PlaceholderColor),
		},
		Cursor: textarea.CursorStyle{
			Color: Accent,
		},
	}

	DialogInputStyle = textinput.Styles{
		Focused: textinput.StyleState{
			Text:        BaseStyle,
			Placeholder: BaseStyle.Foreground(PlaceholderColor),
		},
		Blurred: textinput.StyleState{
			Text:        BaseStyle,
			Placeholder: BaseStyle.Foreground(PlaceholderColor),
		},
		Cursor: textinput.CursorStyle{
			Color: Accent,
		},
	}

	EditorStyle = BaseStyle.Padding(1, AppPadding, 0, AppPadding)
	SuggestionGhostStyle = BaseStyle.Foreground(lipgloss.Color(CurrentTheme().Colors.SuggestionGhost))
	SuggestionCursorStyle = BaseStyle.Background(Accent).Foreground(lipgloss.Color(CurrentTheme().Colors.SuggestionGhost))

	// Attachment styles
	AttachmentBannerStyle = BaseStyle.Foreground(TextSecondary)
	AttachmentBadgeStyle = BaseStyle.Foreground(Info).Bold(true)
	AttachmentSizeStyle = BaseStyle.Foreground(TextMuted).Italic(true)
	AttachmentIconStyle = BaseStyle.Foreground(Info)

	// Scrollbar styles
	TrackStyle = lipgloss.NewStyle().Foreground(BorderSecondary)
	ThumbStyle = lipgloss.NewStyle().Foreground(Info).Background(BackgroundAlt)
	ThumbActiveStyle = lipgloss.NewStyle().Foreground(White).Background(BackgroundAlt)

	// Resize handle styles
	ResizeHandleStyle = BaseStyle.Foreground(BorderSecondary)
	ResizeHandleHoverStyle = BaseStyle.Foreground(Info).Bold(true)
	ResizeHandleActiveStyle = BaseStyle.Foreground(White).Bold(true)

	// Notification styles
	NotificationStyle = BaseStyle.
		Border(lipgloss.RoundedBorder()).
		BorderForeground(Success).
		Padding(0, 1)

	NotificationInfoStyle = BaseStyle.
		Border(lipgloss.RoundedBorder()).
		BorderForeground(Info).
		Padding(0, 1)

	NotificationWarningStyle = BaseStyle.
		Border(lipgloss.RoundedBorder()).
		BorderForeground(Warning).
		Padding(0, 1)

	NotificationErrorStyle = BaseStyle.
		Border(lipgloss.RoundedBorder()).
		BorderForeground(Error).
		Padding(0, 1)

	// Completion styles
	CompletionBoxStyle = BaseStyle.
		Border(lipgloss.RoundedBorder()).
		BorderForeground(BorderSecondary).
		Padding(0, 1)

	CompletionNormalStyle = BaseStyle.Foreground(TextPrimary).Bold(true)
	CompletionSelectedStyle = CompletionNormalStyle.Foreground(White).Background(MobyBlue)
	CompletionDescStyle = BaseStyle.Foreground(TextSecondary)
	CompletionSelectedDescStyle = CompletionDescStyle.Foreground(White).Background(MobyBlue)
	CompletionNoResultsStyle = BaseStyle.Foreground(TextMuted).Italic(true).Align(lipgloss.Center)

	// Agent badge styles
	AgentBadgeStyle = BaseStyle.
		Foreground(AgentBadgeFg).
		Background(AgentBadgeBg).
		Padding(0, 1)

	ThinkingBadgeStyle = BaseStyle.
		Foreground(TextMuted).
		Bold(true).
		Italic(true)

	// Selection styles
	SelectionStyle = BaseStyle.Background(Selected).Foreground(SelectedFg)

	// Spinner styles
	SpinnerDotsAccentStyle = BaseStyle.Foreground(Accent)
	SpinnerDotsHighlightStyle = BaseStyle.Foreground(TabAccentFg)
	SpinnerTextBrightestStyle = BaseStyle.Foreground(lipgloss.Color(CurrentTheme().Colors.SpinnerBrightest))
	SpinnerTextBrightStyle = BaseStyle.Foreground(lipgloss.Color(CurrentTheme().Colors.SpinnerBright))
	SpinnerTextDimStyle = BaseStyle.Foreground(lipgloss.Color(CurrentTheme().Colors.SpinnerDim))
	SpinnerTextDimmestStyle = BaseStyle.Foreground(Accent)
}

// init applies the default theme at package initialization time.
// This ensures color variables are set before any code uses them,
// including tests that don't explicitly call ApplyTheme().
//
//nolint:gochecknoinits // Intentional: color vars must be initialized before use
func init() {
	ApplyTheme(DefaultTheme())
}
