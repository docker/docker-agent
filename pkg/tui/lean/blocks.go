package lean

import (
	"image/color"
	"strings"

	"charm.land/glamour/v2"
	"charm.land/lipgloss/v2"
	ansi "github.com/charmbracelet/x/ansi"
)

type Component interface {
	Render(width int) []string
	Invalidate()
}

type Container struct{ Children []Component }

func (c *Container) AddChild(child Component) { c.Children = append(c.Children, child) }

func (c *Container) SetChildren(children ...Component) {
	c.Children = append([]Component(nil), children...)
}

func (c *Container) Render(width int) []string {
	var lines []string
	for _, child := range c.Children {
		lines = append(lines, child.Render(width)...)
	}
	return lines
}

func (c *Container) Invalidate() {
	for _, child := range c.Children {
		child.Invalidate()
	}
}

type Spacer struct{ Height int }

func (s Spacer) Render(width int) []string {
	height := s.Height
	if height <= 0 {
		height = 1
	}
	lines := make([]string, height)
	for i := range lines {
		lines[i] = padLine("", width, baseStyle.Render)
	}
	return lines
}

func (s Spacer) Invalidate() {}

type markdownRenderer struct{ cache map[int]*glamour.TermRenderer }

func newMarkdownRenderer() *markdownRenderer {
	return &markdownRenderer{cache: make(map[int]*glamour.TermRenderer)}
}

func (md *markdownRenderer) render(text string, width int, preserveSpace bool) []string {
	width = maxInt(20, width)
	renderer, ok := md.cache[width]
	if !ok {
		r, err := glamour.NewTermRenderer(
			glamour.WithStandardStyle("dark"),
			glamour.WithWordWrap(width),
			glamour.WithPreservedNewLines(),
		)
		if err == nil {
			renderer = r
			md.cache[width] = renderer
		}
	}
	if renderer == nil {
		return wrapPlain(text, width, preserveSpace)
	}
	out, err := renderer.Render(text)
	if err != nil {
		return wrapPlain(text, width, preserveSpace)
	}
	out = strings.TrimRight(out, "\n")
	if out == "" {
		return []string{""}
	}
	return strings.Split(out, "\n")
}

type HeaderBlock struct {
	Descriptor     SessionDescriptor
	Connection     ConnectionStatus
	ToolsExpanded  bool
	ThinkingHidden bool
	PromptActive   bool
}

func (b *HeaderBlock) Render(width int) []string {
	sessionShort := b.Descriptor.ID
	if len(sessionShort) > 8 {
		sessionShort = sessionShort[:8]
	}
	line1 := accentStyle.Render("docker-agent") + dimStyle.Render(" · ") + magentaStyle.Render(orDefault(b.Descriptor.AgentName, "unknown"))
	if sessionShort != "" {
		line1 += dimStyle.Render(" · ") + dimStyle.Render(sessionShort)
	}
	promptState := "none"
	if b.PromptActive {
		promptState = "open"
	}
	toolState := map[bool]string{true: "full", false: "preview"}[b.ToolsExpanded]
	thinkingState := map[bool]string{true: "hidden", false: "shown"}[b.ThinkingHidden]
	line2 := strings.Join([]string{
		"Enter send",
		"Shift+Enter newline",
		"Esc stop/quit",
		"Ctrl+T tools:" + toolState,
		"Ctrl+G thinking:" + thinkingState,
		"prompt:" + promptState,
	}, dimStyle.Render(" · "))
	return []string{padLine(line1, width, baseStyle.Render), padLine(mutedStyle.Render(line2), width, baseStyle.Render)}
}

func (b *HeaderBlock) Invalidate() {}

type FooterBlock struct {
	Descriptor SessionDescriptor
	Usage      UsageTotals
	Streaming  bool
}

func (b *FooterBlock) Render(width int) []string {
	pathBits := []string{}
	if b.Descriptor.WorkingDirectory != "" {
		pathBits = append(pathBits, homeTilde(b.Descriptor.WorkingDirectory))
	}
	if b.Descriptor.ID != "" {
		pathBits = append(pathBits, b.Descriptor.ID)
	}
	pathLine := padLine(dimStyle.Render(middleTruncate(strings.Join(pathBits, " · "), width)), width, baseStyle.Render)
	leftParts := []string{"ctx " + formatTokens(b.Usage.Prompt), "↓" + formatTokens(b.Usage.Completion)}
	if b.Usage.CacheRead > 0 {
		leftParts = append(leftParts, "🗎"+formatTokens(b.Usage.CacheRead))
	}
	if b.Usage.Cost > 0 {
		leftParts = append(leftParts, "$"+formatMoney(b.Usage.Cost))
	}
	if b.Streaming {
		leftParts = append(leftParts, yellowStyle.Render("working"))
	} else {
		leftParts = append(leftParts, dimStyle.Render("idle"))
	}
	leftStyled := mutedStyle.Render(strings.Join(leftParts, " · "))
	rightStyled := dimStyle.Render(orDefault(b.Descriptor.Model, "unknown model"))
	if visibleWidth(leftStyled)+1+visibleWidth(rightStyled) <= width {
		spaces := strings.Repeat(" ", maxInt(0, width-visibleWidth(leftStyled)-visibleWidth(rightStyled)))
		return []string{pathLine, leftStyled + spaces + rightStyled}
	}
	return []string{pathLine, padLine(leftStyled+" "+rightStyled, width, baseStyle.Render)}
}

func (b *FooterBlock) Invalidate() {}

type UserBlock struct{ Content string }

func (b *UserBlock) Render(width int) []string {
	wrapped := wrapPlain(b.Content, maxInt(1, width-4), false)
	lines := []string{"", padLine("", width, userBGStyle.Render)}
	for _, line := range wrapped {
		lines = append(lines, padLine("  "+line+"  ", width, userBGStyle.Render))
	}
	lines = append(lines, padLine("", width, userBGStyle.Render), "")
	return lines
}

func (b *UserBlock) Invalidate() {}

type AssistantBlock struct {
	Content string
	MD      *markdownRenderer
}

func (b *AssistantBlock) Render(width int) []string {
	return append([]string{""}, b.MD.render(b.Content, width, true)...)
}

func (b *AssistantBlock) Invalidate() {}

type ThinkingBlock struct {
	Content string
	Hidden  bool
	MD      *markdownRenderer
}

func (b *ThinkingBlock) Render(width int) []string {
	if b.Hidden {
		return []string{"", padLine(mutedStyle.Render("Thinking…"), width, baseStyle.Render)}
	}
	lines := []string{"", padLine(mutedStyle.Render("Thinking"), width, baseStyle.Render)}
	for _, line := range b.MD.render(b.Content, width, true) {
		lines = append(lines, padLine(mutedStyle.Italic(true).Render(line), width, baseStyle.Render))
	}
	return lines
}

func (b *ThinkingBlock) Invalidate() {}

type ToolBlock struct {
	ID              string
	ToolName        string
	Args            string
	Result          string
	Status          ToolStatus
	Expanded        bool
	HideToolResults bool
}

func padStyledLine(text string, width int, bg color.Color) string {
	if width <= 0 {
		return ""
	}
	truncated := ansi.Truncate(text, width, "")
	padding := max(0, width-ansi.StringWidth(truncated))
	return truncated + lipgloss.NewStyle().Background(bg).Render(strings.Repeat(" ", padding))
}

func (b *ToolBlock) Render(width int) []string {
	if strings.TrimSpace(b.ToolName) == "" && strings.TrimSpace(b.Args) == "" && strings.TrimSpace(b.Result) == "" {
		return nil
	}
	previewResult := b.Result
	if b.HideToolResults && previewResult != "" {
		previewResult = "[hidden]"
	}
	preview := formatToolPreview(b.ToolName, b.Args, b.Status, previewResult, b.Expanded)
	bgColor := toolBG
	if b.Status == ToolError {
		bgColor = errorBG
	}
	baseBg := lipgloss.NewStyle().Background(bgColor).Foreground(textColor)
	mutedBg := lipgloss.NewStyle().Background(bgColor).Foreground(muted)
	dimBg := lipgloss.NewStyle().Background(bgColor).Foreground(dim)
	cyanBg := lipgloss.NewStyle().Background(bgColor).Foreground(cyan)
	greenBg := lipgloss.NewStyle().Background(bgColor).Foreground(green)
	yellowBg := lipgloss.NewStyle().Background(bgColor).Foreground(yellow)
	redBg := lipgloss.NewStyle().Background(bgColor).Foreground(red)

	icon := dimBg.Render("○")
	switch b.Status {
	case ToolConfirmation:
		icon = magentaStyle.Background(bgColor).Render("?")
	case ToolRunning:
		icon = yellowBg.Render("●")
	case ToolCompleted:
		icon = greenBg.Render("✓")
	case ToolError:
		icon = redBg.Render("✗")
	}
	lines := []string{"", padStyledLine("", width, bgColor), padStyledLine(baseBg.Render("  ")+icon+baseBg.Render(" ")+cyanBg.Render(b.ToolName)+baseBg.Render(" ")+dimBg.Render(preview.Summary), width, bgColor)}
	for _, detail := range preview.Details {
		switch detail {
		case "args", "result", "error", "write preview", "diff preview", "input", "message":
			styled := mutedBg.Render(detail)
			switch detail {
			case "error":
				styled = redBg.Render(detail)
			case "result":
				styled = greenBg.Render(detail)
			}
			lines = append(lines, padStyledLine(baseBg.Render("    ")+styled, width, bgColor))
		default:
			trimmed := strings.TrimSpace(detail)
			for _, wrapped := range wrapPlain(detail, maxInt(1, width-4), true) {
				styled := baseBg.Render(wrapped)
				switch {
				case strings.HasPrefix(trimmed, "+"):
					styled = greenBg.Render(wrapped)
				case strings.HasPrefix(trimmed, "-"):
					styled = redBg.Render(wrapped)
				case b.Status == ToolError:
					styled = redBg.Render(wrapped)
				}
				lines = append(lines, padStyledLine(baseBg.Render("      ")+styled, width, bgColor))
			}
		}
	}
	lines = append(lines, padStyledLine("", width, bgColor), "")
	return lines
}

func (b *ToolBlock) Invalidate() {}

type BannerBlock struct{}

func (b *BannerBlock) Render(width int) []string {
	art := []string{
		`██████   ██████   ██████ ██   ██ ███████ ██████      █████   ██████  ███████ ███    ██ ████████`,
		`██   ██ ██    ██ ██      ██  ██  ██      ██   ██    ██   ██ ██       ██      ████   ██    ██   `,
		`██   ██ ██    ██ ██      █████   █████   ██████     ███████ ██   ███ █████   ██ ██  ██    ██   `,
		`██   ██ ██    ██ ██      ██  ██  ██      ██   ██    ██   ██ ██    ██ ██      ██  ██ ██    ██   `,
		`██████   ██████   ██████ ██   ██ ███████ ██   ██    ██   ██  ██████  ███████ ██   ████    ██   `,
	}
	if width <= 0 {
		return nil
	}
	lines := []string{""}
	for _, line := range art {
		trimmed := ansi.Truncate(line, width, "")
		styled := cyanStyle.Render(trimmed)
		lines = append(lines, padLine(styled, width, baseStyle.Render))
	}
	lines = append(lines, "")
	return lines
}

func (b *BannerBlock) Invalidate() {}

type NoticeBlock struct {
	Message string
	Kind    string
}

func (b *NoticeBlock) Render(width int) []string {
	bgColor := infoBG
	labelText := "info"
	labelStyle := lipgloss.NewStyle().Background(bgColor).Foreground(cyan)
	textBg := lipgloss.NewStyle().Background(bgColor).Foreground(textColor)
	dimBg := lipgloss.NewStyle().Background(bgColor).Foreground(dim)

	switch b.Kind {
	case "error":
		bgColor = errorBG
		labelText = "error"
		labelStyle = lipgloss.NewStyle().Background(bgColor).Foreground(red)
		textBg = lipgloss.NewStyle().Background(bgColor).Foreground(textColor)
		dimBg = lipgloss.NewStyle().Background(bgColor).Foreground(dim)
	case "sub-agent":
		bgColor = subAgentBG
		labelText = "sub-agent"
		labelStyle = lipgloss.NewStyle().Background(bgColor).Foreground(cyan)
		textBg = lipgloss.NewStyle().Background(bgColor).Foreground(textColor)
		dimBg = lipgloss.NewStyle().Background(bgColor).Foreground(dim)
	}

	wrapped := wrapPlain(b.Message, maxInt(1, width-9), false)
	lines := []string{"", padStyledLine("", width, bgColor)}
	for i, line := range wrapped {
		prefix := textBg.Render("  ")
		if i == 0 {
			prefix += labelStyle.Render(labelText) + textBg.Render(" ") + dimBg.Render("·") + textBg.Render(" ")
		} else {
			prefix += textBg.Render("       ")
		}
		lines = append(lines, padStyledLine(prefix+textBg.Render(line), width, bgColor))
	}
	lines = append(lines, padStyledLine("", width, bgColor), "")
	return lines
}

func (b *NoticeBlock) Invalidate() {}

type PromptBlock struct {
	Title   string
	Body    string
	Actions string
}

func (b *PromptBlock) Render(width int) []string {
	labelStyle := lipgloss.NewStyle().Background(promptBG).Foreground(magenta)
	textBg := lipgloss.NewStyle().Background(promptBG).Foreground(textColor)
	mutedBg := lipgloss.NewStyle().Background(promptBG).Foreground(muted)
	lines := []string{""}
	lines = append(lines, padStyledLine("", width, promptBG))
	wrapped := wrapPlain(b.Body, maxInt(1, width-8), false)
	for i, line := range wrapped {
		prefix := textBg.Render("  ")
		if i == 0 {
			prefix += labelStyle.Render(b.Title) + textBg.Render(" ")
		} else {
			prefix += textBg.Render(strings.Repeat(" ", visibleWidth(b.Title)+1))
		}
		lines = append(lines, padStyledLine(prefix+textBg.Render(line), width, promptBG))
	}
	if b.Actions != "" {
		for _, line := range wrapPlain(b.Actions, maxInt(1, width-4), false) {
			lines = append(lines, padStyledLine(textBg.Render("  ")+mutedBg.Render(line), width, promptBG))
		}
	}
	lines = append(lines, padStyledLine("", width, promptBG))
	return lines
}

func (b *PromptBlock) Invalidate() {}
