package dialog

import (
	"fmt"
	"net/mail"
	"net/url"
	"regexp"
	"sync"
	"time"
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

// validateStringField validates a string value against field constraints.
func validateStringField(val string, field ElicitationField) bool {
	return validateStringFieldWithMessage(val, field) == ""
}

// validateStringFieldWithMessage validates a string value and returns a descriptive error message.
func validateStringFieldWithMessage(val string, field ElicitationField) string {
	if field.MinLength > 0 && len(val) < field.MinLength {
		return fmt.Sprintf("Must be at least %d characters", field.MinLength)
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
	return validateFormatWithMessage(val, field.Format)
}

// validateNumberField validates a numeric value against field constraints.
func validateNumberField(val float64, field ElicitationField) bool {
	return validateNumberFieldWithMessage(val, field) == ""
}

// validateNumberFieldWithMessage validates a numeric value and returns a descriptive error message.
func validateNumberFieldWithMessage(val float64, field ElicitationField) string {
	if field.HasMinimum && val < field.Minimum {
		return fmt.Sprintf("Must be at least %v", field.Minimum)
	}
	if field.HasMaximum && val > field.Maximum {
		return fmt.Sprintf("Must be at most %v", field.Maximum)
	}
	return ""
}

// validateSelectionCount checks a multiselect against minItems and maxItems.
func validateSelectionCount(count int, field ElicitationField) string {
	if count < field.MinItems {
		return fmt.Sprintf("Select at least %d", field.MinItems)
	}
	if field.MaxItems > 0 && count > field.MaxItems {
		return fmt.Sprintf("Select at most %d", field.MaxItems)
	}
	return ""
}

// validateFormat validates a string value against a JSON Schema format.
func validateFormat(val, format string) bool {
	return validateFormatWithMessage(val, format) == ""
}

// validateFormatWithMessage validates a string against a JSON Schema format and returns an error message.
func validateFormatWithMessage(val, format string) string {
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
