package agent

import (
	"time"

	"github.com/docker/docker-agent/pkg/cache"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/config/types"
	"github.com/docker/docker-agent/pkg/model/provider"
	"github.com/docker/docker-agent/pkg/tools"
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

// WithRedactSecrets enables all three halves of the redact_secrets
// feature: the pre_tool_use builtin (via ApplyAgentDefaults), the
// runtime's before_llm_call message transform, and the dispatcher's
// tool-output scrub.
func WithRedactSecrets(redactSecrets bool) Opt {
	return func(a *Agent) {
		a.redactSecrets = redactSecrets
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

// WithMaxConsecutiveToolCalls sets the threshold for consecutive identical tool
// call detection. 0 means "use runtime default of 5". Negative values are
// ignored.
func WithMaxConsecutiveToolCalls(n int) Opt {
	return func(a *Agent) {
		if n >= 0 {
			a.maxConsecutiveToolCalls = n
		}
	}
}

// WithMaxOldToolCallTokens sets the maximum token budget for old tool call content.
// Set to -1 to disable truncation (unlimited tool content).
// Set to 0 to use the default (40000).
func WithMaxOldToolCallTokens(n int) Opt {
	return func(a *Agent) {
		a.maxOldToolCallTokens = n
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
			a.AddToolWarning(w)
		}
	}
}

func WithHooks(hooks *latest.HooksConfig) Opt {
	return func(a *Agent) {
		a.hooks = hooks
	}
}

// WithCache attaches a response cache to the agent. Pass nil to disable.
func WithCache(c *cache.Cache) Opt {
	return func(a *Agent) {
		a.cache = c
	}
}
