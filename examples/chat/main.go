// A very, very basic chat client for `docker agent serve chat`.
//
// PR #2510 (`feat: add docker agent serve chat command`) exposes any
// docker-agent agent through an OpenAI-compatible HTTP server. The whole
// point of that feature is that any tool already speaking OpenAI's
// /v1/chat/completions protocol can drive a docker-agent agent without
// custom integration. This example demonstrates exactly that: it uses the
// official github.com/openai/openai-go SDK, only repointed at the local
// chat server, to run an interactive REPL against an agent.
//
// Prerequisites:
//
//	# Start an agent in chat mode (in another terminal):
//	./bin/docker-agent serve chat ./examples/42.yaml
//	# It listens on http://127.0.0.1:8083 by default.
//
// Then run this client:
//
//	go run ./examples/chat
//	# or, to pin a specific agent in a multi-agent team:
//	go run ./examples/chat -model root
//	# or, to point at a different server:
//	go run ./examples/chat -base http://127.0.0.1:9090/v1
//
// Type a message and press <Enter>. Type "exit" (or send EOF with ^D) to
// quit.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
)

func main() {
	baseURL := flag.String("base", "http://127.0.0.1:8083/v1", "Base URL of the docker-agent chat server")
	model := flag.String("model", "", "Agent name to talk to (defaults to the team's default agent)")
	stream := flag.Bool("stream", true, "Stream the agent's response token-by-token")
	flag.Parse()

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	err := run(ctx, *baseURL, *model, *stream)
	cancel()
	if err != nil && !errors.Is(err, context.Canceled) {
		log.Fatal(err)
	}
}

func run(ctx context.Context, baseURL, model string, stream bool) error {
	// The chat server doesn't validate API keys, but the OpenAI SDK
	// requires *some* string to be passed.
	client := openai.NewClient(
		option.WithBaseURL(baseURL),
		option.WithAPIKey("not-needed"),
	)

	// Ask the server which agents are exposed and pick a default model
	// when the user didn't pin one. This also doubles as a connectivity
	// check.
	if model == "" {
		picked, err := pickDefaultModel(ctx, &client)
		if err != nil {
			return fmt.Errorf("listing models: %w", err)
		}
		model = picked
	}
	fmt.Printf("Connected to %s — chatting with %q. Type \"exit\" to quit.\n", baseURL, model)

	// History keeps the conversation going across turns. The chat server
	// is stateless: it builds a fresh session per request from whatever
	// messages the client sends, so it's the client's job to remember.
	var history []openai.ChatCompletionMessageParamUnion

	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for {
		fmt.Print("\n> ")
		if !in.Scan() {
			if err := in.Err(); err != nil {
				return err
			}
			fmt.Println()
			return nil // EOF
		}
		userInput := strings.TrimSpace(in.Text())
		if userInput == "" {
			continue
		}
		if userInput == "exit" || userInput == "quit" {
			return nil
		}

		history = append(history, openai.UserMessage(userInput))

		reply, err := chat(ctx, &client, model, history, stream)
		if err != nil {
			return err
		}
		history = append(history, openai.AssistantMessage(reply))
	}
}

// pickDefaultModel queries /v1/models and returns the first agent name
// the server advertises.
func pickDefaultModel(ctx context.Context, client *openai.Client) (string, error) {
	page, err := client.Models.List(ctx)
	if err != nil {
		return "", err
	}
	if len(page.Data) == 0 {
		return "", errors.New("server exposes no models")
	}
	return page.Data[0].ID, nil
}

// chat sends the conversation to the server, prints the assistant's reply
// to stdout (streaming if requested) and returns the final assembled
// content so the caller can append it to the history.
func chat(
	ctx context.Context,
	client *openai.Client,
	model string,
	history []openai.ChatCompletionMessageParamUnion,
	stream bool,
) (string, error) {
	params := openai.ChatCompletionNewParams{
		Model:    model,
		Messages: history,
	}

	if !stream {
		resp, err := client.Chat.Completions.New(ctx, params)
		if err != nil {
			return "", err
		}
		if len(resp.Choices) == 0 {
			return "", errors.New("server returned no choices")
		}
		content := resp.Choices[0].Message.Content
		fmt.Println(content)
		return content, nil
	}

	s := client.Chat.Completions.NewStreaming(ctx, params)
	var b strings.Builder
	for s.Next() {
		chunk := s.Current()
		if len(chunk.Choices) == 0 {
			continue
		}
		delta := chunk.Choices[0].Delta.Content
		if delta == "" {
			continue
		}
		fmt.Print(delta)
		b.WriteString(delta)
	}
	if err := s.Err(); err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	fmt.Println()
	return b.String(), nil
}
