package server

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/session"
)

const imageOnlyTitleConfig = `models:
  imagegen:
    provider: openai
    model: fake-image-model
    output_capabilities:
      image: true
agents:
  root:
    model: imagegen
    instruction: Be helpful.
`

const mixedTitleConfig = `models:
  imagegen:
    provider: openai
    model: fake-image-model
    output_capabilities:
      image: true
    title_model: safe
  safe:
    provider: openai
    model: gpt-4o-mini
agents:
  root:
    model: imagegen
    instruction: Be helpful.
`

func TestRuntimeForSession_TitleGeneratorKeepsImageOutputModels(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "dummy")
	ctx := t.Context()

	sources := config.Sources{
		"image-only.yaml": config.NewBytesSource("image-only.yaml", []byte(imageOnlyTitleConfig)),
		"mixed.yaml":      config.NewBytesSource("mixed.yaml", []byte(mixedTitleConfig)),
	}
	store := session.NewInMemorySessionStore()
	sm := NewSessionManager(ctx, sources, store, 0, &config.RuntimeConfig{})

	t.Run("image-output candidates retain title generation", func(t *testing.T) {
		sess := session.New()
		require.NoError(t, store.AddSession(ctx, sess))

		run, titleGen, err := sm.runtimeForSession(ctx, sess, "image-only.yaml", "", &config.RuntimeConfig{})
		require.NoError(t, err)
		t.Cleanup(func() { _ = run.Close() })

		assert.NotNil(t, titleGen, "an image-output-only agent can generate text-only titles")
	})

	t.Run("safe dedicated title model keeps titles enabled", func(t *testing.T) {
		sess := session.New()
		require.NoError(t, store.AddSession(ctx, sess))

		run, titleGen, err := sm.runtimeForSession(ctx, sess, "mixed.yaml", "", &config.RuntimeConfig{})
		require.NoError(t, err)
		t.Cleanup(func() { _ = run.Close() })

		assert.NotNil(t, titleGen, "a safe title_model must keep title generation available")
	})
}
