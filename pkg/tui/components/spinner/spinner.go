package spinner

import (
	"math/rand/v2"
	"strings"

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
	lightPosition       int
	direction           int // 1 for forward, -1 for backward
	pauseStepsRemaining int
	lastLightStep       int
	lightStepInit       bool
	lightPauseSteps     int
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
		lightPosition:       -3,
		direction:           1,
		lightPauseSteps:     max(1, int(lightPauseDuration/lightStepDuration)),
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
	s.lightPosition = -3
	s.direction = 1
	s.pauseStepsRemaining = 0
	s.lastLightStep = animation.TimedStep(s.ar.Now(), lightStepDuration)
	s.lightStepInit = true
}

func (s *spinner) advanceLightStep() {
	if s.pauseStepsRemaining > 0 {
		s.pauseStepsRemaining--
		if s.pauseStepsRemaining == 0 {
			s.direction = -1
		}
		return
	}

	s.lightPosition += s.direction
	if s.direction == 1 && s.lightPosition > len([]rune(s.currentMessage))+2 {
		s.pauseStepsRemaining = s.lightPauseSteps
	} else if s.direction == -1 && s.lightPosition < -3 {
		s.direction = 1
	}
}

func (s *spinner) Update(message tea.Msg) (layout.Model, tea.Cmd) {
	if tick, ok := message.(animation.TickMsg); ok {
		before, after := tick.ElapsedBounds()
		if s.spinnerAnim.FrameIndexAt(before) != s.spinnerAnim.FrameIndexAt(after) {
			tick.MarkDirty()
		}
		if s.mode == ModeBoth {
			targetStep := animation.TimedStep(s.ar.Now(), lightStepDuration)
			beforeStep := animation.TimedStep(before, lightStepDuration)
			if beforeStep != targetStep {
				tick.MarkDirty()
			}
			if !s.lightStepInit {
				s.lastLightStep = targetStep
				s.lightStepInit = true
			} else {
				for s.lastLightStep < targetStep {
					s.lastLightStep++
					s.advanceLightStep()
				}
				if targetStep < s.lastLightStep {
					s.lastLightStep = targetStep
				}
			}
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
	return s.animSub.Start()
}

// Stop unregisters the spinner from the animation coordinator.
// Call this when the spinner is no longer active/visible.
func (s *spinner) Stop() {
	s.animSub.Stop()
}

func (s *spinner) renderMessage(lightStyles *[4]lipgloss.Style) string {
	var out strings.Builder
	for i, char := range s.currentMessage {
		dist := min(max(i-s.lightPosition, s.lightPosition-i), len(lightStyles)-1)
		out.WriteString(lightStyles[dist].Render(string(char)))
	}
	return out.String()
}
