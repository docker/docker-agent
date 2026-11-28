package editor

import (
	"fmt"
	"slices"
	"strings"
	"unicode"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

const (
	bannerContentOffset              = styles.AppPadding
	attachmentBannerChevronExpanded  = "▾"
	attachmentBannerChevronCollapsed = "▸"
	attachmentBannerMarginTop        = 1
)

// attachmentBanner renders the expandable bar above the editor that displays
// attachment pills.
type attachmentBanner struct {
	hoverFade   animation.HoverFade[string]
	attachments []bannerItem
	height      int
	maxHeight   int
	width       int
	regions     []bannerRegion
	hidden      []bannerItem
	canExpand   bool
	expanded    bool
	focused     bool
	theme       *styles.Theme
	view        string
	loadPreview func(bannerItem) (AttachmentPreview, bool)
}

type bannerItem struct {
	label       string
	placeholder string
}

type bannerRegion struct {
	start int
	end   int
	y     int
	item  bannerItem
}

func newAttachmentBanner() *attachmentBanner {
	return &attachmentBanner{maxHeight: 6}
}

func (b *attachmentBanner) SetItems(items []bannerItem) {
	if slices.Equal(b.attachments, items) {
		return
	}
	b.ClearHover()
	b.attachments = slices.Clone(items)
	b.regions = nil
	if len(items) == 0 {
		b.focused = false
	}
	b.reflow()
}

func (b *attachmentBanner) Height() int {
	return b.height
}

func (b *attachmentBanner) CanExpand() bool { return b.canExpand }

func (b *attachmentBanner) SetBounds(width, maxHeight int) {
	b.SetMaxHeight(maxHeight)
	b.SetSize(width)
}

func (b *attachmentBanner) AttachmentAtPosition(x, y int) (AttachmentPreview, bool) {
	if item, ok := b.HitTestPosition(x, y); ok && b.loadPreview != nil {
		return b.loadPreview(item)
	}
	return AttachmentPreview{}, false
}

func (b *attachmentBanner) Toggle() {
	if !b.canExpand {
		return
	}
	b.expanded = !b.expanded
	b.regions = nil
	b.reflow()
}

func (b *attachmentBanner) SetFocused(focused bool) {
	focused = focused && len(b.attachments) > 0 && b.height > 0
	if b.focused == focused {
		return
	}
	b.focused = focused
	b.reflow()
}

func (b *attachmentBanner) SetMaxHeight(height int) {
	height = max(0, height)
	if b.maxHeight == height {
		return
	}
	b.ClearHover()
	b.maxHeight = height
	b.regions = nil
	b.reflow()
}

func (b *attachmentBanner) summaryY() int {
	return min(2, max(0, b.height-1))
}

// SetSize prepares hit regions and immutable paint input at the owner mutation
// boundary. Painting retained input never changes hit-testing geometry.
func (b *attachmentBanner) SetSize(totalWidth int) {
	width := max(0, totalWidth)
	if b.width == width && b.theme == styles.CurrentTheme() {
		return
	}
	b.ClearHover()
	b.width = width
	b.reflow()
}

func (b *attachmentBanner) View() string { return b.view }

func (b *attachmentBanner) reflow() {
	b.theme = styles.CurrentTheme()
	b.regions, b.hidden = nil, nil
	b.canExpand = false
	b.height = 0
	b.view = ""
	if len(b.attachments) == 0 || b.maxHeight == 0 {
		b.expanded, b.focused = false, false
		return
	}
	b.height = min(2+attachmentBannerMarginTop, b.maxHeight)
	if b.width <= 0 {
		b.expanded = false
		return
	}

	leftPadding := min(styles.AppPadding, b.width)
	rightPadding := min(styles.AppPadding, b.width-leftPadding)
	innerWidth := b.width - leftPadding - rightPadding
	count := fmt.Sprintf("%d attachment", len(b.attachments))
	if len(b.attachments) != 1 {
		count += "s"
	}
	left, right := b.prepareSummary(innerWidth, count)
	b.canExpand = len(b.hidden) > 0 && b.maxHeight > b.height && ansi.StringWidth(right) > 0
	if !b.canExpand {
		b.expanded = false
	} else {
		chevron := attachmentBannerChevronCollapsed
		if b.expanded {
			chevron = attachmentBannerChevronExpanded
		}
		left, right = b.prepareSummary(innerWidth, count+" "+chevron)
		if b.focused {
			right = styles.AttachmentSizeStyle.Underline(true).Render(ansi.Strip(right))
		}
		countWidth := max(0, ansi.StringWidth(right)-2)
		right = hoverAttachmentText(ansi.Cut(right, 0, countWidth), b.hoverFade.Fraction("count")) + ansi.Cut(right, countWidth, ansi.StringWidth(right))
	}

	var rows []string
	if b.height >= 3 {
		rows = append(rows, strings.Repeat(" ", innerWidth))
	}
	if b.height >= 2 {
		rows = append(rows, styles.ResizeHandleStyle.Render(strings.Repeat("─", innerWidth)))
	}
	gap := max(0, innerWidth-ansi.StringWidth(left)-ansi.StringWidth(right))
	rows = append(rows, left+strings.Repeat(" ", gap)+right)
	if b.expanded {
		for _, item := range b.hidden {
			if len(rows) >= b.maxHeight {
				break
			}
			pill := ansi.Truncate(b.renderPill(item), innerWidth, "…")
			b.regions = append(b.regions, bannerRegion{start: 0, end: ansi.StringWidth(pill), y: len(rows), item: item})
			rows = append(rows, pill+strings.Repeat(" ", max(0, innerWidth-ansi.StringWidth(pill))))
		}
	}
	b.height = len(rows)
	b.view = lipgloss.NewStyle().Padding(0, rightPadding, 0, leftPadding).Render(strings.Join(rows, "\n"))
}

// prepareSummary retains identities, not a clipped concatenation of pills. The
// first pill may be truncated; expansion only reveals additional attachments.
func (b *attachmentBanner) prepareSummary(innerWidth int, count string) (string, string) {
	b.regions, b.hidden = nil, nil
	// Keep a first attachment visible even when its name needs truncation.
	rightBudget := max(0, innerWidth-min(4, innerWidth)-2)
	right := ansi.Truncate(count, rightBudget, "")
	for _, chevron := range []string{attachmentBannerChevronCollapsed, attachmentBannerChevronExpanded} {
		if strings.HasSuffix(count, " "+chevron) && rightBudget > 0 {
			right = ansi.Truncate(strings.TrimSuffix(count, " "+chevron), max(0, rightBudget-2), "")
			if right != "" {
				right += " "
			}
			right += chevron
		}
	}
	right = styles.AttachmentSizeStyle.Render(right)
	leftBudget := max(0, innerWidth-ansi.StringWidth(right)-2)
	if right == "" {
		leftBudget = innerWidth
	}
	fullWidth := 0
	for i, item := range b.attachments {
		fullWidth += ansi.StringWidth(renderHoveredAttachmentPill(item, 0))
		if i > 0 {
			fullWidth += 2
		}
	}
	overflowCue := len(b.attachments) > 1 && fullWidth > leftBudget && leftBudget >= 3
	if overflowCue {
		leftBudget = max(0, leftBudget-2)
	}
	var left string
	for i, item := range b.attachments {
		pill := b.renderPill(item)
		start := ansi.StringWidth(left)
		if i > 0 {
			start += 2
			if start+ansi.StringWidth(pill) > leftBudget {
				b.hidden = b.attachments[i:]
				break
			}
			left += "  "
		} else {
			pill = ansi.Truncate(pill, leftBudget, "…")
		}
		end := start + ansi.StringWidth(pill)
		if end > start {
			b.regions = append(b.regions, bannerRegion{start: start, end: end, y: b.summaryY(), item: item})
		}
		left += pill
	}
	if len(b.hidden) > 0 && overflowCue {
		left += " …"
	}
	return left, right
}

func (b *attachmentBanner) ToggleAt(x, y int) bool {
	return b.canExpand && x >= 0 && x < b.width && y >= 0 && y < b.height
}

func renderHoveredAttachmentPill(item bannerItem, progress float64) string {
	label := strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, ansi.Strip(item.label))
	name, size := parseLabel(label)
	pill := styles.AttachmentIconStyle.Render("📎 ") + hoverAttachmentText(styles.AttachmentBadgeStyle.Render(name), progress)
	if size != "" {
		pill += " " + styles.AttachmentSizeStyle.Render(size)
	}
	return pill
}

func (b *attachmentBanner) HitTestPosition(x, y int) (bannerItem, bool) {
	if len(b.regions) == 0 || x >= b.width || y < 0 || y >= b.height {
		return bannerItem{}, false
	}

	rel := x - bannerContentOffset
	if rel < 0 {
		return bannerItem{}, false
	}

	for _, region := range b.regions {
		if y == region.y && rel >= region.start && rel < region.end {
			return region.item, true
		}
	}
	return bannerItem{}, false
}

// parseLabel splits a label like "paste-1 (21.1 KB)" into name and size parts.
func parseLabel(label string) (name, size string) {
	idx := strings.LastIndex(label, " (")
	if idx > 0 && strings.HasSuffix(label, ")") {
		return label[:idx], label[idx+1:]
	}
	return label, ""
}

func hoverAttachmentText(text string, progress float64) string {
	if progress == 0 {
		return text
	}
	fg := styles.BlendColor(styles.Info, styles.TextPrimary, progress)
	return styles.BaseStyle.Foreground(fg).Bold(true).Render(ansi.Strip(text))
}
