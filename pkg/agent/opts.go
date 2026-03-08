package agent

import (
	"time"

	"github.com/docker/cagent/pkg/config/latest"
	"github.com/docker/cagent/pkg/config/types"
	"github.com/docker/cagent/pkg/model/provider"
	"github.com/docker/cagent/pkg/tools"
)

type Opt func(a *Agent)

func WithInstruction(instruction string) Opt {
	return func(a *Agent) {
		a.instruction = instruction
	}
}

func WithToolSets(toolSet ...tools.ToolSet) Opt {
	var startableToolSet []*tools.StartableToolSet
	for _, ts := range toolSet {
		startableToolSet = append(startableToolSet, tools.NewStartable(ts))
	}

	return func(a *Agent) {
		a.toolsets = startableToolSet
	}
}

func WithTools(allTools ...tools.Tool) Opt {
	return func(a *Agent) {
		a.tools = allTools
	}
}

func WithDescription(description string) Opt {
	return func(a *Agent) {
		a.description = description
	}
}

func WithWelcomeMessage(welcomeMessage string) Opt {
	return func(a *Agent) {
		a.welcomeMessage = welcomeMessage
	}
}

func WithName(name string) Opt {
	return func(a *Agent) {
		a.name = name
	}
}

func WithModel(model provider.Provider) Opt {
	return func(a *Agent) {
		a.models = append(a.models, model)
	}
}

// WithFallbackModel adds a fallback model to try if the primary model fails.
// For retryable errors (5xx, timeouts), the same model is retried with backoff.
// For non-retryable errors (429), we immediately move to the next model in the chain.
func WithFallbackModel(model provider.Provider) Opt {
	return func(a *Agent) {
		a.fallbackModels = append(a.fallbackModels, model)
	}
}

// WithFallbackRetries sets the number of retries per fallback model with exponential backoff.
func WithFallbackRetries(retries int) Opt {
	return func(a *Agent) {
		a.fallbackRetries = retries
	}
}

// WithFallbackCooldown sets the duration to stick with a successful fallback model
// before retrying the primary. Only applies after a non-retryable error (e.g., 429).
func WithFallbackCooldown(cooldown time.Duration) Opt {
	return func(a *Agent) {
		a.fallbackCooldown = cooldown
	}
}

func WithSubAgents(subAgents ...*Agent) Opt {
	return func(a *Agent) {
		a.subAgents = subAgents
		for _, subAgent := range subAgents {
			subAgent.parents = append(subAgent.parents, a)
		}
	}
}

func WithHandoffs(handoffs ...*Agent) Opt {
	return func(a *Agent) {
		a.handoffs = handoffs
	}
}

func WithAddDate(addDate bool) Opt {
	return func(a *Agent) {
		a.addDate = addDate
	}
}

func WithAddEnvironmentInfo(addEnvironmentInfo bool) Opt {
	return func(a *Agent) {
		a.addEnvironmentInfo = addEnvironmentInfo
	}
}

func WithAddDescriptionParameter(addDescriptionParameter bool) Opt {
	return func(a *Agent) {
		a.addDescriptionParameter = addDescriptionParameter
	}
}

func WithAddPromptFiles(addPromptFiles []string) Opt {
	return func(a *Agent) {
		a.addPromptFiles = addPromptFiles
	}
}

func WithMaxIterations(maxIterations int) Opt {
	return func(a *Agent) {
		a.maxIterations = maxIterations
	}
}

func WithNumHistoryItems(numHistoryItems int) Opt {
	return func(a *Agent) {
		a.numHistoryItems = numHistoryItems
	}
}

func WithCommands(commands types.Commands) Opt {
	return func(a *Agent) {
		a.commands = commands
	}
}

func WithLoadTimeWarnings(warnings []string) Opt {
	return func(a *Agent) {
		for _, w := range warnings {
			a.addToolWarning(w)
		}
	}
}

func WithHooks(hooks *latest.HooksConfig) Opt {
	return func(a *Agent) {
		a.hooks = hooks
	}
}

// WithThinkingConfigured sets whether thinking_budget was explicitly configured in the agent's YAML.
// When true, the session will initialize with thinking enabled.
func WithThinkingConfigured(configured bool) Opt {
	return func(a *Agent) {
		a.thinkingConfigured = configured
	}
}

// WithNoFallbackStatusCodes sets the HTTP status codes for which the fallback
// chain should be skipped entirely when using a models gateway.
func WithNoFallbackStatusCodes(codes []int) Opt {
	return func(a *Agent) {
		a.noFallbackStatusCodes = codes
	}
}
