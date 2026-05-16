package root

import (
	"context"
	"os"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/spf13/cobra"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/creator"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/telemetry"
	"github.com/docker/docker-agent/pkg/tui"
	tuiinput "github.com/docker/docker-agent/pkg/tui/input"
)

type newFlags struct {
	modelParam         string
	maxIterationsParam int
	runConfig          config.RuntimeConfig
}

func newNewCmd() *cobra.Command {
	var flags newFlags

	cmd := &cobra.Command{
		Use:   "new [description]",
		Short: "Create a new agent configuration",
		Long: `Create a new agent configuration interactively.

The agent builder will ask questions about what you want the agent to do,
then generate a YAML configuration file you can use with 'docker-agent run'.

Optionally provide a description as an argument to skip the initial prompt.`,
		Example: `  docker-agent new
  docker-agent new "a web scraper that extracts product prices"
  docker-agent new --model openai/gpt-4o "a code reviewer agent"`,
		GroupID: "core",
		RunE:    flags.runNewCommand,
	}

	cmd.PersistentFlags().StringVar(&flags.modelParam, "model", "", "Model to use, optionally as provider/model where provider is one of: anthropic, openai, google, dmr. If omitted, provider is auto-selected based on available credentials or gateway")
	cmd.PersistentFlags().IntVar(&flags.maxIterationsParam, "max-iterations", 0, "Maximum number of agentic loop iterations to prevent infinite loops (default: 20 for DMR, unlimited for other providers)")
	addRuntimeConfigFlags(cmd, &flags.runConfig)

	return cmd
}

func (f *newFlags) runNewCommand(cmd *cobra.Command, args []string) (commandErr error) {
	ctx := cmd.Context()
	telemetry.TrackCommand(ctx, "new", args)
	defer func() { // do not inline this defer so that commandErr is not resolved early
		telemetry.TrackCommandError(ctx, "new", args, commandErr)
	}()

	t, err := creator.Agent(ctx, &f.runConfig, f.modelParam)
	if err != nil {
		return err
	}
	defer stopToolSets(t)

	rt, err := runtime.New(t)
	if err != nil {
		return err
	}

	var appOpts []app.Opt
	sessOpts := []session.Opt{
		session.WithTitle("New agent"),
		session.WithMaxIterations(f.maxIterationsParam),
		session.WithToolsApproved(true),
	}
	if len(args) > 0 {
		arg := strings.Join(args, " ")
		sessOpts = append(sessOpts, session.WithUserMessage(arg))
		appOpts = append(appOpts, app.WithFirstMessage(arg))
	}

	sess := session.New(sessOpts...)

	return runTUI(ctx, rt, sess, nil, nil, nil, appOpts...)
}

func runTUI(ctx context.Context, rt runtime.Runtime, sess *session.Session, spawner tui.SessionSpawner, cleanup func(), tuiOpts []tui.Option, opts ...app.Opt) error {
	if gen := rt.TitleGenerator(); gen != nil {
		opts = append(opts, app.WithTitleGenerator(gen))
	}

	a := app.New(ctx, rt, sess, opts...)

	coalescer := tuiinput.NewWheelCoalescer()
	filter := func(model tea.Model, msg tea.Msg) tea.Msg {
		wheelMsg, ok := msg.(tea.MouseWheelMsg)
		if !ok {
			return msg
		}
		if coalescer.Handle(wheelMsg) {
			return nil
		}
		return msg
	}

	if cleanup == nil {
		cleanup = func() {}
	}
	wd, _ := os.Getwd()
	model := tui.New(ctx, spawner, a, wd, cleanup, tuiOpts...)

	p := tea.NewProgram(model, tea.WithContext(ctx), tea.WithFilter(filter))
	coalescer.SetSender(p.Send)

	if m, ok := model.(interface{ SetProgram(p *tea.Program) }); ok {
		m.SetProgram(p)
	}

	_, err := p.Run()
	return err
}
