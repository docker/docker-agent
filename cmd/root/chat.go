package root

import (
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/docker/docker-agent/pkg/chatserver"
	"github.com/docker/docker-agent/pkg/cli"
	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/telemetry"
)

type chatFlags struct {
	agentName             string
	listenAddr            string
	corsOrigin            string
	apiKey                string
	apiKeyEnv             string
	maxRequestSize        int64
	requestTimeout        time.Duration
	conversationsMaxItems int
	conversationTTL       time.Duration
	maxIdleRuntimes       int
	runConfig             config.RuntimeConfig
}

func newChatCmd() *cobra.Command {
	var flags chatFlags

	cmd := &cobra.Command{
		Use:   "chat <agent-file>|<registry-ref>",
		Short: "Start an agent as an OpenAI-compatible chat completions server",
		Long: `Start an HTTP server that exposes the agent through an OpenAI-compatible
API at /v1/chat/completions and /v1/models. This lets tools that already
speak OpenAI's chat protocol (such as Open WebUI) drive a docker-agent
agent without any custom integration.`,
		Example: `  docker-agent serve chat ./agent.yaml
  docker-agent serve chat ./team.yaml --agent reviewer
  docker-agent serve chat agentcatalog/pirate --listen 127.0.0.1:9090`,
		Args: cobra.ExactArgs(1),
		RunE: flags.runChatCommand,
	}

	cmd.Flags().StringVarP(&flags.agentName, "agent", "a", "", "Name of the agent to expose (all agents if not specified)")
	cmd.Flags().StringVarP(&flags.listenAddr, "listen", "l", "127.0.0.1:8083", "Address to listen on")
	cmd.Flags().StringVar(&flags.corsOrigin, "cors-origin", "", "Allowed CORS origin (e.g. https://example.com); empty disables CORS entirely")
	cmd.Flags().StringVar(&flags.apiKey, "api-key", "", "Required Bearer token clients must present (Authorization: Bearer <token>); empty disables auth")
	cmd.Flags().StringVar(&flags.apiKeyEnv, "api-key-env", "", "Read the API key from this environment variable instead of the command line")
	cmd.Flags().Int64Var(&flags.maxRequestSize, "max-request-size", 1<<20, "Maximum request body size in bytes (default 1 MiB)")
	cmd.Flags().DurationVar(&flags.requestTimeout, "request-timeout", 5*time.Minute, "Per-request timeout (covers model + tool calls + streaming)")
	cmd.Flags().IntVar(&flags.conversationsMaxItems, "conversations-max", 0, "Cache up to N conversations server-side, keyed by X-Conversation-Id (0 disables; clients must resend full history)")
	cmd.Flags().DurationVar(&flags.conversationTTL, "conversation-ttl", 30*time.Minute, "Idle TTL after which a cached conversation is evicted")
	cmd.Flags().IntVar(&flags.maxIdleRuntimes, "max-idle-runtimes", 4, "Maximum number of idle runtimes pooled per agent (0 disables pooling)")
	addRuntimeConfigFlags(cmd, &flags.runConfig)

	return cmd
}

func (f *chatFlags) runChatCommand(cmd *cobra.Command, args []string) (commandErr error) {
	ctx := cmd.Context()
	telemetry.TrackCommand(ctx, "serve", append([]string{"chat"}, args...))
	defer func() { // do not inline this defer so that commandErr is not resolved early
		telemetry.TrackCommandError(ctx, "serve", append([]string{"chat"}, args...), commandErr)
	}()

	out := cli.NewPrinter(cmd.OutOrStdout())
	agentFilename := args[0]

	ln, cleanup, err := newListener(ctx, f.listenAddr)
	if err != nil {
		return err
	}
	defer cleanup()

	out.Println("Listening on", ln.Addr().String())
	out.Println("OpenAI-compatible chat completions endpoint: http://" + ln.Addr().String() + "/v1/chat/completions")

	apiKey := f.apiKey
	if f.apiKeyEnv != "" {
		if v := os.Getenv(f.apiKeyEnv); v != "" {
			apiKey = v
		}
	}

	return chatserver.Run(ctx, agentFilename, chatserver.Options{
		AgentName:                f.agentName,
		RunConfig:                &f.runConfig,
		CORSOrigin:               f.corsOrigin,
		APIKey:                   apiKey,
		MaxRequestBytes:          f.maxRequestSize,
		RequestTimeout:           f.requestTimeout,
		ConversationsMaxSessions: f.conversationsMaxItems,
		ConversationTTL:          f.conversationTTL,
		MaxIdleRuntimes:          f.maxIdleRuntimes,
	}, ln)
}
