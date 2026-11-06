package runtime

import (
	"context"
	"time"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/runtime/artifacts"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/workspacemedia"
)

func (r *LocalRuntime) artifacts() *artifacts.Service {
	r.artifactsOnce.Do(func() {
		now := r.now
		if now == nil {
			now = time.Now
		}
		r.artifactService = &artifacts.Service{Store: r.sessionStore, Now: now, Write: workspacemedia.Write}
	})
	return r.artifactService
}

// ResolveGeneratedFile reads an artifact only after revalidating its manifest.
func (r *LocalRuntime) ResolveGeneratedFile(ctx context.Context, ref artifacts.GeneratedFileRef) (*artifacts.ResolvedGeneratedFile, error) {
	return r.artifacts().ResolveGeneratedFile(ctx, ref)
}

func (r *LocalRuntime) materializeGeneratedMedia(ctx context.Context, sess *session.Session, media []chat.MediaDelta, agentName string, events EventSink) []chat.MessagePart {
	var warn func(string)
	if events != nil {
		warn = func(message string) { events.Emit(Warning(message, agentName)) }
	}
	return r.artifacts().Materialize(ctx, sess, media, agentName, warn)
}
