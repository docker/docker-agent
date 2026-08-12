package session

import (
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"

	"github.com/docker/docker-agent/pkg/chat"
)

// forkSuffixRe matches a "(fork N)" trailing suffix on a session title,
// with optional leading whitespace before the parenthesis. Anchored at
// the end of the string so mid-title occurrences of "(fork N)" (e.g.
// in a user-chosen title like "Q1 (fork 2) Analysis") are not detected
// as a suffix and the trailing content is not silently dropped.
var forkSuffixRe = regexp.MustCompile(`^(.*?)[ \t]*\(fork (\d+)\)$`)

// BranchSession creates a new session branched from the parent at the given position.
// Messages up to (but not including) branchAtPosition are deep-cloned into the new session.
func BranchSession(parent *Session, branchAtPosition int) (*Session, error) {
	return branchSessionWithTitle(parent, branchAtPosition, generateBranchTitle)
}

// ForkSession is like BranchSession but uses fork-numbered titles
// ("<title> (fork 1)", "(fork 2)", …). It is intended for HTTP/API clients
// that expose a "fork from here" UX distinct from the TUI's branch-from-edit
// flow, which keeps using BranchSession.
func ForkSession(parent *Session, branchAtPosition int) (*Session, error) {
	return branchSessionWithTitle(parent, branchAtPosition, generateForkTitle)
}

func branchSessionWithTitle(parent *Session, branchAtPosition int, titleFn func(string) string) (*Session, error) {
	if parent == nil {
		return nil, errors.New("parent session is nil")
	}

	// Snapshot under parent.mu (like Clone) so a concurrent AddMessage/
	// ApplyCompaction — e.g. from a live HTTP stream on the same session —
	// cannot race with this read or shift branchAtPosition mid-copy.
	items := parent.snapshotItems()
	if branchAtPosition < 0 || branchAtPosition > len(items) {
		return nil, fmt.Errorf("branch position %d out of range", branchAtPosition)
	}

	branched := New()
	copySessionMetadata(branched, parent, titleFn(parent.TitleSnapshot()))

	branched.Messages = make([]Item, 0, branchAtPosition)
	for i := range branchAtPosition {
		cloned, err := cloneSessionItem(items[i])
		if err != nil {
			return nil, err
		}
		branched.Messages = append(branched.Messages, cloned)
	}

	setParentIDs(branched)
	recalculateSessionTotals(branched)
	return branched, nil
}

// Clone returns a deep copy of the session that is safe to mutate without
// affecting the original. Conversation items (messages and sub-sessions)
// are deep-cloned so the two sessions share no mutable state; scalar and
// configuration fields are copied verbatim so the clone runs identically.
// Unlike BranchSession, Clone keeps the original ID, message IDs, and the
// full message history, making it suitable for transactional "work on a
// copy, commit on success" flows.
func (s *Session) Clone() *Session {
	if s == nil {
		return nil
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	clone := &Session{
		ID:                      s.ID,
		InputID:                 s.InputID,
		Title:                   s.Title,
		Evals:                   cloneEvalCriteria(s.Evals),
		EvalResult:              cloneEvalResult(s.EvalResult),
		CreatedAt:               s.CreatedAt,
		ToolsApproved:           s.ToolsApproved,
		SafetyPolicy:            s.SafetyPolicy,
		PriorSafetyPolicy:       s.PriorSafetyPolicy,
		NonInteractive:          s.NonInteractive,
		HideToolResults:         s.HideToolResults,
		WorkingDir:              s.WorkingDir,
		SendUserMessage:         s.SendUserMessage,
		MaxIterations:           s.MaxIterations,
		MaxConsecutiveToolCalls: s.MaxConsecutiveToolCalls,
		MaxOldToolCallTokens:    s.MaxOldToolCallTokens,
		MaxToolResultTokens:     s.MaxToolResultTokens,
		Starred:                 s.Starred,
		InputTokens:             s.InputTokens,
		OutputTokens:            s.OutputTokens,
		Cost:                    s.Cost,
		Permissions:             s.Permissions.Clone(),
		Attributes:              maps.Clone(s.Attributes),
		AgentModelOverrides:     cloneStringMap(s.AgentModelOverrides),
		CustomModelsUsed:        cloneStringSlice(s.CustomModelsUsed),
		AttachedFiles:           cloneStringSlice(s.AttachedFiles),
		ExcludedTools:           cloneStringSlice(s.ExcludedTools),
		AllowedTools:            cloneStringSlice(s.AllowedTools),
		ExtraToolSets:           slices.Clone(s.ExtraToolSets),
		AgentName:               s.AgentName,
		ParentID:                s.ParentID,
		DelegationLineage:       cloneStringSlice(s.DelegationLineage),
		InstructionContext:      cloneInstructionContext(s.InstructionContext),
		MessageUsageHistory:     slices.Clone(s.MessageUsageHistory),
	}

	// Start from a shallow copy of each item so value fields (Summary,
	// Cost, FirstKeptEntry) are preserved verbatim, then deep-copy the
	// pointer fields so the clone shares no mutable state. Sub-sessions
	// recurse through Clone to stay faithful (unlike BranchSession's
	// helper, which mints fresh IDs and resets metadata).
	clone.Messages = make([]Item, len(s.Messages))
	for i, item := range s.Messages {
		clone.Messages[i] = item
		if item.Message != nil {
			clone.Messages[i].Message = cloneMessage(item.Message)
		}
		if item.SubSession != nil {
			clone.Messages[i].SubSession = item.SubSession.Clone()
		}
		if item.Error != nil {
			errCopy := *item.Error
			clone.Messages[i].Error = &errCopy
		}
		if item.Termination != nil {
			termCopy := *item.Termination
			clone.Messages[i].Termination = &termCopy
		}
		if item.Usage != nil {
			usageCopy := *item.Usage
			clone.Messages[i].Usage = &usageCopy
		}
	}
	return clone
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
		cloned := Item{Summary: item.Summary, FirstKeptEntry: item.FirstKeptEntry, Cost: item.Cost, Model: item.Model}
		if item.Usage != nil {
			usageCopy := *item.Usage
			cloned.Usage = &usageCopy
		}
		return cloned, nil
	case item.Error != nil:
		errCopy := *item.Error
		return Item{Error: &errCopy}, nil
	case item.Termination != nil:
		termCopy := *item.Termination
		return Item{Termination: &termCopy}, nil
	default:
		return Item{}, errors.New("cannot clone empty session item")
	}
}

func cloneSubSession(src *Session) (*Session, error) {
	if src == nil {
		return nil, nil
	}

	cloned := New()
	copySessionMetadata(cloned, src, src.TitleSnapshot())
	cloned.CreatedAt = src.CreatedAt

	// Snapshot under src.mu: a sub-session created by a background agent
	// task can still be actively appended to while the top-level session is
	// being branched/forked.
	items := src.snapshotItems()
	cloned.Messages = make([]Item, 0, len(items))
	for _, item := range items {
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
	// AgentName, ParentID, and DelegationLineage are deliberately not
	// copied: a branch/fork is a fresh top-level session, not a pinned
	// sub-session, so transient delegation state must not carry over.
	// Clone() keeps them because it reproduces the session verbatim.
	dst.SetTitle(title)
	dst.ToolsApproved = src.ToolsApproved
	dst.SafetyPolicy = src.SafetyPolicy
	dst.PriorSafetyPolicy = src.PriorSafetyPolicy
	dst.HideToolResults = src.HideToolResults
	dst.WorkingDir = src.WorkingDir
	dst.SendUserMessage = src.SendUserMessage
	dst.MaxIterations = src.MaxIterations
	// MaxConsecutiveToolCalls, MaxOldToolCallTokens and MaxToolResultTokens
	// are safety / context rails that may be configured deliberately by the
	// user or operator (consecutive-tool-call cutoff, old-tool-call
	// truncation budget, per-tool-result cap). Dropping them on a fork or
	// branch would silently make the new session behave differently from its
	// parent. Clone() preserves them all, so do the same here.
	dst.MaxConsecutiveToolCalls = src.MaxConsecutiveToolCalls
	dst.MaxOldToolCallTokens = src.MaxOldToolCallTokens
	dst.MaxToolResultTokens = src.MaxToolResultTokens
	dst.Starred = src.Starred
	dst.Permissions = src.Permissions.Clone()
	dst.Attributes = src.AttributesSnapshot()
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

// generateForkTitle creates a title for a forked session based on the parent
// title using "(fork N)" suffixes that increment on repeated forks.
// If the parent has no title, returns empty string (will trigger auto-generation).
// "<title>" -> "<title> (fork 1)"; "<title> (fork N)" -> "<title> (fork N+1)".
//
// The "(fork N)" detection is anchored at the end of the title so a mid-title
// occurrence (e.g. "Q1 (fork 2) Analysis") is not treated as a suffix.
// generateBranchTitle uses an older LastIndex+Sscanf approach that has the
// same end-anchoring gap; that's a pre-existing issue in the TUI's branch
// flow and is intentionally left alone here.
func generateForkTitle(parentTitle string) string {
	if parentTitle == "" {
		return ""
	}

	if m := forkSuffixRe.FindStringSubmatch(parentTitle); m != nil {
		baseTitle := m[1]
		var n int
		if _, err := fmt.Sscanf(m[2], "%d", &n); err == nil && n >= 1 {
			return fmt.Sprintf("%s (fork %d)", baseTitle, n+1)
		}
	}

	return parentTitle + " (fork 1)"
}

// NextForkTitle returns the next free "<root> (fork N)" title for a
// fork of parentTitle. Forks descending from the same root share a
// single counter scanned out of siblingTitles. Returns empty for an
// empty parentTitle so the caller's auto-title path kicks in.
func NextForkTitle(parentTitle string, siblingTitles []string) string {
	if parentTitle == "" {
		return ""
	}
	baseTitle := parentTitle
	if m := forkSuffixRe.FindStringSubmatch(parentTitle); m != nil {
		baseTitle = m[1]
	}
	siblingRe := regexp.MustCompile(`^` + regexp.QuoteMeta(baseTitle) + `[ \t]*\(fork (\d+)\)$`)
	highest := 0
	for _, t := range siblingTitles {
		m := siblingRe.FindStringSubmatch(t)
		if m == nil {
			continue
		}
		var n int
		if _, err := fmt.Sscanf(m[1], "%d", &n); err == nil && n > highest {
			highest = n
		}
	}
	return fmt.Sprintf("%s (fork %d)", baseTitle, highest+1)
}

func cloneEvalCriteria(src *EvalCriteria) *EvalCriteria {
	if src == nil {
		return nil
	}
	cp := *src
	cp.Relevance = cloneStringSlice(src.Relevance)
	return &cp
}

func cloneEvalResult(src *EvalResult) *EvalResult {
	if src == nil {
		return nil
	}
	cp := *src
	cp.Successes = cloneStringSlice(src.Successes)
	cp.Failures = cloneStringSlice(src.Failures)
	cp.Checks = cloneEvalResultChecks(src.Checks)
	if src.Termination != nil {
		term := *src.Termination
		cp.Termination = &term
	}
	return &cp
}

func cloneEvalResultChecks(src EvalResultChecks) EvalResultChecks {
	var cp EvalResultChecks
	if src.Size != nil {
		size := *src.Size
		cp.Size = &size
	}
	if src.ToolCalls != nil {
		toolCalls := *src.ToolCalls
		cp.ToolCalls = &toolCalls
	}
	if src.Relevance != nil {
		relevance := *src.Relevance
		relevance.Results = slices.Clone(src.Relevance.Results)
		cp.Relevance = &relevance
	}
	return cp
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

	sess.SetUsage(inputTokens, outputTokens)
}
