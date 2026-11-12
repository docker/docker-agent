package acp

import (
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"strconv"
	"strings"
	"unicode/utf8"
	"uuid"

	"github.com/coder/acp-go-sdk"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
)

const (
	replayTextChunkBytes = 64 << 10
	maxReplayUpdateBytes = 1 << 20
)

func (s *Session) finishLoading() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loading = false
	s.turns <- struct{}{}
}

func (a *Agent) replayLoadedSession(ctx context.Context, s *Session) (retErr error) {
	defer func() {
		if retErr != nil {
			// Close owns the join; waiting here would deadlock on this admitted load.
			a.mu.Lock()
			defer a.mu.Unlock()
			if a.sessions[s.id] == s {
				a.closeSessionLocked(context.WithoutCancel(ctx), s.id)
			}
		}
	}()
	if err := ctx.Err(); err != nil {
		return err
	}
	if a.conn == nil {
		return errors.New("ACP connection not configured")
	}
	stored, err := a.sessionStore.GetSession(ctx, s.id)
	if err != nil {
		return fmt.Errorf("reading replay history: %w", err)
	}
	snapshot := stored.Clone()
	if err := a.replayHistory(ctx, s, snapshot); err != nil {
		return err
	}
	if err := a.emitAvailableCommands(ctx, s); err != nil {
		return err
	}
	return ctx.Err()
}

type historicalToolCall struct {
	call       tools.ToolCall
	definition tools.Tool
	id         acp.ToolCallId
	result     *session.Message
}

// Match within each stored session and by occurrence, never by mutable agent identity.
func historicalCalls(items []session.Item) (map[*session.Message][]*historicalToolCall, map[*session.Message]*historicalToolCall) {
	starts := make(map[*session.Message][]*historicalToolCall)
	results := make(map[*session.Message]*historicalToolCall)
	pending := make(map[string][]*historicalToolCall)
	for _, item := range items {
		msg := item.Message
		if msg == nil || msg.Implicit {
			continue
		}
		switch msg.Message.Role {
		case chat.MessageRoleUser:
			clear(pending)
		case chat.MessageRoleAssistant:
			clear(pending)
			for _, call := range msg.Message.ToolCalls {
				definition := tools.Tool{Name: call.Function.Name}
				for _, tool := range msg.Message.ToolDefinitions {
					if tool.Name == call.Function.Name {
						definition = tool
						break
					}
				}
				state := &historicalToolCall{call: call, definition: definition, id: acp.ToolCallId(uuid.NewV4().String())}
				starts[msg] = append(starts[msg], state)
				if call.ID != "" {
					pending[call.ID] = append(pending[call.ID], state)
				}
			}
		case chat.MessageRoleTool:
			if queue := pending[msg.Message.ToolCallID]; len(queue) > 0 {
				state := queue[0]
				state.result = msg
				results[msg] = state
				pending[msg.Message.ToolCallID] = queue[1:]
			}
		}
	}
	return starts, results
}

func (a *Agent) replayHistory(ctx context.Context, s *Session, history *session.Session) error {
	starts, results := historicalCalls(history.Messages)
	for position, item := range history.Messages {
		if err := ctx.Err(); err != nil {
			return err
		}
		if item.SubSession != nil {
			if err := a.replayHistory(ctx, s, item.SubSession); err != nil {
				return err
			}
		}
		if item.Error != nil {
			if err := a.replayText(ctx, s.id, "Error: "+item.Error.Message, identifiedText(messageDisplayID(history.ID, "legacy-error:"+strconv.Itoa(position), "content"), acp.UpdateAgentMessageText)); err != nil {
				return err
			}
		}
		msg := item.Message
		if msg == nil || msg.Implicit {
			continue
		}
		switch msg.Message.Role {
		case chat.MessageRoleUser, chat.MessageRoleAssistant:
			logical := msg.Message.MessageID
			if logical == "" {
				logical = "legacy-item:" + strconv.Itoa(position)
			}
			contentID := messageDisplayID(history.ID, logical, "content")
			thoughtID := messageDisplayID(history.ID, logical, "thought")
			if msg.Message.Role == chat.MessageRoleAssistant {
				if err := a.replayText(ctx, s.id, msg.Message.ReasoningContent, identifiedText(thoughtID, acp.UpdateAgentThoughtText)); err != nil {
					return err
				}
			}
			if err := a.replayMessageContent(ctx, s.id, msg.Message, contentID); err != nil {
				return err
			}
			for _, state := range starts[msg] {
				// Stored arguments precede input transforms; replaying them can leak redacted input.
				title := cmp.Or(chat.SanitizeDisplayName(state.definition.Annotations.Title), chat.SanitizeDisplayName(state.call.Function.Name), "Tool call")
				update := acp.StartToolCall(state.id, title,
					func(call *acp.SessionUpdateToolCall) { call.Meta = toolNameMeta(state.call.Function.Name) },
					acp.WithStartKind(determineToolKind(state.call.Function.Name, state.definition)),
					acp.WithStartStatus(historicalStatus(state.result)))
				if state.result == nil {
					update.ToolCall.Content = []acp.ToolCallContent{acp.ToolContent(acp.TextBlock("Historical tool result unavailable; execution outcome is unknown."))}
				}
				if err := a.sendReplayUpdate(ctx, s.id, update); err != nil {
					return err
				}
			}
		case chat.MessageRoleTool:
			state := results[msg]
			content := []acp.ToolCallContent{acp.ToolContent(acp.TextBlock(replayToolText(msg.Message.Content)))}
			var update acp.SessionUpdate
			if state == nil {
				update = acp.StartToolCall(acp.ToolCallId(uuid.NewV4().String()), "Tool call", acp.WithStartStatus(historicalStatus(msg)), acp.WithStartContent(content))
			} else {
				update = acp.UpdateToolCall(state.id, func(call *acp.SessionToolCallUpdate) { call.Meta = toolNameMeta(state.call.Function.Name) }, acp.WithUpdateStatus(historicalStatus(msg)), acp.WithUpdateContent(content))
			}
			// Stored tool attachments have not passed the text-output transform contract.
			if err := a.sendReplayUpdate(ctx, s.id, update); err != nil {
				return err
			}
		}
	}
	return nil
}

func historicalStatus(result *session.Message) acp.ToolCallStatus {
	if result == nil || result.Message.IsError {
		return acp.ToolCallStatusFailed
	}
	return acp.ToolCallStatusCompleted
}

func replayToolText(text string) string {
	if len(text) <= replayTextChunkBytes {
		return text
	}
	end := replayTextChunkBytes
	for end > 0 && !utf8.RuneStart(text[end]) {
		end--
	}
	return text[:end] + "\n[Historical tool output truncated for replay.]"
}

func (a *Agent) replayMessageContent(ctx context.Context, sid string, msg chat.Message, messageID ...string) error {
	id := uuid.NewV4().String()
	if len(messageID) > 0 {
		id = messageID[0]
	}
	update := acp.UpdateUserMessage
	updateText := acp.UpdateUserMessageText
	if msg.Role == chat.MessageRoleAssistant {
		update = acp.UpdateAgentMessage
		updateText = acp.UpdateAgentMessageText
	}
	updateText = identifiedText(id, updateText)
	if len(msg.MultiContent) == 0 {
		return a.replayText(ctx, sid, msg.Content, updateText)
	}
	for _, part := range msg.MultiContent {
		if part.Type == chat.MessagePartTypeText {
			if err := a.replayText(ctx, sid, part.Text, updateText); err != nil {
				return err
			}
			continue
		}
		name := "attachment"
		var block acp.ContentBlock
		switch part.Type {
		case chat.MessagePartTypeDocument:
			if doc := part.Document; doc != nil {
				if safe := chat.SanitizeDisplayName(doc.Name); safe != "" {
					name = safe
				}
				if doc.Source.ArtifactPath == "" {
					uri := "urn:docker-agent:attachment:" + uuid.NewV4().String()
					mimeType := replayMIME(doc.MimeType)
					audio := strings.HasPrefix(strings.ToLower(strings.TrimSpace(doc.MimeType)), "audio/")
					if len(doc.Source.InlineData) > 0 && len(doc.Source.InlineData) <= maxReplayUpdateBytes/2 {
						data := base64.StdEncoding.EncodeToString(doc.Source.InlineData)
						switch {
						case audio:
							if audioMIME, ok := promptAudioMIME(doc.MimeType); ok && msg.Role == chat.MessageRoleUser {
								block = acp.AudioBlock(data, audioMIME)
							}
						case chat.IsImageMimeType(mimeType):
							block = acp.ImageBlock(data, mimeType)
						default:
							block = acp.ResourceBlock(acp.EmbeddedResourceResource{BlobResourceContents: &acp.BlobResourceContents{Uri: uri, MimeType: &mimeType, Blob: data}})
						}
					} else if len(doc.Source.InlineData) == 0 && (!audio || doc.Source.InlineText != "") {
						if err := a.replayText(ctx, sid, fmt.Sprintf("[Attachment: %s]\n", name)+doc.Source.InlineText, updateText); err != nil {
							return err
						}
						continue
					}
				}
			}
		case chat.MessagePartTypeImageURL:
			if part.ImageURL != nil && len(part.ImageURL.URL) <= maxReplayUpdateBytes/2 {
				header, data, ok := strings.Cut(part.ImageURL.URL, ",")
				if ok && strings.HasPrefix(header, "data:image/") && strings.HasSuffix(header, ";base64") {
					mimeType := strings.TrimSuffix(strings.TrimPrefix(header, "data:"), ";base64")
					if _, err := base64.StdEncoding.DecodeString(data); err == nil && chat.IsImageMimeType(mimeType) {
						block = acp.ImageBlock(data, mimeType)
					}
				}
			}
		}
		if block.Image == nil && block.Resource == nil && block.Audio == nil {
			block = acp.TextBlock(fmt.Sprintf("[Attachment: %s (content unavailable during replay)]", name))
		}
		if err := a.sendReplayUpdate(ctx, sid, withMessageID(update(block), id)); err != nil {
			return err
		}
	}
	return nil
}

func (a *Agent) replayText(ctx context.Context, sid, text string, update func(string) acp.SessionUpdate) error {
	for text != "" {
		end := min(len(text), replayTextChunkBytes)
		if end < len(text) {
			for end > 0 && !utf8.RuneStart(text[end]) {
				end--
			}
		}
		if end == 0 {
			end = min(len(text), replayTextChunkBytes)
		}
		if err := a.sendReplayUpdate(ctx, sid, update(text[:end])); err != nil {
			return err
		}
		text = text[end:]
	}
	return ctx.Err()
}

func (a *Agent) sendReplayUpdate(ctx context.Context, sid string, update acp.SessionUpdate) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	encoded, err := json.Marshal(acp.SessionNotification{SessionId: acp.SessionId(sid), Update: update})
	if err != nil {
		return fmt.Errorf("encoding replay update: %w", err)
	}
	if len(encoded) > maxReplayUpdateBytes {
		return errors.New("historical update exceeds replay size limit")
	}
	return a.sendUpdate(ctx, sid, update)
}

func replayMIME(value string) string {
	if len(value) <= 256 {
		if mediaType, _, err := mime.ParseMediaType(value); err == nil {
			return mediaType
		}
	}
	return "application/octet-stream"
}
