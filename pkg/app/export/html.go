// Package export provides HTML export functionality for docker agent sessions.
package export

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
)

//go:embed export.css
var cssStyles string

//go:embed export.html
var htmlTemplate string

//go:embed export.js
var jsCode string

// SVG icons used in the template.
const (
	svgChevronRight      = `<svg class="chevron-right size-3" xmlns="http://www.w3.org/2000/svg" width="24" height="24" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="m9 18 6-6-6-6"/></svg>`
	svgChevronDown       = `<svg class="chevron-down size-3" style="display:none" xmlns="http://www.w3.org/2000/svg" width="24" height="24" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="m6 9 6 6 6-6"/></svg>`
	svgChevronRightMuted = `<svg class="chevron-right size-3 text-muted-foreground" xmlns="http://www.w3.org/2000/svg" width="24" height="24" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="m9 18 6-6-6-6"/></svg>`
	svgChevronDownMuted  = `<svg class="chevron-down size-3 text-muted-foreground" style="display:none" xmlns="http://www.w3.org/2000/svg" width="24" height="24" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="m6 9 6 6 6-6"/></svg>`
	svgCheckCircle       = `<svg class="size-3 text-tui-green" xmlns="http://www.w3.org/2000/svg" width="24" height="24" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="10"/><path d="m9 12 2 2 4-4"/></svg>`
)

// markdown is the goldmark Markdown parser with common extensions.
// Raw HTML and dangerous URLs in input are escaped (no html.WithUnsafe): assistant
// content is untrusted and would otherwise allow stored XSS in exported HTML files.
var markdown = goldmark.New(
	goldmark.WithExtensions(
		extension.GFM, // GitHub Flavored Markdown (tables, strikethrough, etc.)
	),
)

// SessionData contains the session information needed for HTML export.
type SessionData struct {
	Title            string
	AgentDescription string
	CreatedAt        time.Time
	InputTokens      int64
	OutputTokens     int64
	Cost             float64
	Messages         []Message
}

// Message represents a single message in the session.
type Message struct {
	Role             chat.MessageRole
	Content          string
	ReasoningContent string
	ToolCallID       string
	ToolCalls        []ToolCall
	AgentName        string
	Implicit         bool
}

// ToolCall represents a tool invocation.
type ToolCall struct {
	ID        string
	Name      string
	Arguments string
}

// SessionToFile exports a session to an HTML file.
// If filename is empty, a default name based on the title and timestamp is used.
// Returns the absolute path of the created file.
func SessionToFile(sess *session.Session, agentDescription, filename string) (string, error) {
	if sess == nil {
		return "", errors.New("no session to export")
	}
	data := sessionToData(sess)
	data.AgentDescription = agentDescription
	return ToFile(data, filename)
}

func sessionToData(sess *session.Session) SessionData {
	messages := sess.GetAllMessages()
	exportMessages := make([]Message, len(messages))
	for i, msg := range messages {
		toolCalls := make([]ToolCall, len(msg.Message.ToolCalls))
		for j, tc := range msg.Message.ToolCalls {
			toolCalls[j] = ToolCall{
				ID:        tc.ID,
				Name:      tc.Function.Name,
				Arguments: tc.Function.Arguments,
			}
		}
		exportMessages[i] = Message{
			Role:             msg.Message.Role,
			Content:          msg.Message.Content,
			ReasoningContent: msg.Message.ReasoningContent,
			ToolCallID:       msg.Message.ToolCallID,
			ToolCalls:        toolCalls,
			AgentName:        msg.AgentName,
			Implicit:         msg.Implicit,
		}
	}
	return SessionData{
		Title:        sess.Title,
		CreatedAt:    sess.CreatedAt,
		InputTokens:  sess.InputTokens,
		OutputTokens: sess.OutputTokens,
		Cost:         sess.TotalCost(),
		Messages:     exportMessages,
	}
}

// ToFile exports session data to an HTML file.
// If filename is empty, a default name based on the title and timestamp is used.
// Returns the absolute path of the created file.
func ToFile(data SessionData, filename string) (string, error) {
	if len(data.Messages) == 0 {
		return "", errors.New("session is empty")
	}

	// Generate filename if not provided
	if filename == "" {
		title := data.Title
		if title == "" {
			title = "cagent-session"
		}
		title = sanitizeFilename(title)
		filename = fmt.Sprintf("%s-%s.html", title, time.Now().Format("2006-01-02-150405"))
	}

	// Ensure .html extension
	if !strings.HasSuffix(strings.ToLower(filename), ".html") {
		filename += ".html"
	}

	htmlContent, err := Generate(data)
	if err != nil {
		return "", fmt.Errorf("failed to generate HTML: %w", err)
	}

	if err := os.WriteFile(filename, []byte(htmlContent), 0o644); err != nil { //nolint:gosec // exported HTML is meant to be readable by browsers/users
		return "", fmt.Errorf("failed to write file: %w", err)
	}

	absPath, err := filepath.Abs(filename)
	if err != nil {
		return filename, nil
	}
	return absPath, nil
}

func sanitizeFilename(name string) string {
	replacer := strings.NewReplacer(
		"/", "_",
		"\\", "_",
		":", "_",
		"*", "_",
		"?", "_",
		"\"", "_",
		"<", "_",
		">", "_",
		"|", "_",
		" ", "-",
	)
	name = replacer.Replace(name)
	if len(name) > 50 {
		name = name[:50]
	}
	return name
}

// templateData holds all data needed for the main HTML template.
type templateData struct {
	Title            string
	CSS              template.CSS
	JS               template.JS
	FormattedDate    string
	SidebarDate      string
	MessagesHTML     template.HTML
	PrimaryAgent     string
	AgentDescription string
	ToolsUsedCount   int
	TotalTokens      int64
	FormattedTokens  string
	FormattedCost    template.HTML
}

// messageViewData holds data for rendering a single message.
type messageViewData struct {
	IsUser           bool
	LabelName        string
	LabelClasses     string
	ShowLabel        bool
	ContentHTML      template.HTML
	ReasoningHTML    template.HTML
	HasReasoning     bool
	ToolCallsHTML    template.HTML
	HasToolCalls     bool
	ChevronRight     template.HTML
	ChevronDown      template.HTML
	ChevronRightIcon template.HTML
	ChevronDownIcon  template.HTML
}

// toolCallViewData holds data for rendering a tool call.
type toolCallViewData struct {
	Name              string
	Arguments         string
	Result            string
	HasArguments      bool
	HasResult         bool
	ChevronRightMuted template.HTML
	ChevronDownMuted  template.HTML
	CheckCircle       template.HTML
}

// messageTemplate is the template for rendering individual messages.
var messageTemplate = template.Must(template.New("message").Parse(`
{{if .IsUser}}
<div class="group is-user flex w-full py-3 border-b border-border flex-col gap-1 sm:flex-row sm:items-start sm:gap-3">
    {{if .ShowLabel}}
    <span class="inline-flex items-center self-start shrink-0 px-2 py-0.5 text-xs font-medium rounded sm:w-14 sm:justify-center {{.LabelClasses}}">{{.LabelName}}</span>
    {{else}}
    <div class="hidden sm:block sm:w-14 shrink-0"></div>
    {{end}}
    <div class="flex-1 flex flex-col gap-3 overflow-hidden text-sm">
        <div class="whitespace-pre-wrap">{{.ContentHTML}}</div>
    </div>
</div>
{{else}}
<div class="group is-assistant flex w-full py-3 border-b border-border flex-col gap-1 sm:flex-row sm:items-start sm:gap-3">
    {{if .ShowLabel}}
    <span class="inline-flex items-center self-start shrink-0 px-2 py-0.5 text-xs font-medium rounded sm:w-14 sm:justify-center {{.LabelClasses}}">{{.LabelName}}</span>
    {{else}}
    <div class="hidden sm:block sm:w-14 shrink-0"></div>
    {{end}}
    <div class="flex-1 flex flex-col gap-3 overflow-hidden text-sm">
        {{if .HasReasoning}}
        <div class="border-l-2 border-tui-purple bg-tui-purple/5">
            <div class="flex items-center gap-2 px-3 py-2 cursor-pointer text-xs font-bold text-tui-purple select-none hover:bg-tui-purple/10" onclick="toggle(this)">
                {{.ChevronRightIcon}}{{.ChevronDownIcon}} Thinking
            </div>
            <div class="collapsible-content p-3 pl-5 text-muted-foreground text-xs border-t border-tui-purple/30" style="display: none;">{{.ReasoningHTML}}</div>
        </div>
        {{end}}
        {{if .ContentHTML}}
        <div class="prose prose-invert prose-sm max-w-none">{{.ContentHTML}}</div>
        {{end}}
        {{if .HasToolCalls}}
        {{.ToolCallsHTML}}
        {{end}}
    </div>
</div>
{{end}}
`))

// toolCallTemplate is the template for rendering tool calls.
var toolCallTemplate = template.Must(template.New("toolcall").Parse(`
<div class="text-sm">
    <div class="flex w-full items-center gap-2 py-1 cursor-pointer select-none hover:bg-secondary/50 transition-colors" onclick="toggle(this)">
        {{.ChevronRightMuted}}{{.ChevronDownMuted}}
        <span class="text-tui-purple">⚡</span>
        <span class="font-medium text-tui-blue">{{.Name}}</span>
        <span class="ml-auto">{{.CheckCircle}}</span>
    </div>
    <div class="collapsible-content pl-5 py-2 space-y-3" style="display: none;">
        {{if .HasArguments}}
        <div>
            <div class="text-xs text-muted-foreground mb-1">Arguments</div>
            <pre class="bg-secondary/50 p-2 text-xs overflow-x-auto whitespace-pre-wrap">{{.Arguments}}</pre>
        </div>
        {{end}}
        {{if .HasResult}}
        <div>
            <div class="text-xs text-muted-foreground mb-1">Result</div>
            <pre class="bg-secondary/50 p-2 text-xs overflow-x-auto whitespace-pre-wrap max-h-64 overflow-y-auto">{{.Result}}</pre>
        </div>
        {{end}}
    </div>
</div>
`))

// Generate creates an HTML string from the session data.
func Generate(data SessionData) (string, error) {
	// Build a map of tool call ID -> tool result content
	toolResults := make(map[string]string)
	for _, msg := range data.Messages {
		if msg.Role == chat.MessageRoleTool && msg.ToolCallID != "" {
			toolResults[msg.ToolCallID] = msg.Content
		}
	}

	// Count unique tools used
	toolsUsed := make(map[string]bool)
	for _, msg := range data.Messages {
		for _, tc := range msg.ToolCalls {
			toolsUsed[tc.Name] = true
		}
	}

	// Get primary agent name
	primaryAgent := "assistant"
	for _, msg := range data.Messages {
		if msg.AgentName != "" {
			primaryAgent = msg.AgentName
			break
		}
	}

	// Build messages HTML with label grouping
	var messagesBuilder strings.Builder
	var prevSender string
	for _, msg := range data.Messages {
		if msg.Implicit {
			continue
		}
		// Skip tool messages - they're rendered inline with their tool calls
		if msg.Role == chat.MessageRoleTool {
			continue
		}
		currentSender := getSender(msg)
		showLabel := prevSender != currentSender
		msgHTML, err := renderMessage(msg, toolResults, showLabel)
		if err != nil {
			return "", fmt.Errorf("failed to render message: %w", err)
		}
		messagesBuilder.WriteString(msgHTML)
		prevSender = currentSender
	}

	title := data.Title
	if title == "" {
		title = "Docker Agent Session"
	}

	totalTokens := data.InputTokens + data.OutputTokens

	// Parse and execute the main template
	mainTmpl, err := template.New("main").Parse(htmlTemplate)
	if err != nil {
		return "", fmt.Errorf("failed to parse main template: %w", err)
	}

	tplData := templateData{
		Title:            title,
		CSS:              template.CSS(cssStyles), //nolint:gosec // CSS loaded from embedded asset
		JS:               template.JS(jsCode),     //nolint:gosec // JS loaded from embedded asset
		FormattedDate:    data.CreatedAt.Format("January 2, 2006 at 3:04 PM"),
		SidebarDate:      data.CreatedAt.Format("Jan 2, 2006"),
		MessagesHTML:     template.HTML(messagesBuilder.String()), //nolint:gosec // content is already escaped by sub-templates
		PrimaryAgent:     primaryAgent,
		AgentDescription: data.AgentDescription,
		ToolsUsedCount:   len(toolsUsed),
		TotalTokens:      totalTokens,
		FormattedTokens:  formatTokens(totalTokens),
		FormattedCost:    template.HTML(formatCost(data.Cost)), //nolint:gosec // formatCost returns safe HTML
	}

	var buf bytes.Buffer
	if err := mainTmpl.Execute(&buf, tplData); err != nil {
		return "", fmt.Errorf("failed to execute main template: %w", err)
	}

	return buf.String(), nil
}

func getSender(msg Message) string {
	if msg.Role == chat.MessageRoleUser {
		return "you"
	}
	if msg.AgentName != "" {
		return msg.AgentName
	}
	return "agent"
}

func formatTokens(tokens int64) string {
	if tokens >= 1000000 {
		return fmt.Sprintf("%.1fM", float64(tokens)/1000000)
	}
	if tokens >= 1000 {
		return fmt.Sprintf("%.1fK", float64(tokens)/1000)
	}
	return strconv.FormatInt(tokens, 10)
}

func formatCost(cost float64) string {
	if cost <= 0 {
		return ""
	}
	return fmt.Sprintf(`<span class="text-tui-green ml-2">$%.2f</span>`, cost)
}

func renderMessage(msg Message, toolResults map[string]string, showLabel bool) (string, error) {
	switch msg.Role {
	case chat.MessageRoleUser:
		return renderUserMessage(msg, showLabel)
	case chat.MessageRoleAssistant:
		return renderAssistantMessage(msg, toolResults, showLabel)
	default:
		return "", nil
	}
}

func renderUserMessage(msg Message, showLabel bool) (string, error) {
	// User messages are plain text, escape HTML and convert newlines
	content := template.HTMLEscapeString(msg.Content)
	content = strings.ReplaceAll(content, "\n", "<br>")

	data := messageViewData{
		IsUser:       true,
		LabelName:    "you",
		LabelClasses: "bg-tui-yellow/20 text-tui-yellow",
		ShowLabel:    showLabel,
		ContentHTML:  template.HTML(content), //nolint:gosec // content is escaped above
	}

	var buf bytes.Buffer
	if err := messageTemplate.Execute(&buf, data); err != nil {
		return "", err
	}
	return buf.String(), nil
}

func renderAssistantMessage(msg Message, toolResults map[string]string, showLabel bool) (string, error) {
	agentName := msg.AgentName
	if agentName == "" {
		agentName = "agent"
	}

	data := messageViewData{
		IsUser:           false,
		LabelName:        agentName,
		LabelClasses:     "bg-tui-cyan/20 text-tui-cyan",
		ShowLabel:        showLabel,
		ChevronRightIcon: template.HTML(svgChevronRight), //nolint:gosec // constant SVG
		ChevronDownIcon:  template.HTML(svgChevronDown),  //nolint:gosec // constant SVG
	}

	// Reasoning content (if present)
	if msg.ReasoningContent != "" {
		reasoning := template.HTMLEscapeString(msg.ReasoningContent)
		reasoning = strings.ReplaceAll(reasoning, "\n", "<br>")
		data.HasReasoning = true
		data.ReasoningHTML = template.HTML(reasoning) //nolint:gosec // content is escaped above
	}

	// Main content - render as Markdown
	if msg.Content != "" {
		contentHTML, err := renderMarkdown(msg.Content)
		if err != nil {
			return "", fmt.Errorf("failed to render markdown: %w", err)
		}
		data.ContentHTML = template.HTML(contentHTML) //nolint:gosec // goldmark escapes raw HTML and dangerous URLs (no WithUnsafe option)
	}

	// Tool calls with their results
	if len(msg.ToolCalls) > 0 {
		var toolsBuilder strings.Builder
		toolsBuilder.WriteString(`<div class="space-y-2">`)
		for _, tc := range msg.ToolCalls {
			args := formatJSONForDisplay(tc.Arguments)
			result := ""
			if r, ok := toolResults[tc.ID]; ok {
				result = formatJSONForDisplay(r)
			}
			toolHTML, err := renderToolCall(tc.Name, args, result)
			if err != nil {
				return "", fmt.Errorf("failed to render tool call: %w", err)
			}
			toolsBuilder.WriteString(toolHTML)
		}
		toolsBuilder.WriteString(`</div>`)
		data.HasToolCalls = true
		data.ToolCallsHTML = template.HTML(toolsBuilder.String()) //nolint:gosec // content is escaped by renderToolCall
	}

	var buf bytes.Buffer
	if err := messageTemplate.Execute(&buf, data); err != nil {
		return "", err
	}
	return buf.String(), nil
}

func renderToolCall(name, args, result string) (string, error) {
	data := toolCallViewData{
		Name:              name,
		Arguments:         args,
		Result:            result,
		HasArguments:      args != "" && args != "{}" && args != "null",
		HasResult:         result != "",
		ChevronRightMuted: template.HTML(svgChevronRightMuted), //nolint:gosec // constant SVG
		ChevronDownMuted:  template.HTML(svgChevronDownMuted),  //nolint:gosec // constant SVG
		CheckCircle:       template.HTML(svgCheckCircle),       //nolint:gosec // constant SVG
	}

	var buf bytes.Buffer
	if err := toolCallTemplate.Execute(&buf, data); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// renderMarkdown converts markdown text to HTML using goldmark.
func renderMarkdown(text string) (string, error) {
	var buf bytes.Buffer
	if err := markdown.Convert([]byte(text), &buf); err != nil {
		return "", err
	}
	return buf.String(), nil
}

func formatJSONForDisplay(s string) string {
	var v any
	if err := json.Unmarshal([]byte(s), &v); err == nil {
		if pretty, err := json.MarshalIndent(v, "", "  "); err == nil {
			return string(pretty)
		}
	}
	return s
}
