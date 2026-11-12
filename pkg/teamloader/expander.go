package teamloader

import (
	"context"
	"regexp"
	"strings"

	"github.com/docker/docker-agent/pkg/config/types"
	"github.com/docker/docker-agent/pkg/environment"
)

// Expander expands ${...} expressions in agent text: instructions,
// descriptions, welcome messages, toolset instructions and slash commands.
// The default only substitutes ${env.NAME} references and bound values;
// pkg/js provides the full JavaScript evaluator (see WithExpander).
type Expander interface {
	Expand(ctx context.Context, text string, values map[string]string) string
	ExpandCommands(ctx context.Context, cmds types.Commands) types.Commands
}

// WithExpander replaces the default ${...} expander with one built from the
// runtime environment when the team is loaded. Pass js.NewJsExpander to
// enable JavaScript expressions:
//
//	teamloader.WithExpander(js.NewJsExpander)
//
// Slash commands additionally need the runtime evaluator; loaderdefaults.Opts
// registers it via jscommands.Register.
func WithExpander[E Expander](newExpander func(environment.Provider) E) Opt {
	return func(opts *loadOptions) error {
		opts.newExpander = func(env environment.Provider) Expander { return newExpander(env) }
		return nil
	}
}

// envRef matches ${env.NAME} and ${NAME} where NAME is a plain identifier.
var envRef = regexp.MustCompile(`\$\{\s*(env\.)?([A-Za-z_][A-Za-z0-9_]*)\s*\}`)

// EnvExpander is the default Expander: it resolves ${env.NAME} from the
// environment and ${name} from the bound values, and leaves any other
// expression untouched so it stays visible rather than silently vanishing.
type EnvExpander struct {
	env environment.Provider
}

func newEnvExpander(env environment.Provider) Expander {
	return NewEnvExpander(env)
}

func (e EnvExpander) Expand(ctx context.Context, text string, values map[string]string) string {
	if !strings.Contains(text, "${") {
		return text
	}
	return envRef.ReplaceAllStringFunc(text, func(match string) string {
		m := envRef.FindStringSubmatch(match)
		name := m[2]
		if m[1] == "" {
			if v, ok := values[name]; ok {
				return v
			}
			return match
		}
		if e.env != nil {
			if v, ok := e.env.Get(ctx, name); ok {
				return v
			}
		}
		return match
	})
}

func (e EnvExpander) ExpandCommands(ctx context.Context, cmds types.Commands) types.Commands {
	if cmds == nil {
		return nil
	}
	expanded := make(types.Commands, len(cmds))
	for k, cmd := range cmds {
		cmd.Description = e.Expand(ctx, cmd.Description, nil)
		cmd.Instruction = e.Expand(ctx, cmd.Instruction, nil)
		cmd.URL = e.Expand(ctx, cmd.URL, nil)
		expanded[k] = cmd
	}
	return expanded
}

// NewEnvExpander resolves ${env.NAME} and bound ${name} placeholders without
// evaluating JavaScript. Unknown placeholders are left untouched.
func NewEnvExpander(env environment.Provider) *EnvExpander {
	return &EnvExpander{env: env}
}

// ExpandMap expands each value without modifying the input map.
func (e EnvExpander) ExpandMap(ctx context.Context, values map[string]string) map[string]string {
	expanded := make(map[string]string, len(values))
	for key, value := range values {
		expanded[key] = e.Expand(ctx, value, nil)
	}
	return expanded
}
