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
	// RawFrame returns the current spinner character without any styling applied.
	RawFrame() string
}
type spinner struct {
	animSub             *animation.Subscription // manages animation tick subscription
	dotsStyle           lipgloss.Style
	styledSpinnerFrames []string // pre-rendered spinner frames
	mode                Mode
	currentMessage      string
	lightPosition       int
	frame               int
	direction           int // 1 for forward, -1 for backward
	pauseFrames         int
}

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

func New(mode Mode, dotsStyle lipgloss.Style) Spinner {
	// Pre-render all spinner frames for fast lookup during render
	styledFrames := make([]string, len(spinnerFrames))
	for i, char := range spinnerFrames {
		styledFrames[i] = dotsStyle.Render(char)
	}

	sub := &animation.Subscription{}
	return &spinner{
		animSub:             sub,
		dotsStyle:           dotsStyle,
		styledSpinnerFrames: styledFrames,
		mode:                mode,
		currentMessage:      defaultMessages[rand.IntN(len(defaultMessages))],
		lightPosition:       -3,
		direction:           1,
	}
}

func (s *spinner) Reset() Spinner {
	return New(s.mode, s.dotsStyle)
}

func (s *spinner) Update(message tea.Msg) (layout.Model, tea.Cmd) {
	if msg, ok := message.(animation.TickMsg); ok {
		// Respond to global animation tick (all spinners advance together)
		s.frame = msg.Frame
		// Light animation for ModeBoth spinners
		if s.mode == ModeBoth {
			if s.pauseFrames > 0 {
				s.pauseFrames--
				if s.pauseFrames == 0 {
					s.direction = -1
				}
			} else {
				s.lightPosition += s.direction
				if s.direction == 1 && s.lightPosition > len([]rune(s.currentMessage))+2 {
					s.pauseFrames = 6
				} else if s.direction == -1 && s.lightPosition < -3 {
					s.direction = 1
				}
			}
		}
	}
	return s, nil
}

func (s *spinner) View() string {
	spinner := s.styledSpinnerFrames[s.frame%len(s.styledSpinnerFrames)]
	if s.mode == ModeSpinnerOnly {
		return spinner
	}
	return spinner + " " + s.renderMessage()
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

// RawFrame returns the current spinner character without any styling applied.
func (s *spinner) RawFrame() string {
	return spinnerFrames[s.frame%len(spinnerFrames)]
}

// spinnerFrames holds the animation frames for the current terminal.
var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// Frame returns the spinner character for the given animation frame.
func Frame(index int) string {
	return spinnerFrames[index%len(spinnerFrames)]
}

// lightStyles maps distance from light position to style (0=brightest, 1=bright, 2=dim, 3+=dimmest).
var lightStyles = []lipgloss.Style{
	styles.SpinnerTextBrightestStyle,
	styles.SpinnerTextBrightStyle,
	styles.SpinnerTextDimStyle,
	styles.SpinnerTextDimmestStyle,
}

func (s *spinner) renderMessage() string {
	var out strings.Builder
	for i, char := range s.currentMessage {
		dist := min(max(i-s.lightPosition, s.lightPosition-i), len(lightStyles)-1)
		out.WriteString(lightStyles[dist].Render(string(char)))
	}
	return out.String()
}
