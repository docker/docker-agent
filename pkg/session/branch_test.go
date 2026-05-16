package session

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
)

func TestGenerateBranchTitle(t *testing.T) {
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
			result := generateBranchTitle(tt.parentTitle)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestCloneSessionItem(t *testing.T) {
	t.Run("empty item returns error", func(t *testing.T) {
		_, err := cloneSessionItem(Item{})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "cannot clone empty session item")
	})

	t.Run("message item clones successfully", func(t *testing.T) {
		item := NewMessageItem(UserMessage("test"))
		cloned, err := cloneSessionItem(item)
		require.NoError(t, err)
		assert.Equal(t, "test", cloned.Message.Message.Content)
	})

	t.Run("summary item clones successfully", func(t *testing.T) {
		item := Item{Summary: "test summary"}
		cloned, err := cloneSessionItem(item)
		require.NoError(t, err)
		assert.Equal(t, "test summary", cloned.Summary)
	})
}

func TestBranchSession(t *testing.T) {
	t.Run("nil parent returns error", func(t *testing.T) {
		_, err := BranchSession(nil, 0)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "parent session is nil")
	})

	t.Run("negative position returns error", func(t *testing.T) {
		parent := &Session{Messages: []Item{NewMessageItem(UserMessage("test"))}}
		_, err := BranchSession(parent, -1)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "out of range")
	})

	t.Run("position beyond messages returns error", func(t *testing.T) {
		parent := &Session{Messages: []Item{NewMessageItem(UserMessage("test"))}}
		_, err := BranchSession(parent, 2)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "out of range")
	})

	t.Run("position equal to messages length no error", func(t *testing.T) {
		parent := &Session{Messages: []Item{NewMessageItem(UserMessage("test"))}}
		_, err := BranchSession(parent, 1)
		require.NoError(t, err)
	})

	t.Run("valid branch copies messages up to position", func(t *testing.T) {
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

	t.Run("deep-copies pointer fields in MultiContent", func(t *testing.T) {
		// Regression test: the previous branch.go cloneChatMessage forgot to
		// duplicate MessagePart.File, leaving branched sessions sharing the
		// pointer with the parent. Mutating either side now must NOT affect
		// the other.
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
					},
				},
			})},
		}

		branched, err := BranchSession(parent, 1)
		require.NoError(t, err)
		require.Len(t, branched.Messages, 1)

		parts := branched.Messages[0].Message.Message.MultiContent
		require.Len(t, parts, 2)

		// Mutate branched copies; parent must be unaffected.
		parts[0].ImageURL.URL = "http://branched"
		parts[1].File.Path = "/tmp/branched.txt"

		parentParts := parent.Messages[0].Message.Message.MultiContent
		assert.Equal(t, "http://parent", parentParts[0].ImageURL.URL, "ImageURL must be deep-copied")
		assert.Equal(t, "/tmp/parent.txt", parentParts[1].File.Path, "File must be deep-copied")
	})
}
