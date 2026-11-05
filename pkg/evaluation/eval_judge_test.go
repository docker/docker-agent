package evaluation

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/modelsdev/embedded"
)

func TestEvaluateWithEvaluatorJudge(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake container runtime is a POSIX shell script")
	}
	t.Parallel()

	for _, tt := range []struct {
		name, criteria, answer, wantErr string
		wantCalls                       int32
	}{
		{"relevance", `"relevance":["says hello"]`, `{"type":"noul","noul":0.9}`, "", 2},
		{"unused judge", `"assertions":[{"name":"says hello","type":"contains","value":"hello"}]`, "", "", 0},
		{"malformed answer", `"relevance":["says hello"]`, `{"type":"noul","noul":2}`, "judge model validation failed", 1},
		{"negative validation", `"relevance":["says hello"]`, `{"type":"noul","noul":0.1}`, "expected the test criterion to pass", 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				call := calls.Add(1)
				var payload struct {
					State map[string]string `json:"state"`
				}
				assert.NoError(t, json.NewDecoder(r.Body).Decode(&payload))
				if call == 2 {
					assert.Contains(t, payload.State["transcript"], "hello")
					assert.Equal(t, "says hello", payload.State["criterion"])
				}
				_, err := io.WriteString(w, `{"model":"test","answers":{"evaluation":`+tt.answer+`}}`)
				assert.NoError(t, err)
			}))
			t.Cleanup(server.Close)
			dir := t.TempDir()
			agentPath := filepath.Join(dir, "agent.yaml")
			agent := "agents:\n  root:\n    model: openai/gpt-4o\nevaluators:\n  relevance:\n    provider: typesafe\n    model: test\n    type: boolean\n    instructions: Does transcript satisfy criterion?\n    endpoint: " + server.URL + "/judge\n"
			require.NoError(t, os.WriteFile(agentPath, []byte(agent), 0o600))
			evalsDir := filepath.Join(dir, "evals")
			require.NoError(t, os.Mkdir(evalsDir, 0o700))
			require.NoError(t, os.WriteFile(filepath.Join(evalsDir, "session.json"), []byte(`{"evals":{`+tt.criteria+`}}`), 0o600))
			fakeRuntime := filepath.Join(dir, "runtime")
			writeFakeContainerRuntime(t, fakeRuntime, `{"type":"agent_choice","content":"hello"}`)
			run, err := Evaluate(t.Context(), &bytes.Buffer{}, &bytes.Buffer{}, false, "test",
				&config.RuntimeConfig{EnvProviderOverride: environment.NewMapEnvProvider(map[string]string{"TYPESAFE_API_KEY": "secret"})},
				Config{AgentFilename: agentPath, EvalsDir: evalsDir, JudgeType: JudgeTypeEvaluator, JudgeModel: "relevance", Concurrency: 1, ContainerRuntime: fakeRuntime})
			assert.Equal(t, tt.wantCalls, calls.Load())
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				_, statErr := os.Stat(filepath.Join(dir, "args"))
				assert.True(t, os.IsNotExist(statErr), "judge validation must fail before container work")
				return
			}
			require.NoError(t, err)
			require.Len(t, run.Results, 1)
			assert.Equal(t, 0, run.Summary.FailedEvals)
			if tt.wantCalls != 0 {
				assert.InDelta(t, 1, run.Results[0].RelevancePassed, 1e-9)
				require.NotNil(t, run.Results[0].RelevanceResults[0].Probability)
				assert.InDelta(t, 0.9, *run.Results[0].RelevanceResults[0].Probability, 1e-9)
			}
		})
	}
}

func TestEvaluatorJudgeExampleRequiresJudge(t *testing.T) {
	t.Parallel()

	for _, agent := range []string{"judge-evaluator.yaml", "demo.yaml"} {
		t.Run(agent, func(t *testing.T) {
			t.Parallel()
			model := "typesafe/jev-latest"
			if agent == "judge-evaluator.yaml" {
				model = "relevance_judge"
			}
			var out bytes.Buffer
			run, err := Evaluate(t.Context(), &bytes.Buffer{}, &out, false, "example",
				&config.RuntimeConfig{EnvProviderOverride: environment.NewNoEnvProvider()},
				Config{
					AgentFilename: filepath.Join("..", "..", "examples", "eval", agent),
					EvalsDir:      filepath.Join("..", "..", "examples", "eval", "judge-evals"),
					JudgeType:     JudgeTypeEvaluator, JudgeModel: model, Concurrency: 1,
				})
			require.ErrorContains(t, err, "judge model validation failed")
			require.ErrorContains(t, err, "evaluator API key is missing")
			require.NotNil(t, run)
			assert.Contains(t, out.String(), "Validating judge model")
			assert.NotContains(t, out.String(), "Pre-building")
		})
	}
}

type encryptedJudgeSource struct {
	config.Source

	encrypted string
}

func (s encryptedJudgeSource) EncryptedConfig() string { return s.encrypted }

func TestJudgeSourceEncryptedConfig(t *testing.T) {
	t.Parallel()
	for _, explicit := range []string{"", "explicit"} {
		t.Run(explicit, func(t *testing.T) {
			t.Parallel()
			rc := &config.RuntimeConfig{
				Config:                 config.Config{EncryptedConfig: explicit},
				EnvProviderOverride:    environment.NewNoEnvProvider(),
				ModelsDevStoreOverride: modelsdev.NewDatabaseStore(embedded.Snapshot()),
			}
			source := encryptedJudgeSource{Source: config.NewBytesSource("agent.yaml", nil), encrypted: "discovered"}
			runner := newRunner(source, rc, Config{})
			want := explicit
			if want == "" {
				want = "discovered"
			}
			assert.Equal(t, want, runner.runConfig.EncryptedConfig)
			assert.Equal(t, explicit, rc.EncryptedConfig, "discovery must not mutate caller config")
		})
	}
}
