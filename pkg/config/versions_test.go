package config

import (
	"errors"
	"fmt"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/config/legacy"
)

const versionTestAgent = "agents:\n  root:\n    model: openai/gpt-4o\n    instruction: hello\n"

func TestLoadLatestOnly(t *testing.T) {
	t.Parallel()
	for _, version := range []string{"", latest.Version, "0", "15", "99"} {
		t.Run("version="+version, func(t *testing.T) {
			t.Parallel()
			data := versionTestAgent
			if version != "" {
				data = fmt.Sprintf("version: %q\n", version) + data
			}
			cfg, err := Load(t.Context(), NewBytesSource("test.yaml", []byte(data)))
			if version != "" && version != latest.Version {
				require.EqualError(t, err, fmt.Sprintf("parsing config file\nunsupported config version: %s (valid versions: %s)", version, latest.Version))
				return
			}
			require.NoError(t, err)
			assert.Equal(t, latest.Version, cfg.Version)
			require.Len(t, cfg.Agents, 1)
			assert.Equal(t, "root", cfg.Agents[0].Name)
			assert.Equal(t, "openai", cfg.Models["openai/gpt-4o"].Provider)
		})
	}
}

func TestLoadVersionsLastOptionWins(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		opts       []LoadOption
		wantLegacy bool
	}{
		{name: "nil", opts: []LoadOption{WithVersions(nil)}},
		{name: "legacy", opts: []LoadOption{WithVersions(legacy.Register)}, wantLegacy: true},
		{name: "reset", opts: []LoadOption{WithVersions(legacy.Register), WithVersions(nil)}},
		{name: "enable", opts: []LoadOption{WithVersions(nil), WithVersions(legacy.Register)}, wantLegacy: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := Load(t.Context(), NewFileSource("testdata/provider_v0.yaml"), tc.opts...)
			if tc.wantLegacy {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, "unsupported config version: 0")
			}
		})
	}
}

func TestLoadVersionsConcurrentIsolation(t *testing.T) {
	t.Parallel()
	// Every historical parser must reach latest, without enabling another load.
	for n := range 16 {
		for _, historical := range []bool{false, true} {
			t.Run(fmt.Sprintf("v%d/legacy=%t", n, historical), func(t *testing.T) {
				t.Parallel()
				modelField := "provider"
				if n == 0 {
					modelField = "type"
				}
				data := fmt.Sprintf("version: %q\nagents:\n  first:\n    model: main\n  second:\n    model: main\nmodels:\n  main:\n    %s: openai\n    model: gpt-4o\n", strconv.Itoa(n), modelField)
				var opts []LoadOption
				if historical {
					opts = append(opts, WithVersions(legacy.Register))
				}
				cfg, err := Load(t.Context(), NewBytesSource("test.yaml", []byte(data)), opts...)
				if !historical {
					require.ErrorContains(t, err, "unsupported config version")
					return
				}
				require.NoError(t, err)
				assert.Equal(t, strconv.Itoa(n), cfg.Version)
				assert.Equal(t, "openai", cfg.Models["main"].Provider)
				require.Len(t, cfg.Agents, 2)
				assert.Equal(t, "first", cfg.Agents[0].Name)
				assert.Equal(t, "second", cfg.Agents[1].Name)
			})
		}
	}
}

func TestLoadVersionsRegisterOnce(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, data, wantErr string
	}{
		{name: "success", data: "version: '10'\n" + versionTestAgent},
		{name: "newer hint", data: "version: '10'\nagents:\n  root:\n    model: openai/gpt-4o\n    instruction_file: prompt.md\n", wantErr: "supported by config version 11"},
		{name: "removed hint", data: versionTestAgent + "    toolsets:\n      - type: shell\n        safer: true\n", wantErr: "has since been removed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			calls := 0
			register := func(parsers map[string]func([]byte) (any, error), upgraders *[]func(any, []byte) (any, error)) {
				calls++
				legacy.Register(parsers, upgraders)
			}
			_, err := Load(t.Context(), NewBytesSource("test.yaml", []byte(tc.data)), WithVersions(register))
			if tc.wantErr == "" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, tc.wantErr)
			}
			assert.Equal(t, 1, calls)
		})
	}
}

func TestLoadVersionsStrictLatestParser(t *testing.T) {
	t.Parallel()
	register := func(parsers map[string]func([]byte) (any, error), _ *[]func(any, []byte) (any, error)) {
		parsers[latest.Version] = func([]byte) (any, error) { return latest.Config{}, nil }
	}
	_, err := Load(t.Context(), NewBytesSource("test.yaml", []byte(versionTestAgent+"typo: true\n")), WithVersions(register))
	require.ErrorContains(t, err, `unknown field "typo"`)
}

func TestLoadVersionsIncompleteMigration(t *testing.T) {
	t.Parallel()
	register := func(parsers map[string]func([]byte) (any, error), _ *[]func(any, []byte) (any, error)) {
		parsers["custom"] = func([]byte) (any, error) { return struct{}{}, nil }
	}
	_, err := Load(t.Context(), NewBytesSource("test.yaml", []byte("version: custom\n")), WithVersions(register))
	require.ErrorContains(t, err, "migrating config: migration did not produce latest.Config")
}

func TestLoadVersionsMigrationError(t *testing.T) {
	t.Parallel()
	failure := errors.New("migration failed")
	register := func(_ map[string]func([]byte) (any, error), upgraders *[]func(any, []byte) (any, error)) {
		*upgraders = append(*upgraders, func(any, []byte) (any, error) { return nil, failure })
	}
	_, err := Load(t.Context(), NewBytesSource("test.yaml", []byte(versionTestAgent)), WithVersions(register))
	require.ErrorIs(t, err, failure)
}

func TestLoadVersionsFlavorSelectsVersion(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		from, to string
		legacy   bool
	}{
		{from: latest.Version, to: "15"},
		{from: latest.Version, to: "15", legacy: true},
		{from: "15", to: latest.Version},
	} {
		t.Run(fmt.Sprintf("%s-to-%s/legacy=%t", tc.from, tc.to, tc.legacy), func(t *testing.T) {
			t.Parallel()
			data := fmt.Sprintf("version: %q\n%sflavors:\n  selected:\n    version: %q\n", tc.from, versionTestAgent, tc.to)
			opts := []LoadOption{WithFlavors("selected")}
			if tc.legacy {
				opts = append(opts, WithVersions(legacy.Register))
			}
			cfg, err := Load(t.Context(), NewBytesSource("test.yaml", []byte(data)), opts...)
			if tc.to != latest.Version && !tc.legacy {
				require.ErrorContains(t, err, "unsupported config version: 15")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.to, cfg.Version)
		})
	}
}
