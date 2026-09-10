package builtins

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"os"
	"strconv"
	"strings"

	"github.com/docker/docker-agent/pkg/hooks"
	"github.com/docker/docker-agent/pkg/promptfiles"
)

// AddPromptFiles is the registered name of the add_prompt_files builtin.
const AddPromptFiles = "add_prompt_files"

const promptFilesGroup = "core/prompt-files"

// promptFilesIndexKey identifies the listing of nested prompt files. Fixed
// (unlike the per-path keys) so the listing is diffed as a whole.
const promptFilesIndexKey = "core/prompt-file-index"

// depthArgPrefix marks the optional add_prompt_files argument selecting how
// many directory levels below the working dir are scanned for further prompt
// files to list by path. A flag-shaped argument keeps the builtin's args a
// flat []string. Repeated, the last one wins.
const depthArgPrefix = "--depth="

// addPromptFiles reads each filename from the workdir hierarchy and home
// directory, preserving each resolved path in independently diffable context.
// With --depth=N it also lists, without reading them, the same filenames
// found up to N levels below the working dir.
func addPromptFiles(ctx context.Context, in *hooks.Input, args []string) (*hooks.Output, error) {
	if in == nil || in.Cwd == "" || len(args) == 0 {
		return nil, nil
	}
	names, depth := parsePromptFileArgs(args)
	home, _ := os.UserHomeDir()
	var sources []hooks.InstructionContext
	var loaded []string
	for _, name := range names {
		for _, path := range promptfiles.PathsFromEnv(in.Cwd, home, name) {
			content, err := os.ReadFile(path)
			if err != nil {
				slog.WarnContext(ctx, "reading prompt file", "path", path, "error", err)
				return instructionContextOutput(hooks.InstructionContext{
					Group: promptFilesGroup, Unavailable: true, SetMarker: true,
				}), nil
			}
			loaded = append(loaded, path)
			rendered := "Instructions from: " + path + "\n" + string(content)
			sources = append(sources, hooks.InstructionContext{
				Key:            promptFileKey(path),
				Group:          promptFilesGroup,
				Label:          "instructions from " + path,
				Content:        rendered,
				ChangedContent: "The instructions from " + path + " have changed and replace the previous instructions from that file.\n\n" + rendered,
				RemovedContent: "The previously loaded instructions from " + path + " no longer apply.",
			})
		}
	}
	if note := promptfiles.Index(ctx, in.Cwd, names, depth, loaded); note != "" {
		sources = append(sources, hooks.InstructionContext{
			Key:            promptFilesIndexKey,
			Group:          promptFilesGroup,
			Label:          "prompt files below " + in.Cwd,
			Content:        note,
			ChangedContent: "The list of prompt files below " + in.Cwd + " has changed and replaces the previous list.\n\n" + note,
			RemovedContent: "No prompt files remain below " + in.Cwd + "; the previously listed ones no longer apply.",
		})
	}
	if len(sources) == 0 {
		sources = append(sources, hooks.InstructionContext{
			Group: promptFilesGroup, CompleteGroup: true, SetMarker: true,
		})
	} else {
		sources[0].CompleteGroup = true
	}
	return instructionContextOutput(sources...), nil
}

// parsePromptFileArgs splits the builtin's arguments into prompt-file names
// and the optional nested-scan depth. An unusable depth is warned about and
// ignored rather than failing the turn: the file contents, which are the
// point of the hook, still make it into the context.
func parsePromptFileArgs(args []string) (names []string, depth int) {
	for _, arg := range args {
		raw, ok := strings.CutPrefix(arg, depthArgPrefix)
		if !ok {
			names = append(names, arg)
			continue
		}
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			slog.Warn("ignoring invalid add_prompt_files depth", "arg", arg)
			continue
		}
		depth = n
	}
	return names, depth
}

func instructionContextOutput(sources ...hooks.InstructionContext) *hooks.Output {
	return &hooks.Output{HookSpecificOutput: &hooks.HookSpecificOutput{
		HookEventName:      hooks.EventTurnStart,
		InstructionContext: sources,
	}}
}

func promptFileKey(path string) string {
	sum := sha256.Sum256([]byte(path))
	return "core/prompt-file-" + hex.EncodeToString(sum[:])
}
