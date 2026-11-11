package latest

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/goccy/go-yaml"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStreamIdleTimeoutParsingAndRoundTrip(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		input string
		want  time.Duration
		valid bool
	}{
		{`"1m30s"`, 90 * time.Second, true},
		{`"1ns"`, time.Nanosecond, true},
		{`-1`, DisabledStreamIdleTimeout, true},
		{`0`, 0, false},
		{`-2`, 0, false},
		{`2`, 0, false},
		{`"-1"`, 0, false},
		{`"-1ns"`, 0, false},
		{`"0s"`, 0, false},
		{`""`, 0, false},
		{`"bogus"`, 0, false},
		{`1.5`, 0, false},
		{`true`, 0, false},
		{`[]`, 0, false},
	} {
		t.Run(tc.input, func(t *testing.T) {
			t.Parallel()
			for _, format := range []string{"yaml", "json"} {
				t.Run(format, func(t *testing.T) {
					var got StreamIdleTimeout
					var err error
					if format == "yaml" {
						err = yaml.Unmarshal([]byte(tc.input), &got)
					} else {
						err = json.Unmarshal([]byte(tc.input), &got)
					}
					if !tc.valid {
						require.Error(t, err)
						return
					}
					require.NoError(t, err)
					assert.Equal(t, tc.want, got.Duration)
					var encoded []byte
					if format == "yaml" {
						encoded, err = yaml.Marshal(got)
					} else {
						encoded, err = json.Marshal(got)
					}
					require.NoError(t, err)
					var again StreamIdleTimeout
					if format == "yaml" {
						err = yaml.Unmarshal(encoded, &again)
					} else {
						err = json.Unmarshal(encoded, &again)
					}
					require.NoError(t, err)
					assert.Equal(t, got, again)
				})
			}
		})
	}
}

func TestStreamIdleTimeoutInConfig(t *testing.T) {
	t.Parallel()
	for _, field := range []string{"runtime:\n  stream_idle_timeout: %s\n", "models:\n  x:\n    provider: openai\n    model: gpt\n    stream_idle_timeout: %s\n"} {
		for _, value := range []string{"-1", "1m", "0", "-2", "0s", "-2s"} {
			var cfg Config
			err := yaml.Unmarshal([]byte(fmt.Sprintf(field, value)), &cfg)
			if value == "-1" || value == "1m" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		}
	}
	err := (&Config{Runtime: &RuntimeDefaults{StreamIdleTimeout: &StreamIdleTimeout{}}}).Validate()
	require.ErrorContains(t, err, "runtime.stream_idle_timeout")
	err = (&Config{Models: map[string]ModelConfig{"x": {StreamIdleTimeout: &StreamIdleTimeout{Duration: -2}}}}).Validate()
	require.ErrorContains(t, err, "models.x.stream_idle_timeout")
	err = (&Config{Models: map[string]ModelConfig{"x": {FirstAvailable: []string{"openai/gpt"}, StreamIdleTimeout: &StreamIdleTimeout{Duration: time.Second}}}}).Validate()
	require.ErrorContains(t, err, "first_available cannot be combined with stream_idle_timeout")
}

func TestFlexibleModelTimeoutRoundTrip(t *testing.T) {
	t.Parallel()
	original := FlexibleModelConfig{ModelConfig: ModelConfig{Provider: "openai", Model: "gpt", StreamIdleTimeout: &StreamIdleTimeout{Duration: DisabledStreamIdleTimeout}}}
	data, err := yaml.Marshal(original)
	require.NoError(t, err)
	assert.Contains(t, string(data), "stream_idle_timeout: -1")
	var result FlexibleModelConfig
	require.NoError(t, yaml.Unmarshal(data, &result))
	assert.Equal(t, original, result)
}

func TestStreamIdleTimeoutOmittedAndNullInherit(t *testing.T) {
	t.Parallel()
	for _, format := range []string{"yaml", "json"} {
		for _, tc := range []struct {
			name string
			yaml string
			json string
		}{
			{name: "omitted", yaml: "runtime: {}\nmodels:\n  x: {}\n", json: `{"runtime":{},"models":{"x":{}}}`},
			{name: "null", yaml: "runtime:\n  stream_idle_timeout: null\nmodels:\n  x:\n    stream_idle_timeout: null\n", json: `{"runtime":{"stream_idle_timeout":null},"models":{"x":{"stream_idle_timeout":null}}}`},
		} {
			t.Run(format+"/"+tc.name, func(t *testing.T) {
				var cfg Config
				if format == "yaml" {
					require.NoError(t, yaml.Unmarshal([]byte(tc.yaml), &cfg))
				} else {
					require.NoError(t, json.Unmarshal([]byte(tc.json), &cfg))
				}
				require.NotNil(t, cfg.Runtime)
				assert.Nil(t, cfg.Runtime.StreamIdleTimeout)
				assert.Nil(t, cfg.Models["x"].StreamIdleTimeout)
			})
		}
	}
}
