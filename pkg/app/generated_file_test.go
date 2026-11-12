package app

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/runtime/artifacts"
	"github.com/docker/docker-agent/pkg/session"
)

// resolvingRuntime is mockRuntime plus the generated-file resolver
// capability (see generatedFileResolver), recording the refs it was asked
// to resolve.
type resolvingRuntime struct {
	mockRuntime

	refs     []artifacts.GeneratedFileRef
	resolved *artifacts.ResolvedGeneratedFile
	err      error
}

func (r *resolvingRuntime) ResolveGeneratedFile(_ context.Context, ref artifacts.GeneratedFileRef) (*artifacts.ResolvedGeneratedFile, error) {
	r.refs = append(r.refs, ref)
	return r.resolved, r.err
}

func TestApp_ResolveGeneratedFile_ForwardsToCapableRuntime(t *testing.T) {
	t.Parallel()
	rt := &resolvingRuntime{resolved: &artifacts.ResolvedGeneratedFile{Data: []byte("png"), Path: "/ws/cat.png"}}
	app := New(t.Context(), rt, session.New())
	ref := artifacts.GeneratedFileRef{OwnerSessionID: "sess", Root: chat.ArtifactRootWorkspace, Path: "cat.png"}

	assert.True(t, app.CanResolveGeneratedFiles())
	resolved, err := app.ResolveGeneratedFile(t.Context(), ref)

	require.NoError(t, err)
	assert.Equal(t, rt.resolved, resolved)
	assert.Equal(t, []artifacts.GeneratedFileRef{ref}, rt.refs)
}

// TestApp_ResolveGeneratedFile_UnsupportedWithoutCapability pins the
// remote-runtime shape: a runtime without the resolver capability reports
// it upfront and resolution fails with runtime.ErrUnsupported.
func TestApp_ResolveGeneratedFile_UnsupportedWithoutCapability(t *testing.T) {
	t.Parallel()
	app := New(t.Context(), &mockRuntime{}, session.New())

	assert.False(t, app.CanResolveGeneratedFiles())
	_, err := app.ResolveGeneratedFile(t.Context(), artifacts.GeneratedFileRef{
		OwnerSessionID: "sess", Root: chat.ArtifactRootWorkspace, Path: "cat.png",
	})
	assert.ErrorIs(t, err, runtime.ErrUnsupported)
}
