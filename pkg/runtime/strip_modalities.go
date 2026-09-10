package runtime

import (
	"context"
	"log/slog"
	"strings"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/hooks"
	"github.com/docker/docker-agent/pkg/modelinfo"
)

// BuiltinStripUnsupportedModalities is the name of the runtime-shipped
// before_llm_call message transform that drops image, audio, and video
// content from the outgoing messages when the resolved capabilities of
// the agent's current model don't cover that media kind. It's the
// runtime-shipped peer of [BuiltinCacheResponse] (a stop hook) — the
// constant exists mostly for log filtering and diagnostics.
//
// Sending unsupported media produces hard provider errors (HTTP 400
// from OpenAI, "image input is not supported" from Anthropic text
// variants, etc.); promoting the strip into a registered transform
// replaced an inline branch in runStreamLoop and opened the door to a
// family of message-mutating transforms (redactors, scrubbers, ...).
const BuiltinStripUnsupportedModalities = "strip_unsupported_modalities"

// stripUnsupportedModalitiesTransform is the [MessageTransform]
// registered under [BuiltinStripUnsupportedModalities]. It consumes
// the already-resolved capability set from
// [hooks.Input.ModelCapabilities] — populated by the loop for the
// model it actually chose (per-tool override + alloy-mode selection),
// with any explicit `capabilities:` config override applied — and
// drops the image/audio/video parts that model does not support.
//
// The transform must NOT resolve capabilities itself: a models.dev
// lookup here would ignore explicit config overrides, so a model the
// user declared `capabilities.audio: true` for would have its audio
// stripped anyway. Unknown models resolve (upstream, via
// [modelinfo.ResolveCapsFromModel]) to the conservative text-only
// default and lose their media parts — matching the attachment
// pipeline, which would drop them at provider conversion regardless.
//
// A nil capability set means the dispatching path had nothing to
// resolve against (e.g. a coding-harness label, or an embedder-built
// Input); the messages then pass through untouched, with a Debug log
// so operators can tell that apart from a silently inactive transform.
func (r *LocalRuntime) stripUnsupportedModalitiesTransform(
	ctx context.Context,
	in *hooks.Input,
	msgs []chat.Message,
) ([]chat.Message, error) {
	if in == nil || in.ModelCapabilities == nil {
		slog.DebugContext(ctx, "strip_unsupported_modalities: skipping, no resolved capabilities on input")
		return msgs, nil
	}
	mc := *in.ModelCapabilities
	if mc.SupportsImage() && mc.SupportsAudio() && mc.SupportsVideo() {
		return msgs, nil
	}
	return stripUnsupportedMediaContent(ctx, msgs, mc), nil
}

// stripUnsupportedMediaContent returns a copy of messages with the
// media parts (image/audio/video) the model does not support removed.
// Text parts, PDFs, and any other non-media content are preserved,
// and the relative order of the surviving parts is unchanged.
//
// A part carrying an ArtifactPath (a runtime-materialized, model-generated
// artifact — see [isGeneratedMediaPart]) is never stripped here, even when
// its MIME kind would otherwise be unsupported: [BuiltinStripGeneratedMedia]
// is registered to run first and is solely responsible for replacing that
// part with a safe placeholder. This check makes that independent of
// registration order — if the transform chain is ever reordered or this
// transform is invoked directly (as some tests do, bypassing the chain),
// a generated artifact still cannot be silently dropped without its
// placeholder, which would otherwise strand a media-only assistant turn
// with no content at all for a capability-less or unknown model.
//
// Lives next to [stripUnsupportedModalitiesTransform] (rather than in
// streaming.go where its image-only ancestor originated) so the
// builtin's registration, transform, and helper are co-located. Kept
// as an unexported helper because the only legitimate caller is the
// transform itself — direct use bypasses the capability resolution.
func stripUnsupportedMediaContent(ctx context.Context, messages []chat.Message, mc modelinfo.ModelCapabilities) []chat.Message {
	result := make([]chat.Message, len(messages))
	for i, msg := range messages {
		result[i] = msg

		if len(msg.MultiContent) == 0 {
			continue
		}

		var filtered []chat.MessagePart
		for _, part := range msg.MultiContent {
			if isGeneratedMediaPart(part) {
				filtered = append(filtered, part)
				continue
			}
			if kind := partMediaKind(part); kind != "" && !supportsMediaKind(mc, kind) {
				slog.DebugContext(ctx, "strip_unsupported_modalities: stripped media part",
					"kind", kind,
					"role", msg.Role,
					"reason", "model does not support "+kind+" input")
				continue
			}
			filtered = append(filtered, part)
		}

		if len(filtered) != len(msg.MultiContent) {
			result[i].MultiContent = filtered
			slog.DebugContext(ctx, "Stripped media content from message",
				"role", msg.Role,
				"original_parts", len(msg.MultiContent),
				"remaining_parts", len(filtered))
		}
	}
	return result
}

// partMediaKind classifies a message part into the media kind gated by
// model input modalities: "image", "audio", or "video". Everything
// else — text parts, PDFs, unknown binaries — returns "" and is never
// touched by this transform (PDF gating stays at provider conversion).
// Legacy ImageURL parts carry no MIME type and are images by
// construction.
func partMediaKind(part chat.MessagePart) string {
	switch part.Type {
	case chat.MessagePartTypeImageURL:
		return "image"
	case chat.MessagePartTypeFile:
		if part.File != nil {
			return mimeMediaKind(part.File.MimeType)
		}
	case chat.MessagePartTypeDocument:
		if part.Document != nil {
			return mimeMediaKind(part.Document.MimeType)
		}
	}
	return ""
}

// mimeMediaKind maps a MIME type to "image", "audio", or "video" by
// family prefix — the same classification
// [modelinfo.ModelCapabilities.Supports] uses — or "" for any other
// type.
func mimeMediaKind(mimeType string) string {
	switch mt := strings.ToLower(mimeType); {
	case strings.HasPrefix(mt, "image/"):
		return "image"
	case strings.HasPrefix(mt, "audio/"):
		return "audio"
	case strings.HasPrefix(mt, "video/"):
		return "video"
	default:
		return ""
	}
}

// supportsMediaKind reports whether the resolved capability set covers
// a media kind produced by [partMediaKind].
func supportsMediaKind(mc modelinfo.ModelCapabilities, kind string) bool {
	switch kind {
	case "image":
		return mc.SupportsImage()
	case "audio":
		return mc.SupportsAudio()
	case "video":
		return mc.SupportsVideo()
	default:
		return true
	}
}
