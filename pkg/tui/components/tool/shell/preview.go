package shell

import (
	"path"
	"regexp"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/alecthomas/chroma/v2"
	"github.com/alecthomas/chroma/v2/lexers"
	"github.com/charmbracelet/x/ansi"

	"github.com/docker/docker-agent/pkg/tui/styles"
)

var (
	heredocWrapper = regexp.MustCompile(`^([\w/.-]+)(?:[ \t]+-[\w-]*)*[ \t]*<<(-?)[ \t]*(?:'([\w]+)'|"([\w]+)"|([\w]+))[ \t]*\n`)
	inlineWrapper  = regexp.MustCompile(`^([\w/.-]+)[ \t]+(-c|-e|--eval)[ \t]+(['"])`)
)

// scriptRange identifies source without removing shell quotes or trailing commands.
func scriptRange(command string) (start, end int, language string, ok bool) {
	if match := heredocWrapper.FindStringSubmatchIndex(command); match != nil {
		language = interpreterLanguage(command[match[2]:match[3]])
		if language == "" {
			return 0, 0, "", false
		}
		delimiter := ""
		for group := 3; group <= 5; group++ {
			if match[group*2] >= 0 {
				delimiter = command[match[group*2]:match[group*2+1]]
			}
		}
		start = match[1]
		end = start
		stripTabs := command[match[4]:match[5]] == "-"
		for line := range strings.SplitSeq(command[start:], "\n") {
			candidate := line
			if stripTabs {
				candidate = strings.TrimLeft(candidate, "\t")
			}
			if candidate == delimiter {
				return start, end, language, true
			}
			end += len(line) + 1
		}
		// Streaming arguments may not contain the closing delimiter yet.
		return start, len(command), language, true
	}
	if match := inlineWrapper.FindStringSubmatchIndex(command); match != nil {
		language = interpreterLanguage(command[match[2]:match[3]])
		flag := command[match[4]:match[5]]
		if language == "" || !interpreterFlag(language, flag) {
			return 0, 0, "", false
		}
		quote := command[match[6]]
		start = match[1]
		for i := start; i < len(command); i++ {
			if quote == '"' && command[i] == '\\' {
				i++
				continue
			}
			if command[i] == quote {
				// Concatenated shell words are not a single quoted script.
				if i+1 < len(command) && !strings.ContainsRune(" \t\n;|&", rune(command[i+1])) {
					return 0, 0, "", false
				}
				return start, i, language, true
			}
		}
	}
	return 0, 0, "", false
}

func interpreterLanguage(executable string) string {
	switch name := path.Base(executable); name {
	case "python", "python2", "python3":
		return "python"
	case "node", "nodejs":
		return "javascript"
	case "ruby", "perl":
		return name
	case "sh", "bash", "zsh":
		return "bash"
	default:
		return ""
	}
}

func interpreterFlag(language, flag string) bool {
	switch language {
	case "python", "bash":
		return flag == "-c"
	case "javascript":
		return flag == "-e" || flag == "--eval"
	default:
		return flag == "-e"
	}
}

func highlightSource(source, language string) string {
	lexer := lexers.Get(language)
	if lexer == nil || source == "" {
		return source
	}
	iterator, err := chroma.Coalesce(lexer).Tokenise(nil, source)
	if err != nil {
		return source
	}
	var result strings.Builder
	remaining := len(source)
	for _, token := range iterator.Tokens() {
		// Lexers may append a newline to incomplete source.
		text := token.Value[:min(len(token.Value), remaining)]
		remaining -= len(text)
		entry := styles.ChromaStyle().Get(token.Type)
		style := lipgloss.NewStyle().TabWidth(-1).
			Bold(entry.Bold == chroma.Yes).Italic(entry.Italic == chroma.Yes).
			Underline(entry.Underline == chroma.Yes)
		if entry.Colour.IsSet() {
			style = style.Foreground(lipgloss.Color(entry.Colour.String()))
		}
		for i, line := range strings.Split(text, "\n") {
			if i > 0 {
				result.WriteByte('\n')
			}
			result.WriteString(style.Render(line))
		}
	}
	return result.String()
}

func commandPreview(command string, width int) (string, bool) {
	start, end, language, recognized := scriptRange(command)
	if !recognized && !strings.Contains(command, "\n") {
		return "", false
	}
	var highlighted string
	if recognized {
		highlighted = highlightSource(command[:start], "bash") +
			highlightSource(command[start:end], language) +
			highlightSource(command[end:], "bash")
	} else {
		highlighted = highlightSource(command, "bash")
	}
	availableWidth := max(width-styles.ToolCallResult.GetHorizontalFrameSize(), 1)
	highlighted = strings.ReplaceAll(highlighted, "\t", "    ")
	return styles.ToolCallResult.Render(ansi.Hardwrap(highlighted, availableWidth, true)), true
}
