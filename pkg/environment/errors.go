package environment

import (
	"fmt"
	"slices"
	"strings"

	"github.com/docker/docker-agent/pkg/chatgpt"
	"github.com/docker/docker-agent/pkg/cli/invocation"
)

// SecretsDocsURL is the documentation page describing every built-in secret
// source and how to configure it.
const SecretsDocsURL = "https://docs.docker.com/ai/docker-agent/guides/secrets/"

// ModelSetupDocsURL is the step-by-step tutorial for making a model available,
// covering both the cloud API-key path and the local Docker Model Runner path.
const ModelSetupDocsURL = "https://docker.github.io/docker-agent/getting-started/set-up-a-model/"

type RequiredEnvError struct {
	Missing []string

	// MissingModelCredentials reports whether at least one missing variable is
	// a model-provider credential, in which case the error also suggests
	// running a local model instead of configuring an API key.
	MissingModelCredentials bool
}

var _ error = &RequiredEnvError{}

func (e *RequiredEnvError) Error() string {
	var msg strings.Builder
	command := invocation.DockerAgent()

	fmt.Fprintln(&msg, "The following environment variables must be set:")
	for _, v := range e.Missing {
		fmt.Fprintf(&msg, " - %s\n", v)
	}

	example := "OPENAI_API_KEY"
	if len(e.Missing) > 0 {
		example = e.Missing[0]
	}
	msg.WriteString("\n")
	msg.WriteString(SecretSourcesHelp(example))

	if slices.Contains(e.Missing, chatgpt.TokenEnvVar) {
		fmt.Fprintf(&msg, "\n%s is normally supplied by signing in with your ChatGPT account: %s setup\n", chatgpt.TokenEnvVar, command)
	}

	if e.MissingModelCredentials {
		fmt.Fprintf(&msg, "\nNo API key? Run a local model instead: %s run --model dmr/ai/qwen3 ...\n(the model is pulled on first use; `docker model ls` shows models already pulled)\n", command)
		fmt.Fprintf(&msg, "Or run `%s setup` to configure a provider or local model interactively.\n", command)
		fmt.Fprintf(&msg, "Step-by-step model setup (API key or local): %s\n", ModelSetupDocsURL)
	}

	return msg.String()
}

// SecretSourcesHelp returns guidance naming the built-in secret sources able
// to supply arbitrary variables, with a one-line example for the given
// variable and a link to the docs. Docker Desktop and the credential helper
// are deliberately absent: they only resolve fixed Docker variables
// (DOCKER_TOKEN, ...) and can never satisfy the keys reported missing here.
// Shared by the errors that report missing credentials so the advice never
// drifts between them.
func SecretSourcesHelp(exampleVar string) string {
	var b strings.Builder
	command := invocation.DockerAgent()
	b.WriteString("Provide them using any of these sources:\n")
	fmt.Fprintf(&b, " - Shell environment:      export %s=<value>\n", exampleVar)
	fmt.Fprintf(&b, " - Env file:               %s run --env-from-file <file> ...\n", command)
	fmt.Fprintf(&b, " - Docker Agent env file:  %s setup (stores the key in ~/.config/cagent/.env)\n", command)
	fmt.Fprintf(&b, "\nSee %s for details.\n", SecretsDocsURL)
	return b.String()
}
