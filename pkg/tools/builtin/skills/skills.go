package skills

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/docker/docker-agent/pkg/skills"
	"github.com/docker/docker-agent/pkg/tools"
)

const (
	ToolNameReadSkill     = "read_skill"
	ToolNameReadSkillFile = "read_skill_file"
	ToolNameRunSkill      = "run_skill"
)

var (
	_ tools.ToolSet      = (*Toolset)(nil)
	_ tools.Instructable = (*Toolset)(nil)
)

// Toolset provides the read_skill and read_skill_file tools that let an
// agent load skill content and supporting resources by name. It hides whether
// a skill is local or remote — the agent just sees a name and description.
type Toolset struct {
	skills     []skills.Skill
	workingDir string
}

func NewSkillsToolset(loadedSkills []skills.Skill, workingDir string) *Toolset {
	return &Toolset{
		skills:     loadedSkills,
		workingDir: workingDir,
	}
}

// Skills returns the loaded skills (used by the app layer for slash commands).
func (s *Toolset) Skills() []skills.Skill {
	return s.skills
}

func (s *Toolset) findSkill(name string) *skills.Skill {
	for i := range s.skills {
		if s.skills[i].Name == name {
			return &s.skills[i]
		}
	}
	return nil
}

// FindSkill returns the skill with the given name, or nil if not found.
func (s *Toolset) FindSkill(name string) *skills.Skill {
	return s.findSkill(name)
}

// ReadSkillContent returns the content of a skill's SKILL.md by name.
// For local skills, it expands any !`command` patterns in the content by
// executing the commands and replacing the patterns with their stdout output.
// Command expansion is disabled for remote skills to prevent arbitrary code execution.
func (s *Toolset) ReadSkillContent(ctx context.Context, name string) (string, error) {
	skill := s.findSkill(name)
	if skill == nil {
		return "", fmt.Errorf("skill %q not found", name)
	}

	content, err := readFileContent(skill.FilePath)
	if err != nil {
		return "", err
	}

	if skill.Local {
		content = skills.ExpandCommands(ctx, content, s.workingDir)
	}

	return content, nil
}

// ReadSkillFile returns the content of a supporting file within a skill.
// The path is relative to the skill's base directory (e.g. "references/FORMS.md").
func (s *Toolset) ReadSkillFile(skillName, relativePath string) (string, error) {
	skill := s.findSkill(skillName)
	if skill == nil {
		return "", fmt.Errorf("skill %q not found", skillName)
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

func (s *Toolset) handleReadSkill(ctx context.Context, args readSkillArgs) (*tools.ToolCallResult, error) {
	content, err := s.ReadSkillContent(ctx, args.Name)
	if err != nil {
		return tools.ResultError(err.Error()), nil
	}
	return tools.ResultSuccess(content), nil
}

func (s *Toolset) handleReadSkillFile(_ context.Context, args readSkillFileArgs) (*tools.ToolCallResult, error) {
	content, err := s.ReadSkillFile(args.SkillName, args.Path)
	if err != nil {
		return tools.ResultError(err.Error()), nil
	}
	return tools.ResultSuccess(content), nil
}

// hasFiles reports whether any loaded skill has supporting files beyond SKILL.md.
func (s *Toolset) hasFiles() bool {
	for _, skill := range s.skills {
		if len(skill.Files) > 1 {
			return true
		}
	}
	return false
}

// hasForkSkills reports whether any loaded skill uses context: fork.
func (s *Toolset) hasForkSkills() bool {
	for i := range s.skills {
		if s.skills[i].IsFork() {
			return true
		}
	}
	return false
}

func (s *Toolset) Instructions() string {
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
	// Content is the expanded SKILL.md content, intended to be used as the
	// system message of the child session.
	Content string
	// Model is the optional model override declared in the SKILL.md
	// frontmatter. Empty means "use the parent agent's current model".
	Model string
}

// PrepareForkSubSession validates a run_skill request and loads the expanded
// skill content. It returns either a populated PreparedSkillFork, or a
// ToolCallResult describing why the call cannot proceed (skill missing,
// skill not configured for fork mode, content read failure). The caller is
// responsible for the runtime-specific orchestration (sub-session creation,
// tracing, event forwarding).
func (s *Toolset) PrepareForkSubSession(ctx context.Context, args RunSkillArgs) (*PreparedSkillFork, *tools.ToolCallResult) {
	skill := s.findSkill(args.Name)
	if skill == nil {
		return nil, tools.ResultError(fmt.Sprintf("skill %q not found", args.Name))
	}

	if !skill.IsFork() {
		return nil, tools.ResultError(fmt.Sprintf(
			"skill %q is not configured for forked execution (missing context: fork in SKILL.md frontmatter); use read_skill instead",
			args.Name,
		))
	}

	content, err := s.ReadSkillContent(ctx, args.Name)
	if err != nil {
		return nil, tools.ResultError(fmt.Sprintf("failed to read skill content: %s", err))
	}

	return &PreparedSkillFork{
		SkillName: args.Name,
		Task:      args.Task,
		Content:   content,
		Model:     skill.Model,
	}, nil
}

func (s *Toolset) Tools(context.Context) ([]tools.Tool, error) {
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
			Handler:      tools.NewHandler(s.handleReadSkill),
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
