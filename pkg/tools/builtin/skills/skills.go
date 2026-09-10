package skills

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/docker/docker-agent/pkg/safety"
	"github.com/docker/docker-agent/pkg/skills"
	"github.com/docker/docker-agent/pkg/tools"
)

const (
	ToolNameReadSkill     = "read_skill"
	ToolNameReadSkillFile = "read_skill_file"
	ToolNameRunSkill      = "run_skill"
)

// metaSkill names the confirmation metadata key that tells the user
// which skill an embedded command comes from.
const metaSkill = "skill"

var (
	_ tools.ToolSet      = (*ToolSet)(nil)
	_ tools.Instructable = (*ToolSet)(nil)
	_ tools.Named        = (*ToolSet)(nil)
)

// BuildSkillUserMessage formats a PreparedSkillFork as the implicit user
// message of a forked sub-session. It mirrors the inline `<skill name=...>`
// envelope used by ResolveSkillCommand for non-fork skills, strips the YAML
// frontmatter (already consumed by the runtime), and surfaces a non-empty
// Task as a "User's request:" header.
func BuildSkillUserMessage(prepared *PreparedSkillFork) string {
	body := stripFrontmatter(prepared.Content)
	if prepared.Task != "" {
		return fmt.Sprintf("Use the following skill.\n\nUser's request: %s\n\n<skill name=%q>\n%s\n</skill>", prepared.Task, prepared.SkillName, body)
	}
	return fmt.Sprintf("Use the following skill.\n\n<skill name=%q>\n%s\n</skill>", prepared.SkillName, body)
}

// BuildSkillSystemMessage returns the system prompt of a forked skill
// sub-session. It avoids the task-delegation boilerplate from
// buildTaskSystemMessage (which references <task> / "team of agents")
// since skills aren't delegations. attachedFiles, when non-empty, exposes
// parent-attached files by absolute path.
func BuildSkillSystemMessage(prepared *PreparedSkillFork, attachedFiles []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are running the %q skill in an isolated sub-session. ", prepared.SkillName)
	b.WriteString("The next user message contains the full skill instructions. Follow them; ")
	b.WriteString("do not improvise around them or mix in unrelated tasks from prior conversations.")
	if len(attachedFiles) > 0 {
		b.WriteString("\n\nThe user attached these files in the parent conversation. Prefer them over any bare filenames in the skill body:\n<attached_files>")
		for _, p := range attachedFiles {
			fmt.Fprintf(&b, "\n- %s", p)
		}
		b.WriteString("\n</attached_files>")
	}
	return b.String()
}

// stripFrontmatter removes a leading YAML `---`-delimited frontmatter
// block from a SKILL.md payload. Returns the input unchanged if no
// leading fence or no closing fence is found.
func stripFrontmatter(content string) string {
	if !strings.HasPrefix(content, "---") {
		return content
	}
	rest := content[3:]
	_, after, found := strings.Cut(rest, "\n---")
	if !found {
		return content
	}
	return strings.TrimPrefix(after, "\n")
}

// ToolSet provides the read_skill and read_skill_file tools that let an
// agent load skill content and supporting resources by name. It hides whether
// a skill is local or remote — the agent just sees a name and description.
type ToolSet struct {
	skills     []skills.Skill
	workingDir string
	// forkToolSets maps a fork skill's name to the additional toolsets it
	// exposes while running in its sub-session (resolved from the skill's
	// declared toolset names by the loader). Nil for skills without any.
	forkToolSets map[string][]tools.ToolSet
}

func New(loadedSkills []skills.Skill, workingDir string) *ToolSet {
	return &ToolSet{
		skills:     loadedSkills,
		workingDir: workingDir,
	}
}

// SetForkToolSets records the additional toolsets each fork skill exposes
// while running in its sub-session, keyed by skill name. Called by the loader
// after resolving the skills' declared toolset names.
func (s *ToolSet) SetForkToolSets(m map[string][]tools.ToolSet) {
	s.forkToolSets = m
}

// Name implements tools.Named; loader-created, so no registry WithName wrapper.
func (s *ToolSet) Name() string {
	return "skills"
}

// Skills returns the loaded skills (used by the app layer for slash commands).
func (s *ToolSet) Skills() []skills.Skill {
	return s.skills
}

func (s *ToolSet) findSkill(name string) *skills.Skill {
	for i := range s.skills {
		if s.skills[i].Name == name {
			return &s.skills[i]
		}
	}
	return nil
}

// FindSkill returns the skill with the given name, or nil if not found.
func (s *ToolSet) FindSkill(name string) *skills.Skill {
	return s.findSkill(name)
}

// ReadSkillContent returns the content of a skill's SKILL.md by name, or the
// body carried in the agent config for an inline skill.
// For local and inline skills, it expands any !`command` patterns in the
// content by executing the commands and replacing the patterns with their
// stdout output. Every command is submitted to rt for approval first: a skill
// body is data, not agent config, so a command hidden in one must never run
// unannounced. Command expansion is disabled for remote skills to prevent
// arbitrary code execution on behalf of a third-party server.
func (s *ToolSet) ReadSkillContent(ctx context.Context, name string, rt tools.Runtime) (string, error) {
	skill := s.findSkill(name)
	if skill == nil {
		return "", fmt.Errorf("skill %q not found", name)
	}

	// Inline skills carry their body in memory; there is no file to read.
	content := skill.InlineContent
	if !skill.IsInline() {
		var err error
		if content, err = readFileContent(skill.FilePath); err != nil {
			return "", err
		}
	}

	if !skill.ExpandsCommands() {
		return content, nil
	}

	if rt == nil {
		rt = tools.NopRuntime{}
	}
	return skills.ExpandCommandsWithError(ctx, content, s.confirmedRunner(rt, skill.Name))
}

// confirmedRunner returns the [skills.Runner] used to expand a local or inline
// skill: each embedded command is presented to the user as a shell tool call
// and only runs once approved.
func (s *ToolSet) confirmedRunner(rt tools.Runtime, skillName string) skills.Runner {
	return func(ctx context.Context, command string) (string, error) {
		return rt.ConfirmAndRun(ctx, tools.ConfirmedRun{
			ToolName: safety.ShellToolName,
			Args:     map[string]any{"cmd": command, "cwd": s.workingDir},
			Metadata: map[string]string{metaSkill: skillName},
		}, func(ctx context.Context, approved tools.ConfirmedRun) (string, error) {
			command, ok := safety.CommandArg(approved.Args)
			if !ok || strings.TrimSpace(command) == "" {
				return "", errors.New("pre_tool_use hook removed the shell command")
			}
			workDir, _ := approved.Args["cwd"].(string)
			if workDir == "" {
				workDir = s.workingDir
			}
			return skills.ShellRunner(workDir)(ctx, command)
		})
	}
}

// ReadSkillFile returns the content of a supporting file within a skill.
// The path is relative to the skill's base directory (e.g. "references/FORMS.md").
func (s *ToolSet) ReadSkillFile(skillName, relativePath string) (string, error) {
	skill := s.findSkill(skillName)
	if skill == nil {
		return "", fmt.Errorf("skill %q not found", skillName)
	}

	// Inline skills live in the agent config and have no backing directory,
	// so there are no supporting files to read. Reject explicitly rather than
	// joining against an empty BaseDir (which would resolve against the
	// process working directory).
	if skill.IsInline() {
		return "", fmt.Errorf("skill %q is defined inline and has no supporting files", skillName)
	}

	if !isValidRelativePath(relativePath) {
		return "", fmt.Errorf("invalid file path %q", relativePath)
	}

	absPath := filepath.Join(skill.BaseDir, filepath.FromSlash(relativePath))

	// Ensure the resolved path stays within the skill's base directory
	cleanBase := filepath.Clean(skill.BaseDir)
	cleanPath := filepath.Clean(absPath)
	if !strings.HasPrefix(cleanPath, cleanBase+string(filepath.Separator)) && cleanPath != cleanBase {
		return "", fmt.Errorf("path %q escapes skill directory", relativePath)
	}

	content, err := readFileContent(absPath)
	if err != nil {
		return "", err
	}

	return content, nil
}

func readFileContent(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("reading file: %w", err)
	}
	return string(data), nil
}

func isValidRelativePath(p string) bool {
	if p == "" || strings.HasPrefix(p, "/") || strings.HasPrefix(p, "\\") {
		return false
	}
	if strings.Contains(p, "..") {
		return false
	}
	return true
}

type readSkillArgs struct {
	Name string `json:"name" jsonschema:"The name of the skill to read"`
}

type readSkillFileArgs struct {
	SkillName string `json:"skill_name" jsonschema:"The name of the skill that contains the file"`
	Path      string `json:"path" jsonschema:"The relative path to the file within the skill (e.g. references/FORMS.md)"`
}

func (s *ToolSet) handleReadSkill(ctx context.Context, args readSkillArgs, rt tools.Runtime) (*tools.ToolCallResult, error) {
	content, err := s.ReadSkillContent(ctx, args.Name, rt)
	if err != nil {
		if abortsExpansion(err) {
			return nil, err
		}
		return tools.ResultError(err.Error()), nil
	}
	return tools.ResultSuccess(content), nil
}

func (s *ToolSet) handleReadSkillFile(_ context.Context, args readSkillFileArgs) (*tools.ToolCallResult, error) {
	content, err := s.ReadSkillFile(args.SkillName, args.Path)
	if err != nil {
		return tools.ResultError(err.Error()), nil
	}
	return tools.ResultSuccess(content), nil
}

// hasFiles reports whether any loaded skill has supporting files beyond SKILL.md.
func (s *ToolSet) hasFiles() bool {
	for _, skill := range s.skills {
		if len(skill.Files) > 1 {
			return true
		}
	}
	return false
}

// hasForkSkills reports whether any loaded skill uses context: fork.
func (s *ToolSet) hasForkSkills() bool {
	for i := range s.skills {
		if s.skills[i].IsFork() {
			return true
		}
	}
	return false
}

func (s *ToolSet) Instructions() string {
	if len(s.skills) == 0 {
		return ""
	}

	var sb strings.Builder
	sb.WriteString("Skills provide specialized instructions for specific tasks. ")
	sb.WriteString("When a user's request matches a skill's description, use read_skill to load its instructions.\n\n")

	hasFork := s.hasForkSkills()
	if hasFork {
		sb.WriteString("Some skills are configured to run in a forked context with their own conversation history. ")
		sb.WriteString("For those skills, you MUST use the run_skill tool (not transfer_task or read_skill). ")
		sb.WriteString("The run_skill tool handles the isolated execution automatically.\n\n")
	}

	if s.hasFiles() {
		sb.WriteString("Some skills have supporting files. ")
		sb.WriteString("Use read_skill_file to load referenced files on demand — do not preload them.\n\n")
	}

	sb.WriteString("<available_skills>\n")
	for _, skill := range s.skills {
		sb.WriteString("  <skill>\n")
		sb.WriteString("    <name>")
		sb.WriteString(skill.Name)
		sb.WriteString("</name>\n")
		sb.WriteString("    <description>")
		sb.WriteString(skill.Description)
		sb.WriteString("</description>\n")
		if skill.IsFork() {
			sb.WriteString("    <mode>forked</mode>\n")
		}
		if len(skill.Files) > 1 {
			sb.WriteString("    <files>")
			// List files excluding SKILL.md itself
			first := true
			for _, f := range skill.Files {
				if f == "SKILL.md" {
					continue
				}
				if !first {
					sb.WriteString(", ")
				}
				sb.WriteString(f)
				first = false
			}
			sb.WriteString("</files>\n")
		}
		sb.WriteString("  </skill>\n")
	}
	sb.WriteString("</available_skills>")

	return sb.String()
}

// RunSkillArgs specifies the parameters for the run_skill tool.
type RunSkillArgs struct {
	Name string `json:"name" jsonschema:"The name of the skill to run in a forked context"`
	Task string `json:"task" jsonschema:"A clear description of the task the skill should achieve"`
}

// PreparedSkillFork carries the validated and expanded data needed to launch a
// skill as an isolated sub-agent. Callers (typically the runtime) use it to
// build the child session; this lets the skill-specific business rules
// (lookup, fork validation, content expansion) live with the toolset rather
// than the runtime.
type PreparedSkillFork struct {
	// SkillName is the validated skill name, suitable for span attributes,
	// log fields, and sub-session titles ("Skill: <name>").
	SkillName string
	// Task is the caller-supplied task description, intended to be used as
	// the implicit user message of the child session.
	Task string
	// Content is the expanded SKILL.md content. Wrap with
	// BuildSkillUserMessage to use as the child's first user message.
	Content string
	// Model is the optional model override declared in the SKILL.md
	// frontmatter. Empty means "use the parent agent's current model".
	Model string
	// AllowedTools, when non-empty, restricts the sub-session to the parent
	// agent's tools whose names match an entry (glob or exact). Empty means
	// the parent agent's full tool set is inherited.
	AllowedTools []string
	// ToolSets holds the additional toolsets the skill exposes in its
	// sub-session, on top of the (optionally filtered) inherited tools.
	ToolSets []tools.ToolSet
}

// PrepareForkSubSession validates a run_skill request and loads the expanded
// skill content. It returns either a populated PreparedSkillFork, or a
// ToolCallResult describing why the call cannot proceed (skill missing,
// skill not configured for fork mode, content read failure). rt gates the
// commands the skill body embeds — see [ToolSet.ReadSkillContent]. The caller
// is responsible for the runtime-specific orchestration (sub-session creation,
// tracing, event forwarding).
func (s *ToolSet) PrepareForkSubSession(ctx context.Context, args RunSkillArgs, rt tools.Runtime) (*PreparedSkillFork, *tools.ToolCallResult, error) {
	skill := s.findSkill(args.Name)
	if skill == nil {
		return nil, tools.ResultError(fmt.Sprintf("skill %q not found", args.Name)), nil
	}

	if !skill.IsFork() {
		return nil, tools.ResultError(fmt.Sprintf(
			"skill %q is not configured for forked execution (set context: fork); use read_skill instead",
			args.Name,
		)), nil
	}

	content, err := s.ReadSkillContent(ctx, args.Name, rt)
	if err != nil {
		if abortsExpansion(err) {
			return nil, nil, err
		}
		return nil, tools.ResultError(fmt.Sprintf("failed to read skill content: %s", err)), nil
	}

	return &PreparedSkillFork{
		SkillName:    args.Name,
		Task:         args.Task,
		Content:      content,
		Model:        skill.Model,
		AllowedTools: skill.AllowedTools,
		ToolSets:     s.forkToolSets[args.Name],
	}, nil, nil
}

func abortsExpansion(err error) bool {
	var abort skills.ExpansionAbort
	return errors.As(err, &abort) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

func (s *ToolSet) Tools(context.Context) ([]tools.Tool, error) {
	if len(s.skills) == 0 {
		return nil, nil
	}

	result := []tools.Tool{
		{
			Name:         ToolNameReadSkill,
			Category:     "skills",
			Description:  "Read the content of a skill by name. Use this when a user's request matches an available skill.",
			Parameters:   tools.MustSchemaFor[readSkillArgs](),
			OutputSchema: tools.MustSchemaFor[string](),
			Handler:      tools.NewRuntimeHandler(s.handleReadSkill),
			Annotations: tools.ToolAnnotations{
				Title:        "Read Skill",
				ReadOnlyHint: true,
			},
		},
	}

	// Only expose read_skill_file if any skill has supporting files
	if s.hasFiles() {
		result = append(result, tools.Tool{
			Name:         ToolNameReadSkillFile,
			Category:     "skills",
			Description:  "Read a supporting file from a skill (e.g. references, scripts, assets). Use when skill instructions reference additional files.",
			Parameters:   tools.MustSchemaFor[readSkillFileArgs](),
			OutputSchema: tools.MustSchemaFor[string](),
			Handler:      tools.NewHandler(s.handleReadSkillFile),
			Annotations: tools.ToolAnnotations{
				Title:        "Read Skill File",
				ReadOnlyHint: true,
			},
		})
	}

	// Expose run_skill if any skill uses context: fork
	if s.hasForkSkills() {
		result = append(result, tools.Tool{
			Name:         ToolNameRunSkill,
			Category:     "skills",
			Description:  "Run a skill in a forked context with its own conversation history. Use this for skills marked with forked mode — never use transfer_task for skills.",
			Parameters:   tools.MustSchemaFor[RunSkillArgs](),
			OutputSchema: tools.MustSchemaFor[string](),
			Annotations: tools.ToolAnnotations{
				Title: "Run Skill",
			},
		})
	}

	return result, nil
}
