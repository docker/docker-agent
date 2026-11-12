package builtins

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/docker/docker-agent/pkg/hooks"
	"github.com/docker/docker-agent/pkg/promptfiles"
	"github.com/docker/docker-agent/pkg/skills"
)

// AddPromptFiles is the registered name of the add_prompt_files builtin.
const AddPromptFiles = "add_prompt_files"

const strictPromptFilesArg = "--strict"

// GuardPromptFiles makes discovery failures visible when a prompt-file guard is configured.
func GuardPromptFiles(cfg *hooks.Config) *hooks.Config {
	if cfg == nil || len(cfg.PromptFileGuard) == 0 {
		return cfg
	}
	guarded := *cfg
	guard := func(defs []hooks.Hook) []hooks.Hook {
		defs = slices.Clone(defs)
		for i := range defs {
			h := &defs[i]
			if h.Type == hooks.HookTypeBuiltin && h.Command == AddPromptFiles && !slices.Contains(h.Args, strictPromptFilesArg) {
				h.Args = append(slices.Clone(h.Args), strictPromptFilesArg)
			}
		}
		return defs
	}
	guarded.SessionStart = guard(cfg.SessionStart)
	guarded.TurnStart = guard(cfg.TurnStart)
	return &guarded
}

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
	strict := slices.Contains(args, strictPromptFilesArg)
	names, depth := parsePromptFileArgs(args)
	home, _ := os.UserHomeDir()
	var sources []hooks.InstructionContext
	var loaded []string
	for _, name := range names {
		paths, err := promptfiles.PathsWithError(in.Cwd, home, os.Getenv(skills.KitDirEnv), name)
		if strict && err != nil {
			slog.WarnContext(ctx, "discovering prompt files", "error", err)
			return instructionContextOutput(hooks.InstructionContext{Group: promptfiles.InstructionGroup, Unavailable: true, SetMarker: true}), nil
		}
		for _, path := range paths {
			content, err := os.ReadFile(path)
			if err != nil {
				slog.WarnContext(ctx, "reading prompt file", "path", path, "error", err)
				return instructionContextOutput(hooks.InstructionContext{
					Group: promptfiles.InstructionGroup, Unavailable: true, SetMarker: true,
				}), nil
			}
			loaded = append(loaded, path)
			rendered := "Instructions from: " + path + "\n" + string(content)
			sources = append(sources, hooks.InstructionContext{
				Key:            promptFileKey(path),
				Path:           path,
				Group:          promptfiles.InstructionGroup,
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
			Group:          promptfiles.InstructionGroup,
			Label:          "prompt files below " + in.Cwd,
			Content:        note,
			ChangedContent: "The list of prompt files below " + in.Cwd + " has changed and replaces the previous list.\n\n" + note,
			RemovedContent: "No prompt files remain below " + in.Cwd + "; the previously listed ones no longer apply.",
		})
	}
	if len(sources) == 0 {
		sources = append(sources, hooks.InstructionContext{
			Group: promptfiles.InstructionGroup, CompleteGroup: true, SetMarker: true,
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
		if arg == strictPromptFilesArg {
			continue
		}
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
