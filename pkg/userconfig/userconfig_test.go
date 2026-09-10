package userconfig

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"

	"github.com/goccy/go-yaml"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/paths"
)

func TestConfig_Empty(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	configFile := filepath.Join(tmpDir, "config.yaml")

	config, err := loadFrom(configFile, "")
	require.NoError(t, err)
	assert.Empty(t, config.Aliases)
}

func TestConfig_LoadWithNilAliases(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	configFile := filepath.Join(tmpDir, "config.yaml")

	// Create config file without aliases field
	require.NoError(t, os.WriteFile(configFile, []byte("# empty config\n"), 0o644))

	config, err := loadFrom(configFile, "")
	require.NoError(t, err)
	assert.NotNil(t, config.Aliases)
	assert.Empty(t, config.Aliases)
}

func TestConfig_SetGetAlias(t *testing.T) {
	t.Parallel()

	config := &Config{Aliases: make(map[string]*Alias)}

	err := config.SetAlias("test", &Alias{Path: "myorg/test-agent"})
	require.NoError(t, err)

	alias, ok := config.GetAlias("test")
	assert.True(t, ok)
	assert.Equal(t, "myorg/test-agent", alias.Path)

	_, ok = config.GetAlias("nonexistent")
	assert.False(t, ok)
}

func TestConfig_SetAlias_Validation(t *testing.T) {
	t.Parallel()

	config := &Config{Aliases: make(map[string]*Alias)}

	tests := []struct {
		name      string
		aliasName string
		path      string
		wantErr   string
	}{
		{"empty name", "", "some/path", "alias name cannot be empty"},
		{"empty path", "valid", "", "agent path cannot be empty"},
		{"starts with hyphen", "-invalid", "some/path", "invalid alias name"},
		{"starts with underscore", "_invalid", "some/path", "invalid alias name"},
		{"contains slash", "in/valid", "some/path", "invalid alias name"},
		{"contains space", "in valid", "some/path", "invalid alias name"},
		{"contains dot", "in.valid", "some/path", "invalid alias name"},
		{"path traversal attempt", "../etc/passwd", "some/path", "invalid alias name"},
		{"valid simple", "myalias", "some/path", ""},
		{"valid with hyphen", "my-alias", "some/path", ""},
		{"valid with underscore", "my_alias", "some/path", ""},
		{"valid with numbers", "alias123", "some/path", ""},
		{"valid starts with number", "123alias", "some/path", ""},
		{"valid default", "default", "some/path", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := config.SetAlias(tt.aliasName, &Alias{Path: tt.path})
			if tt.wantErr == "" {
				assert.NoError(t, err)
			} else {
				assert.ErrorContains(t, err, tt.wantErr)
			}
		})
	}
}

func TestConfig_SetGetProviders(t *testing.T) {
	t.Parallel()

	config := &Config{}

	require.Error(t, config.SetProvider("", latest.ProviderConfig{}))
	require.Error(t, config.SetProvider("   ", latest.ProviderConfig{}))
	assert.Nil(t, config.GetProviders())

	provider := latest.ProviderConfig{
		BaseURL:  "https://llm.corp.example.com/v1",
		APIType:  "openai_chatcompletions",
		TokenKey: "MYPROVIDER_API_KEY",
	}
	require.NoError(t, config.SetProvider("myprovider", provider))

	providers := config.GetProviders()
	require.Len(t, providers, 1)
	assert.Equal(t, provider, providers["myprovider"])

	// The returned map is a copy: mutating it must not affect the config.
	delete(providers, "myprovider")
	assert.Len(t, config.GetProviders(), 1)
}

func TestConfig_ProvidersRoundTrip(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	configFile := filepath.Join(tmpDir, "config.yaml")

	config := &Config{}
	require.NoError(t, config.SetProvider("myprovider", latest.ProviderConfig{
		BaseURL:  "https://llm.corp.example.com/v1",
		APIType:  "openai_responses",
		TokenKey: "MYPROVIDER_API_KEY",
	}))
	require.NoError(t, config.saveTo(configFile))

	loaded, err := loadFrom(configFile, "")
	require.NoError(t, err)

	providers := loaded.GetProviders()
	require.Len(t, providers, 1)
	assert.Equal(t, "https://llm.corp.example.com/v1", providers["myprovider"].BaseURL)
	assert.Equal(t, "openai_responses", providers["myprovider"].APIType)
	assert.Equal(t, "MYPROVIDER_API_KEY", providers["myprovider"].TokenKey)
}

func TestValidateAliasName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		wantErr bool
	}{
		{"", true},
		{"-starts-with-hyphen", true},
		{"_starts-with-underscore", true},
		{"has space", true},
		{"has/slash", true},
		{"has.dot", true},
		{"has:colon", true},
		{"valid", false},
		{"valid-name", false},
		{"valid_name", false},
		{"ValidName", false},
		{"valid123", false},
		{"123valid", false},
		{"a", false},
		{"A", false},
		{"1", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := ValidateAliasName(tt.name)
			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestConfig_DeleteAlias(t *testing.T) {
	t.Parallel()

	config := &Config{
		Aliases: map[string]*Alias{
			"code":    {Path: "myorg/notion-expert"},
			"myagent": {Path: "/path/to/myagent.yaml"},
		},
	}

	assert.True(t, config.DeleteAlias("code"))
	assert.Len(t, config.Aliases, 1)

	assert.False(t, config.DeleteAlias("nonexistent"))
}

func TestConfig_SaveAndLoad(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	configFile := filepath.Join(tmpDir, "config.yaml")

	config := &Config{
		Aliases: map[string]*Alias{
			"code":    {Path: "myorg/notion-expert"},
			"myagent": {Path: "/path/to/myagent.yaml"},
		},
	}

	require.NoError(t, config.saveTo(configFile))

	loaded, err := loadFrom(configFile, "")
	require.NoError(t, err)

	assert.Equal(t, CurrentVersion, loaded.Version)
	assert.Equal(t, config.Aliases["code"].Path, loaded.Aliases["code"].Path)
	assert.Equal(t, config.Aliases["myagent"].Path, loaded.Aliases["myagent"].Path)
}

func TestSettings_LayoutRoundTrip(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	configFile := filepath.Join(tmpDir, "config.yaml")

	config := &Config{
		Settings: &Settings{
			Layout: &LayoutSettings{
				SidebarPosition:  "left",
				SectionSpacing:   "compact",
				SidebarInfoMode:  "detailed",
				ActiveAgentsOnly: true,
				HideSessionPath:  true,
				HideUsage:        true,
				HideTodos:        true,
			},
		},
	}

	require.NoError(t, config.saveTo(configFile))

	data, err := os.ReadFile(configFile)
	require.NoError(t, err)
	assert.Contains(t, string(data), "hide_session_path: true")
	assert.Contains(t, string(data), "sidebar_info_mode: detailed")
	assert.Contains(t, string(data), "active_agents_only: true")

	loaded, err := loadFrom(configFile, "")
	require.NoError(t, err)

	layout := loaded.GetSettings().GetLayout()
	assert.Equal(t, "left", layout.SidebarPosition)
	assert.Equal(t, "compact", layout.SectionSpacing)
	assert.Equal(t, "detailed", layout.SidebarInfoMode)
	assert.True(t, layout.ActiveAgentsOnly)
	assert.True(t, layout.HideSessionPath)
	assert.True(t, layout.HideUsage)
	assert.False(t, layout.HideAgents)
	assert.False(t, layout.HideTools)
	assert.True(t, layout.HideTodos)
}

func TestSettings_GetLayoutDefaults(t *testing.T) {
	t.Parallel()

	var nilSettings *Settings
	assert.Equal(t, LayoutSettings{}, nilSettings.GetLayout())
	assert.Equal(t, LayoutSettings{}, (&Settings{}).GetLayout())
}

func TestConfig_MigrateFromLegacy(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	configFile := filepath.Join(tmpDir, "config.yaml")
	legacyFile := filepath.Join(tmpDir, "aliases.yaml")

	// Create legacy aliases file
	legacyContent := `code: myorg/notion-expert
myagent: /path/to/myagent.yaml
`
	require.NoError(t, os.WriteFile(legacyFile, []byte(legacyContent), 0o644))

	// Load config - should migrate from legacy and persist
	config, err := loadFrom(configFile, legacyFile)
	require.NoError(t, err)

	assert.Len(t, config.Aliases, 2)
	assert.Equal(t, "myorg/notion-expert", config.Aliases["code"].Path)

	// Verify migration was persisted
	assert.FileExists(t, configFile)

	// Verify legacy file was deleted (not renamed to .bak)
	assert.NoFileExists(t, legacyFile)
	assert.NoFileExists(t, legacyFile+".bak")
}

func TestConfig_MigrateFromLegacy_MalformedFile(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	configFile := filepath.Join(tmpDir, "config.yaml")
	legacyFile := filepath.Join(tmpDir, "aliases.yaml")

	// Create malformed legacy aliases file
	require.NoError(t, os.WriteFile(legacyFile, []byte("not: valid: yaml: content"), 0o644))

	// Load config - should not fail, just skip migration
	config, err := loadFrom(configFile, legacyFile)
	require.NoError(t, err)
	assert.Empty(t, config.Aliases)

	// Legacy file should remain (not renamed since migration failed)
	assert.FileExists(t, legacyFile)
}

func TestConfig_NoMigrationWhenAliasesExist(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	configFile := filepath.Join(tmpDir, "config.yaml")
	legacyFile := filepath.Join(tmpDir, "aliases.yaml")

	// Create config with existing alias - use new struct format
	require.NoError(t, os.WriteFile(configFile, []byte("aliases:\n  existing:\n    path: already-here\n"), 0o644))

	// Create legacy file
	require.NoError(t, os.WriteFile(legacyFile, []byte("code: should-not-migrate\n"), 0o644))

	config, err := loadFrom(configFile, legacyFile)
	require.NoError(t, err)

	assert.Len(t, config.Aliases, 1)
	assert.Equal(t, "already-here", config.Aliases["existing"].Path)
	_, hasCode := config.Aliases["code"]
	assert.False(t, hasCode)
}

func TestConfig_MigrateWhenConfigEmpty(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	configFile := filepath.Join(tmpDir, "config.yaml")
	legacyFile := filepath.Join(tmpDir, "aliases.yaml")

	// Create empty config
	require.NoError(t, os.WriteFile(configFile, []byte("aliases: {}\n"), 0o644))

	// Create legacy file
	require.NoError(t, os.WriteFile(legacyFile, []byte("code: myorg/notion-expert\n"), 0o644))

	config, err := loadFrom(configFile, legacyFile)
	require.NoError(t, err)

	assert.Len(t, config.Aliases, 1)
	assert.Equal(t, "myorg/notion-expert", config.Aliases["code"].Path)
}

func TestConfig_NoLegacyFile(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	configFile := filepath.Join(tmpDir, "config.yaml")
	nonExistentLegacy := filepath.Join(tmpDir, "aliases.yaml")

	// Load config with non-existent legacy path
	config, err := loadFrom(configFile, nonExistentLegacy)
	require.NoError(t, err)

	// Aliases should be empty
	assert.Empty(t, config.Aliases)
}

func TestConfig_AtomicWrite(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	configFile := filepath.Join(tmpDir, "config.yaml")

	config := &Config{
		Aliases: map[string]*Alias{
			"test": {Path: "myorg/test-agent"},
		},
	}

	// Save should succeed
	require.NoError(t, config.saveTo(configFile))

	// Verify file exists and has correct content
	loaded, err := loadFrom(configFile, "")
	require.NoError(t, err)
	assert.Equal(t, "myorg/test-agent", loaded.Aliases["test"].Path)

	// Verify no temp files left behind
	entries, err := os.ReadDir(tmpDir)
	require.NoError(t, err)
	assert.Len(t, entries, 1)
	assert.Equal(t, "config.yaml", entries[0].Name())
}

func TestConfig_AtomicWrite_Permissions(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	configFile := filepath.Join(tmpDir, "config.yaml")

	config := &Config{
		Aliases: map[string]*Alias{
			"test": {Path: "myorg/test-agent"},
		},
	}

	require.NoError(t, config.saveTo(configFile))

	// Verify file permissions are 0600
	info, err := os.Stat(configFile)
	require.NoError(t, err)
	if runtime.GOOS != "windows" {
		assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	}
}

func TestConfig_AliasWithOptions(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	configFile := filepath.Join(tmpDir, "config.yaml")

	config := &Config{
		Aliases: map[string]*Alias{
			"yolo-agent":  {Path: "myorg/coder", Yolo: true},
			"model-agent": {Path: "myorg/coder", Model: "openai/gpt-4o-mini"},
			"both":        {Path: "myorg/coder", Yolo: true, Model: "anthropic/claude-sonnet-4-0"},
		},
	}

	require.NoError(t, config.saveTo(configFile))

	loaded, err := loadFrom(configFile, "")
	require.NoError(t, err)

	// Verify yolo option
	yoloAlias, ok := loaded.GetAlias("yolo-agent")
	require.True(t, ok)
	assert.Equal(t, "myorg/coder", yoloAlias.Path)
	assert.True(t, yoloAlias.Yolo)
	assert.Empty(t, yoloAlias.Model)

	// Verify model option
	modelAlias, ok := loaded.GetAlias("model-agent")
	require.True(t, ok)
	assert.Equal(t, "myorg/coder", modelAlias.Path)
	assert.False(t, modelAlias.Yolo)
	assert.Equal(t, "openai/gpt-4o-mini", modelAlias.Model)

	// Verify both options
	bothAlias, ok := loaded.GetAlias("both")
	require.True(t, ok)
	assert.Equal(t, "myorg/coder", bothAlias.Path)
	assert.True(t, bothAlias.Yolo)
	assert.Equal(t, "anthropic/claude-sonnet-4-0", bothAlias.Model)
}

func TestConfig_SetAliasWithOptions(t *testing.T) {
	t.Parallel()

	config := &Config{Aliases: make(map[string]*Alias)}

	// Set alias with yolo option
	err := config.SetAlias("yolo-test", &Alias{
		Path: "myorg/test",
		Yolo: true,
	})
	require.NoError(t, err)

	alias, ok := config.GetAlias("yolo-test")
	require.True(t, ok)
	assert.True(t, alias.Yolo)

	// Set alias with model option
	err = config.SetAlias("model-test", &Alias{
		Path:  "myorg/test",
		Model: "openai/gpt-4o",
	})
	require.NoError(t, err)

	alias, ok = config.GetAlias("model-test")
	require.True(t, ok)
	assert.Equal(t, "openai/gpt-4o", alias.Model)
}

func TestConfig_ModelsGateway(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	configFile := filepath.Join(tmpDir, "config.yaml")

	config := &Config{
		ModelsGateway: "https://models.example.com",
		Aliases: map[string]*Alias{
			"test": {Path: "myorg/test-agent"},
		},
	}

	require.NoError(t, config.saveTo(configFile))

	loaded, err := loadFrom(configFile, "")
	require.NoError(t, err)

	assert.Equal(t, "https://models.example.com", loaded.ModelsGateway)
	assert.Equal(t, "myorg/test-agent", loaded.Aliases["test"].Path)
}

func TestConfig_ModelsGateway_Empty(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	configFile := filepath.Join(tmpDir, "config.yaml")

	config, err := loadFrom(configFile, "")
	require.NoError(t, err)

	assert.Empty(t, config.ModelsGateway)
}

func TestConfig_ModelsGateway_OnlyGateway(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	configFile := filepath.Join(tmpDir, "config.yaml")

	// Create config file with only models_gateway
	require.NoError(t, os.WriteFile(configFile, []byte("models_gateway: https://my-gateway.example.com\n"), 0o644))

	config, err := loadFrom(configFile, "")
	require.NoError(t, err)

	assert.Equal(t, "https://my-gateway.example.com", config.ModelsGateway)
	assert.Empty(t, config.Aliases)
}

func TestConfig_Version(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	configFile := filepath.Join(tmpDir, "config.yaml")

	// Create config without version
	config := &Config{
		Aliases: map[string]*Alias{
			"test": {Path: "myorg/test-agent"},
		},
	}

	// Save should set version to CurrentVersion
	require.NoError(t, config.saveTo(configFile))
	assert.Equal(t, CurrentVersion, config.Version)

	// Load should read the version
	loaded, err := loadFrom(configFile, "")
	require.NoError(t, err)
	assert.Equal(t, CurrentVersion, loaded.Version)

	// Verify version is written to file
	data, err := os.ReadFile(configFile)
	require.NoError(t, err)
	assert.Contains(t, string(data), "version: v1")
}

func TestConfig_Version_LoadLegacyWithoutVersion(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	configFile := filepath.Join(tmpDir, "config.yaml")

	// Create config file without version field (simulates old config)
	require.NoError(t, os.WriteFile(configFile, []byte("aliases:\n  test:\n    path: myorg/test\n"), 0o644))

	// Load should work and version should be empty (not automatically upgraded on read)
	config, err := loadFrom(configFile, "")
	require.NoError(t, err)
	assert.Empty(t, config.Version)
	assert.Equal(t, "myorg/test", config.Aliases["test"].Path)

	// Saving should add the version
	require.NoError(t, config.saveTo(configFile))
	assert.Equal(t, CurrentVersion, config.Version)
}

func TestConfig_Settings_HideToolResults(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	configFile := filepath.Join(tmpDir, "config.yaml")

	config := &Config{
		Settings: &Settings{
			HideToolResults: true,
		},
	}

	require.NoError(t, config.saveTo(configFile))

	loaded, err := loadFrom(configFile, "")
	require.NoError(t, err)

	assert.NotNil(t, loaded.Settings)
	assert.True(t, loaded.Settings.HideToolResults)
}

func TestConfig_Settings_Lean(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	configFile := filepath.Join(tmpDir, "config.yaml")

	config := &Config{
		Settings: &Settings{
			Lean: true,
		},
	}

	require.NoError(t, config.saveTo(configFile))

	loaded, err := loadFrom(configFile, "")
	require.NoError(t, err)

	assert.NotNil(t, loaded.Settings)
	assert.True(t, loaded.Settings.Lean)
}

func TestConfig_Settings_Empty(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	configFile := filepath.Join(tmpDir, "config.yaml")

	config, err := loadFrom(configFile, "")
	require.NoError(t, err)

	// GetSettings should return an empty Settings struct, not nil
	settings := config.GetSettings()
	assert.NotNil(t, settings)
	assert.False(t, settings.HideToolResults)
	assert.False(t, settings.Lean)
	assert.False(t, settings.GetExpandThinking())
	assert.True(t, settings.GetRenderImages())
	assert.True(t, settings.GetShowBanner())
}

func TestConfig_Settings_RenderImages(t *testing.T) {
	t.Parallel()

	configFile := filepath.Join(t.TempDir(), "config.yaml")
	renderImages := false
	config := &Config{Settings: &Settings{RenderImages: &renderImages}}
	require.NoError(t, config.saveTo(configFile))

	loaded, err := loadFrom(configFile, "")
	require.NoError(t, err)
	assert.False(t, loaded.Settings.GetRenderImages())
}

func TestConfig_Settings_ShowBanner(t *testing.T) {
	t.Parallel()

	configFile := filepath.Join(t.TempDir(), "config.yaml")
	showBanner := false
	config := &Config{Settings: &Settings{ShowBanner: &showBanner}}
	require.NoError(t, config.saveTo(configFile))

	loaded, err := loadFrom(configFile, "")
	require.NoError(t, err)
	assert.False(t, loaded.Settings.GetShowBanner())
	assert.True(t, (*Settings)(nil).GetShowBanner(), "banner is shown by default")
}

func TestConfig_Settings_ExpandThinking(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	configFile := filepath.Join(tmpDir, "config.yaml")
	expandThinking := false

	config := &Config{
		Settings: &Settings{
			ExpandThinking: &expandThinking,
		},
	}

	require.NoError(t, config.saveTo(configFile))

	loaded, err := loadFrom(configFile, "")
	require.NoError(t, err)
	require.NotNil(t, loaded.Settings)
	require.NotNil(t, loaded.Settings.ExpandThinking)
	assert.False(t, loaded.Settings.GetExpandThinking())
}

func TestConfig_Settings_GetSettingsNil(t *testing.T) {
	t.Parallel()

	config := &Config{Aliases: make(map[string]*Alias)}

	// GetSettings should return an empty Settings struct when Settings is nil
	settings := config.GetSettings()
	assert.NotNil(t, settings)
	assert.False(t, settings.HideToolResults)
	assert.False(t, settings.GetExpandThinking())
	assert.True(t, settings.GetRenderImages())
}

func TestConfig_AliasWithHideToolResults(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	configFile := filepath.Join(tmpDir, "config.yaml")

	config := &Config{
		Aliases: map[string]*Alias{
			"hidden": {Path: "myorg/coder", HideToolResults: true},
			"full":   {Path: "myorg/coder", Yolo: true, Model: "openai/gpt-4o", HideToolResults: true},
		},
	}

	require.NoError(t, config.saveTo(configFile))

	loaded, err := loadFrom(configFile, "")
	require.NoError(t, err)

	// Verify hide_tool_results option
	hiddenAlias, ok := loaded.GetAlias("hidden")
	require.True(t, ok)
	assert.Equal(t, "myorg/coder", hiddenAlias.Path)
	assert.True(t, hiddenAlias.HideToolResults)
	assert.False(t, hiddenAlias.Yolo)
	assert.Empty(t, hiddenAlias.Model)

	// Verify all options together
	fullAlias, ok := loaded.GetAlias("full")
	require.True(t, ok)
	assert.True(t, fullAlias.HideToolResults)
	assert.True(t, fullAlias.Yolo)
	assert.Equal(t, "openai/gpt-4o", fullAlias.Model)
}

func TestAlias_HasOptions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		alias    *Alias
		expected bool
	}{
		{"nil alias", nil, false},
		{"empty alias", &Alias{Path: "test"}, false},
		{"yolo only", &Alias{Path: "test", Yolo: true}, true},
		{"safety only", &Alias{Path: "test", Safety: latest.SafetyModeBalanced}, true},
		{"model only", &Alias{Path: "test", Model: "openai/gpt-4o"}, true},
		{"hide_tool_results only", &Alias{Path: "test", HideToolResults: true}, true},
		{"sandbox only", &Alias{Path: "test", Sandbox: true}, true},
		{"all options", &Alias{Path: "test", Yolo: true, Model: "openai/gpt-4o", HideToolResults: true, Sandbox: true}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.expected, tt.alias.HasOptions())
		})
	}
}

func TestConfig_CredentialHelper(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	configFile := filepath.Join(tmpDir, "config.yaml")

	config := &Config{
		CredentialHelper: &CredentialHelper{
			Command: "my-credential-helper",
			Args:    []string{"get-token"},
		},
	}

	require.NoError(t, config.saveTo(configFile))

	loaded, err := loadFrom(configFile, "")
	require.NoError(t, err)

	assert.NotNil(t, loaded.CredentialHelper)
	assert.Equal(t, "my-credential-helper", loaded.CredentialHelper.Command)
	assert.Equal(t, []string{"get-token"}, loaded.CredentialHelper.Args)
}

func TestConfig_CredentialHelper_Empty(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	configFile := filepath.Join(tmpDir, "config.yaml")

	config, err := loadFrom(configFile, "")
	require.NoError(t, err)

	assert.Nil(t, config.CredentialHelper)
}

func TestDefaultModelConfig_Shorthand(t *testing.T) {
	t.Parallel()

	yamlContent := `default_model: anthropic/claude-sonnet-4-5`

	var config Config
	err := yaml.Unmarshal([]byte(yamlContent), &config)
	require.NoError(t, err)

	require.NotNil(t, config.DefaultModel)
	assert.Equal(t, "anthropic", config.DefaultModel.Provider)
	assert.Equal(t, "claude-sonnet-4-5", config.DefaultModel.Model)
	assert.Nil(t, config.DefaultModel.MaxTokens)
}

func TestDefaultModelConfig_FullDefinition(t *testing.T) {
	t.Parallel()

	yamlContent := `default_model:
  provider: anthropic
  model: claude-sonnet-4-5
  max_tokens: 64000
  thinking_budget: 10000`

	var config Config
	err := yaml.Unmarshal([]byte(yamlContent), &config)
	require.NoError(t, err)

	require.NotNil(t, config.DefaultModel)
	assert.Equal(t, "anthropic", config.DefaultModel.Provider)
	assert.Equal(t, "claude-sonnet-4-5", config.DefaultModel.Model)
	require.NotNil(t, config.DefaultModel.MaxTokens)
	assert.Equal(t, int64(64000), *config.DefaultModel.MaxTokens)
	require.NotNil(t, config.DefaultModel.ThinkingBudget)
	assert.Equal(t, 10000, config.DefaultModel.ThinkingBudget.Tokens)
}

func TestDefaultModelConfig_FullDefinitionWithEffort(t *testing.T) {
	t.Parallel()

	yamlContent := `default_model:
  provider: openai
  model: o1
  thinking_budget: high`

	var config Config
	err := yaml.Unmarshal([]byte(yamlContent), &config)
	require.NoError(t, err)

	require.NotNil(t, config.DefaultModel)
	assert.Equal(t, "openai", config.DefaultModel.Provider)
	assert.Equal(t, "o1", config.DefaultModel.Model)
	require.NotNil(t, config.DefaultModel.ThinkingBudget)
	assert.Equal(t, "high", config.DefaultModel.ThinkingBudget.Effort)
}

func TestDefaultModelConfig_Marshal_ShorthandOutput(t *testing.T) {
	t.Parallel()

	config := &latest.FlexibleModelConfig{
		ModelConfig: latest.ModelConfig{
			Provider: "anthropic",
			Model:    "claude-sonnet-4-5",
		},
	}

	data, err := yaml.Marshal(config)
	require.NoError(t, err)

	// Should output shorthand format when only provider/model are set
	assert.Equal(t, "anthropic/claude-sonnet-4-5\n", string(data))
}

func TestDefaultModelConfig_Marshal_FullOutput(t *testing.T) {
	t.Parallel()

	maxTokens := int64(64000)
	config := &latest.FlexibleModelConfig{
		ModelConfig: latest.ModelConfig{
			Provider:  "anthropic",
			Model:     "claude-sonnet-4-5",
			MaxTokens: &maxTokens,
		},
	}

	data, err := yaml.Marshal(config)
	require.NoError(t, err)

	// Should output full format when extra options are set
	assert.Contains(t, string(data), "provider:")
	assert.Contains(t, string(data), "model:")
	assert.Contains(t, string(data), "max_tokens:")
}

func TestDefaultModelConfig_InvalidShorthand(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		yaml    string
		wantErr bool
	}{
		{"no slash", "default_model: anthropic", true},
		{"empty provider", "default_model: /model", true},
		{"empty model", "default_model: provider/", true},
		{"valid shorthand", "default_model: anthropic/claude-sonnet-4-5", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var config Config
			err := yaml.Unmarshal([]byte(tt.yaml), &config)
			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestConfig_DefaultModel_SaveAndLoad(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	configFile := filepath.Join(tmpDir, "config.yaml")

	maxTokens := int64(64000)
	config := &Config{
		DefaultModel: &latest.FlexibleModelConfig{
			ModelConfig: latest.ModelConfig{
				Provider:       "anthropic",
				Model:          "claude-sonnet-4-5",
				MaxTokens:      &maxTokens,
				ThinkingBudget: &latest.ThinkingBudget{Tokens: 10000},
			},
		},
	}

	require.NoError(t, config.saveTo(configFile))

	loaded, err := loadFrom(configFile, "")
	require.NoError(t, err)

	require.NotNil(t, loaded.DefaultModel)
	assert.Equal(t, "anthropic", loaded.DefaultModel.Provider)
	assert.Equal(t, "claude-sonnet-4-5", loaded.DefaultModel.Model)
	require.NotNil(t, loaded.DefaultModel.MaxTokens)
	assert.Equal(t, int64(64000), *loaded.DefaultModel.MaxTokens)
	require.NotNil(t, loaded.DefaultModel.ThinkingBudget)
	assert.Equal(t, 10000, loaded.DefaultModel.ThinkingBudget.Tokens)
}

func TestGet_Empty(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	// No config file exists
	settings := Get()
	require.NotNil(t, settings)
	assert.False(t, settings.HideToolResults)
	assert.False(t, settings.GetExpandThinking())
}

func TestGet_WithHideToolResults(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	// Set up config with settings
	cfg, err := Load()
	require.NoError(t, err)
	cfg.Settings = &Settings{
		HideToolResults: true,
	}
	require.NoError(t, cfg.Save())

	// Get settings
	settings := Get()
	require.NotNil(t, settings)
	assert.True(t, settings.HideToolResults)
}

func TestSettings_GetSound(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		settings *Settings
		expected bool
	}{
		{"nil settings", nil, false},
		{"empty settings", &Settings{}, false},
		{"explicitly enabled", &Settings{Sound: true}, true},
		{"explicitly disabled", &Settings{Sound: false}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.expected, tt.settings.GetSound())
		})
	}
}

func TestSettings_CacheStablePromptsEnabled(t *testing.T) {
	t.Parallel()

	assert.False(t, (*Settings)(nil).CacheStablePromptsEnabled())
	assert.False(t, (&Settings{}).CacheStablePromptsEnabled())
	assert.True(t, (&Settings{CacheStablePrompts: new(true)}).CacheStablePromptsEnabled())
	assert.False(t, (&Settings{CacheStablePrompts: new(false)}).CacheStablePromptsEnabled())
}

func TestSettings_SnapshotsEnabled(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name     string
		settings *Settings
		want     bool
	}{
		{"nil settings", nil, false},
		{"empty settings", &Settings{}, false},
		{"explicitly enabled", &Settings{Snapshot: new(true)}, true},
		{"explicitly disabled", &Settings{Snapshot: new(false)}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, tt.settings.SnapshotsEnabled())
		})
	}
}

func TestSettings_RestoreTabs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		settings *Settings
		expected bool
	}{
		{"nil settings", nil, false},
		{"empty settings", &Settings{}, false},
		{"explicitly disabled", &Settings{RestoreTabs: new(false)}, false},
		{"explicitly enabled", &Settings{RestoreTabs: new(true)}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			// Get settings through GetSettings to test default behavior
			config := &Config{Settings: tt.settings}
			settings := config.GetSettings()
			if settings.RestoreTabs == nil {
				t.Fatal("RestoreTabs should never be nil after GetSettings()")
			}
			assert.Equal(t, tt.expected, *settings.RestoreTabs)
		})
	}
}

func TestConfig_PermissionsRoundTrip(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	configFile := filepath.Join(tmpDir, "config.yaml")

	original := &Config{
		Aliases: make(map[string]*Alias),
		Settings: &Settings{
			Permissions: &latest.PermissionsConfig{
				Allow: []string{"read_*", "shell:cmd=git*"},
				Deny:  []string{"shell:cmd=rm*"},
				Ask:   []string{"shell:cmd=docker*"},
			},
		},
	}

	err := original.saveTo(configFile)
	require.NoError(t, err)

	loaded, err := loadFrom(configFile, "")
	require.NoError(t, err)

	require.NotNil(t, loaded.Settings)
	require.NotNil(t, loaded.Settings.Permissions)
	assert.Equal(t, original.Settings.Permissions.Allow, loaded.Settings.Permissions.Allow)
	assert.Equal(t, original.Settings.Permissions.Deny, loaded.Settings.Permissions.Deny)
	assert.Equal(t, original.Settings.Permissions.Ask, loaded.Settings.Permissions.Ask)
}

func TestConfig_AddSandboxHosts(t *testing.T) {
	t.Parallel()

	cfg := &Config{}

	added, err := cfg.AddSandboxHosts("api.example.com", "registry.npmjs.org:443")
	require.NoError(t, err)
	assert.Equal(t, []string{"api.example.com", "registry.npmjs.org:443"}, added)
	assert.Equal(t, []string{"api.example.com", "registry.npmjs.org:443"}, cfg.SandboxAllowlist)

	// Adding an existing host is a no-op and does not duplicate it.
	added, err = cfg.AddSandboxHosts("api.example.com", "new.example.com")
	require.NoError(t, err)
	assert.Equal(t, []string{"new.example.com"}, added)
	assert.Equal(t, []string{"api.example.com", "registry.npmjs.org:443", "new.example.com"}, cfg.SandboxAllowlist)

	// Whitespace is trimmed; embedded whitespace and commas are rejected.
	added, err = cfg.AddSandboxHosts("  trimmed.example.com  ")
	require.NoError(t, err)
	assert.Equal(t, []string{"trimmed.example.com"}, added)

	_, err = cfg.AddSandboxHosts("a.example.com,b.example.com")
	require.Error(t, err)

	_, err = cfg.AddSandboxHosts("has space.example.com")
	require.Error(t, err)
}

// A failed batch must leave SandboxAllowlist untouched: a valid
// host followed by a malformed one used to mutate the slice for the
// valid entry before bailing out, leaving the in-memory *Config
// inconsistent with what would have been persisted.
func TestConfig_AddSandboxHosts_AllOrNothing(t *testing.T) {
	t.Parallel()

	cfg := &Config{SandboxAllowlist: []string{"existing.example.com"}}

	_, err := cfg.AddSandboxHosts("valid.example.com", "bad,host.example.com")
	require.Error(t, err)
	assert.Equal(t, []string{"existing.example.com"}, cfg.SandboxAllowlist,
		"a malformed entry must not partially mutate the allowlist")
}

func TestConfig_RemoveSandboxHost(t *testing.T) {
	t.Parallel()

	cfg := &Config{SandboxAllowlist: []string{"a.example.com", "b.example.com"}}

	assert.True(t, cfg.RemoveSandboxHost("a.example.com"))
	assert.Equal(t, []string{"b.example.com"}, cfg.SandboxAllowlist)

	assert.False(t, cfg.RemoveSandboxHost("missing.example.com"))
	assert.False(t, cfg.RemoveSandboxHost(""))
}

func TestConfig_SandboxAllowlistRoundTrip(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	configFile := filepath.Join(tmpDir, "config.yaml")

	original := &Config{
		Aliases:          make(map[string]*Alias),
		SandboxAllowlist: []string{"api.example.com", "registry.npmjs.org:443"},
	}
	require.NoError(t, original.saveTo(configFile))

	loaded, err := loadFrom(configFile, "")
	require.NoError(t, err)
	assert.Equal(t, original.SandboxAllowlist, loaded.SandboxAllowlist)
}

// Regression test for docker/docker-agent#3536: a hand-written config using
// the single-mapping hook form must not fail the whole config load, which
// silently dropped aliases and reset every setting.
func TestConfig_SingleMappingHooksKeepAliases(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	configFile := filepath.Join(tmpDir, "config.yaml")

	content := `version: v1
aliases:
  default:
    path: /Users/blabla/dev/tools/cagent/agent.yaml
settings:
  theme: catppuccin-mocha
  hooks:
    on_user_input:
      type: command
      command: terminal-notifier -message "needs input"
    stop:
      type: command
      command: terminal-notifier -message "completed"
`
	require.NoError(t, os.WriteFile(configFile, []byte(content), 0o600))

	cfg, err := loadFrom(configFile, "")
	require.NoError(t, err)

	alias, ok := cfg.GetAlias("default")
	require.True(t, ok, "alias must survive a config with single-mapping hooks")
	assert.Equal(t, "/Users/blabla/dev/tools/cagent/agent.yaml", alias.Path)

	require.NotNil(t, cfg.Settings)
	assert.Equal(t, "catppuccin-mocha", cfg.Settings.Theme)

	hooks := cfg.Settings.GlobalHooks()
	require.NotNil(t, hooks)
	require.Len(t, hooks.OnUserInput, 1)
	require.Len(t, hooks.Stop, 1)
	assert.Equal(t, "command", hooks.OnUserInput[0].Type)
}

func TestConfig_HooksRoundTrip(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	configFile := filepath.Join(tmpDir, "config.yaml")

	original := &Config{
		Aliases: make(map[string]*Alias),
		Settings: &Settings{
			Hooks: &latest.HooksConfig{
				SessionStart: []latest.HookDefinition{{Type: "command", Command: "echo start"}},
				PreCompact:   []latest.HookDefinition{{Type: "command", Command: "echo compact"}},
			},
		},
	}

	require.NoError(t, original.saveTo(configFile))

	loaded, err := loadFrom(configFile, "")
	require.NoError(t, err)
	require.NotNil(t, loaded.Settings)
	require.NotNil(t, loaded.Settings.Hooks)
	assert.Equal(t, original.Settings.Hooks.SessionStart, loaded.Settings.Hooks.SessionStart)
	assert.Equal(t, original.Settings.Hooks.PreCompact, loaded.Settings.Hooks.PreCompact)

	reloaded, err := yaml.Marshal(loaded)
	require.NoError(t, err)
	assert.Contains(t, string(reloaded), "session_start:")
	assert.Contains(t, string(reloaded), "pre_compact:")
}

func TestConfig_Settings_HooksEmpty(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	configFile := filepath.Join(tmpDir, "config.yaml")
	require.NoError(t, os.WriteFile(configFile, []byte("settings:\n  hide_tool_results: true\n"), 0o644))

	cfg, err := loadFrom(configFile, "")
	require.NoError(t, err)
	require.NotNil(t, cfg.Settings)
	assert.Nil(t, cfg.Settings.Hooks)
	assert.Nil(t, cfg.Settings.GlobalHooks())
	assert.Nil(t, (*Settings)(nil).GlobalHooks())
}

func TestGet_MalformedConfigReturnsSafeDefaults(t *testing.T) {
	// Not parallel: SetConfigDir mutates process-global state.
	dir := t.TempDir()
	paths.SetConfigDir(dir)
	t.Cleanup(func() { paths.SetConfigDir("") })

	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("settings: [not a map"), 0o600))

	settings := Get()
	require.NotNil(t, settings)
	require.NotNil(t, settings.RestoreTabs, "RestoreTabs must be non-nil even when the config cannot be loaded")
	assert.False(t, settings.GetRestoreTabs())
}

func TestSettings_GetRestoreTabs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		settings *Settings
		expected bool
	}{
		{"nil settings", nil, false},
		{"empty settings", &Settings{}, false},
		{"explicitly disabled", &Settings{RestoreTabs: new(false)}, false},
		{"explicitly enabled", &Settings{RestoreTabs: new(true)}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.expected, tt.settings.GetRestoreTabs())
		})
	}
}

func TestLoadSave_PreservesCommentsAndUnknownKeys(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	configFile := filepath.Join(tmpDir, "config.yaml")
	input := `# docker agent user configuration
version: v1
aliases:
  dev:
    path: ./dev.yaml
settings:
  theme: dark # my favorite
  future_flag: true
top_secret_future: keep-me
`
	require.NoError(t, os.WriteFile(configFile, []byte(input), 0o600))

	cfg, err := loadFrom(configFile, "")
	require.NoError(t, err)
	require.NoError(t, cfg.SetAlias("extra", &Alias{Path: "./extra.yaml"}))
	require.NoError(t, cfg.saveTo(configFile))

	data, err := os.ReadFile(configFile)
	require.NoError(t, err)
	out := string(data)

	assert.Contains(t, out, "# docker agent user configuration")
	assert.Contains(t, out, "# my favorite")
	assert.Contains(t, out, "future_flag: true")
	assert.Contains(t, out, "top_secret_future: keep-me")
	assert.Contains(t, out, "extra")

	reloaded, err := loadFrom(configFile, "")
	require.NoError(t, err)
	assert.Equal(t, "dark", reloaded.Settings.Theme)
	assert.Len(t, reloaded.Aliases, 2)
}

func TestSave_ClearedCommentedSectionStillSaves(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	configFile := filepath.Join(tmpDir, "config.yaml")
	input := `version: v1
settings:
  theme: dark
  # sidebar on the left
  layout:
    sidebar_position: left
`
	require.NoError(t, os.WriteFile(configFile, []byte(input), 0o600))

	cfg, err := loadFrom(configFile, "")
	require.NoError(t, err)
	cfg.Settings.Layout = nil
	require.NoError(t, cfg.saveTo(configFile))

	reloaded, err := loadFrom(configFile, "")
	require.NoError(t, err)
	assert.Nil(t, reloaded.Settings.Layout)
	assert.Equal(t, "dark", reloaded.Settings.Theme)
}

func TestUpdate_ConcurrentWritersDoNotLoseChanges(t *testing.T) {
	// Not parallel: SetConfigDir mutates process-global state.
	dir := t.TempDir()
	paths.SetConfigDir(dir)
	t.Cleanup(func() { paths.SetConfigDir("") })

	const writers = 8
	var wg sync.WaitGroup
	errs := make([]error, writers)
	for i := range writers {
		wg.Go(func() {
			errs[i] = Update(func(cfg *Config) error {
				return cfg.SetAlias(fmt.Sprintf("alias-%d", i), &Alias{Path: fmt.Sprintf("./agent-%d.yaml", i)})
			})
		})
	}
	wg.Wait()

	for i, err := range errs {
		require.NoError(t, err, "writer %d", i)
	}

	cfg, err := Load()
	require.NoError(t, err)
	assert.Len(t, cfg.Aliases, writers, "every concurrent update must be persisted")
}

func TestUpdate_MutateErrorLeavesFileUntouched(t *testing.T) {
	// Not parallel: SetConfigDir mutates process-global state.
	dir := t.TempDir()
	paths.SetConfigDir(dir)
	t.Cleanup(func() { paths.SetConfigDir("") })

	original := []byte("version: v1\nmodels_gateway: https://gw.example.com\n")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.yaml"), original, 0o600))

	boom := errors.New("boom")
	err := Update(func(*Config) error { return boom })
	require.ErrorIs(t, err, boom)

	data, err := os.ReadFile(filepath.Join(dir, "config.yaml"))
	require.NoError(t, err)
	assert.Equal(t, original, data)
}

func TestSettings_GlobalHooksValidation(t *testing.T) {
	t.Parallel()

	valid := &latest.HooksConfig{
		SessionStart: []latest.HookDefinition{{Type: "command", Command: "echo hi"}},
	}
	invalid := &latest.HooksConfig{
		SessionStart: []latest.HookDefinition{{Type: "command"}},
	}

	assert.Nil(t, (*Settings)(nil).GlobalHooks())
	assert.Nil(t, (&Settings{}).GlobalHooks())
	assert.Same(t, valid, (&Settings{Hooks: valid}).GlobalHooks())
	assert.Nil(t, (&Settings{Hooks: invalid}).GlobalHooks(), "invalid hooks must not reach the runtime")
}

func TestLoad_UnknownVersionStillLoads(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	configFile := filepath.Join(tmpDir, "config.yaml")
	require.NoError(t, os.WriteFile(configFile, []byte("version: v99\nmodels_gateway: https://gw.example.com\n"), 0o600))

	cfg, err := loadFrom(configFile, "")
	require.NoError(t, err)
	assert.Equal(t, "https://gw.example.com", cfg.ModelsGateway)
}

func TestConfig_Settings_Safety(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	configFile := filepath.Join(tmpDir, "config.yaml")
	require.NoError(t, os.WriteFile(configFile, []byte(`settings:
  safety: balanced
`), 0o644))

	cfg, err := loadFrom(configFile, "")
	require.NoError(t, err)
	assert.Equal(t, latest.SafetyModeBalanced, cfg.GetSettings().Safety)
	assert.Equal(t, latest.SafetyModeBalanced, cfg.GetSettings().GetSafety())
}

func TestConfig_Settings_SafetyRestricted(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	configFile := filepath.Join(tmpDir, "config.yaml")
	require.NoError(t, os.WriteFile(configFile, []byte(`settings:
  safety: restricted
`), 0o644))

	cfg, err := loadFrom(configFile, "")
	require.NoError(t, err)
	assert.Equal(t, latest.SafetyModeRestricted, cfg.GetSettings().Safety)
	assert.Equal(t, latest.SafetyModeRestricted, cfg.GetSettings().GetSafety())
}

func TestConfig_Settings_InvalidSafetyFailsLoad(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	configFile := filepath.Join(tmpDir, "config.yaml")
	require.NoError(t, os.WriteFile(configFile, []byte(`settings:
  safety: yolo
`), 0o644))

	_, err := loadFrom(configFile, "")
	require.ErrorContains(t, err, "settings.safety")
	require.ErrorContains(t, err, `invalid safety mode "yolo" (valid: strict, balanced, restricted, autonomous)`)
}

func TestConfig_Alias_InvalidSafetyFailsLoad(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	configFile := filepath.Join(tmpDir, "config.yaml")
	require.NoError(t, os.WriteFile(configFile, []byte(`aliases:
  coder:
    path: myorg/coder
    safety: unsafe
`), 0o644))

	_, err := loadFrom(configFile, "")
	require.ErrorContains(t, err, "aliases.coder.safety")
	require.ErrorContains(t, err, `invalid safety mode "unsafe"`)
}

func TestSettings_GetSafety(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		settings *Settings
		want     latest.SafetyMode
	}{
		{"nil settings", nil, ""},
		{"unset", &Settings{}, ""},
		{"safety set", &Settings{Safety: latest.SafetyModeStrict}, latest.SafetyModeStrict},
		{"legacy YOLO maps to autonomous", &Settings{YOLO: true}, latest.SafetyModeAutonomous},
		{"safety wins over legacy YOLO", &Settings{YOLO: true, Safety: latest.SafetyModeBalanced}, latest.SafetyModeBalanced},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, tt.settings.GetSafety())
		})
	}
}

func TestAlias_GetSafety(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		alias *Alias
		want  latest.SafetyMode
	}{
		{"nil alias", nil, ""},
		{"unset", &Alias{Path: "x"}, ""},
		{"safety set", &Alias{Path: "x", Safety: latest.SafetyModeBalanced}, latest.SafetyModeBalanced},
		{"legacy yolo maps to autonomous", &Alias{Path: "x", Yolo: true}, latest.SafetyModeAutonomous},
		{"safety wins over legacy yolo", &Alias{Path: "x", Yolo: true, Safety: latest.SafetyModeStrict}, latest.SafetyModeStrict},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, tt.alias.GetSafety())
		})
	}
}

func TestConfig_SetAlias_InvalidSafety(t *testing.T) {
	t.Parallel()

	config := &Config{Aliases: make(map[string]*Alias)}
	err := config.SetAlias("bad", &Alias{Path: "myorg/coder", Safety: "yolo"})
	require.ErrorContains(t, err, "safety")
	require.ErrorContains(t, err, "invalid safety mode")
}

// Alias safety must survive a save/load round trip alongside the other
// options, and unknown keys must still round-trip via Extra.
func TestConfig_AliasSafetyRoundTrip(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	configFile := filepath.Join(tmpDir, "config.yaml")

	config := &Config{
		Aliases: map[string]*Alias{
			"careful": {Path: "myorg/coder", Safety: latest.SafetyModeStrict},
		},
		Settings: &Settings{Safety: latest.SafetyModeBalanced},
	}
	require.NoError(t, config.saveTo(configFile))

	loaded, err := loadFrom(configFile, "")
	require.NoError(t, err)

	alias, ok := loaded.GetAlias("careful")
	require.True(t, ok)
	assert.Equal(t, latest.SafetyModeStrict, alias.Safety)
	assert.Equal(t, latest.SafetyModeBalanced, loaded.GetSettings().Safety)
}
