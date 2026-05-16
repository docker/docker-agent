package types

import (
	"errors"
	"fmt"
)

// Command represents an agent command with optional metadata.
// It supports two YAML formats:
//
// Simple format (string value):
//
//	fix-lint: "Fix the lint issues"
//
// Advanced format (object value):
//
//	fix-lint:
//	  description: "Fix linting errors in the codebase"
//	  instruction: |
//	    Fix the lint issues reported by: !`golangci-lint run`
//	    Focus on files: $1
type Command struct {
	// Description is shown in completion dialogs and help text.
	// For simple format commands, this is empty and the instruction is used for display.
	Description string `json:"description,omitempty"`

	// Instruction is the prompt sent to the agent.
	// Supports:
	// - Bang commands: !`command` (executed and output inserted)
	// - Positional arguments: $1, $2, etc.
	Instruction string `json:"instruction,omitempty"`
}

// DisplayText returns the text to show in completion dialogs.
// Returns Description if available, otherwise truncates the Instruction.
func (c Command) DisplayText() string {
	if c.Description != "" {
		return c.Description
	}
	return c.Instruction
}

// Commands represents a set of named prompts for quick-starting conversations.
// It supports multiple YAML formats:
//
// Map of simple strings:
//
//	commands:
//	  df: "check disk space"
//	  ls: "list files"
//
// List of singleton maps (simple strings):
//
//	commands:
//	  - df: "check disk space"
//	  - ls: "list files"
//
// Map of advanced objects:
//
//	commands:
//	  fix-lint:
//	    description: "Fix linting errors"
//	    instruction: "Fix the lint issues"
//
// Mixed format (simple and advanced):
//
//	commands:
//	  simple: "A simple command"
//	  advanced:
//	    description: "An advanced command"
//	    instruction: "Do something complex"
type Commands map[string]Command

// UnmarshalYAML supports both map and list-of-singleton-maps syntaxes,
// with values being either simple strings or Command objects.
func (c *Commands) UnmarshalYAML(unmarshal func(any) error) error {
	// Try direct map first (handles both simple and advanced formats)
	var m map[string]any
	if err := unmarshal(&m); err == nil && m != nil {
		result := make(map[string]Command)
		for k, v := range m {
			cmd, err := parseCommandValue(v)
			if err != nil {
				return fmt.Errorf("command %q: %w", k, err)
			}
			result[k] = cmd
		}
		*c = result
		return nil
	}

	// Try list of maps [{k:v}, {k:v}]
	var list []map[string]any
	if err := unmarshal(&list); err == nil && list != nil {
		result := make(map[string]Command)
		for _, item := range list {
			for k, v := range item {
				cmd, err := parseCommandValue(v)
				if err != nil {
					return fmt.Errorf("command %q: %w", k, err)
				}
				result[k] = cmd
			}
		}
		*c = result
		return nil
	}

	// If none matched, treat as empty
	*c = map[string]Command{}
	return nil
}

// parseCommandValue parses a command value which can be either:
// - a simple string (becomes the instruction)
// - a map with description/instruction fields.
func parseCommandValue(v any) (Command, error) {
	switch val := v.(type) {
	case string:
		return Command{Instruction: val}, nil
	case map[string]any:
		desc, _ := val["description"].(string)
		inst, _ := val["instruction"].(string)

		if inst == "" && desc == "" {
			return Command{}, errors.New("command must have at least 'instruction' or 'description'")
		}
		if inst == "" {
			inst = desc
		}

		return Command{Description: desc, Instruction: inst}, nil
	default:
		return Command{}, fmt.Errorf("invalid command value type: %T", v)
	}
}
