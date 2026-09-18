package main

import (
	"go/ast"
	"go/token"
	"strconv"
	"strings"

	"github.com/dgageot/rubocop-go/cop"
)

const (
	legacyEnvPrefix  = "CAGENT_"
	currentEnvPrefix = "DOCKER_AGENT_"
)

var internalCagentEnvVars = map[string]bool{
	"CAGENT_ASKPASS_SOCKET": true,
	"CAGENT_ASKPASS_TOKEN":  true,
}

// EnvironmentVariablePrefix prevents new public environment variables from
// using the legacy CAGENT_ prefix. Legacy aliases remain valid when the same
// file also declares the equivalent DOCKER_AGENT_ name. Internal askpass
// variables are exempt because they form a private subprocess protocol.
var EnvironmentVariablePrefix = &cop.Func{
	Meta: cop.Meta{
		Name:        "Lint/EnvironmentVariablePrefix",
		Description: "public environment variables must use the DOCKER_AGENT_ prefix",
		Severity:    cop.Error,
	},
	Run: func(p *cop.Pass) {
		if p.IsTestFile() {
			return
		}

		literals := stringLiterals(p.File)
		values := literalsByValue(literals)
		for lit, value := range literals {
			if !isLegacyEnvVar(value) || internalCagentEnvVars[value] {
				continue
			}
			current := currentEnvPrefix + strings.TrimPrefix(value, legacyEnvPrefix)
			if values[current] {
				continue
			}
			p.Reportf(lit, "%s uses the legacy CAGENT_ prefix without a %s alias", value, current)
		}
	},
}

func stringLiterals(file *ast.File) map[*ast.BasicLit]string {
	result := map[*ast.BasicLit]string{}
	ast.Inspect(file, func(n ast.Node) bool {
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		value, err := strconv.Unquote(lit.Value)
		if err == nil {
			result[lit] = value
		}
		return true
	})
	return result
}

func literalsByValue(literals map[*ast.BasicLit]string) map[string]bool {
	result := make(map[string]bool, len(literals))
	for _, value := range literals {
		result[value] = true
	}
	return result
}

func isLegacyEnvVar(value string) bool {
	if !strings.HasPrefix(value, legacyEnvPrefix) || len(value) == len(legacyEnvPrefix) {
		return false
	}
	for _, r := range value[len(legacyEnvPrefix):] {
		if r != '_' && (r < 'A' || r > 'Z') && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}
