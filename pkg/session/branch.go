package session

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/docker/docker-agent/pkg/chat"
)

// BranchSession creates a new session branched from the parent at the given position.
// Messages up to (but not including) branchAtPosition are deep-cloned into the new session.
func BranchSession(parent *Session, branchAtPosition int) (*Session, error) {
	if parent == nil {
		return nil, errors.New("parent session is nil")
	}
	if branchAtPosition < 0 || branchAtPosition > len(parent.Messages) {
		return nil, fmt.Errorf("branch position %d out of range", branchAtPosition)
	}

	branched := New()
	copySessionMetadata(branched, parent, generateBranchTitle(parent.Title))

	branched.Messages = make([]Item, 0, branchAtPosition)
	for i := range branchAtPosition {
		cloned, err := cloneSessionItem(parent.Messages[i])
		if err != nil {
			return nil, err
		}
		branched.Messages = append(branched.Messages, cloned)
	}

	setParentIDs(branched)
	recalculateSessionTotals(branched)
	return branched, nil
}

func cloneSessionItem(item Item) (Item, error) {
	switch {
	case item.Message != nil:
		cloned := cloneMessage(item.Message)
		// Branched messages must get fresh database IDs when persisted, so
		// drop the parent's row ID.
		cloned.ID = 0
		return Item{Message: cloned}, nil
	case item.SubSession != nil:
		clonedSub, err := cloneSubSession(item.SubSession)
		if err != nil {
			return Item{}, err
		}
		return Item{SubSession: clonedSub}, nil
	case item.Summary != "":
		return Item{Summary: item.Summary, Cost: item.Cost}, nil
	default:
		return Item{}, errors.New("cannot clone empty session item")
	}
}

func cloneSubSession(src *Session) (*Session, error) {
	if src == nil {
		return nil, nil
	}

	cloned := New()
	copySessionMetadata(cloned, src, src.Title)
	cloned.CreatedAt = src.CreatedAt

	cloned.Messages = make([]Item, 0, len(src.Messages))
	for _, item := range src.Messages {
		clonedItem, err := cloneSessionItem(item)
		if err != nil {
			return nil, err
		}
		cloned.Messages = append(cloned.Messages, clonedItem)
	}

	recalculateSessionTotals(cloned)
	return cloned, nil
}

func copySessionMetadata(dst, src *Session, title string) {
	if src == nil || dst == nil {
		return
	}
	dst.Title = title
	dst.ToolsApproved = src.ToolsApproved
	dst.HideToolResults = src.HideToolResults
	dst.WorkingDir = src.WorkingDir
	dst.SendUserMessage = src.SendUserMessage
	dst.MaxIterations = src.MaxIterations
	dst.Starred = src.Starred
	dst.Permissions = clonePermissionsConfig(src.Permissions)
	dst.AgentModelOverrides = cloneStringMap(src.AgentModelOverrides)
	dst.CustomModelsUsed = cloneStringSlice(src.CustomModelsUsed)
	dst.AttachedFiles = src.AttachedFilesSnapshot()
}

// generateBranchTitle creates a title for a branched session based on the parent title.
// If the parent has no title, returns empty string (will trigger auto-generation).
// If the parent title already ends with "(branched)" or "(branch N)", increment the number.
func generateBranchTitle(parentTitle string) string {
	if parentTitle == "" {
		return ""
	}

	// Check for existing branch suffix patterns
	// Pattern: "(branch N)" where N >= 2
	// Pattern: "(branched)" which is equivalent to branch 1

	// Check for "(branch N)" pattern
	if idx := strings.LastIndex(parentTitle, "(branch "); idx >= 0 {
		suffix := parentTitle[idx:]
		var n int
		if _, err := fmt.Sscanf(suffix, "(branch %d)", &n); err == nil && n >= 2 {
			baseTitle := strings.TrimRight(parentTitle[:idx], " \t")
			return fmt.Sprintf("%s (branch %d)", baseTitle, n+1)
		}
	}

	// Check for "(branched)" pattern
	const branchedSuffix = "(branched)"
	if strings.HasSuffix(parentTitle, branchedSuffix) {
		baseTitle := strings.TrimRight(parentTitle[:len(parentTitle)-len(branchedSuffix)], " \t")
		return baseTitle + " (branch 2)"
	}

	return parentTitle + " (branched)"
}

func clonePermissionsConfig(src *PermissionsConfig) *PermissionsConfig {
	if src == nil {
		return nil
	}
	return &PermissionsConfig{
		Allow: cloneStringSlice(src.Allow),
		Deny:  cloneStringSlice(src.Deny),
	}
}

func cloneStringMap(src map[string]string) map[string]string {
	if len(src) == 0 {
		return nil
	}
	return maps.Clone(src)
}

func cloneStringSlice(src []string) []string {
	if src == nil {
		return nil
	}
	return slices.Clone(src)
}

func setParentIDs(sess *Session) {
	if sess == nil {
		return
	}
	for _, item := range sess.Messages {
		if item.SubSession == nil {
			continue
		}
		item.SubSession.ParentID = sess.ID
		setParentIDs(item.SubSession)
	}
}

func recalculateSessionTotals(sess *Session) {
	if sess == nil {
		return
	}

	var inputTokens int64
	var outputTokens int64

	for _, msg := range sess.GetAllMessages() {
		if msg.Message.Role != chat.MessageRoleAssistant {
			continue
		}
		if msg.Message.Usage != nil {
			inputTokens += msg.Message.Usage.InputTokens
			outputTokens += msg.Message.Usage.OutputTokens
		}
	}

	sess.InputTokens = inputTokens
	sess.OutputTokens = outputTokens
}
