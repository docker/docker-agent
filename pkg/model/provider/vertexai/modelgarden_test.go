package vertexai

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
)

func TestIsModelGardenConfig(t *testing.T) {
	tests := []struct {
		name string
		cfg  *latest.ModelConfig
		want bool
	}{
		{
			name: "nil config",
			cfg:  nil,
			want: false,
		},
		{
			name: "no provider_opts",
			cfg:  &latest.ModelConfig{Provider: "google", Model: "gemini-2.5-flash"},
			want: false,
		},
		{
			name: "no publisher",
			cfg: &latest.ModelConfig{
				Provider:     "google",
				Model:        "gemini-2.5-flash",
				ProviderOpts: map[string]any{"project": "my-project", "location": "us-central1"},
			},
			want: false,
		},
		{
			name: "publisher=google",
			cfg: &latest.ModelConfig{
				Provider:     "google",
				Model:        "gemini-2.5-flash",
				ProviderOpts: map[string]any{"project": "my-project", "location": "us-central1", "publisher": "google"},
			},
			want: false,
		},
		{
			name: "publisher=anthropic",
			cfg: &latest.ModelConfig{
				Provider:     "google",
				Model:        "claude-sonnet-4-20250514",
				ProviderOpts: map[string]any{"project": "my-project", "location": "us-east5", "publisher": "anthropic"},
			},
			want: true,
		},
		{
			name: "publisher=meta",
			cfg: &latest.ModelConfig{
				Provider:     "google",
				Model:        "meta/llama-4-maverick-17b-128e-instruct-maas",
				ProviderOpts: map[string]any{"project": "my-project", "location": "us-central1", "publisher": "meta"},
			},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsModelGardenConfig(tt.cfg)
			if got != tt.want {
				t.Errorf("IsModelGardenConfig() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestWithModelsDevProvider(t *testing.T) {
	tests := []struct {
		name         string
		cfg          *latest.ModelConfig
		wantProvider string
	}{
		{
			name: "anthropic publisher rewrites Provider to anthropic",
			cfg: &latest.ModelConfig{
				Provider:     "google",
				Model:        "claude-sonnet-4-20250514",
				ProviderOpts: map[string]any{"publisher": "anthropic"},
			},
			wantProvider: "anthropic",
		},
		{
			name: "meta publisher rewrites Provider to google-vertex",
			cfg: &latest.ModelConfig{
				Provider:     "google",
				Model:        "meta/llama-4-maverick-17b-128e-instruct-maas",
				ProviderOpts: map[string]any{"publisher": "meta"},
			},
			wantProvider: "google-vertex",
		},
		{
			name: "mistral publisher rewrites Provider to google-vertex",
			cfg: &latest.ModelConfig{
				Provider:     "google",
				Model:        "mistral/mistral-large-2411",
				ProviderOpts: map[string]any{"publisher": "mistral"},
			},
			wantProvider: "google-vertex",
		},
		{
			name: "anthropic publisher is case-folded",
			cfg: &latest.ModelConfig{
				Provider:     "google",
				Model:        "claude-sonnet-4-20250514",
				ProviderOpts: map[string]any{"publisher": "Anthropic"},
			},
			wantProvider: "anthropic",
		},
		{
			name: "publisher with surrounding whitespace is trimmed",
			cfg: &latest.ModelConfig{
				Provider:     "google",
				Model:        "claude-sonnet-4-20250514",
				ProviderOpts: map[string]any{"publisher": "  anthropic  "},
			},
			wantProvider: "anthropic",
		},
		{
			name: "missing publisher leaves Provider untouched",
			cfg: &latest.ModelConfig{
				Provider: "google",
				Model:    "gemini-2.5-flash",
			},
			wantProvider: "google",
		},
		{
			name: "empty publisher leaves Provider untouched",
			cfg: &latest.ModelConfig{
				Provider:     "google",
				Model:        "gemini-2.5-flash",
				ProviderOpts: map[string]any{"publisher": ""},
			},
			wantProvider: "google",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			originalProvider := tt.cfg.Provider
			originalPublisher, _ := tt.cfg.ProviderOpts["publisher"].(string)

			got := withModelsDevProvider(tt.cfg)
			require.NotNil(t, got)
			require.NotSame(t, tt.cfg, got, "withModelsDevProvider must return a copy")
			assert.Equal(t, tt.wantProvider, got.Provider)

			// The original config must be unchanged so the wider runtime keeps its
			// "google" provider view (used for routing, telemetry, ...).
			assert.Equal(t, originalProvider, tt.cfg.Provider, "input cfg.Provider was mutated")
			gotPublisher, _ := tt.cfg.ProviderOpts["publisher"].(string)
			assert.Equal(t, originalPublisher, gotPublisher, "input cfg.ProviderOpts[publisher] was mutated")
		})
	}
}

// TestWithModelsDevProvider_CapabilityLookupIDs pins the bug fix for issue
// #2740: the underlying provider client must compute its capability-lookup ID
// in a form that exists in the models.dev database. Concretely:
//
//   - Anthropic Claude on Vertex AI: model name is bare (no prefix) and
//     models.dev keys it under "anthropic/<model>".
//   - Other publishers on Vertex AI: model name carries a "<publisher>/"
//     prefix and models.dev keys it under "google-vertex/<publisher>/<model>".
//
// The test guards against the regression where rewriting Provider to the
// publisher name (e.g. "meta") produced double-prefixed IDs like
// "meta/meta/llama-..." that do not exist in models.dev.
func TestWithModelsDevProvider_CapabilityLookupIDs(t *testing.T) {
	tests := []struct {
		name   string
		cfg    *latest.ModelConfig
		wantID string
	}{
		{
			name: "anthropic claude",
			cfg: &latest.ModelConfig{
				Provider: "google",
				Model:    "claude-sonnet-4-20250514",
				ProviderOpts: map[string]any{
					"project":   "my-project",
					"location":  "us-east5",
					"publisher": "anthropic",
				},
			},
			wantID: "anthropic/claude-sonnet-4-20250514",
		},
		{
			name: "meta llama keeps the meta/ prefix exactly once",
			cfg: &latest.ModelConfig{
				Provider: "google",
				Model:    "meta/llama-4-maverick-17b-128e-instruct-maas",
				ProviderOpts: map[string]any{
					"project":   "my-project",
					"location":  "us-central1",
					"publisher": "meta",
				},
			},
			wantID: "google-vertex/meta/llama-4-maverick-17b-128e-instruct-maas",
		},
		{
			name: "mistral keeps its mistral/ prefix exactly once",
			cfg: &latest.ModelConfig{
				Provider: "google",
				Model:    "mistral/mistral-large-2411",
				ProviderOpts: map[string]any{
					"project":   "my-project",
					"location":  "us-central1",
					"publisher": "mistral",
				},
			},
			wantID: "google-vertex/mistral/mistral-large-2411",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rewritten := withModelsDevProvider(tt.cfg)
			gotID := rewritten.Provider + "/" + rewritten.DisplayOrModel()
			assert.Equal(t, tt.wantID, gotID)
		})
	}
}

func TestPublisher(t *testing.T) {
	tests := []struct {
		name string
		cfg  *latest.ModelConfig
		want string
	}{
		{name: "nil config", cfg: nil, want: ""},
		{name: "no provider_opts", cfg: &latest.ModelConfig{}, want: ""},
		{
			name: "anthropic",
			cfg:  &latest.ModelConfig{ProviderOpts: map[string]any{"publisher": "anthropic"}},
			want: "anthropic",
		},
		{
			name: "non-string value",
			cfg:  &latest.ModelConfig{ProviderOpts: map[string]any{"publisher": 42}},
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := publisher(tt.cfg); got != tt.want {
				t.Errorf("publisher() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestValidGCPIdentifier(t *testing.T) {
	valid := []string{"my-project", "us-central1", "project123", "ab"}
	for _, s := range valid {
		if !validGCPIdentifier.MatchString(s) {
			t.Errorf("expected %q to be valid", s)
		}
	}

	invalid := []string{"", "A", "../foo", "my project", "a", "123abc", "my_project/../../evil"}
	for _, s := range invalid {
		if validGCPIdentifier.MatchString(s) {
			t.Errorf("expected %q to be invalid", s)
		}
	}
}

func TestResolveProjectLocation(t *testing.T) {
	tests := []struct {
		name        string
		cfg         *latest.ModelConfig
		env         map[string]string
		wantProject string
		wantLoc     string
		wantErrSub  string // substring expected in the error message; empty means no error
	}{
		{
			name:       "nil config",
			cfg:        nil,
			wantErrSub: "model configuration is required",
		},
		{
			name: "from provider_opts",
			cfg: &latest.ModelConfig{
				ProviderOpts: map[string]any{
					"project":  "my-project",
					"location": "us-central1",
				},
			},
			wantProject: "my-project",
			wantLoc:     "us-central1",
		},
		{
			name: "from env vars",
			cfg:  &latest.ModelConfig{},
			env: map[string]string{
				"GOOGLE_CLOUD_PROJECT":  "env-project",
				"GOOGLE_CLOUD_LOCATION": "europe-west1",
			},
			wantProject: "env-project",
			wantLoc:     "europe-west1",
		},
		{
			name: "provider_opts wins over env",
			cfg: &latest.ModelConfig{
				ProviderOpts: map[string]any{
					"project":  "opts-project",
					"location": "us-east5",
				},
			},
			env: map[string]string{
				"GOOGLE_CLOUD_PROJECT":  "env-project",
				"GOOGLE_CLOUD_LOCATION": "europe-west1",
			},
			wantProject: "opts-project",
			wantLoc:     "us-east5",
		},
		{
			name: "env var expansion in provider_opts",
			cfg: &latest.ModelConfig{
				ProviderOpts: map[string]any{
					"project":  "${MY_PROJECT}",
					"location": "${MY_LOC}",
				},
			},
			env: map[string]string{
				"MY_PROJECT": "expanded-project",
				"MY_LOC":     "us-central1",
			},
			wantProject: "expanded-project",
			wantLoc:     "us-central1",
		},
		{
			name: "unset env var in expansion fails",
			cfg: &latest.ModelConfig{
				ProviderOpts: map[string]any{
					"project":  "${MISSING}",
					"location": "us-central1",
				},
			},
			wantErrSub: "expanding project",
		},
		{
			name:       "missing project",
			cfg:        &latest.ModelConfig{ProviderOpts: map[string]any{"location": "us-central1"}},
			wantErrSub: "requires a GCP project",
		},
		{
			name:       "missing location",
			cfg:        &latest.ModelConfig{ProviderOpts: map[string]any{"project": "my-project"}},
			wantErrSub: "requires a GCP location",
		},
		{
			name: "url-injection attempt in project",
			cfg: &latest.ModelConfig{
				ProviderOpts: map[string]any{
					"project":  "../../evil",
					"location": "us-central1",
				},
			},
			wantErrSub: "invalid GCP project ID",
		},
		{
			name: "url-injection attempt in location",
			cfg: &latest.ModelConfig{
				ProviderOpts: map[string]any{
					"project":  "my-project",
					"location": "us-central1/../evil",
				},
			},
			wantErrSub: "invalid GCP location",
		},
		{
			name: "uppercase rejected",
			cfg: &latest.ModelConfig{
				ProviderOpts: map[string]any{
					"project":  "MY-PROJECT",
					"location": "us-central1",
				},
			},
			wantErrSub: "invalid GCP project ID",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := environment.NewMapEnvProvider(tt.env)
			gotProject, gotLoc, err := resolveProjectLocation(t.Context(), tt.cfg, env)

			if tt.wantErrSub != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tt.wantErrSub)
				}
				if !strings.Contains(err.Error(), tt.wantErrSub) {
					t.Fatalf("expected error containing %q, got %q", tt.wantErrSub, err.Error())
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if gotProject != tt.wantProject {
				t.Errorf("project = %q, want %q", gotProject, tt.wantProject)
			}
			if gotLoc != tt.wantLoc {
				t.Errorf("location = %q, want %q", gotLoc, tt.wantLoc)
			}
		})
	}
}
