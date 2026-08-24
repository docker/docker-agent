package message

import (
	"bytes"
	"encoding/base64"
	stdimage "image"
	"image/color"
	"image/png"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/animation"
	tuiimage "github.com/docker/docker-agent/pkg/tui/image"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func testImageURI(t *testing.T, colorValue color.RGBA) string {
	t.Helper()
	img := stdimage.NewRGBA(stdimage.Rect(0, 0, 2, 1))
	img.Set(0, 0, colorValue)
	var data bytes.Buffer
	require.NoError(t, png.Encode(&data, img))
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(data.Bytes())
}

func TestAppendContentPreservesSplitImageOpenerAndExactContent(t *testing.T) {
	msg := types.Agent(types.MessageTypeAssistant, "root", "prefix ")
	m := New(animation.NewRuntime(), msg, nil)
	for _, chunk := range []string{"!", "[alt]", "(https://example.com/image.png)", " suffix"} {
		_ = m.AppendContent(chunk)
	}
	require.Equal(t, "prefix ![alt](https://example.com/image.png) suffix", m.message.Content)
	require.Equal(t, "prefix ![alt](https://example.com/image.png) suffix", m.contentBuf.String())
}

func TestAppendContentSplitImageSchedulesAndRendersInline(t *testing.T) {
	tuiimage.SetRenderingEnabled(true)
	uri := testImageURI(t, color.RGBA{G: 255, A: 255})
	m := New(animation.NewRuntime(), types.Agent(types.MessageTypeAssistant, "root", ""), nil)
	m.SetSize(80, 0)
	chunks := []string{"!", "[alt]", "(" + uri + ")"}
	for _, chunk := range chunks[:2] {
		require.Nil(t, m.AppendContent(chunk))
	}
	cmd := m.AppendContent(chunks[2])
	require.NotNil(t, cmd, "completing a pending image reference must schedule loading")
	_, _ = m.Update(cmd())
	view := m.View()
	require.Contains(t, view, "cagent-image")
	require.Contains(t, ansi.Strip(view), "alt")
	require.NotContains(t, ansi.Strip(view), strings.Join(chunks, ""))
}

func TestAppendContentCompletesInitialImageAcrossPartitions(t *testing.T) {
	uri := testImageURI(t, color.RGBA{R: 128, G: 64, A: 255})
	full := "prefix ![界](" + uri + ") suffix"
	for split := range len(full) + 1 {
		if split == 0 || split == len(full) || !utf8.RuneStart(full[split]) {
			continue
		}
		initial, appended := full[:split], full[split:]
		if len(tuiimage.MarkdownReferences(initial)) != 0 || !strings.Contains(initial, "![") {
			continue
		}
		t.Run(strconv.Itoa(split), func(t *testing.T) {
			m := New(animation.NewRuntime(), types.Agent(types.MessageTypeAssistant, "root", initial), nil)
			_ = m.Init()
			require.GreaterOrEqual(t, m.imageScanOffset, 0)
			cmd := m.AppendContent(appended)
			require.NotNil(t, cmd, "partition %d must schedule the completed initial image", split)
			_, _ = m.Update(cmd())
			require.Contains(t, m.markdownImages, uri)
			require.Equal(t, full, m.message.Content)
		})
	}
}

func TestAppendContentInitialUnresolvedImageForms(t *testing.T) {
	uri := testImageURI(t, color.RGBA{B: 128, A: 255})
	for _, tc := range []struct {
		initial, appended string
	}{
		{"![alt", "](" + uri + ")"},
		{"prefix ![界", "](" + uri + ")"},
		{"![alt](", uri + ")"},
	} {
		m := New(animation.NewRuntime(), types.Agent(types.MessageTypeAssistant, "root", tc.initial), nil)
		_ = m.Init()
		require.GreaterOrEqual(t, m.imageScanOffset, 0)
		cmd := m.AppendContent(tc.appended)
		require.NotNil(t, cmd)
		_, _ = m.Update(cmd())
		require.Contains(t, m.markdownImages, uri)
	}
}

func TestAppendContentSchedulesSecondImageSplitAfterFirstCompletion(t *testing.T) {
	first := testImageURI(t, color.RGBA{R: 255, A: 255})
	second := testImageURI(t, color.RGBA{B: 255, A: 255})
	m := New(animation.NewRuntime(), types.Agent(types.MessageTypeAssistant, "root", ""), nil)

	cmd := m.AppendContent("![one](" + first + ") ![two")
	require.NotNil(t, cmd)
	_, _ = m.Update(cmd())
	require.GreaterOrEqual(t, m.imageScanOffset, 0, "incomplete second opener remains tracked")

	cmd = m.AppendContent("](" + second + ")")
	require.NotNil(t, cmd, "completing the second image must schedule loading")
	_, _ = m.Update(cmd())
	require.Contains(t, m.markdownImages, first)
	require.Contains(t, m.markdownImages, second)
	require.Equal(t, -1, m.imageScanOffset)
}

func TestAppendContentSplitImageSyntaxInCodeDoesNotFetch(t *testing.T) {
	for _, chunks := range [][]string{
		{"`!", "[inline]", "(https://example.com/inline.png)`"},
		{"```md\n!", "[fenced]", "(https://example.com/fenced.png)\n```"},
	} {
		m := New(animation.NewRuntime(), types.Agent(types.MessageTypeAssistant, "root", ""), nil)
		for _, chunk := range chunks {
			require.Nil(t, m.AppendContent(chunk))
		}
		require.Empty(t, m.loadingImages)
	}
}

func TestAppendContentImageScanStateIsBoundedAndCleared(t *testing.T) {
	m := New(animation.NewRuntime(), types.Agent(types.MessageTypeAssistant, "root", ""), nil)
	long := "![abandoned" + strings.Repeat("界", 100_000)
	require.Nil(t, m.AppendContent(long))
	require.Equal(t, 0, m.imageScanOffset)
	require.Equal(t, len(long), m.contentBuf.Len(), "scan state must not duplicate the canonical suffix")

	m.Finalize()
	require.Equal(t, -1, m.imageScanOffset)
	_ = m.SetMessage(types.Agent(types.MessageTypeAssistant, "root", "reset"))
	require.Equal(t, -1, m.imageScanOffset)
}

func TestAppendContentImageScanOffsetIsUTF8Safe(t *testing.T) {
	m := New(animation.NewRuntime(), types.Agent(types.MessageTypeAssistant, "root", ""), nil)
	require.Nil(t, m.AppendContent("界!"))
	require.Equal(t, -1, m.imageScanOffset)
	require.Nil(t, m.AppendContent("[界"))
	require.Equal(t, len("界"), m.imageScanOffset)
	require.Equal(t, "![界", m.message.Content[m.imageScanOffset:])
}
