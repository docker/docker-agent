package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"strings"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/compaction"
	"github.com/docker/docker-agent/pkg/promptfiles"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
)

// ContextCategory aggregates the estimated token footprint of one slice of
// the context window along with the number of items composing it (messages,
// tool definitions, or prompt files depending on the category).
type ContextCategory struct {
	Tokens int64 `json:"tokens"`
	Items  int   `json:"items"`
}

func (c *ContextCategory) add(tokens int64) {
	c.Tokens += tokens
	c.Items++
}

// ContextFile describes one file contributing to the context window, with
// the estimated token footprint of its current on-disk content.
type ContextFile struct {
	// Path is the file's absolute location on disk.
	Path string `json:"path"`
	// Tokens is the estimated in-context footprint of the file. It is 0
	// for files whose content never reaches the prompt inline (unsupported
	// types, oversized text files) and for missing files.
	Tokens int64 `json:"tokens"`
	// Missing marks files that can no longer be read from disk.
	Missing bool `json:"missing,omitempty"`
}

// ContextBreakdown describes the estimated composition of the prompt the
// runtime would send on the next model call, broken down by category. All
// token counts are estimates produced by [compaction.EstimateMessageTokens]
// (provider-reported counts where available, a chars-per-token heuristic
// otherwise); the actual provider tokenizer may count differently.
type ContextBreakdown struct {
	// SystemPrompt covers the invariant system messages: the agent
	// instruction, multi-agent/handoff prompts, and toolset instructions.
	SystemPrompt ContextCategory `json:"system_prompt"`
	// ToolDefinitions covers the JSON schemas (name, description,
	// parameters) of every tool exposed to the agent.
	ToolDefinitions ContextCategory `json:"tool_definitions"`
	// PromptFiles covers the add_prompt_files content (AGENTS.md, ...)
	// injected as transient context at every turn.
	PromptFiles ContextCategory `json:"prompt_files"`
	// Messages covers the user and assistant conversation turns.
	Messages ContextCategory `json:"messages"`
	// ToolResults covers the tool-role result messages.
	ToolResults ContextCategory `json:"tool_results"`
	// CompactionSummary covers the synthetic message that carries the
	// latest compaction summary, when the session has been compacted.
	CompactionSummary ContextCategory `json:"compaction_summary"`

	// PromptFileItems details the individual files behind the PromptFiles
	// category, in resolution order.
	PromptFileItems []ContextFile `json:"prompt_file_items,omitempty"`
	// AttachedFiles lists the files attached to the session (/attach,
	// @-mentions), in attach order. Their content was inlined into user
	// messages at send time, so their tokens are already counted in the
	// Messages category; the per-file numbers here are re-estimated from
	// the current on-disk content and are informational, not an extra
	// bucket in TotalTokens.
	AttachedFiles []ContextFile `json:"attached_files,omitempty"`
	// LatestCompactionSummary is the verbatim text behind the
	// CompactionSummary category: the most recent compaction summary
	// stored on the session, or "" when it has never been compacted.
	LatestCompactionSummary string `json:"latest_compaction_summary,omitempty"`

	// ContextLimit is the resolved context window of the effective model,
	// or 0 when it cannot be determined (harness-backed agents, models
	// absent from the catalogue).
	ContextLimit int64 `json:"context_limit"`
	// Model is the effective model label ("provider/model", or the
	// harness label for harness-backed agents).
	Model string `json:"model"`

	// CompactionModel is the identity ("provider/model") of the agent's
	// dedicated compaction model, set whenever one is configured (regardless
	// of whether its window actually caps ContextLimit) — renderers need the
	// unresolved-primary edge case (PrimaryContextLimit == 0) too. Use
	// CompactionContextLimit and PrimaryContextLimit together with
	// [LocalRuntime.effectiveContextLimit]'s cap predicate to decide whether
	// to attribute a reduction.
	CompactionModel string `json:"compaction_model,omitempty"`
	// CompactionContextLimit is the dedicated compaction model's own context
	// window, or 0 when it cannot be resolved. Set only alongside
	// CompactionModel.
	CompactionContextLimit int64 `json:"compaction_context_limit,omitempty"`
	// PrimaryContextLimit is the primary model's own context window (before
	// any compaction-model cap is applied), or 0 when it cannot be resolved.
	// Set only alongside CompactionModel.
	PrimaryContextLimit int64 `json:"primary_context_limit,omitempty"`
}

// TotalTokens returns the estimated size of the whole prompt.
func (b *ContextBreakdown) TotalTokens() int64 {
	return b.SystemPrompt.Tokens +
		b.ToolDefinitions.Tokens +
		b.PromptFiles.Tokens +
		b.Messages.Tokens +
		b.ToolResults.Tokens +
		b.CompactionSummary.Tokens
}

// ContextBreakdown computes the estimated context-window composition for
// sess, categorizing the output of
// [session.Session.GetMessagesAndLastSummary] and adding the tool
// definitions and prompt files that accompany every model call.
//
// Tool listing failures and unreadable prompt files degrade gracefully: the
// corresponding category is computed from whatever could be gathered and the
// failure is logged, so a broken toolset never hides the rest of the data.
func (r *LocalRuntime) ContextBreakdown(ctx context.Context, sess *session.Session) (*ContextBreakdown, error) {
	if sess == nil {
		return nil, errors.New("no active session")
	}
	a := r.resolveSessionAgent(sess)
	if a == nil {
		return nil, errors.New("no active agent")
	}

	b := &ContextBreakdown{Model: agentModelLabel(ctx, a)}
	if !a.HasHarness() {
		modelID := r.getEffectiveModelID(ctx, a)
		b.ContextLimit = r.contextLimitForAgentModel(ctx, a, modelID)
		if a.CompactionModel() != nil {
			b.CompactionModel, b.PrimaryContextLimit, b.CompactionContextLimit = r.compactionCapAttribution(ctx, a, modelID)
		}
	}

	messages, summary := sess.GetMessagesAndLastSummary(a)
	// Calibrate the heuristic against the provider-reported usage already
	// recorded on this conversation, mirroring what the proactive
	// compaction trigger does (see compactIfNeeded).
	estimator := compaction.NewSliceEstimator(messages)

	summaryContent := ""
	if summary != "" {
		b.LatestCompactionSummary = summary
		summaryContent = session.SummaryMessageContent(summary)
	}

	for i := range messages {
		msg := &messages[i]
		tokens := estimator.EstimateMessageTokens(msg)
		switch {
		case msg.Role == chat.MessageRoleSystem:
			b.SystemPrompt.add(tokens)
		case msg.Role == chat.MessageRoleTool:
			b.ToolResults.add(tokens)
		case summaryContent != "" && msg.Role == chat.MessageRoleUser && msg.Content == summaryContent:
			b.CompactionSummary.add(tokens)
		default:
			b.Messages.add(tokens)
		}
	}

	agentTools, err := a.Tools(ctx)
	if err != nil {
		slog.WarnContext(ctx, "Context breakdown: failed to list tools; tool definitions omitted",
			"agent", a.Name(), "session_id", sess.ID, "error", err)
	}
	for i := range agentTools {
		b.ToolDefinitions.add(estimateToolDefinitionTokens(&agentTools[i]))
	}

	b.PromptFiles, b.PromptFileItems = r.promptFilesCategory(ctx, a.AddPromptFiles(), a.AddPromptFilesDepth())
	b.AttachedFiles = attachedFilesInventory(ctx, estimator, sess.AttachedFilesSnapshot())

	return b, nil
}

// estimateToolDefinitionTokens estimates the prompt cost of one tool
// definition from the parts every provider serializes into the request:
// name, description, and the parameters JSON schema. The estimate runs
// through the same chars-per-token heuristic as messages; the synthetic
// message's per-message overhead stands in for the provider's per-tool
// wrapper tokens.
func estimateToolDefinitionTokens(tool *tools.Tool) int64 {
	content := tool.Name + tool.Description
	if tool.Parameters != nil {
		if params, err := json.Marshal(tool.Parameters); err == nil {
			content += string(params)
		}
	}
	return compaction.EstimateMessageTokens(&chat.Message{
		Role:    chat.MessageRoleSystem,
		Content: content,
	})
}

// promptFilesCategory estimates the transient context injected by the
// add_prompt_files builtin at every turn. It resolves each configured name
// through the same lookup the hook uses (workdir hierarchy plus home or
// staged kit, keyed off the runtime working directory the hooks executor is
// built with) and sizes the joined contents as the single system message the
// hook would produce. Items counts the files found, not the names configured;
// the nested-file listing adds tokens but no item, since it is a note rather
// than a file. The returned files detail the same lookup per resolved path;
// their individual estimates each carry the per-message overhead the category
// counts once, so their sum can slightly exceed the category total.
func (r *LocalRuntime) promptFilesCategory(ctx context.Context, names []string, depth int) (ContextCategory, []ContextFile) {
	var category ContextCategory
	var files []ContextFile
	if len(names) == 0 {
		return category, nil
	}
	home, _ := os.UserHomeDir() // empty string disables the home-dir lookup
	var parts []string
	var loaded []string
	for _, name := range names {
		for _, path := range promptfiles.PathsFromEnv(r.workingDir, home, name) {
			content, err := os.ReadFile(path)
			if err != nil {
				slog.WarnContext(ctx, "Context breakdown: failed to read prompt file", "path", path, "error", err)
				continue
			}
			loaded = append(loaded, path)
			parts = append(parts, string(content))
			category.Items++
			files = append(files, ContextFile{
				Path: path,
				Tokens: compaction.EstimateMessageTokens(&chat.Message{
					Role:    chat.MessageRoleSystem,
					Content: string(content),
				}),
			})
		}
	}
	if note := promptfiles.Index(ctx, r.workingDir, names, depth, loaded); note != "" {
		parts = append(parts, note)
	}
	if len(parts) == 0 {
		return category, files
	}
	category.Tokens = compaction.EstimateMessageTokens(&chat.Message{
		Role:    chat.MessageRoleSystem,
		Content: strings.Join(parts, "\n\n"),
	})
	return category, files
}

// attachedFilesInventory sizes each file attached to the session from its
// current on-disk content. Files that disappeared or became unreadable are
// kept in the inventory (marked missing) so users can spot and drop dangling
// references.
func attachedFilesInventory(ctx context.Context, estimator compaction.Estimator, paths []string) []ContextFile {
	if len(paths) == 0 {
		return nil
	}
	files := make([]ContextFile, 0, len(paths))
	for _, path := range paths {
		file := ContextFile{Path: path}
		if fi, err := os.Stat(path); err != nil || !fi.Mode().IsRegular() {
			file.Missing = true
		} else {
			file.Tokens = attachedFileTokens(ctx, estimator, path, fi.Size())
		}
		files = append(files, file)
	}
	return files
}

// attachedFileTokens estimates the in-context footprint of one attached
// file, mirroring how the attachment pipeline shipped it: text files are
// inlined verbatim (when under the inline size cap), supported binary types
// (images, PDFs) travel as binary parts sized by the estimator's flat
// attachment charge, and anything else never reaches the prompt inline.
func attachedFileTokens(ctx context.Context, estimator compaction.Estimator, path string, size int64) int64 {
	switch {
	case chat.IsTextFile(path):
		if size > chat.MaxInlineFileSize {
			return 0
		}
		content, err := os.ReadFile(path)
		if err != nil {
			slog.WarnContext(ctx, "Context breakdown: failed to read attached file", "path", path, "error", err)
			return 0
		}
		return estimator.EstimateMessageTokens(&chat.Message{
			Role:    chat.MessageRoleUser,
			Content: string(content),
		})
	case chat.IsSupportedMimeType(chat.DetectMimeType(path)):
		// A synthetic document part with inline data stands in for the real
		// binary attachment so the estimator applies its flat per-attachment
		// charge, mirroring how the sent message is sized.
		return estimator.EstimateMessageTokens(&chat.Message{
			Role: chat.MessageRoleUser,
			MultiContent: []chat.MessagePart{
				{Type: chat.MessagePartTypeDocument, Document: &chat.Document{Source: chat.DocumentSource{InlineData: []byte{0}}}},
			},
		})
	default:
		return 0
	}
}
