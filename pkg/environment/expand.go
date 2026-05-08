package environment

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
)

// envDotPattern matches ${env.VAR} and captures VAR.
// This normalizes JS-style env access to POSIX-style before os.Expand.
var envDotPattern = regexp.MustCompile(`\$\{env\.([a-zA-Z_][a-zA-Z0-9_]*)\}`)

func ExpandAll(ctx context.Context, values []string, env Provider) ([]string, error) {
	var expandedEnv []string

	for _, value := range values {
		expanded, err := Expand(ctx, value, env)
		if err != nil {
			return nil, err
		}

		expandedEnv = append(expandedEnv, expanded)
	}

	return expandedEnv, nil
}

// Expand resolves environment variable references in value using the provided Provider.
// It accepts three equivalent syntaxes: $VAR, ${VAR}, and ${env.VAR}.
// ~ expansion is intentionally excluded; use path.ExpandHome for path fields.
//
// If a referenced variable is not set, Expand returns an ErrMissingVarsError error
// wrapping all missing names, but still returns the partially-expanded string
// so callers can decide whether to hard-fail or warn.
func Expand(ctx context.Context, value string, env Provider) (string, error) {
	if env == nil {
		return value, nil
	}

	// Normalize ${env.VAR} → ${VAR} so os.Expand handles both uniformly,
	// but only for simple cases without additional JS logic.
	normalized := envDotPattern.ReplaceAllString(value, `${$1}`)

	var missing []string
	expanded := os.Expand(normalized, func(name string) string {
		if name == "" {
			return ""
		}
		if name == "$" {
			return "$"
		}

		// If it's a complex JS expression (contains spaces, dots, quotes, etc),
		// we leave it untouched for Layer 2 (JS engine) to handle.
		// Valid POSIX env names only contain alphanumeric characters and underscores.
		if strings.ContainsAny(name, " .|'\"(){}[],+-*/=!<>?^&%@~`\\#") {
			return "${" + name + "}"
		}

		v, found := env.Get(ctx, name)
		if !found {
			missing = append(missing, name)
			return "" // match os.ExpandEnv behavior: empty on missing
		}
		return v
	})

	if len(missing) > 0 {
		return expanded, &ErrMissingVarsError{Names: missing}
	}

	return expanded, nil
}

// ErrMissingVarsError is returned when one or more referenced variables are not set.
// It is a distinct type so callers can decide severity (hard fail vs warn).
type ErrMissingVarsError struct {
	Names []string
}

func (e *ErrMissingVarsError) Error() string {
	return "environment variables not set: " + strings.Join(e.Names, ", ")
}

func ToValues(envMap map[string]string) []string {
	var values []string
	for k, v := range envMap {
		values = append(values, fmt.Sprintf("%s=%s", k, v))
	}
	slices.Sort(values)
	return values
}
