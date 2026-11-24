//go:build js && wasm && docker_agent_wasm_small

package main

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/model/provider"
	"github.com/docker/docker-agent/pkg/model/provider/base"
)

func TestSmallProfileForwardsInlineMedia(t *testing.T) {
	const yaml = `
agents:
  root:
    model: mock/root
`
	part := chat.MessagePart{
		Type: chat.MessagePartTypeDocument,
		Document: &chat.Document{
			Name:     "image.png",
			MimeType: "image/png",
			Source:   chat.DocumentSource{InlineData: []byte("image data")},
		},
	}
	model := visionScriptedModel{newScriptedModel("mock/root", textTurn("ok"))}
	s := openTestSession(t, testHost(&echoToolSet{}, map[string]provider.Provider{"root": model}), sessionOptions{
		YAML: yaml,
		History: []chat.Message{{
			Role:         chat.MessageRoleUser,
			MultiContent: []chat.MessagePart{part},
		}},
	})

	_, err := s.send("what did I attach?", func(context.Context, map[string]any) {})
	require.NoError(t, err)
	var content []chat.MessagePart
	for _, message := range model.lastCall() {
		content = append(content, message.MultiContent...)
	}
	assert.Contains(t, content, part)
}

type visionScriptedModel struct{ *scriptedModel }

func (m visionScriptedModel) BaseConfig() base.Config {
	return base.Config{ModelConfig: latest.ModelConfig{Capabilities: &latest.CapabilitiesConfig{Image: true}}}
}
