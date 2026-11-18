package elicitform

import (
	"fmt"
	"net/mail"
	"net/url"
	"regexp"
	"sync"
	"time"
	"unicode/utf8"
)

// patternCache caches compiled regex patterns to avoid repeated compilation.
var patternCache sync.Map // map[string]*regexp.Regexp

// getCompiledPattern returns a compiled regex pattern, using cache when available.
func getCompiledPattern(pattern string) (*regexp.Regexp, error) {
	if cached, ok := patternCache.Load(pattern); ok {
		return cached.(*regexp.Regexp), nil
	}

	compiled, err := regexp.Compile(pattern)
	if err != nil {
		return nil, err
	}

	patternCache.Store(pattern, compiled)
	return compiled, nil
}

// validateString validates a string value and returns a descriptive error message.
func validateString(val string, field Field) string {
	// JSON Schema lengths count characters, not bytes.
	n := utf8.RuneCountInString(val)
	if field.MinLength > 0 && n < field.MinLength {
		return fmt.Sprintf("Must be at least %d characters", field.MinLength)
	}
	if field.MaxLength > 0 && n > field.MaxLength {
		return fmt.Sprintf("Must be at most %d characters", field.MaxLength)
	}
	if field.Pattern != "" {
		compiled, err := getCompiledPattern(field.Pattern)
		if err != nil {
			return "Invalid pattern in schema"
		}
		if !compiled.MatchString(val) {
			return "Invalid format"
		}
	}
	return validateFormat(val, field.Format)
}

// validateNumber validates a numeric value and returns a descriptive error message.
func validateNumber(val float64, field Field) string {
	if field.HasMinimum && val < field.Minimum {
		return fmt.Sprintf("Must be at least %v", field.Minimum)
	}
	if field.HasMaximum && val > field.Maximum {
		return fmt.Sprintf("Must be at most %v", field.Maximum)
	}
	return ""
}

// validateSelectionCount checks a multiselect against minItems and maxItems.
func validateSelectionCount(count int, field Field) string {
	if count < field.MinItems {
		return fmt.Sprintf("Select at least %d", field.MinItems)
	}
	if field.MaxItems > 0 && count > field.MaxItems {
		return fmt.Sprintf("Select at most %d", field.MaxItems)
	}
	return ""
}

// validateFormat validates a string against a JSON Schema format and returns an error message.
func validateFormat(val, format string) string {
	switch format {
	case "":
		return ""
	case "email":
		if _, err := mail.ParseAddress(val); err != nil {
			return "Must be a valid email address"
		}
		return ""
	case "uri":
		u, err := url.Parse(val)
		if err != nil || u.Scheme == "" || u.Host == "" {
			return "Must be a valid URL (http:// or https://)"
		}
		return ""
	case "date":
		if _, err := time.Parse("2006-01-02", val); err != nil {
			return "Must be a valid date (YYYY-MM-DD)"
		}
		return ""
	case "date-time":
		if _, err := time.Parse(time.RFC3339, val); err != nil {
			return "Must be a valid date-time (RFC3339 format)"
		}
		return ""
	default:
		return "" // Unknown format - be permissive
	}
}
