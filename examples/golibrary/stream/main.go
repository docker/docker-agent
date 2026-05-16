package main

import (
	"context"
	"log"
	"os/signal"
	"syscall"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/model/provider/openai"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if err := run(ctx); err != nil {
		log.Println(err)
	}
}

func run(ctx context.Context) error {
	llm, err := openai.NewClient(
		ctx,
		&latest.ModelConfig{
			Provider: "openai",
			Model:    "gpt-4o",
		},
		environment.NewDefaultProvider(),
	)
	if err != nil {
		return err
	}

	human := agent.New(
		"root",
		"You are a human, with feelings and emotions.",
		agent.WithModel(llm),
		agent.WithDescription("A human."),
	)

	humanTeam := team.New(team.WithAgents(human))

	rt, err := runtime.New(humanTeam)
	if err != nil {
		return err
	}

	sess := session.New(session.WithUserMessage("How are you doing?"))

	events := rt.RunStream(ctx, sess)
	for event := range events {
		switch e := event.(type) {
		case *runtime.AgentChoiceEvent:
			log.Printf("Agent %s: %s\n", e.AgentName, e.Content)
		case *runtime.StreamStartedEvent:
			log.Println("Stream started for session")
		case *runtime.StreamStoppedEvent:
			log.Println("Stream stopped for session")
		case *runtime.ToolCallConfirmationEvent:
			rt.Resume(ctx, runtime.ResumeRequest{Type: runtime.ResumeTypeApproveSession})
		case *runtime.ToolCallEvent:
			log.Printf("Tool call: %s\n", e.ToolCall.Function.Name)
		case *runtime.ToolCallResponseEvent:
			log.Printf("Tool call response: %s\n", e.Response)
			// etc...
		}
	}

	return nil
}
