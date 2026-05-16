package editor

import (
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/mattn/go-runewidth"

	"github.com/docker/docker-agent/pkg/tui/styles"
)

const (
	bannerSeparatorText = "  •  "
	// BorderLeft + PaddingLeft from banner style
	bannerContentOffset = 2
)

type attachmentBanner struct {
	items   []bannerItem
	height  int // cached height after last render
	regions []bannerRegion
}

type bannerItem struct {
	label       string
	placeholder string
}

type bannerRegion struct {
	start int
	end   int
	item  bannerItem
}

func newAttachmentBanner() *attachmentBanner {
	return &attachmentBanner{}
}

func (b *attachmentBanner) SetItems(items []bannerItem) {
	b.items = items
	b.updateHeight()
}

// Height returns the number of lines this banner will take when rendered.
func (b *attachmentBanner) Height() int {
	return b.height
}

// updateHeight recalculates the banner height based on current state.
func (b *attachmentBanner) updateHeight() {
	if len(b.items) == 0 {
		b.height = 0
		return
	}
	// Banner takes 1 line when visible
	b.height = 1
}

func (b *attachmentBanner) View() string {
	if len(b.items) == 0 {
		return ""
	}

	// Build pill-style badges for each attachment
	var pills []string
	for _, item := range b.items {
		name, size := parseLabel(item.label)

		// Create a nice pill: icon + name + size
		pill := styles.AttachmentIconStyle.Render("📎 ") +
			styles.AttachmentBadgeStyle.Render(name)
		if size != "" {
			pill += " " + styles.AttachmentSizeStyle.Render(size)
		}
		pills = append(pills, pill)
	}

	separator := styles.MutedStyle.Render(bannerSeparatorText)
	content := strings.Join(pills, separator)

	b.buildRegions(pills, separator)

	// Wrap in banner style with subtle left border
	banner := lipgloss.NewStyle().
		BorderLeft(true).
		BorderStyle(lipgloss.ThickBorder()).
		BorderForeground(styles.Info).
		PaddingLeft(1).
		PaddingRight(1).
		Foreground(styles.TextSecondary).
		Render(content)

	return styles.AttachmentBannerStyle.Render(banner)
}

func (b *attachmentBanner) buildRegions(pills []string, separator string) {
	b.regions = b.regions[:0]
	if len(pills) == 0 {
		return
	}

	pos := 0
	sepWidth := runewidth.StringWidth(stripANSI(separator))

	for i, pill := range pills {
		if i > 0 {
			pos += sepWidth
		}
		width := runewidth.StringWidth(stripANSI(pill))
		b.regions = append(b.regions, bannerRegion{
			start: pos,
			end:   pos + width,
			item:  b.items[i],
		})
		pos += width
	}
}

func (b *attachmentBanner) HitTest(x int) (bannerItem, bool) {
	if len(b.regions) == 0 {
		return bannerItem{}, false
	}

	rel := x - bannerContentOffset
	if rel < 0 {
		return bannerItem{}, false
	}

	for _, region := range b.regions {
		if rel >= region.start && rel < region.end {
			return region.item, true
		}
	}
	return bannerItem{}, false
}

// parseLabel splits a label like "paste-1 (21.1 KB)" into name and size parts.
func parseLabel(label string) (name, size string) {
	// Find the last opening parenthesis for size
	idx := strings.LastIndex(label, " (")
	if idx > 0 && strings.HasSuffix(label, ")") {
		return label[:idx], label[idx+1:]
	}
	return label, ""
}
