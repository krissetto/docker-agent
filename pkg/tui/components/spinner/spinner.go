package spinner

import (
	"math/rand/v2"
	"reflect"
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
	dotsStyle           lipgloss.Style
	themeStyle          func() lipgloss.Style
	themeGeneration     uint64
	spinnerAnim         animation.Spinner
	spinnerFrames       animation.Frames
	styledSpinnerFrames []string // pre-rendered spinner frames
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

// NewWithStyleProvider binds a semantic role independently of coincident theme colors.
func NewWithStyleProvider(ar *animation.Runtime, mode Mode, style func() lipgloss.Style) Spinner {
	if style == nil {
		panic("spinner: nil style provider")
	}
	s := NewWithAnimation(ar, mode, style(), animation.Chat).(*spinner)
	s.themeStyle = style
	return s
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
	if ar == nil {
		panic("spinner: nil animation runtime")
	}
	if anim.Len() == 0 {
		anim = animation.Chat
	}
	spinnerFrames := anim.Frames()

	// Pre-render all spinner frames for fast lookup during render.
	styledFrames := make([]string, len(spinnerFrames))
	for i, char := range spinnerFrames {
		styledFrames[i] = dotsStyle.Render(char)
	}

	var themeStyle func() lipgloss.Style
	switch {
	case reflect.DeepEqual(dotsStyle, styles.SpinnerDotsAccentStyle):
		themeStyle = func() lipgloss.Style { return styles.SpinnerDotsAccentStyle }
	case reflect.DeepEqual(dotsStyle, styles.SpinnerDotsHighlightStyle):
		themeStyle = func() lipgloss.Style { return styles.SpinnerDotsHighlightStyle }
	}
	return &spinner{
		themeStyle:          themeStyle,
		themeGeneration:     styles.ThemeGeneration(),
		ar:                  ar,
		animSub:             ar.Subscribe(),
		dotsStyle:           dotsStyle,
		spinnerAnim:         anim,
		spinnerFrames:       spinnerFrames,
		styledSpinnerFrames: styledFrames,
		mode:                mode,
		currentMessage:      defaultMessages[rand.IntN(len(defaultMessages))],
		lightPosition:       -3,
		direction:           1,
		lightPauseSteps:     max(1, int(lightPauseDuration/lightStepDuration)),
	}
}

func (s *spinner) Reset() Spinner {
	s.ensureTheme()
	reset := NewWithAnimation(s.ar, s.mode, s.dotsStyle, s.spinnerAnim).(*spinner)
	reset.themeStyle = s.themeStyle
	return reset
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
	s.ensureTheme()
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

func (s *spinner) View() string {
	s.ensureTheme()
	frame := s.spinnerAnim.FrameIndexAt(s.ar.Now())
	spinner := s.styledSpinnerFrames[frame]
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

func (s *spinner) ensureTheme() {
	generation := styles.ThemeGeneration()
	if s.themeGeneration == generation {
		return
	}
	s.themeGeneration = generation
	if s.themeStyle != nil {
		s.dotsStyle = s.themeStyle()
		for i, char := range s.spinnerFrames {
			s.styledSpinnerFrames[i] = s.dotsStyle.Render(char)
		}
	}
}

func (s *spinner) renderMessage() string {
	lightStyles := [...]lipgloss.Style{
		styles.SpinnerTextBrightestStyle,
		styles.SpinnerTextBrightStyle,
		styles.SpinnerTextDimStyle,
		styles.SpinnerTextDimmestStyle,
	}
	var out strings.Builder
	for i, char := range s.currentMessage {
		dist := min(max(i-s.lightPosition, s.lightPosition-i), len(lightStyles)-1)
		out.WriteString(lightStyles[dist].Render(string(char)))
	}
	return out.String()
}
