package styles

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/goccy/go-yaml"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDefaultThemeRef(t *testing.T) {
	t.Parallel()

	// DefaultThemeRef should be "default"
	assert.Equal(t, "default", DefaultThemeRef)
}

func TestDefaultTheme(t *testing.T) {
	t.Parallel()

	theme := DefaultTheme()
	require.NotNil(t, theme)

	assert.Equal(t, 1, theme.Version)
	assert.Equal(t, "Default", theme.Name)
	assert.Equal(t, DefaultThemeRef, theme.Ref)

	// Check that colors are populated (values come from embedded default.yaml)
	assert.NotEmpty(t, theme.Colors.TextBright)
	assert.NotEmpty(t, theme.Colors.Accent)
	assert.NotEmpty(t, theme.Colors.Background)
	assert.NotEmpty(t, theme.Colors.Success)

	// Check chroma colors
	assert.NotEmpty(t, theme.Chroma.Keyword)

	// Check markdown theme
	assert.NotEmpty(t, theme.Markdown.Heading)
}

func TestListThemeRefs_EmptyDir(t *testing.T) {
	t.Parallel()

	themesDir := t.TempDir()

	refs, err := listThemeRefsFrom(themesDir)
	require.NoError(t, err)

	// listThemeRefsFrom only lists files, no default injection
	assert.Empty(t, refs)
}

func TestListThemeRefs_NonexistentDir(t *testing.T) {
	t.Parallel()

	refs, err := listThemeRefsFrom("/nonexistent/path/that/does/not/exist")
	require.NoError(t, err)

	// listThemeRefsFrom returns empty for nonexistent dir
	assert.Empty(t, refs)
}

func TestListThemeRefs_WithThemes(t *testing.T) {
	t.Parallel()

	themesDir := t.TempDir()

	// Create some theme files
	require.NoError(t, os.WriteFile(filepath.Join(themesDir, "dark.yaml"), []byte("version: 1\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(themesDir, "light.yml"), []byte("version: 1\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(themesDir, "notatheme.txt"), []byte(""), 0o644))

	refs, err := listThemeRefsFrom(themesDir)
	require.NoError(t, err)

	// Should contain dark + light (not the .txt file), no default injection
	assert.Contains(t, refs, "dark")
	assert.Contains(t, refs, "light")
	assert.NotContains(t, refs, "notatheme")
	assert.Len(t, refs, 2)
}

func TestLoadTheme_Default(t *testing.T) {
	t.Parallel()

	// LoadTheme("default") should return the built-in default theme
	theme, err := LoadTheme(DefaultThemeRef)
	require.NoError(t, err)
	require.NotNil(t, theme)

	assert.Equal(t, "Default", theme.Name)
	assert.Equal(t, DefaultThemeRef, theme.Ref)
	assert.NotEmpty(t, theme.Colors.TextBright)
}

func TestLoadTheme_EmptyRef_Error(t *testing.T) {
	t.Parallel()

	// LoadTheme("") should return an error - caller should pass a valid ref
	_, err := LoadTheme("")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty ref")
}

func TestValidateThemeRef(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		ref     string
		wantErr bool
	}{
		{"empty is valid", "", false},
		{"default is valid", "default", false},
		{"simple name is valid", "tokyo-night", false},
		{"path separator rejected", "foo/bar", true},
		{"backslash rejected", "foo\\bar", true},
		{"traversal rejected", "..", true},
		{"hidden traversal rejected", "foo..bar", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateThemeRef(tt.ref)
			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestLoadTheme_NotFound(t *testing.T) {
	t.Parallel()

	themesDir := t.TempDir()

	_, err := loadThemeFrom("nonexistent", themesDir)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestLoadTheme_PartialOverride(t *testing.T) {
	t.Parallel()

	themesDir := t.TempDir()

	// Create a theme that only overrides a few colors
	themeContent := `version: 1
name: Custom Theme
colors:
  accent: "#FF0000"
  background: "#000000"
`
	require.NoError(t, os.WriteFile(filepath.Join(themesDir, "custom.yaml"), []byte(themeContent), 0o644))

	theme, err := loadThemeFrom("custom", themesDir)
	require.NoError(t, err)
	require.NotNil(t, theme)

	assert.Equal(t, "Custom Theme", theme.Name)
	assert.Equal(t, "custom", theme.Ref)

	// Overridden values
	assert.Equal(t, "#FF0000", theme.Colors.Accent)
	assert.Equal(t, "#000000", theme.Colors.Background)

	// Non-overridden values should be defaults (from default.yaml)
	assert.Equal(t, DefaultTheme().Colors.TextBright, theme.Colors.TextBright)
	assert.Equal(t, DefaultTheme().Colors.Success, theme.Colors.Success)
}

func TestLoadTheme_YmlExtension(t *testing.T) {
	t.Parallel()

	themesDir := t.TempDir()

	themeContent := `version: 1
name: YML Theme
`
	require.NoError(t, os.WriteFile(filepath.Join(themesDir, "ymltheme.yml"), []byte(themeContent), 0o644))

	theme, err := loadThemeFrom("ymltheme", themesDir)
	require.NoError(t, err)
	require.NotNil(t, theme)

	assert.Equal(t, "YML Theme", theme.Name)
}

func TestApplyTheme(t *testing.T) {
	// Note: Cannot use t.Parallel() because ApplyTheme modifies global state

	// Create a custom theme
	theme := DefaultTheme()
	theme.Colors.Accent = "#123456"
	theme.Name = "Test Theme"
	theme.Ref = "test"

	ApplyTheme(theme)

	// CurrentTheme should return the applied theme
	current := CurrentTheme()
	assert.Equal(t, "Test Theme", current.Name)
	assert.Equal(t, "test", current.Ref)

	// Reset to default for other tests
	ApplyTheme(DefaultTheme())
}

func TestMergeTheme_AllFields(t *testing.T) {
	t.Parallel()

	base := DefaultTheme()
	override := &Theme{
		Version: 2,
		Name:    "Override",
		Colors: ThemeColors{
			TextBright: "#FFFFFF",
			Accent:     "#0000FF",
		},
		Chroma: ChromaColors{
			Keyword: "#FF00FF",
		},
		Markdown: MarkdownTheme{
			Heading: "#00FF00",
		},
	}

	merged := mergeTheme(base, override)

	// Overridden values
	assert.Equal(t, 2, merged.Version)
	assert.Equal(t, "Override", merged.Name)
	assert.Equal(t, "#FFFFFF", merged.Colors.TextBright)
	assert.Equal(t, "#0000FF", merged.Colors.Accent)
	assert.Equal(t, "#FF00FF", merged.Chroma.Keyword)
	assert.Equal(t, "#00FF00", merged.Markdown.Heading)

	// Non-overridden values from base (default theme)
	assert.Equal(t, DefaultTheme().Colors.Background, merged.Colors.Background)
	assert.Equal(t, DefaultTheme().Chroma.Comment, merged.Chroma.Comment)
	assert.Equal(t, DefaultTheme().Markdown.Link, merged.Markdown.Link)
}

// --- Theme Infrastructure Reliability Tests ---
// These tests use reflection to automatically catch when new fields are added
// but not properly handled in DefaultTheme(), merge functions, or built-in themes.

// TestDefaultTheme_AllColorsPopulated ensures DefaultTheme() sets every ThemeColors field.
// This catches: adding a new field to ThemeColors but forgetting to set it in DefaultTheme().
func TestDefaultTheme_AllColorsPopulated(t *testing.T) {
	t.Parallel()

	theme := DefaultTheme()

	// Check ThemeColors - all fields must be non-empty
	colorsVal := reflect.ValueOf(theme.Colors)
	for field, value := range colorsVal.Fields() {
		assert.NotEmpty(t, value.String(), "DefaultTheme().Colors.%s is empty - add default in DefaultTheme()", field.Name)
	}

	// Check ChromaColors - all fields must be non-empty
	chromaVal := reflect.ValueOf(theme.Chroma)
	for field, value := range chromaVal.Fields() {
		assert.NotEmpty(t, value.String(), "DefaultTheme().Chroma.%s is empty - add default in DefaultTheme()", field.Name)
	}

	// Check MarkdownTheme - all fields must be non-empty
	mdVal := reflect.ValueOf(theme.Markdown)
	for field, value := range mdVal.Fields() {
		assert.NotEmpty(t, value.String(), "DefaultTheme().Markdown.%s is empty - add default in DefaultTheme()", field.Name)
	}
}

// TestMergeColors_HandlesAllFields ensures mergeColors handles every ThemeColors field.
// This catches: adding a new field to ThemeColors but forgetting to merge it.
func TestMergeColors_HandlesAllFields(t *testing.T) {
	t.Parallel()

	// Create a base with all string fields set to "BASE"
	base := ThemeColors{}
	baseVal := reflect.ValueOf(&base).Elem()
	for _, field := range baseVal.Fields() {
		if field.Kind() == reflect.String {
			field.SetString("BASE")
		}
	}
	base.AgentHues = []float64{10, 20}

	// Create an override with all string fields set to "OVERRIDE"
	override := ThemeColors{}
	overrideVal := reflect.ValueOf(&override).Elem()
	for _, field := range overrideVal.Fields() {
		if field.Kind() == reflect.String {
			field.SetString("OVERRIDE")
		}
	}
	override.AgentHues = []float64{30, 40, 50}

	// Merge should replace all base values with override values
	merged := mergeColors(base, override)
	mergedVal := reflect.ValueOf(merged)

	for field, value := range mergedVal.Fields() {
		if value.Kind() == reflect.String {
			assert.Equal(t, "OVERRIDE", value.String(),
				"mergeColors() doesn't handle ThemeColors.%s - add merge logic in mergeColors()", field.Name)
		}
	}
	assert.Equal(t, []float64{30, 40, 50}, merged.AgentHues,
		"mergeColors() doesn't handle ThemeColors.AgentHues")
}

// TestMergeChromaColors_HandlesAllFields ensures mergeChromaColors handles every ChromaColors field.
func TestMergeChromaColors_HandlesAllFields(t *testing.T) {
	t.Parallel()

	base := ChromaColors{}
	baseVal := reflect.ValueOf(&base).Elem()
	for _, field := range baseVal.Fields() {
		field.SetString("BASE")
	}

	override := ChromaColors{}
	overrideVal := reflect.ValueOf(&override).Elem()
	for _, field := range overrideVal.Fields() {
		field.SetString("OVERRIDE")
	}

	merged := mergeChromaColors(base, override)
	mergedVal := reflect.ValueOf(merged)

	for field, value := range mergedVal.Fields() {
		assert.Equal(t, "OVERRIDE", value.String(),
			"mergeChromaColors() doesn't handle ChromaColors.%s - add merge logic", field.Name)
	}
}

// TestMergeMarkdownTheme_HandlesAllFields ensures mergeMarkdownTheme handles every MarkdownTheme field.
func TestMergeMarkdownTheme_HandlesAllFields(t *testing.T) {
	t.Parallel()

	base := MarkdownTheme{}
	baseVal := reflect.ValueOf(&base).Elem()
	for _, field := range baseVal.Fields() {
		field.SetString("BASE")
	}

	override := MarkdownTheme{}
	overrideVal := reflect.ValueOf(&override).Elem()
	for _, field := range overrideVal.Fields() {
		field.SetString("OVERRIDE")
	}

	merged := mergeMarkdownTheme(base, override)
	mergedVal := reflect.ValueOf(merged)

	for field, value := range mergedVal.Fields() {
		assert.Equal(t, "OVERRIDE", value.String(),
			"mergeMarkdownTheme() doesn't handle MarkdownTheme.%s - add merge logic", field.Name)
	}
}

// TestAllBuiltinThemes_LoadSuccessfully ensures all embedded theme YAMLs parse correctly.
// This catches: YAML syntax errors, incorrect field names, or broken theme files.
func TestAllBuiltinThemes_LoadSuccessfully(t *testing.T) {
	t.Parallel()

	refs, err := listBuiltinThemeRefs()
	require.NoError(t, err)
	require.NotEmpty(t, refs, "no built-in themes found - check //go:embed directive")

	for _, ref := range refs {
		t.Run(ref, func(t *testing.T) {
			t.Parallel()
			theme, err := loadBuiltinTheme(ref)
			require.NoError(t, err, "failed to load built-in theme %q", ref)
			require.NotNil(t, theme)
			assert.Equal(t, ref, theme.Ref)
			assert.NotEmpty(t, theme.Name, "built-in theme %q has no name", ref)
		})
	}
}

// TestAllBuiltinThemes_HaveCoreColors ensures built-in themes explicitly define critical colors.
// These are colors that significantly affect usability and should be intentionally designed.
func TestAllBuiltinThemes_HaveCoreColors(t *testing.T) {
	t.Parallel()

	// Core colors that every theme should explicitly define for good UX
	coreColorFields := []string{
		"TextPrimary",
		"TextSecondary",
		"Background",
		"BackgroundAlt",
		"Accent",
		"Success",
		"Error",
	}

	refs, err := listBuiltinThemeRefs()
	require.NoError(t, err)

	for _, ref := range refs {
		t.Run(ref, func(t *testing.T) {
			t.Parallel()

			// Load the raw theme without merging to check what's explicitly defined
			data, err := builtinThemes.ReadFile("themes/" + ref + ".yaml")
			require.NoError(t, err)

			var rawTheme Theme
			require.NoError(t, yaml.Unmarshal(data, &rawTheme))

			colorsVal := reflect.ValueOf(rawTheme.Colors)
			colorsType := colorsVal.Type()

			for _, fieldName := range coreColorFields {
				field, found := colorsType.FieldByName(fieldName)
				require.True(t, found, "field %s not found in ThemeColors struct", fieldName)

				value := colorsVal.FieldByName(field.Name).String()
				assert.NotEmpty(t, value,
					"built-in theme %q should explicitly define Colors.%s for good UX", ref, fieldName)
			}
		})
	}
}
