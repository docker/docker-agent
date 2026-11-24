package spinner

import (
	"math"
	"math/rand/v2"
	"strings"
	"time"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

type Mode int

const (
	ModeBoth Mode = iota
	ModeSpinnerOnly
)

type Spinner interface {
	layout.Model
	Reset() Spinner
	Stop()
	SetMessage(msg string)
	RawFrame() string
}

type spinner struct {
	ar                  *animation.Runtime
	animSub             animation.Subscription // manages animation tick subscription
	dotsStyle           func() lipgloss.Style
	themed              bool
	frameTheme          *[4]lipgloss.Style
	spinnerAnim         animation.Spinner
	spinnerFrames       animation.Frames
	styledSpinnerFrames []string
	mode                Mode
	currentMessage      string
	lightStartedAt      time.Duration
}

const (
	spinnerFrameDuration = animation.ChatSpinnerFrameDuration
	lightStepDuration    = animation.SpinnerLightStepDuration
	lightPauseDuration   = animation.SpinnerLightPauseDuration
)

// Default messages for the spinner
var defaultMessages = []string{
	"Working",
	"Reticulating splines",
	"Computing",
	"Thinking",
	"Processing",
	"Analyzing",
	"Calibrating",
	"Initializing",
	"Generating",
	"Evaluating",
	"Synthesizing",
	"Optimizing",
	"Consulting the oracle",
	"Summoning electrons",
	"Warming up the flux capacitor",
	"Reversing the polarity",
	"Spinning up the hamster wheels",
	"Herding cats",
	"Untangling yarn",
	"Aligning the cosmos",
	"Brewing digital coffee",
	"Wrangling bits and bytes",
	"Charging the crystals",
	"Consulting the rubber duck",
	"Feeding the gremlins",
	"Polishing the pixels",
	"Calibrating the thrusters",
}

func New(ar *animation.Runtime, mode Mode, dotsStyle lipgloss.Style) Spinner {
	return NewWithAnimation(ar, mode, dotsStyle, animation.Chat)
}

func NewThemed(ar *animation.Runtime, mode Mode, dotsStyle func() lipgloss.Style) Spinner {
	return newSpinner(ar, mode, dotsStyle, animation.Chat, true)
}

// NewWithFrames creates a spinner that animates using the provided frame set.
// If frames is empty, animation.Chat is used.
func NewWithFrames(ar *animation.Runtime, mode Mode, dotsStyle lipgloss.Style, frames animation.Frames) Spinner {
	if len(frames) == 0 {
		return NewWithAnimation(ar, mode, dotsStyle, animation.Chat)
	}
	return NewWithAnimation(ar, mode, dotsStyle, animation.NewSpinner(frames, spinnerFrameDuration))
}

// NewWithAnimation creates a spinner that uses the provided duration-aware
// animation. If anim is empty, animation.Chat is used.
func NewWithAnimation(ar *animation.Runtime, mode Mode, dotsStyle lipgloss.Style, anim animation.Spinner) Spinner {
	return newSpinner(ar, mode, func() lipgloss.Style { return dotsStyle }, anim, false)
}

func newSpinner(ar *animation.Runtime, mode Mode, dotsStyle func() lipgloss.Style, anim animation.Spinner, themed bool) Spinner {
	if ar == nil {
		panic("spinner: nil animation runtime")
	}
	if anim.Len() == 0 {
		anim = animation.Chat
	}
	spinnerFrames := anim.Frames()

	s := &spinner{
		ar:                  ar,
		animSub:             ar.Subscribe(),
		dotsStyle:           dotsStyle,
		themed:              themed,
		spinnerAnim:         anim,
		spinnerFrames:       spinnerFrames,
		styledSpinnerFrames: make([]string, len(spinnerFrames)),
		mode:                mode,
		currentMessage:      defaultMessages[rand.IntN(len(defaultMessages))],
	}
	s.renderFrames(styles.CurrentSpinnerPalette())
	return s
}

func (s *spinner) Reset() Spinner {
	return newSpinner(s.ar, s.mode, s.dotsStyle, s.spinnerAnim, s.themed)
}

// SetMessage replaces the current spinner text.
func (s *spinner) SetMessage(msg string) {
	s.currentMessage = msg
	s.lightStartedAt = s.ar.Now()
}

func (s *spinner) lightPositionAt(elapsed time.Duration) int {
	const left = -3
	const turn = left - 1
	right := utf8.RuneCountInString(s.currentMessage) + 3
	outbound := time.Duration(right-left) * lightStepDuration
	back := time.Duration(right-turn) * lightStepDuration
	phase := max(0, elapsed-s.lightStartedAt) % (outbound + lightPauseDuration + back + lightStepDuration)
	switch {
	case phase < outbound:
		progress := animation.ProgressAt(phase, outbound, nil)
		return left + int(math.Floor(math.Nextafter(progress*float64(right-left), math.Inf(1))))
	case phase < outbound+lightPauseDuration:
		return right
	default:
		progress := animation.ProgressAt(phase-outbound-lightPauseDuration, back, nil)
		return right - int(math.Floor(math.Nextafter(progress*float64(right-turn), math.Inf(1))))
	}
}

func (s *spinner) Update(message tea.Msg) (layout.Model, tea.Cmd) {
	if tick, ok := message.(animation.TickMsg); ok {
		before, after := tick.ElapsedBounds()
		if s.spinnerAnim.FrameIndexAt(before) != s.spinnerAnim.FrameIndexAt(after) {
			tick.MarkDirty()
		}
		if s.mode == ModeBoth && s.lightPositionAt(before) != s.lightPositionAt(after) {
			tick.MarkDirty()
		}
	}
	return s, nil
}

func (s *spinner) RawFrame() string {
	return s.spinnerFrames[s.spinnerAnim.FrameIndexAt(s.ar.Now())]
}

func (s *spinner) renderFrames(theme *[4]lipgloss.Style) {
	style := s.dotsStyle()
	for i, frame := range s.spinnerFrames {
		s.styledSpinnerFrames[i] = style.Render(frame)
	}
	s.frameTheme = theme
}

func (s *spinner) View() string {
	theme := styles.CurrentSpinnerPalette()
	if s.themed && s.frameTheme != theme {
		s.renderFrames(theme)
	}
	spinner := s.styledSpinnerFrames[s.spinnerAnim.FrameIndexAt(s.ar.Now())]
	if s.mode == ModeSpinnerOnly {
		return spinner
	}
	return spinner + " " + s.renderMessage(theme)
}

func (s *spinner) SetSize(_, _ int) tea.Cmd { return nil }

// Init registers the spinner with the animation coordinator.
// If this is the first active animation, it starts the global tick.
func (s *spinner) Init() tea.Cmd {
	if !s.animSub.IsActive() {
		s.lightStartedAt = s.ar.Now()
	}
	return s.animSub.Start()
}

// Stop unregisters the spinner from the animation coordinator.
// Call this when the spinner is no longer active/visible.
func (s *spinner) Stop() {
	s.animSub.Stop()
}

func (s *spinner) renderMessage(lightStyles *[4]lipgloss.Style) string {
	var out strings.Builder
	position := s.lightPositionAt(s.ar.Now())
	for i, char := range s.currentMessage {
		dist := min(max(i-position, position-i), len(lightStyles)-1)
		out.WriteString(lightStyles[dist].Render(string(char)))
	}
	return out.String()
}
