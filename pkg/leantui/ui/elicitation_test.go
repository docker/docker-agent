package ui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tools"
)

func typeText(e *ElicitationModel, s string) {
	for _, r := range s {
		e.HandleKey(Key{Typ: KeyRune, Runes: []rune{r}})
	}
}

func plainLines(e *ElicitationModel) []string {
	lines, _, _ := e.Render(80)
	for i, l := range lines {
		lines[i] = ansi.Strip(l)
	}
	return lines
}

func TestFormElicitationSubmitsValidatedContent(t *testing.T) {
	t.Parallel()
	schema := map[string]any{
		"type":     "object",
		"required": []any{"name"},
		"properties": map[string]any{
			"name":  map[string]any{"type": "string", "title": "Name"},
			"color": map[string]any{"type": "string", "enum": []any{"red", "blue"}},
			"agree": map[string]any{"type": "boolean"},
		},
	}
	e := NewFormElicitation("", "Tell me", schema)

	res := e.HandleKey(Key{Typ: KeyEnter})
	assert.False(t, res.Done())
	assert.Contains(t, strings.Join(plainLines(e), "\n"), "This field is required")

	typeText(e, "Ada")
	e.HandleKey(Key{Typ: KeyTab}) // agree (sorted: required first, then alphabetical)
	e.HandleKey(Key{Typ: KeyRune, Runes: []rune{' '}})
	e.HandleKey(Key{Typ: KeyTab}) // color
	e.HandleKey(Key{Typ: KeyDown})

	res = e.HandleKey(Key{Typ: KeyEnter})
	require.True(t, res.Done())
	assert.Equal(t, tools.ElicitationActionAccept, res.Action)
	assert.Equal(t, map[string]any{"name": "Ada", "agree": true, "color": "blue"}, res.Content)
}

func TestFormElicitationRejectsOverlongText(t *testing.T) {
	t.Parallel()
	schema := map[string]any{"type": "object", "properties": map[string]any{
		"code": map[string]any{"type": "string", "maxLength": float64(3)},
	}}
	e := NewFormElicitation("", "Code", schema)
	e.HandleKey(Key{Typ: KeyPaste, Runes: []rune("abcd")})

	assert.False(t, e.HandleKey(Key{Typ: KeyEnter}).Done())
	assert.Contains(t, strings.Join(plainLines(e), "\n"), "Must be at most 3 characters")
}

func TestFormElicitationMultiSelect(t *testing.T) {
	t.Parallel()
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"pets": map[string]any{"type": "array", "items": map[string]any{"enum": []any{"cat", "dog"}}},
		},
	}
	e := NewFormElicitation("", "Pick", schema)
	e.HandleKey(Key{Typ: KeyDown})
	e.HandleKey(Key{Typ: KeyRune, Runes: []rune{' '}})

	assert.Contains(t, plainLines(e), "  [x] dog")
	res := e.HandleKey(Key{Typ: KeyEnter})
	assert.Equal(t, map[string]any{"pets": []any{"dog"}}, res.Content)
}

func TestFormElicitationMasksSecrets(t *testing.T) {
	t.Parallel()
	schema := map[string]any{"type": "object", "properties": map[string]any{
		"password": map[string]any{"type": "string", "format": "password"},
	}}
	e := NewFormElicitation("", "Password", schema)
	typeText(e, "hunter2")

	rendered := strings.Join(plainLines(e), "\n")
	assert.NotContains(t, rendered, "hunter2")
	assert.Contains(t, rendered, "•••••••")
	assert.Equal(t, map[string]any{"password": "hunter2"}, e.HandleKey(Key{Typ: KeyEnter}).Content)
}

func TestFreeFormElicitation(t *testing.T) {
	t.Parallel()
	e := NewFormElicitation("", "Anything?", nil)
	e.HandleKey(Key{Typ: KeyPaste, Runes: []rune("a\nb")})

	lines, row, col := e.Render(80)
	assert.Equal(t, "❯ a b", ansi.Strip(lines[row]))
	assert.Equal(t, PromptWidth+3, col)
	assert.Equal(t, map[string]any{"response": "a b"}, e.HandleKey(Key{Typ: KeyEnter}).Content)
}

func TestFormElicitationEscCancels(t *testing.T) {
	t.Parallel()
	e := NewFormElicitation("", "?", nil)
	assert.Equal(t, tools.ElicitationActionCancel, e.HandleKey(Key{Typ: KeyEsc}).Action)
}

func TestURLElicitationKeys(t *testing.T) {
	t.Parallel()
	e := NewURLElicitation("Sign in", "https://example.com/login")

	assert.Contains(t, strings.Join(plainLines(e), "\n"), "https://example.com/login")
	assert.True(t, e.HandleKey(Key{Typ: KeyRune, Runes: []rune{'o'}}).OpenURL)
	assert.Equal(t, tools.ElicitationActionAccept, e.HandleKey(Key{Typ: KeyRune, Runes: []rune{'y'}}).Action)
	assert.Equal(t, tools.ElicitationActionDecline, e.HandleKey(Key{Typ: KeyRune, Runes: []rune{'n'}}).Action)
	assert.Equal(t, tools.ElicitationActionCancel, e.HandleKey(Key{Typ: KeyEsc}).Action)
}

func TestOAuthElicitationKeys(t *testing.T) {
	t.Parallel()
	e := NewOAuthElicitation("https://mcp.example.com")

	assert.Contains(t, strings.Join(plainLines(e), "\n"), "https://mcp.example.com requires OAuth")
	assert.False(t, e.HandleKey(Key{Typ: KeyRune, Runes: []rune{'o'}}).OpenURL)
	assert.Equal(t, tools.ElicitationActionAccept, e.HandleKey(Key{Typ: KeyRune, Runes: []rune{'y'}}).Action)
	assert.Equal(t, tools.ElicitationActionDecline, e.HandleKey(Key{Typ: KeyEsc}).Action)
}

func TestElicitationRenderFitsWidth(t *testing.T) {
	t.Parallel()
	schema := map[string]any{"type": "object", "properties": map[string]any{
		"choice": map[string]any{"type": "string", "oneOf": []any{
			map[string]any{"const": "a", "title": strings.Repeat("long title ", 10), "description": "desc"},
		}},
	}}
	e := NewFormElicitation("A very long title for the prompt", strings.Repeat("message ", 20), schema)
	lines, row, col := e.Render(30)
	for _, l := range lines {
		assert.LessOrEqual(t, DisplayWidth(l), 30)
	}
	assert.Equal(t, len(lines)-1, row)
	assert.LessOrEqual(t, col, 29)
}
