package lean

import (
	"context"

	appcore "github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
)

func Run(ctx context.Context, rt runtime.Runtime, sess *session.Session, opts Options) error {
	appOpts := []appcore.Opt{}
	if gen := rt.TitleGenerator(); gen != nil {
		appOpts = append(appOpts, appcore.WithTitleGenerator(gen))
	}
	a := appcore.New(ctx, rt, sess, appOpts...)
	model := NewApp(a, DescriptorFromState(rt, sess), opts)
	return model.Run()
}
