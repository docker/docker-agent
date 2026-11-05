package teamloader

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/legacy"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/modelsdev"
)

func TestConfigVersionsAndExternalInheritance(t *testing.T) {
	t.Parallel()
	const agent = "agents:\n  root:\n    model: anthropic/test-model\n    instruction: hello\n"
	for _, external := range []bool{false, true} {
		for _, tc := range []struct {
			name       string
			opts       []Opt
			wantLegacy bool
		}{
			{name: "default"},
			{name: "nil", opts: []Opt{WithConfigVersions(nil)}},
			{name: "legacy", opts: []Opt{WithConfigVersions(legacy.Register)}, wantLegacy: true},
			{name: "reset", opts: []Opt{WithConfigVersions(legacy.Register), WithConfigVersions(nil)}},
			{name: "enable", opts: []Opt{WithConfigVersions(nil), WithConfigVersions(legacy.Register)}, wantLegacy: true},
		} {
			t.Run(fmt.Sprintf("external=%t/%s", external, tc.name), func(t *testing.T) {
				t.Parallel()
				root := "version: '2'\n" + agent
				if external {
					root = agent + "    sub_agents: [child:example/child]\n"
				}
				var refs []string
				resolve := WithSourceResolver(func(ref string, _ environment.Provider) (config.Source, error) {
					refs = append(refs, ref)
					data := "version: '2'\n" + agent
					if ref == "example/child" {
						data += "    sub_agents: [grandchild:example/grandchild]\n"
					}
					return config.NewBytesSource(ref, []byte(data)), nil
				})
				rc := strictRunConfig()
				rc.ModelsDevStoreOverride = modelsdev.NewDatabaseStore(&modelsdev.Database{})
				opts := strictTestOpts(resolve)
				opts = append(opts, tc.opts...)
				loaded, err := Load(t.Context(), config.NewBytesSource("root.yaml", []byte(root)), rc, opts...)
				if !tc.wantLegacy {
					require.ErrorContains(t, err, "unsupported config version: 2")
					if external {
						assert.Equal(t, []string{"example/child"}, refs)
					} else {
						assert.Empty(t, refs)
					}
					return
				}
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, loaded.Dispose(t.Context())) })
				if external {
					assert.Equal(t, []string{"example/child", "example/grandchild"}, refs)
					_, err := loaded.Agent("child")
					require.NoError(t, err)
				}
			})
		}
	}
}
