package runtime

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/modelsdev/embedded"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
)

func TestRunStream_CatalogAliasesFilterImages(t *testing.T) {
	t.Parallel()

	for _, provider := range []struct{ configured, catalog, model string }{
		{"fireworks", "fireworks-ai", "accounts/fireworks/models/kimi-k3"},
		{"together", "togetherai", "Qwen/Qwen3.5-397B-A17B"},
		{"moonshot", "moonshotai", "kimi-k3"},
		{"opencode-zen", "opencode", "kimi-k3"},
		{"ovhcloud", "ovhcloud", "Qwen3.5-397B-A17B"},
		{"fireworks-ai", "fireworks-ai", "accounts/fireworks/models/kimi-k3"},
		{"togetherai", "togetherai", "Qwen/Qwen3.5-397B-A17B"},
		{"moonshotai", "moonshotai", "kimi-k3"},
		{"opencode", "opencode", "kimi-k3"},
	} {
		for _, mode := range []string{"snapshot vision", "text only", "unknown", "override disables", "override enables", "direct text wins"} {
			t.Run(provider.configured+"/"+mode, func(t *testing.T) {
				t.Parallel()
				model := provider.model
				store := modelsdev.NewDatabaseStore(embedded.Snapshot())
				var override *latest.CapabilitiesConfig
				wantImages := false
				switch mode {
				case "snapshot vision":
					wantImages = true
				case "text only", "override enables":
					model = "text-only"
					store = modelsdev.NewDatabaseStore(&modelsdev.Database{Providers: map[string]modelsdev.Provider{
						provider.catalog: {Models: map[string]modelsdev.Model{model: {Modalities: modelsdev.Modalities{Input: []string{"text"}}}}},
					}})
					if mode == "override enables" {
						override = &latest.CapabilitiesConfig{Image: true}
						wantImages = true
					}
				case "unknown":
					model = "unknown-catalog-model"
				case "override disables":
					override = &latest.CapabilitiesConfig{Image: false}
				case "direct text wins":
					catalogModel := model
					if provider.configured == "ovhcloud" {
						catalogModel = "qwen3.5-397b-a17b"
					}
					providers := map[string]modelsdev.Provider{
						provider.catalog: {Models: map[string]modelsdev.Model{catalogModel: {Modalities: modelsdev.Modalities{Input: []string{"text", "image"}}}}},
					}
					direct := providers[provider.configured]
					if direct.Models == nil {
						direct.Models = map[string]modelsdev.Model{}
					}
					direct.Models[model] = modelsdev.Model{Modalities: modelsdev.Modalities{Input: []string{"text"}}}
					providers[provider.configured] = direct
					store = modelsdev.NewDatabaseStore(&modelsdev.Database{Providers: providers})
				}
				prov := &recordingMsgProvider{
					mockProvider: mockProvider{id: provider.configured + "/" + model, stream: &mockStream{}},
					baseConfig:   base.Config{ModelConfig: latest.ModelConfig{Capabilities: override}},
				}
				a := agent.New("root", "instructions", agent.WithModel(prov))
				tm := team.New(team.WithAgents(a))
				lazy := &lazyModelStore{}
				lazy.once.Do(func() { lazy.st = store })
				rt, err := NewLocalRuntime(t.Context(), tm, WithModelStore(lazy))
				require.NoError(t, err)
				defer rt.Close()

				user := mixedMediaMsg()
				user.MultiContent = append(user.MultiContent, chat.MessagePart{Type: chat.MessagePartTypeDocument, Document: &chat.Document{
					Name: "attachment.png", MimeType: "image/png", Source: chat.DocumentSource{InlineData: []byte{1}},
				}})
				tool := chat.Message{Role: chat.MessageRoleTool, ToolCallID: "call-image", MultiContent: slices.Clone(user.MultiContent)}
				assistant := chat.Message{Role: chat.MessageRoleAssistant, ToolCalls: []tools.ToolCall{{
					ID: "call-image", Type: "function", Function: tools.FunctionCall{Name: "read_file", Arguments: `{}`},
				}}}
				sess := session.New(session.WithMessages([]session.Item{
					session.NewMessageItem(&session.Message{Message: user}),
					session.NewMessageItem(&session.Message{Message: assistant}),
					session.NewMessageItem(&session.Message{Message: tool}),
				}))
				for range rt.RunStream(t.Context(), sess) {
				}
				require.NotEmpty(t, prov.got)
				for _, role := range []chat.MessageRole{chat.MessageRoleUser, chat.MessageRoleTool} {
					idx := slices.IndexFunc(prov.got[0], func(m chat.Message) bool { return m.Role == role })
					require.NotEqual(t, -1, idx)
					parts := prov.got[0][idx].MultiContent
					assert.Equal(t, wantImages, slices.ContainsFunc(parts, func(p chat.MessagePart) bool { return p.Type == chat.MessagePartTypeImageURL }))
					assert.Equal(t, wantImages, slices.ContainsFunc(parts, func(p chat.MessagePart) bool {
						return p.Document != nil && p.Document.MimeType == "image/png"
					}))
					assert.True(t, slices.ContainsFunc(parts, func(p chat.MessagePart) bool { return p.Type == chat.MessagePartTypeText }))
				}
			})
		}
	}
}
