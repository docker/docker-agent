package types

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
)

// ParseEditFileArgs parses LLM-generated edit_file arguments, handling two
// common failure modes:
//  1. The outer JSON itself is malformed — typically extra closing braces/brackets
//     or stray escape sequences caused by the model losing track of nesting depth
//     when the text payload contains structural characters (e.g. YAML, Dockerfiles).
//  2. The "edits" field is double-serialized (a JSON string instead of an array).
func ParseEditFileArgs(data []byte) (EditFileArgs, error) {
	var raw struct {
		Path  string          `json:"path"`
		Edits json.RawMessage `json:"edits"`
	}

	didRepair := false
	if err := json.Unmarshal(data, &raw); err != nil {
		repaired, ok := tryRepairEditFileJSON(data)
		if !ok {
			return EditFileArgs{}, fmt.Errorf("failed to parse edit_file arguments: %w", err)
		}
		if err := json.Unmarshal(repaired, &raw); err != nil {
			return EditFileArgs{}, fmt.Errorf("failed to parse edit_file arguments after repair: %w", err)
		}
		slog.Debug("Repaired malformed edit_file JSON arguments")
		didRepair = true
	}

	args := EditFileArgs{Path: raw.Path}

	// When edits is missing or null (e.g. during argument streaming in
	// the TUI, or partial tool calls), accept the partial result.
	if len(raw.Edits) == 0 || string(raw.Edits) == "null" {
		return args, nil
	}

	// Try parsing edits as an array first (normal case).
	if err := json.Unmarshal(raw.Edits, &args.Edits); err != nil {
		// Try unwrapping a double-serialized JSON string.
		var editsStr string
		if err := json.Unmarshal(raw.Edits, &editsStr); err != nil {
			return EditFileArgs{}, fmt.Errorf("edits field is neither an array nor a JSON string: %w", err)
		}
		if err := json.Unmarshal([]byte(editsStr), &args.Edits); err != nil {
			// The inner payload can carry the same brace/bracket-counting
			// mistakes as the outer JSON, so give it the same repair pass.
			repaired, ok := tryRepairEditFileJSON([]byte(editsStr))
			if !ok {
				return EditFileArgs{}, fmt.Errorf("failed to parse double-serialized edits string: %w", err)
			}
			if err := json.Unmarshal(repaired, &args.Edits); err != nil {
				return EditFileArgs{}, fmt.Errorf("failed to parse double-serialized edits string after repair: %w", err)
			}
			slog.Debug("Repaired malformed double-serialized edits payload")
			didRepair = true
		}
	}

	// Validated once, for either repair path: a repair is only trustworthy if it
	// removed a spurious character rather than a load-bearing one. Well-formed
	// payloads are deliberately not second-guessed here — the TUI parses
	// partially-streamed arguments with this function, where a not-yet-filled
	// oldText is normal; handleEditFile is what refuses to apply it.
	if didRepair {
		if err := validateRepairedEdits(args.Edits); err != nil {
			return EditFileArgs{}, err
		}
	}

	return args, nil
}

// validateRepairedEdits guards against repair output that is structurally
// valid but semantically corrupted: an empty oldText after a repair means the
// repair removed a load-bearing character (a quote closing the string) rather
// than a spurious one, so the whole repaired payload is untrustworthy.
//
// This is about distrusting the repair, not about protecting the edit loop —
// handleEditFile refuses an empty oldText on its own. Rejecting here just
// reports the real problem at the parse boundary, where the message can say
// the payload was mis-repaired.
func validateRepairedEdits(edits []Edit) error {
	for i, edit := range edits {
		if edit.OldText == "" {
			return fmt.Errorf("repaired edits payload is invalid: edit %d has empty oldText", i+1)
		}
	}
	return nil
}

// tryRepairEditFileJSON attempts to fix common LLM JSON malformations by
// iteratively removing the offending character(s) at each json.SyntaxError
// offset. Observed failure modes from production sessions:
//
//   - Extra '}' — model loses brace count (e.g. "}}]}" instead of "}]}")
//   - Extra ']' — model adds a spurious array wrapper
//   - Stray '\' — model emits an escape sequence outside of a string value
//     (e.g. literal \n between tokens, or \" where " is expected)
func tryRepairEditFileJSON(data []byte) ([]byte, bool) {
	var current []byte
	if len(data) > 0 {
		current = slices.Clone(data)
	}
	for range 3 {
		var synErr *json.SyntaxError
		if err := json.Unmarshal(current, &json.RawMessage{}); err == nil {
			return current, true
		} else if !errors.As(err, &synErr) {
			return nil, false
		}

		// json.SyntaxError.Offset is 1-based.
		offset := int(synErr.Offset) - 1
		if offset < 0 || offset >= len(current) {
			return nil, false
		}

		ch := current[offset]
		removeCount := 1

		switch ch {
		case '}', ']':
			// Extra closing delimiter — just remove it.
		case '\\':
			// Stray escape sequence outside a string value. For \n, \t, \r
			// both characters are garbage so remove them. For \" the quote
			// is a valid structural character (string delimiter), so only
			// strip the backslash.
			if offset+1 < len(current) {
				switch current[offset+1] {
				case 'n', 't', 'r':
					removeCount = 2
				}
			}
		default:
			return nil, false
		}

		repaired := slices.Concat(current[:offset], current[offset+removeCount:])
		current = repaired
	}

	if json.Valid(current) {
		return current, true
	}
	return nil, false
}
