package runtime

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/workspacemedia"
)

func TestMaterializeGeneratedMedia_RedirectsEscapingPaths(t *testing.T) {
	tests := []struct{ name, requested, want string }{
		{"posix absolute", "/outside/cat.jpg", "cat.png"},
		{"windows absolute", `C:\outside\cat.jpg`, "cat.png"},
		{"windows UNC", `\\server\share\cat.jpg`, "cat.png"},
		{"windows drive relative", `C:outside\cat.jpg`, "cat.png"},
		{"posix traversal", "../../outside/cat.jpg", "cat.png"},
		{"windows traversal", `..\..\outside\cat.jpg`, "cat.png"},
		{"home", "~/outside/cat.jpg", "cat.png"},
		{"reserved basename", "/outside/CON.png", "generated-1.png"},
		{"control basename", "/outside/bad\nname.jpg", "bad-name.png"},
		{"invalid UTF-8 basename", "/outside/bad" + string([]byte{0xff}) + ".jpg", "generated-1.png"},
		{"overlong basename", "/outside/" + strings.Repeat("x", 300) + ".png", "generated-1.png"},
		{"unusable basename", "../..", "generated-1.png"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, store, _ := newMediaTestRuntime(t)
			sess, root := workspaceSession(t, "redirect-"+tt.name)
			sink := &collectingSink{}
			parts := r.materializeGeneratedMedia(t.Context(), sess, []chat.MediaDelta{{Data: []byte("png"), MimeType: "image/png", Name: "provider.png", RequestedPath: tt.requested, Size: 3}}, "root", sink)
			require.Len(t, parts, 1)
			doc := parts[0].Document
			require.NotNil(t, doc)
			assert.Equal(t, tt.want, doc.Source.ArtifactPath)
			assert.Equal(t, chat.ArtifactRootWorkspace, doc.Source.ArtifactRoot)
			assert.Equal(t, []byte("png"), mustReadFile(t, filepath.Join(root, filepath.FromSlash(tt.want))))
			_, err := manifestOf(t, store).LookupGeneratedFile(t.Context(), sess.ID, tt.want)
			require.NoError(t, err)
			warnings := sink.warnings()
			require.NotEmpty(t, warnings)
			for _, warning := range warnings {
				assert.NotContains(t, warning.Message, tt.requested)
				assert.LessOrEqual(t, len(warning.Message), maxPlaceholderOrWarningBytes)
			}
		})
	}
}

func TestMaterializeGeneratedMedia_RedirectsSymlinkEscapeAndAvoidsCollision(t *testing.T) {
	requireSymlinkSupport(t)
	r, _, _ := newMediaTestRuntime(t)
	sess, root := workspaceSession(t, "redirect-symlink")
	outside := t.TempDir()
	require.NoError(t, os.Symlink(outside, filepath.Join(root, "link")))
	require.NoError(t, os.WriteFile(filepath.Join(root, "cat.png"), []byte("existing"), 0o644))
	parts := r.materializeGeneratedMedia(t.Context(), sess, []chat.MediaDelta{{Data: []byte("new"), MimeType: "image/png", RequestedPath: "link/cat.png", Size: 3}}, "root", &collectingSink{})
	require.Len(t, parts, 1)
	assert.Equal(t, "cat-1.png", parts[0].Document.Source.ArtifactPath)
	assert.Equal(t, []byte("existing"), mustReadFile(t, filepath.Join(root, "cat.png")))
	assert.Equal(t, []byte("new"), mustReadFile(t, filepath.Join(root, "cat-1.png")))
	entries, err := os.ReadDir(outside)
	require.NoError(t, err)
	assert.Empty(t, entries)
}

func requireSymlinkSupport(t *testing.T) {
	t.Helper()
	root := t.TempDir()
	err := os.Symlink("target", filepath.Join(root, "probe"))
	if err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
}

func TestRunStream_EscapingGeneratedMediaCompletesWithoutElicitation(t *testing.T) {
	for _, nonInteractive := range []bool{false, true} {
		t.Run(fmt.Sprintf("nonInteractive=%t", nonInteractive), func(t *testing.T) {
			workspace := t.TempDir()
			external := filepath.Join(t.TempDir(), "cat.png")
			store := session.NewInMemorySessionStore()
			stream := &mockStream{responses: []chat.MessageStreamResponse{
				{Choices: []chat.MessageStreamChoice{{Index: 0, Delta: chat.MessageDelta{Content: "completed", Media: []chat.MediaDelta{{Data: []byte("png"), MimeType: "image/png", Name: "provider.png", RequestedPath: external, Size: 3}}}}}},
				{Choices: []chat.MessageStreamChoice{{Index: 0, FinishReason: chat.FinishReasonStop}}, Usage: &chat.Usage{InputTokens: 1, OutputTokens: 1}},
			}}
			root := agent.New("root", "instructions", agent.WithModel(&mockProvider{id: "test/media", stream: stream}))
			rt, err := New(t.Context(), team.New(team.WithAgents(root)), WithSessionCompaction(false), WithSessionStore(store))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, rt.Close()) })
			rt.OnElicitationRequest(func(Event) { t.Error("generated media escape must not elicit") })
			opts := []session.Opt{session.WithUserMessage("save the generated image")}
			if nonInteractive {
				opts = append(opts, session.WithNonInteractive(true))
			}
			sess := session.New(opts...)
			sess.ID = "media-escape"
			sess.WorkingDir = workspace
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			var warnings int
			var stopped bool
			for event := range rt.RunStream(ctx, sess) {
				switch event.(type) {
				case *WarningEvent:
					warnings++
				case *StreamStoppedEvent:
					stopped = true
				}
			}
			require.NoError(t, ctx.Err())
			assert.True(t, stopped)
			assert.Positive(t, warnings)
			assert.NoFileExists(t, external)
			assert.Equal(t, []byte("png"), mustReadFile(t, filepath.Join(workspace, "cat.png")))
			stored, err := store.GetSession(t.Context(), sess.ID)
			require.NoError(t, err)
			messages := stored.GetAllMessages()
			require.Len(t, messages, 2)
			assistant := messages[1].Message
			assert.Equal(t, "completed", assistant.Content)
			require.Len(t, assistant.MultiContent, 2)
			document := assistant.MultiContent[1].Document
			require.NotNil(t, document)
			assert.Equal(t, "cat.png", document.Source.ArtifactPath)
		})
	}
}

func TestMaterializeGeneratedMedia_RedirectFailurePreservesSibling(t *testing.T) {
	r, _, _ := newMediaTestRuntime(t)
	sess, root := workspaceSession(t, "redirect-partial")
	original := workspacemediaWrite
	workspacemediaWrite = func(workspaceRoot, requested string, data []byte, mimeType string) (workspacemedia.Result, error) {
		if requested == "bad.png" {
			return workspacemedia.Result{}, os.ErrPermission
		}
		return original(workspaceRoot, requested, data, mimeType)
	}
	t.Cleanup(func() { workspacemediaWrite = original })
	parts := r.materializeGeneratedMedia(t.Context(), sess, []chat.MediaDelta{
		{Data: []byte("bad"), MimeType: "image/png", RequestedPath: "/outside/bad.png", Size: 3},
		{Data: []byte("good"), MimeType: "image/png", RequestedPath: "nested/good.png", Size: 4},
	}, "root", &collectingSink{})
	require.Len(t, parts, 1)
	assert.Equal(t, "nested/good.png", parts[0].Document.Source.ArtifactPath)
	assert.Equal(t, []byte("good"), mustReadFile(t, filepath.Join(root, "nested", "good.png")))
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	return b
}
