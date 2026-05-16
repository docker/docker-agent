package styles

import (
	"image/color"
	"strings"
	"time"

	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/textinput"
	"charm.land/glamour/v2/ansi"
	"charm.land/lipgloss/v2"
	"github.com/alecthomas/chroma/v2"
)

const (
	defaultListIndent = 2
	defaultMargin     = 2
)

// Color variables - initialized by ApplyTheme() before TUI starts.
// These are set from the theme's YAML values (see themes/default.yaml for defaults).
var (
	// Background colors
	Background    color.Color
	BackgroundAlt color.Color

	// Primary accent colors

	White    color.Color
	MobyBlue color.Color
	Accent   color.Color

	// Status colors

	Success   color.Color
	Error     color.Color
	Warning   color.Color
	Info      color.Color
	Highlight color.Color

	// Text hierarchy

	TextPrimary   color.Color
	TextSecondary color.Color
	TextMuted     color.Color
	TextMutedGray color.Color

	// Border colors

	BorderPrimary   color.Color
	BorderSecondary color.Color
	BorderMuted     color.Color
	BorderWarning   color.Color

	// Diff colors

	DiffAddBg    color.Color
	DiffRemoveBg color.Color
	DiffAddFg    color.Color
	DiffRemoveFg color.Color

	// UI element colors

	LineNumber color.Color
	Separator  color.Color

	// Interactive element colors

	Selected         color.Color
	SelectedFg       color.Color
	PlaceholderColor color.Color

	// Badge colors

	AgentBadgeFg color.Color
	AgentBadgeBg color.Color
	BadgePurple  color.Color
	BadgeCyan    color.Color
	BadgeGreen   color.Color

	// Error colors (extended)

	ErrorStrong color.Color
	ErrorDark   color.Color

	// Additional muted colors

	FadedGray color.Color

	// Tabs

	TabBg         color.Color
	TabPrimaryFg  color.Color
	TabAccentFg   color.Color
	TabActiveBg   color.Color
	TabActiveFg   color.Color
	TabInactiveFg color.Color
	TabBorder     color.Color
)

// Base Styles
const (
	AppPadding = 1 // Symmetric left/right padding used by AppStyle and EditorStyle

	// DoubleClickThreshold is the maximum time between clicks to register as a double-click
	DoubleClickThreshold = 400 * time.Millisecond
)

var (
	NoStyle   = lipgloss.NewStyle()
	BaseStyle = NoStyle.Foreground(TextPrimary)
	AppStyle  = BaseStyle.Padding(0, AppPadding, 0, AppPadding)
)

// Text Styles
var (
	HighlightWhiteStyle = BaseStyle.Foreground(White).Bold(true)
	MutedStyle          = BaseStyle.Foreground(TextMutedGray)
	SecondaryStyle      = BaseStyle.Foreground(TextSecondary)
	BoldStyle           = BaseStyle.Bold(true)
	FadingStyle         = NoStyle.Foreground(FadedGray) // Very dim for fade-out animations (rebuilt by ApplyTheme)
)

// Status Styles
var (
	SuccessStyle    = BaseStyle.Foreground(Success)
	ErrorStyle      = BaseStyle.Foreground(Error)
	WarningStyle    = BaseStyle.Foreground(Warning)
	InfoStyle       = BaseStyle.Foreground(Info)
	ActiveStyle     = BaseStyle.Foreground(Success)
	ToBeDoneStyle   = BaseStyle.Foreground(TextPrimary)
	InProgressStyle = BaseStyle.Foreground(Highlight)
	CompletedStyle  = BaseStyle.Foreground(TextMutedGray)
)

// Layout Styles
var (
	CenterStyle = BaseStyle.Align(lipgloss.Center, lipgloss.Center)
)

// Border Styles
var (
	BaseMessageStyle = BaseStyle.
				Padding(1, 1).
				BorderLeft(true).
				BorderStyle(lipgloss.HiddenBorder()).
				BorderForeground(BorderPrimary)

	UserMessageStyle = BaseMessageStyle.
				BorderStyle(lipgloss.ThickBorder()).
				BorderForeground(BorderPrimary).
				Foreground(TextPrimary).
				Background(BackgroundAlt).
				Bold(true)

	AssistantMessageStyle = BaseMessageStyle.
				Padding(0, 1)

	WelcomeMessageStyle = BaseMessageStyle.
				BorderStyle(lipgloss.DoubleBorder()).
				Bold(true)

	ErrorMessageStyle = BaseMessageStyle.
				BorderStyle(lipgloss.ThickBorder()).
				Foreground(Error)

	SelectedMessageStyle = AssistantMessageStyle.
				BorderStyle(lipgloss.NormalBorder()).
				BorderForeground(Success)

	SelectedUserMessageStyle = UserMessageStyle.
					BorderStyle(lipgloss.ThickBorder()).
					BorderForeground(Success)
)

// Dialog Styles
var (
	DialogStyle = BaseStyle.
			Border(lipgloss.RoundedBorder()).
			BorderForeground(BorderSecondary).
			Foreground(TextPrimary).
			Padding(1, 2).
			Align(lipgloss.Left)

	DialogWarningStyle = BaseStyle.
				Border(lipgloss.RoundedBorder()).
				BorderForeground(BorderWarning).
				Foreground(TextPrimary).
				Padding(1, 2).
				Align(lipgloss.Left)

	DialogTitleStyle = BaseStyle.
				Bold(true).
				Foreground(TextSecondary).
				Align(lipgloss.Center)

	DialogTitleWarningStyle = BaseStyle.
				Bold(true).
				Foreground(Warning).
				Align(lipgloss.Center)

	DialogTitleInfoStyle = BaseStyle.
				Bold(true).
				Foreground(Info).
				Align(lipgloss.Center)

	DialogContentStyle = BaseStyle.
				Foreground(TextPrimary)

	DialogSeparatorStyle = BaseStyle.
				Foreground(BorderMuted)

	DialogQuestionStyle = BaseStyle.
				Bold(true).
				Foreground(TextPrimary).
				Align(lipgloss.Center)

	DialogOptionsStyle = BaseStyle.
				Foreground(TextMuted).
				Align(lipgloss.Center)

	DialogHelpStyle = BaseStyle.
			Foreground(TextMuted).
			Italic(true)

	TabTitleStyle = BaseStyle.
			Foreground(TabPrimaryFg)

	TabPrimaryStyle = BaseStyle.
			Foreground(TextPrimary)

	TabStyle = TabPrimaryStyle.
			Padding(1, 0)

	TabAccentStyle = BaseStyle.
			Foreground(TabAccentFg)
)

// Command Palette Styles - rebuilt by ApplyTheme()
var (
	PaletteCategoryStyle = BaseStyle.
				Bold(true).
				Foreground(White).
				MarginTop(1)

	PaletteUnselectedActionStyle = BaseStyle.
					Foreground(TextPrimary).
					Bold(true)

	PaletteSelectedActionStyle = PaletteUnselectedActionStyle.
					Background(MobyBlue).
					Foreground(White)

	PaletteUnselectedDescStyle = BaseStyle.
					Foreground(TextSecondary)

	PaletteSelectedDescStyle = PaletteUnselectedDescStyle.
					Background(MobyBlue).
					Foreground(White)

	// Badge styles for model picker - use color vars set by ApplyTheme()

	BadgeAlloyStyle = BaseStyle.
			Foreground(BadgePurple)

	BadgeDefaultStyle = BaseStyle.
				Foreground(BadgeCyan)

	BadgeCurrentStyle = BaseStyle.
				Foreground(BadgeGreen)
)

// Star Styles for session browser and sidebar
var (
	StarredStyle   = BaseStyle.Foreground(Success)
	UnstarredStyle = BaseStyle.Foreground(TextMuted)
)

// StarIndicator returns the styled star indicator for a given starred status
func StarIndicator(starred bool) string {
	if starred {
		return StarredStyle.Render("★") + " "
	}
	return UnstarredStyle.Render("☆") + " "
}

// Diff Styles (matching glamour markdown theme)
var (
	DiffAddStyle = BaseStyle.
			Background(DiffAddBg).
			Foreground(DiffAddFg)

	DiffRemoveStyle = BaseStyle.
			Background(DiffRemoveBg).
			Foreground(DiffRemoveFg)

	DiffUnchangedStyle = BaseStyle.Background(BackgroundAlt)
)

// Syntax highlighting UI element styles
var (
	LineNumberStyle = BaseStyle.Foreground(LineNumber).Background(BackgroundAlt)
	SeparatorStyle  = BaseStyle.Foreground(Separator).Background(BackgroundAlt)
)

// Tool Call Styles
var (
	ToolMessageStyle = BaseStyle.
				Foreground(TextMutedGray)

	ToolErrorMessageStyle = BaseStyle.
				Foreground(ErrorStrong)

	ToolName = ToolMessageStyle.
			Foreground(TextMutedGray).
			Padding(0, 1)

	ToolNameError = ToolName.
			Foreground(ErrorStrong).
			Background(ErrorDark)

	ToolNameDim = ToolMessageStyle.
			Foreground(TextMutedGray).
			Italic(true)

	ToolDescription = ToolMessageStyle.
			Foreground(TextPrimary)

	ToolCompletedIcon = BaseStyle.
				MarginLeft(2).
				Foreground(TextMutedGray)

	ToolErrorIcon = ToolCompletedIcon.
			Background(ErrorStrong)

	ToolPendingIcon = ToolCompletedIcon.
			Background(Warning)

	ToolCallArgs = ToolMessageStyle.
			Padding(0, 0, 0, 2)

	ToolCallResult = ToolMessageStyle.
			Padding(0, 0, 0, 2)
)

// Input Styles
var (
	InputStyle = textarea.Styles{
		Focused: textarea.StyleState{
			Base:        BaseStyle,
			Placeholder: BaseStyle.Foreground(PlaceholderColor),
		},
		Blurred: textarea.StyleState{
			Base:        BaseStyle,
			Placeholder: BaseStyle.Foreground(PlaceholderColor),
		},
		Cursor: textarea.CursorStyle{
			Color: Accent,
		},
	}

	// DialogInputStyle is the style for textinput fields in dialogs,
	// matching the main editor's look (cursor color, text color).
	DialogInputStyle = textinput.Styles{
		Focused: textinput.StyleState{
			Text:        BaseStyle,
			Placeholder: BaseStyle.Foreground(PlaceholderColor),
		},
		Blurred: textinput.StyleState{
			Text:        BaseStyle,
			Placeholder: BaseStyle.Foreground(PlaceholderColor),
		},
		Cursor: textinput.CursorStyle{
			Color: Accent,
		},
	}
	EditorStyle = BaseStyle.Padding(1, AppPadding, 0, AppPadding)
	// SuggestionGhostStyle renders inline auto-complete hints in a muted tone.
	// Use a distinct grey so suggestion text is visually separate from the user's input.
	// NOTE: Rebuilt by ApplyTheme() using theme's suggestion_ghost color.
	SuggestionGhostStyle = BaseStyle.Foreground(TextMutedGray)
	// SuggestionCursorStyle renders the first character of a suggestion inside the cursor.
	// Uses the same blue accent background as the normal cursor, with ghost-colored foreground text.
	// NOTE: Rebuilt by ApplyTheme().
	SuggestionCursorStyle = BaseStyle.Background(Accent).Foreground(TextMutedGray)

	// Attachment banner styles - polished look with subtle border

	AttachmentBannerStyle = BaseStyle.
				Foreground(TextSecondary)

	AttachmentBadgeStyle = BaseStyle.
				Foreground(Info).
				Bold(true)

	AttachmentSizeStyle = BaseStyle.
				Foreground(TextMuted).
				Italic(true)

	AttachmentIconStyle = BaseStyle.
				Foreground(Info)
)

// Scrollbar
var (
	TrackStyle       = lipgloss.NewStyle().Foreground(BorderSecondary)
	ThumbStyle       = lipgloss.NewStyle().Foreground(Info).Background(BackgroundAlt)
	ThumbActiveStyle = lipgloss.NewStyle().Foreground(White).Background(BackgroundAlt)
)

// Resize Handle Style
var (
	ResizeHandleStyle = BaseStyle.
				Foreground(BorderSecondary)

	ResizeHandleHoverStyle = BaseStyle.
				Foreground(Info).
				Bold(true)

	ResizeHandleActiveStyle = BaseStyle.
				Foreground(White).
				Bold(true)
)

// Notification Styles
var (
	NotificationStyle = BaseStyle.
				Border(lipgloss.RoundedBorder()).
				BorderForeground(Success).
				Padding(0, 1)

	NotificationInfoStyle = BaseStyle.
				Border(lipgloss.RoundedBorder()).
				BorderForeground(Info).
				Padding(0, 1)

	NotificationWarningStyle = BaseStyle.
					Border(lipgloss.RoundedBorder()).
					BorderForeground(Warning).
					Padding(0, 1)

	NotificationErrorStyle = BaseStyle.
				Border(lipgloss.RoundedBorder()).
				BorderForeground(Error).
				Padding(0, 1)
)

// Completion Styles
var (
	CompletionBoxStyle = BaseStyle.
				Border(lipgloss.RoundedBorder()).
				BorderForeground(BorderSecondary).
				Padding(0, 1)

	CompletionNormalStyle = BaseStyle.
				Foreground(TextPrimary).
				Bold(true)

	CompletionSelectedStyle = CompletionNormalStyle.
				Foreground(White).
				Background(MobyBlue)

	CompletionDescStyle = BaseStyle.
				Foreground(TextSecondary)

	CompletionSelectedDescStyle = CompletionDescStyle.
					Foreground(White).
					Background(MobyBlue)

	CompletionNoResultsStyle = BaseStyle.
					Foreground(TextMuted).
					Italic(true).
					Align(lipgloss.Center)
)

// Agent and transfer badge styles
var (
	AgentBadgeStyle = BaseStyle.
			Foreground(AgentBadgeFg).
			Background(AgentBadgeBg).
			Padding(0, 1)

	ThinkingBadgeStyle = BaseStyle.
				Foreground(TextMuted). // Muted blue, distinct from gray italic content
				Bold(true).
				Italic(true)
)

// Deprecated styles (kept for backward compatibility)
var (
	ChatStyle = BaseStyle
)

// Selection Styles
var (
	SelectionStyle = BaseStyle.
		Background(Selected).
		Foreground(SelectedFg)
)

// Spinner Styles - rebuilt by ApplyTheme() with actual spinner colors from theme
var (
	SpinnerDotsAccentStyle    = BaseStyle.Foreground(Accent)
	SpinnerDotsHighlightStyle = BaseStyle.Foreground(TabAccentFg)
	SpinnerTextBrightestStyle = BaseStyle.Foreground(Accent)
	SpinnerTextBrightStyle    = BaseStyle.Foreground(Accent)
	SpinnerTextDimStyle       = BaseStyle.Foreground(Accent)
	SpinnerTextDimmestStyle   = BaseStyle.Foreground(Accent)
)

func toChroma(style ansi.StylePrimitive) string {
	var s []string

	if style.Color != nil {
		s = append(s, *style.Color)
	}
	if style.BackgroundColor != nil {
		s = append(s, "bg:"+*style.BackgroundColor)
	}
	if style.Italic != nil && *style.Italic {
		s = append(s, "italic")
	}
	if style.Bold != nil && *style.Bold {
		s = append(s, "bold")
	}
	if style.Underline != nil && *style.Underline {
		s = append(s, "underline")
	}

	return strings.Join(s, " ")
}

func getChromaTheme() chroma.StyleEntries {
	md := MarkdownStyle().CodeBlock
	return chroma.StyleEntries{
		chroma.Text:                toChroma(md.Chroma.Text),
		chroma.Error:               toChroma(md.Chroma.Error),
		chroma.Comment:             toChroma(md.Chroma.Comment),
		chroma.CommentPreproc:      toChroma(md.Chroma.CommentPreproc),
		chroma.Keyword:             toChroma(md.Chroma.Keyword),
		chroma.KeywordReserved:     toChroma(md.Chroma.KeywordReserved),
		chroma.KeywordNamespace:    toChroma(md.Chroma.KeywordNamespace),
		chroma.KeywordType:         toChroma(md.Chroma.KeywordType),
		chroma.Operator:            toChroma(md.Chroma.Operator),
		chroma.Punctuation:         toChroma(md.Chroma.Punctuation),
		chroma.Name:                toChroma(md.Chroma.Name),
		chroma.NameBuiltin:         toChroma(md.Chroma.NameBuiltin),
		chroma.NameTag:             toChroma(md.Chroma.NameTag),
		chroma.NameAttribute:       toChroma(md.Chroma.NameAttribute),
		chroma.NameClass:           toChroma(md.Chroma.NameClass),
		chroma.NameDecorator:       toChroma(md.Chroma.NameDecorator),
		chroma.NameFunction:        toChroma(md.Chroma.NameFunction),
		chroma.LiteralNumber:       toChroma(md.Chroma.LiteralNumber),
		chroma.LiteralString:       toChroma(md.Chroma.LiteralString),
		chroma.LiteralStringEscape: toChroma(md.Chroma.LiteralStringEscape),
		chroma.GenericDeleted:      toChroma(md.Chroma.GenericDeleted),
		chroma.GenericEmph:         toChroma(md.Chroma.GenericEmph),
		chroma.GenericInserted:     toChroma(md.Chroma.GenericInserted),
		chroma.GenericStrong:       toChroma(md.Chroma.GenericStrong),
		chroma.GenericSubheading:   toChroma(md.Chroma.GenericSubheading),
		chroma.Background:          toChroma(md.Chroma.Background),
	}
}

func ChromaStyle() *chroma.Style {
	style, err := chroma.NewStyle("cagent", getChromaTheme())
	if err != nil {
		panic(err)
	}
	return style
}

// MarkdownStyle returns the markdown style configuration, deriving colors from the current theme.
func MarkdownStyle() ansi.StyleConfig {
	theme := CurrentTheme()
	md := theme.Markdown
	ch := theme.Chroma
	colors := theme.Colors

	// Use theme markdown colors with fallbacks to theme.Colors
	headingColor := md.Heading
	if headingColor == "" {
		headingColor = colors.Accent
	}
	linkColor := md.Link
	if linkColor == "" {
		linkColor = colors.Accent
	}
	strongColor := md.Strong
	if strongColor == "" {
		strongColor = colors.TextPrimary
	}
	codeColor := md.Code
	if codeColor == "" {
		codeColor = colors.TextPrimary
	}
	codeBgColor := md.CodeBg
	if codeBgColor == "" {
		codeBgColor = colors.BackgroundAlt
	}
	blockquoteColor := md.Blockquote
	if blockquoteColor == "" {
		blockquoteColor = colors.TextSecondary
	}
	listColor := md.List
	if listColor == "" {
		listColor = colors.TextPrimary
	}
	hrColor := md.HR
	if hrColor == "" {
		hrColor = colors.BorderSecondary
	}
	// Use text primary for document text
	textColor := colors.TextPrimary
	textSecondary := colors.TextSecondary

	// Chroma colors with fallbacks from default theme
	defaults := DefaultTheme().Chroma
	chromaComment := ch.Comment
	if chromaComment == "" {
		chromaComment = defaults.Comment
	}
	chromaCommentPreproc := ch.CommentPreproc
	if chromaCommentPreproc == "" {
		chromaCommentPreproc = defaults.CommentPreproc
	}
	chromaKeyword := ch.Keyword
	if chromaKeyword == "" {
		chromaKeyword = defaults.Keyword
	}
	chromaKeywordReserved := ch.KeywordReserved
	if chromaKeywordReserved == "" {
		chromaKeywordReserved = defaults.KeywordReserved
	}
	chromaKeywordNamespace := ch.KeywordNamespace
	if chromaKeywordNamespace == "" {
		chromaKeywordNamespace = defaults.KeywordNamespace
	}
	chromaKeywordType := ch.KeywordType
	if chromaKeywordType == "" {
		chromaKeywordType = defaults.KeywordType
	}
	chromaOperator := ch.Operator
	if chromaOperator == "" {
		chromaOperator = defaults.Operator
	}
	chromaPunctuation := ch.Punctuation
	if chromaPunctuation == "" {
		chromaPunctuation = defaults.Punctuation
	}
	chromaNameBuiltin := ch.NameBuiltin
	if chromaNameBuiltin == "" {
		chromaNameBuiltin = defaults.NameBuiltin
	}
	chromaNameTag := ch.NameTag
	if chromaNameTag == "" {
		chromaNameTag = defaults.NameTag
	}
	chromaNameAttribute := ch.NameAttribute
	if chromaNameAttribute == "" {
		chromaNameAttribute = defaults.NameAttribute
	}
	chromaNameDecorator := ch.NameDecorator
	if chromaNameDecorator == "" {
		chromaNameDecorator = defaults.NameDecorator
	}
	chromaLiteralNumber := ch.LiteralNumber
	if chromaLiteralNumber == "" {
		chromaLiteralNumber = defaults.LiteralNumber
	}
	chromaLiteralString := ch.LiteralString
	if chromaLiteralString == "" {
		chromaLiteralString = defaults.LiteralString
	}
	chromaLiteralStringEscape := ch.LiteralStringEscape
	if chromaLiteralStringEscape == "" {
		chromaLiteralStringEscape = defaults.LiteralStringEscape
	}
	chromaGenericDeleted := ch.GenericDeleted
	if chromaGenericDeleted == "" {
		chromaGenericDeleted = defaults.GenericDeleted
	}
	chromaGenericSubheading := ch.GenericSubheading
	if chromaGenericSubheading == "" {
		chromaGenericSubheading = defaults.GenericSubheading
	}
	chromaBackground := ch.Background
	if chromaBackground == "" {
		chromaBackground = colors.BackgroundAlt
	}
	chromaErrorFg := ch.ErrorFg
	if chromaErrorFg == "" {
		chromaErrorFg = defaults.ErrorFg
	}
	chromaErrorBg := ch.ErrorBg
	if chromaErrorBg == "" {
		chromaErrorBg = defaults.ErrorBg
	}
	chromaSuccess := ch.Success
	if chromaSuccess == "" {
		chromaSuccess = colors.Success
	}

	customDarkStyle := ansi.StyleConfig{
		Document: ansi.StyleBlock{
			StylePrimitive: ansi.StylePrimitive{
				BlockPrefix: "",
				BlockSuffix: "",
				Color:       &textColor,
			},
			Margin: new(uint(0)),
		},
		BlockQuote: ansi.StyleBlock{
			StylePrimitive: ansi.StylePrimitive{
				Color: &blockquoteColor,
			},
			Indent:      new(uint(1)),
			IndentToken: nil,
		},
		List: ansi.StyleList{
			LevelIndent: defaultListIndent,
		},
		Heading: ansi.StyleBlock{
			StylePrimitive: ansi.StylePrimitive{
				BlockSuffix: "\n",
				Color:       &headingColor,
				Bold:        new(true),
			},
		},
		H1: ansi.StyleBlock{
			StylePrimitive: ansi.StylePrimitive{
				Prefix: "## ",
				Color:  &headingColor,
				Bold:   new(true),
			},
		},
		H2: ansi.StyleBlock{
			StylePrimitive: ansi.StylePrimitive{
				Prefix: "## ",
				Color:  &headingColor,
			},
		},
		H3: ansi.StyleBlock{
			StylePrimitive: ansi.StylePrimitive{
				Prefix: "### ",
				Color:  &headingColor,
			},
		},
		H4: ansi.StyleBlock{
			StylePrimitive: ansi.StylePrimitive{
				Prefix: "#### ",
				Color:  &headingColor,
			},
		},
		H5: ansi.StyleBlock{
			StylePrimitive: ansi.StylePrimitive{
				Prefix: "##### ",
				Color:  &headingColor,
			},
		},
		H6: ansi.StyleBlock{
			StylePrimitive: ansi.StylePrimitive{
				Prefix: "###### ",
				Color:  &headingColor,
			},
		},
		Strikethrough: ansi.StylePrimitive{
			CrossedOut: new(true),
		},
		Emph: ansi.StylePrimitive{
			Italic: new(true),
		},
		Strong: ansi.StylePrimitive{
			Color: &strongColor,
			Bold:  new(true),
		},
		HorizontalRule: ansi.StylePrimitive{
			Color:  &hrColor,
			Format: "\n--------\n",
		},
		Item: ansi.StylePrimitive{
			BlockPrefix: "• ",
		},
		Enumeration: ansi.StylePrimitive{
			BlockPrefix: ". ",
		},
		Task: ansi.StyleTask{
			StylePrimitive: ansi.StylePrimitive{},
			Ticked:         "[✓] ",
			Unticked:       "[ ] ",
		},
		Link: ansi.StylePrimitive{
			Color:     &linkColor,
			Underline: new(true),
		},
		LinkText: ansi.StylePrimitive{
			Color: &linkColor,
			Bold:  new(true),
		},
		Image: ansi.StylePrimitive{
			Color:     &linkColor,
			Underline: new(true),
		},
		ImageText: ansi.StylePrimitive{
			Color:  &textSecondary,
			Format: "Image: {{.text}} →",
		},
		Code: ansi.StyleBlock{
			StylePrimitive: ansi.StylePrimitive{
				Prefix:          " ",
				Suffix:          " ",
				Color:           &codeColor,
				BackgroundColor: &codeBgColor,
			},
		},
		CodeBlock: ansi.StyleCodeBlock{
			StyleBlock: ansi.StyleBlock{
				StylePrimitive: ansi.StylePrimitive{
					Color: &textSecondary,
				},
				Margin: new(uint(defaultMargin)),
			},
			Theme: "monokai",
			Chroma: &ansi.Chroma{
				Text: ansi.StylePrimitive{
					Color: &textColor,
				},
				Error: ansi.StylePrimitive{
					Color:           &chromaErrorFg,
					BackgroundColor: &chromaErrorBg,
				},
				Comment: ansi.StylePrimitive{
					Color: &chromaComment,
				},
				CommentPreproc: ansi.StylePrimitive{
					Color: &chromaCommentPreproc,
				},
				Keyword: ansi.StylePrimitive{
					Color: &chromaKeyword,
				},
				KeywordReserved: ansi.StylePrimitive{
					Color: &chromaKeywordReserved,
				},
				KeywordNamespace: ansi.StylePrimitive{
					Color: &chromaKeywordNamespace,
				},
				KeywordType: ansi.StylePrimitive{
					Color: &chromaKeywordType,
				},
				Operator: ansi.StylePrimitive{
					Color: &chromaOperator,
				},
				Punctuation: ansi.StylePrimitive{
					Color: &chromaPunctuation,
				},
				Name: ansi.StylePrimitive{
					Color: &textColor,
				},
				NameBuiltin: ansi.StylePrimitive{
					Color: &chromaNameBuiltin,
				},
				NameTag: ansi.StylePrimitive{
					Color: &chromaNameTag,
				},
				NameAttribute: ansi.StylePrimitive{
					Color: &chromaNameAttribute,
				},
				NameClass: ansi.StylePrimitive{
					Color:     &chromaErrorFg,
					Underline: new(true),
					Bold:      new(true),
				},
				NameDecorator: ansi.StylePrimitive{
					Color: &chromaNameDecorator,
				},
				NameFunction: ansi.StylePrimitive{
					Color: &chromaSuccess,
				},
				LiteralNumber: ansi.StylePrimitive{
					Color: &chromaLiteralNumber,
				},
				LiteralString: ansi.StylePrimitive{
					Color: &chromaLiteralString,
				},
				LiteralStringEscape: ansi.StylePrimitive{
					Color: &chromaLiteralStringEscape,
				},
				GenericDeleted: ansi.StylePrimitive{
					Color: &chromaGenericDeleted,
				},
				GenericEmph: ansi.StylePrimitive{
					Italic: new(true),
				},
				GenericInserted: ansi.StylePrimitive{
					Color: &chromaSuccess,
				},
				GenericStrong: ansi.StylePrimitive{
					Bold: new(true),
				},
				GenericSubheading: ansi.StylePrimitive{
					Color: &chromaGenericSubheading,
				},
				Background: ansi.StylePrimitive{
					BackgroundColor: &chromaBackground,
				},
			},
		},
		Table: ansi.StyleTable{
			StyleBlock: ansi.StyleBlock{
				StylePrimitive: ansi.StylePrimitive{},
			},
		},
		DefinitionDescription: ansi.StylePrimitive{
			BlockPrefix: "\n🠶 ",
		},
	}

	customDarkStyle.List.Color = &listColor
	customDarkStyle.CodeBlock.BackgroundColor = &codeBgColor

	return customDarkStyle
}
