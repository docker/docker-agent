// A very, very basic chat client for `docker agent serve chat`.
//
// `docker agent serve chat` exposes any docker-agent agent through an
// OpenAI-compatible HTTP server. This example runs an interactive REPL
// against it using oaistream's net/http client.
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
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/docker/docker-agent/pkg/model/provider/oaistream"
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
	client := oaistream.NewClient(oaistream.ClientConfig{
		BaseURL: baseURL,
		APIKey:  "not-needed",
	})

	if model == "" {
		picked, err := pickDefaultModel(ctx, client)
		if err != nil {
			return fmt.Errorf("listing models: %w", err)
		}
		model = picked
	}
	fmt.Printf("Connected to %s — chatting with %q. Type \"exit\" to quit.\n", baseURL, model)

	var history []oaistream.ChatMessage

	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for {
		fmt.Print("\n> ")
		if !in.Scan() {
			if err := in.Err(); err != nil {
				return err
			}
			fmt.Println()
			return nil
		}
		userInput := strings.TrimSpace(in.Text())
		if userInput == "" {
			continue
		}
		if userInput == "exit" || userInput == "quit" {
			return nil
		}

		history = append(history, oaistream.UserMessage(userInput))

		reply, err := chat(ctx, client, model, history, stream)
		if err != nil {
			return err
		}
		history = append(history, oaistream.ChatMessage{Role: "assistant", Content: new(reply)})
	}
}

func pickDefaultModel(ctx context.Context, client *oaistream.Client) (string, error) {
	var page struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := client.DoJSON(ctx, http.MethodGet, "/models", nil, nil, nil, &page); err != nil {
		return "", err
	}
	if len(page.Data) == 0 {
		return "", errors.New("server exposes no models")
	}
	return page.Data[0].ID, nil
}

func chat(
	ctx context.Context,
	client *oaistream.Client,
	model string,
	history []oaistream.ChatMessage,
	stream bool,
) (string, error) {
	params := oaistream.ChatCompletionRequest{
		Model:    model,
		Messages: history,
	}

	if !stream {
		resp, err := client.CreateChatCompletion(ctx, params)
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

	s := client.NewChatCompletionStream(ctx, params)
	defer s.Close()
	var b strings.Builder
	for s.Next() {
		chunk := s.Current()
		if len(chunk.Choices) == 0 {
			continue
		}
		delta := chunk.Choices[0].Delta.Content
		if delta != "" {
			fmt.Print(delta)
			b.WriteString(delta)
		}
	}
	if err := s.Err(); err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	fmt.Println()
	return b.String(), nil
}
