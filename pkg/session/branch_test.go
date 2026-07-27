package session

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
)

func TestGenerateBranchTitle(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		parentTitle string
		expected    string
	}{
		{
			name:        "empty title returns empty",
			parentTitle: "",
			expected:    "",
		},
		{
			name:        "simple title gets branched suffix",
			parentTitle: "My Session",
			expected:    "My Session (branched)",
		},
		{
			name:        "branched becomes branch 2",
			parentTitle: "My Session (branched)",
			expected:    "My Session (branch 2)",
		},
		{
			name:        "branch 2 becomes branch 3",
			parentTitle: "My Session (branch 2)",
			expected:    "My Session (branch 3)",
		},
		{
			name:        "branch 99 becomes branch 100",
			parentTitle: "My Session (branch 99)",
			expected:    "My Session (branch 100)",
		},
		{
			name:        "title with branch in middle is treated as simple title",
			parentTitle: "branch analysis session",
			expected:    "branch analysis session (branched)",
		},
		{
			name:        "title ending with (branch but no number treated as simple",
			parentTitle: "My Session (branch",
			expected:    "My Session (branch (branched)",
		},
		{
			name:        "branch 1 is treated as simple title",
			parentTitle: "My Session (branch 1)",
			expected:    "My Session (branch 1) (branched)",
		},
		{
			name:        "trims whitespace before suffix",
			parentTitle: "My Session  (branched)",
			expected:    "My Session (branch 2)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			result := generateBranchTitle(tt.parentTitle)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestGenerateForkTitle(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		parentTitle string
		expected    string
	}{
		{
			name:        "empty title returns empty",
			parentTitle: "",
			expected:    "",
		},
		{
			name:        "simple title gets fork 1 suffix",
			parentTitle: "My Session",
			expected:    "My Session (fork 1)",
		},
		{
			name:        "fork 1 becomes fork 2",
			parentTitle: "My Session (fork 1)",
			expected:    "My Session (fork 2)",
		},
		{
			name:        "fork 2 becomes fork 3",
			parentTitle: "My Session (fork 2)",
			expected:    "My Session (fork 3)",
		},
		{
			name:        "fork 99 becomes fork 100",
			parentTitle: "My Session (fork 99)",
			expected:    "My Session (fork 100)",
		},
		{
			name:        "title with fork in middle is treated as simple title",
			parentTitle: "fork analysis session",
			expected:    "fork analysis session (fork 1)",
		},
		{
			name:        "title ending with (fork but no number treated as simple",
			parentTitle: "My Session (fork",
			expected:    "My Session (fork (fork 1)",
		},
		{
			name:        "branched title is treated as a simple title (separate series)",
			parentTitle: "My Session (branched)",
			expected:    "My Session (branched) (fork 1)",
		},
		{
			name:        "trims whitespace before suffix",
			parentTitle: "My Session  (fork 1)",
			expected:    "My Session (fork 2)",
		},
		{
			// Regression: a user-chosen title containing "(fork N)" anywhere
			// other than at the end must not be treated as a suffix — the
			// trailing content has to be preserved.
			name:        "mid-title (fork N) is not treated as suffix",
			parentTitle: "Q1 (fork 2) Analysis",
			expected:    "Q1 (fork 2) Analysis (fork 1)",
		},
		{
			name:        "title ending with ) but not (fork N)) is appended verbatim",
			parentTitle: "My Session (notes)",
			expected:    "My Session (notes) (fork 1)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			result := generateForkTitle(tt.parentTitle)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestNextForkTitle(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		parentTitle string
		siblings    []string
		expected    string
	}{
		{
			name:        "empty parent title returns empty",
			parentTitle: "",
			siblings:    []string{"Other"},
			expected:    "",
		},
		{
			name:        "no siblings: starts at fork 1",
			parentTitle: "My Session",
			siblings:    []string{},
			expected:    "My Session (fork 1)",
		},
		{
			name:        "ignores unrelated siblings, picks next free N",
			parentTitle: "My Session",
			siblings:    []string{"My Session (fork 1)", "Unrelated", "My Session (fork 2)"},
			expected:    "My Session (fork 3)",
		},
		{
			name:        "parent already has (fork N), counter rooted on base title",
			parentTitle: "My Session (fork 2)",
			siblings:    []string{"My Session (fork 1)", "My Session (fork 2)"},
			expected:    "My Session (fork 3)",
		},
		{
			name:        "gaps in fork numbering are tolerated, picks max+1",
			parentTitle: "My Session",
			siblings:    []string{"My Session (fork 1)", "My Session (fork 5)"},
			expected:    "My Session (fork 6)",
		},
		{
			name:        "mid-title (fork N) is not a sibling",
			parentTitle: "Q1 Analysis",
			siblings:    []string{"Q1 (fork 2) Analysis"},
			expected:    "Q1 Analysis (fork 1)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.expected, NextForkTitle(tt.parentTitle, tt.siblings))
		})
	}
}

func TestCloneSessionItem(t *testing.T) {
	t.Parallel()
	t.Run("empty item returns error", func(t *testing.T) {
		t.Parallel()
		_, err := cloneSessionItem(Item{})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "cannot clone empty session item")
	})

	t.Run("message item clones successfully", func(t *testing.T) {
		t.Parallel()
		item := NewMessageItem(UserMessage("test"))
		cloned, err := cloneSessionItem(item)
		require.NoError(t, err)
		assert.Equal(t, "test", cloned.Message.Message.Content)
	})

	t.Run("summary item clones successfully", func(t *testing.T) {
		t.Parallel()
		item := Item{Summary: "test summary", FirstKeptEntry: 3, Cost: 0.01, Model: "gpt-4o-mini", Usage: &chat.Usage{InputTokens: 10}}
		cloned, err := cloneSessionItem(item)
		require.NoError(t, err)
		assert.Equal(t, "test summary", cloned.Summary)
		assert.Equal(t, 3, cloned.FirstKeptEntry, "the kept-tail boundary must survive branching")
		assert.InDelta(t, 0.01, cloned.Cost, 1e-9)
		assert.Equal(t, "gpt-4o-mini", cloned.Model)
		require.NotNil(t, cloned.Usage)
		assert.NotSame(t, item.Usage, cloned.Usage, "usage must be deep-copied")
		assert.Equal(t, int64(10), cloned.Usage.InputTokens)
	})

	t.Run("error item clones into a distinct pointer", func(t *testing.T) {
		t.Parallel()
		item := NewErrorItem(&Error{Message: "boom", Code: "model_error"})
		cloned, err := cloneSessionItem(item)
		require.NoError(t, err)
		require.NotNil(t, cloned.Error)
		assert.Equal(t, "boom", cloned.Error.Message)
		assert.Equal(t, "model_error", cloned.Error.Code)
		// Mutating the clone must not affect the original item.
		cloned.Error.Message = "mutated"
		assert.Equal(t, "boom", item.Error.Message)
	})
}

func TestBranchSession(t *testing.T) {
	t.Parallel()
	t.Run("nil parent returns error", func(t *testing.T) {
		t.Parallel()
		_, err := BranchSession(nil, 0)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "parent session is nil")
	})

	t.Run("negative position returns error", func(t *testing.T) {
		t.Parallel()
		parent := &Session{Messages: []Item{NewMessageItem(UserMessage("test"))}}
		_, err := BranchSession(parent, -1)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "out of range")
	})

	t.Run("position beyond messages returns error", func(t *testing.T) {
		t.Parallel()
		parent := &Session{Messages: []Item{NewMessageItem(UserMessage("test"))}}
		_, err := BranchSession(parent, 2)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "out of range")
	})

	t.Run("position equal to messages length no error", func(t *testing.T) {
		t.Parallel()
		parent := &Session{Messages: []Item{NewMessageItem(UserMessage("test"))}}
		_, err := BranchSession(parent, 1)
		require.NoError(t, err)
	})

	t.Run("branches across a persisted error item", func(t *testing.T) {
		t.Parallel()
		// Regression: a session containing a recorded failure must remain
		// branchable. cloneSessionItem previously errored on error items,
		// breaking branch/fork for any failed session.
		parent := &Session{
			Messages: []Item{
				NewMessageItem(UserMessage("hi")),
				NewErrorItem(&Error{Message: "boom", Code: "model_error"}),
				NewMessageItem(UserMessage("again")),
			},
		}

		branched, err := BranchSession(parent, 3)
		require.NoError(t, err)
		require.Len(t, branched.Messages, 3)
		require.True(t, branched.Messages[1].IsError())
		assert.Equal(t, "boom", branched.Messages[1].Error.Message)

		// Deep copy: mutating the branch must not touch the parent.
		branched.Messages[1].Error.Message = "mutated"
		assert.Equal(t, "boom", parent.Messages[1].Error.Message)
	})

	t.Run("valid branch copies messages up to position", func(t *testing.T) {
		t.Parallel()
		parent := &Session{
			ID:    "parent-id",
			Title: "Parent Title",
			Messages: []Item{
				NewMessageItem(UserMessage("msg1")),
				NewMessageItem(UserMessage("msg2")),
				NewMessageItem(UserMessage("msg3")),
			},
		}

		branched, err := BranchSession(parent, 2)
		require.NoError(t, err)
		assert.NotNil(t, branched)

		assert.NotEqual(t, parent.ID, branched.ID)
		assert.Equal(t, "Parent Title (branched)", branched.Title)
		assert.Len(t, branched.Messages, 2)
		assert.Equal(t, "msg1", branched.Messages[0].Message.Message.Content)
		assert.Equal(t, "msg2", branched.Messages[1].Message.Message.Content)
	})

	t.Run("preserves the safety policy", func(t *testing.T) {
		t.Parallel()
		parent := &Session{
			SafetyPolicy: SafetyPolicyBalanced,
			Messages:     []Item{NewMessageItem(UserMessage("hi"))},
		}

		branched, err := BranchSession(parent, 1)
		require.NoError(t, err)
		assert.Equal(t, SafetyPolicyBalanced, branched.SafetyPolicy)
		assert.Equal(t, SafetyPolicyBalanced, branched.GetSafetyPolicy())
	})

	t.Run("preserves the yolo-toggle memory", func(t *testing.T) {
		t.Parallel()
		parent := New(WithSafetyPolicy(SafetyPolicyBalanced))
		parent.ToggleYolo() // autonomous, remembering balanced
		parent.AddMessage(UserMessage("hi"))

		branched, err := BranchSession(parent, 1)
		require.NoError(t, err)
		assert.Equal(t, SafetyPolicyAutonomous, branched.GetSafetyPolicy())

		branched.ToggleYolo()
		assert.Equal(t, SafetyPolicyBalanced, branched.GetSafetyPolicy(),
			"the branch must keep the parent's toggle-back destination")
	})

	t.Run("deep-copies pointer fields in MultiContent", func(t *testing.T) {
		t.Parallel()
		// Regression test: MultiContent pointer fields must not be shared
		// between branched sessions and their parents.
		parent := &Session{
			Messages: []Item{NewMessageItem(&Message{
				Message: chat.Message{
					Role: chat.MessageRoleUser,
					MultiContent: []chat.MessagePart{
						{
							Type:     chat.MessagePartTypeImageURL,
							ImageURL: &chat.MessageImageURL{URL: "http://parent"},
						},
						{
							Type: chat.MessagePartTypeFile,
							File: &chat.MessageFile{Path: "/tmp/parent.txt", MimeType: "text/plain"},
						},
						{
							Type: chat.MessagePartTypeDocument,
							Document: &chat.Document{
								Name:     "parent.pdf",
								MimeType: "application/pdf",
								Source:   chat.DocumentSource{InlineData: []byte("parent")},
							},
						},
					},
				},
			})},
		}

		branched, err := BranchSession(parent, 1)
		require.NoError(t, err)
		require.Len(t, branched.Messages, 1)

		parts := branched.Messages[0].Message.Message.MultiContent
		require.Len(t, parts, 3)

		// Mutate branched copies; parent must be unaffected.
		parts[0].ImageURL.URL = "http://branched"
		parts[1].File.Path = "/tmp/branched.txt"
		parts[2].Document.Name = "branched.pdf"
		parts[2].Document.Source.InlineData[0] = 'B'

		parentParts := parent.Messages[0].Message.Message.MultiContent
		assert.Equal(t, "http://parent", parentParts[0].ImageURL.URL, "ImageURL must be deep-copied")
		assert.Equal(t, "/tmp/parent.txt", parentParts[1].File.Path, "File must be deep-copied")
		assert.Equal(t, "parent.pdf", parentParts[2].Document.Name, "Document must be deep-copied")
		assert.Equal(t, []byte("parent"), parentParts[2].Document.Source.InlineData, "Document InlineData must be deep-copied")
	})
}

func TestForkSession(t *testing.T) {
	t.Parallel()
	t.Run("nil parent returns error", func(t *testing.T) {
		t.Parallel()
		_, err := ForkSession(nil, 0)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "parent session is nil")
	})

	t.Run("out-of-range position returns error", func(t *testing.T) {
		t.Parallel()
		parent := &Session{Messages: []Item{NewMessageItem(UserMessage("test"))}}
		_, err := ForkSession(parent, 2)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "out of range")
	})

	t.Run("copies messages up to position and uses fork-numbered title", func(t *testing.T) {
		t.Parallel()
		parent := &Session{
			ID:    "parent-id",
			Title: "Parent Title",
			Messages: []Item{
				NewMessageItem(UserMessage("msg1")),
				NewMessageItem(UserMessage("msg2")),
				NewMessageItem(UserMessage("msg3")),
			},
		}

		forked, err := ForkSession(parent, 2)
		require.NoError(t, err)
		assert.NotEqual(t, parent.ID, forked.ID)
		assert.Equal(t, "Parent Title (fork 1)", forked.Title)
		assert.Len(t, forked.Messages, 2)
		assert.Equal(t, "msg1", forked.Messages[0].Message.Message.Content)
		assert.Equal(t, "msg2", forked.Messages[1].Message.Message.Content)
	})

	t.Run("fork of a fork increments the counter", func(t *testing.T) {
		t.Parallel()
		parent := &Session{
			Title:    "Parent Title (fork 2)",
			Messages: []Item{NewMessageItem(UserMessage("hi"))},
		}

		forked, err := ForkSession(parent, 1)
		require.NoError(t, err)
		assert.Equal(t, "Parent Title (fork 3)", forked.Title)
	})

	t.Run("position zero produces an empty fork", func(t *testing.T) {
		t.Parallel()
		parent := &Session{
			Title:    "Parent Title",
			Messages: []Item{NewMessageItem(UserMessage("only"))},
		}

		forked, err := ForkSession(parent, 0)
		require.NoError(t, err)
		assert.Empty(t, forked.Messages)
		assert.Equal(t, "Parent Title (fork 1)", forked.Title)
	})

	t.Run("copies safety-rail limits onto the fork", func(t *testing.T) {
		t.Parallel()
		// Regression: MaxConsecutiveToolCalls / MaxOldToolCallTokens used to
		// be silently zeroed out, making the fork behave differently from
		// the parent (tool-call cutoff and old-tool-call truncation budget
		// would reset to "use built-in default") despite the user having
		// configured them deliberately. MaxToolResultTokens rides the same
		// rail.
		parent := &Session{
			Title:                   "Parent Title",
			Messages:                []Item{NewMessageItem(UserMessage("hi"))},
			MaxIterations:           42,
			MaxConsecutiveToolCalls: 17,
			MaxOldToolCallTokens:    9000,
			MaxToolResultTokens:     4000,
		}

		forked, err := ForkSession(parent, 1)
		require.NoError(t, err)
		assert.Equal(t, 42, forked.MaxIterations)
		assert.Equal(t, 17, forked.MaxConsecutiveToolCalls)
		assert.Equal(t, 9000, forked.MaxOldToolCallTokens)
		assert.Equal(t, 4000, forked.MaxToolResultTokens)
	})
}

// TestBranchSession_LiveSubSessionBecomesEmbedded pins sub-session cost
// provenance across a branch/fork. On the parent, a child attached live via
// AddLiveSubSession is excluded from EmbeddedSubSessionCost because it
// reported its own cost through its own TokenUsageEvents. A branched/forked
// session is newly minted: its sub-session items are embedded history that
// will never emit events, so the same child's cost must fold into the new
// session's own usage reporting (own + embedded, the accounting SessionUsage
// emits).
func TestBranchSession_LiveSubSessionBecomesEmbedded(t *testing.T) {
	t.Parallel()

	parent := New()
	parent.AddMessage(UserMessage("hi"))
	child := New(WithParentID(parent.ID))
	child.AddMessage(&Message{Message: chat.Message{Role: chat.MessageRoleAssistant, Content: "done", Cost: 0.05}})
	parent.AddLiveSubSession(child)
	require.Zero(t, parent.EmbeddedSubSessionCost(), "the live-attached child is excluded on the parent")

	tests := []struct {
		name string
		mint func(*Session, int) (*Session, error)
	}{
		{name: "branch", mint: BranchSession},
		{name: "fork", mint: ForkSession},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			minted, err := tt.mint(parent, 2)
			require.NoError(t, err)
			require.Len(t, minted.Messages, 2)
			require.True(t, minted.Messages[1].IsSubSession())

			assert.InDelta(t, 0.05, minted.EmbeddedSubSessionCost(), 1e-9,
				"the minted session embeds the child, so its cost is included")
			assert.InDelta(t, 0.05, minted.OwnCost()+minted.EmbeddedSubSessionCost(), 1e-9,
				"own + embedded — what SessionUsage emits — must carry the child's cost")
		})
	}
}
