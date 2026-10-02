package leantui

import (
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/app/lifecycle"
	"github.com/docker/docker-agent/pkg/audio/transcribe"
	"github.com/docker/docker-agent/pkg/gitbranch"
	"github.com/docker/docker-agent/pkg/history"
	"github.com/docker/docker-agent/pkg/leantui/ui"
	"github.com/docker/docker-agent/pkg/plans"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/sound"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/tui/components/messagebar"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/service/supervisor"
	"github.com/docker/docker-agent/pkg/tui/service/tuistate"
	"github.com/docker/docker-agent/pkg/tui/styles"
	"github.com/docker/docker-agent/pkg/tui/subagentindex"
	"github.com/docker/docker-agent/pkg/userconfig"
)

// Transcriber is the existing platform audio service, injectable for tests.
type Transcriber interface {
	Start(ctx context.Context, handler transcribe.TranscriptHandler) error
	Stop()
	IsRunning() bool
	IsSupported() bool
}

// TabStore reuses the host's existing ordered tab metadata store.
type TabStore interface {
	GetTabs(ctx context.Context) ([]tuistate.TabEntry, string, error)
	AddTab(ctx context.Context, sessionID, workingDir string) error
	RemoveTab(ctx context.Context, sessionID string) error
	SetActiveTab(ctx context.Context, sessionID string) error
	ClearTabs(ctx context.Context) error
	ReplaceTab(ctx context.Context, oldID, newID, workingDir string) error
}

// Config wires the lean TUI to a prepared App and the initial run parameters.
type Config struct {
	SessionViews   supervisor.SessionViewAcquirer
	TabStore       TabStore
	RestoreTabs    bool
	RestoreSession func(context.Context, string, string) (*app.App, func(), error)
	Transcriber    Transcriber
	App            *app.App
	WorkingDir     string
	Cleanup        func()
	History        *history.History
	// SpawnSession is the host spawner: nil source creates; nonnil source forks.
	// Cleanup is supplied only for an owned runtime.
	SpawnSession func(context.Context, string, *session.Session) (*app.App, func(), error)

	FirstMessage           *string
	FirstMessageAttachment string
	QueuedMessages         []string

	AppName          string
	DisabledCommands []string
	RenderImages     *bool
	// ShowBanner displays the ASCII-art welcome banner. Defaults to true
	// when nil.
	ShowBanner *bool

	// Banner overrides the ASCII-art welcome banner. When nil the built-in
	// bannerLines ("docker agent") is used; embedders set it to brand the lean
	// TUI with their own art (each line ideally within 56 columns).
	Banner []string
}

// Run drives the lean TUI until the user exits. It owns the terminal (raw
// mode, no alternate screen) for its lifetime and restores it on return.
func Run(ctx context.Context, cfg Config) error {
	return runWithTerminalFactory(ctx, cfg, ui.NewTerminal)
}

// The factory seam lets lifecycle failure tests avoid a physical terminal.
func runWithTerminalFactory(ctx context.Context, cfg Config, factory func(*os.File, *os.File) (*ui.Terminal, error)) error {
	if cfg.Cleanup != nil {
		defer cfg.Cleanup()
	}
	loopCtx, loopCancel := context.WithCancel(ctx)
	defer loopCancel()
	host := &viewerHost{ctx: func() context.Context { return loopCtx }, views: make(map[*app.App]*model), applications: []*app.App{cfg.App}}
	defer host.close()
	term, err := factory(os.Stdin, os.Stdout)
	if err != nil {
		return err
	}
	defer term.Restore()

	if cfg.History == nil {
		cfg.History, err = history.New("")
		if err != nil {
			slog.WarnContext(ctx, "Failed to initialize command history", "error", err)
		}
	}

	m := newModel(term, cfg)
	m.viewers = host
	defer m.stopSpeech()
	m.restoreViewerMetadata(loopCtx, cfg)
	host.cleanup = m.restoredCleanup
	branchWatcher, err := gitbranch.Watch(loopCtx, m.app.Session().WorkingDir)
	if err != nil {
		return err
	}
	m.status.Branch = branchWatcher.Current()
	m.commitWelcome()
	m.loadInitialSessionTranscript()
	m.refreshCommands(loopCtx)

	keys := make(chan ui.Key, 64)
	events := make(chan any, 256)
	resizes := make(chan [2]int, 4)
	done := make(chan struct{})
	defer close(done)

	keyReaderDone := make(chan struct{})
	keyReaderStop := make(chan struct{})
	startKeyReader := func() {
		reader := term.Reader()
		finished := keyReaderDone
		stop := keyReaderStop
		go func() {
			defer close(finished)
			readKeys(reader, keys, stop)
		}()
	}
	startKeyReader()
	defer func() { close(keyReaderStop) }()
	m.runExternal = func(command *exec.Cmd) error {
		close(keyReaderStop)
		term.Reader().Cancel()
		<-keyReaderDone
		err := term.RunExternal(command)
		keyReaderDone = make(chan struct{})
		keyReaderStop = make(chan struct{})
		startKeyReader()
		m.r.Repaint()
		return err
	}
	host.events, host.store, host.cold, host.restore = events, cfg.TabStore, m.restoredEntries, cfg.RestoreSession
	if theme := styles.CurrentTheme(); theme != nil {
		m.retargetThemeWatcher(theme.Ref)
	}
	m.subscribeViewer(loopCtx, m.app)
	m.app.Start(loopCtx)
	go func() {
		for {
			w, h, ok := term.Resized()
			if !ok {
				return
			}
			select {
			case resizes <- [2]int{w, h}:
			case <-done:
				return
			}
		}
	}()

	if cfg.FirstMessage != nil || cfg.FirstMessageAttachment != "" {
		first := ""
		if cfg.FirstMessage != nil {
			first = *cfg.FirstMessage
		}
		m.sendFirstMessage(loopCtx, first, cfg.FirstMessageAttachment)
	}
	for _, msg := range cfg.QueuedMessages {
		if command, ok := strings.CutPrefix(strings.TrimSpace(msg), "!"); ok {
			m.runBangCommand(loopCtx, command)
			continue
		}
		m.enqueueFollowUp(msg, msg)
	}

	animationTicker := time.NewTicker(100 * time.Millisecond)
	defer animationTicker.Stop()
	branchChanges := branchWatcher.Changes()
	branchOwner := m.app

	m.render()
	for !m.quitting {
		select {
		case <-loopCtx.Done():
			m.quitting = true
		case k := <-keys:
			m.handleKey(loopCtx, k)
			m.render()
		case ev := <-events:
			m.routeViewerEvent(loopCtx, ev)
			m.render()
		case sz := <-resizes:
			m.width, m.height = sz[0], sz[1]
			m.r.SetSize(sz[0], sz[1])
			m.render()
		case <-animationTicker.C:
			if m.majorNoticeVisible && !m.hasMajorNotice(time.Now()) {
				m.render()
			}
			if m.busy() {
				m.spinnerFrame++
				m.render()
			}
		case branch, ok := <-branchChanges:
			if !ok {
				branchChanges = nil
				continue
			}
			m.routeViewerEvent(loopCtx, viewerEvent{origin: branchOwner, event: viewerBranch(branch)})
			m.render()
		}
	}

	m.renderFinal()
	m.viewers.close()
	return nil
}

func readKeys(r io.Reader, keys chan<- ui.Key, done <-chan struct{}) {
	p := &ui.InputParser{}
	buf := make([]byte, 8192)
	for {
		n, err := r.Read(buf)
		for _, k := range p.Feed(buf[:n]) {
			select {
			case keys <- k:
			case <-done:
				return
			}
		}
		if err != nil {
			return
		}
	}
}

type model struct {
	subagentsCompletionText string
	sessionViews            supervisor.SessionViewAcquirer
	viewAcquireCancel       context.CancelFunc
	viewAcquireGeneration   uint64
	transcriber             Transcriber
	speechGeneration        uint64
	majorEvents             *messagebar.Aggregator
	majorHighWater          [2]uint64
	majorNoticeVisible      bool
	priorityNoticeUntil     time.Time
	restoredEntries         []tuistate.TabEntry
	restoredCleanup         []func()
	soundSequence           uint64
	soundEnabled            bool
	soundThreshold          time.Duration
	streamStarted           time.Time
	playSound               func(context.Context, sound.Event)
	historyStore            *history.History
	spawnSession            func(context.Context, string, *session.Session) (*app.App, func(), error)
	runExternal             func(*exec.Cmd) error
	plansService            plans.Service
	themeResolve            func(string) string
	themeList               func() ([]string, error)
	themeLoad               func(string) (*styles.Theme, error)
	themeApply              func(*styles.Theme)
	settingsSave            func(func(*userconfig.Config) error) error
	queueSendMode           bool
	interruptMode           string
	interruptPending        bool
	lastInterrupt           time.Time
	budgetUsage             *runtime.BudgetUsageEvent
	subagentSnapshot        *subagent.Snapshot
	elicitations            map[string]*runtime.ElicitationRequestEvent
	maxIterations           map[string]*runtime.MaxIterationsReachedEvent
	viewers                 *viewerHost
	draftAttachments        []messages.Attachment
	app                     *app.App
	term                    *ui.Terminal
	r                       *ui.Renderer

	width  int
	height int

	screen *ui.Screen

	status       ui.StatusModel
	sessionState *service.SessionState
	usage        *ui.UsageTracker

	lifecycle            lifecycle.State
	spinnerFrame         int
	cancelMarkerPending  bool
	queue                []ui.PendingUserMessage
	pendingUsers         []ui.PendingUserMessage
	inputReplay          lifecycle.InputReplay
	inputReferences      *subagentindex.Index
	inputParentSessionID string
	ownedSkillOperation  string

	quitting         bool
	appName          string
	banner           []string
	disabledCommands map[string]bool
	renderImages     bool
	// hideBanner drops the ASCII-art welcome banner; the zero value keeps it.
	hideBanner bool
}

// busy is presentation derived from the canonical lifecycle projection. A
// submission, local cancel request or viewer switch cannot invent execution.
func (m *model) busy() bool {
	switch m.lifecycle.Status {
	case runtime.SessionStateRunning, runtime.SessionStateQueued, runtime.SessionStateCancelling:
		return true
	default:
		return m.status.Compacting
	}
}

func newModel(term *ui.Terminal, cfg Config) *model {
	w, h := term.Size()
	appName := cfg.AppName
	if appName == "" {
		appName = "docker agent"
	}
	disabled := make(map[string]bool, len(cfg.DisabledCommands))
	for _, c := range cfg.DisabledCommands {
		disabled[strings.TrimPrefix(c, "/")] = true
	}

	sessionState := service.NewSessionState(nil)
	if cfg.App != nil {
		sessionState = service.NewSessionState(cfg.App.Session())
	}

	if cfg.Transcriber == nil {
		cfg.Transcriber = transcribe.New(os.Getenv("OPENAI_API_KEY"))
	}
	renderImages := cfg.RenderImages == nil || *cfg.RenderImages

	branch := gitbranch.Current(cfg.WorkingDir)

	settings := userconfig.Get()
	applySavedSubagentsPreference(cfg.App)
	interruptMode := settings.GetInterruptConfirmation()
	return &model{
		app:              cfg.App,
		sessionViews:     cfg.SessionViews,
		historyStore:     cfg.History,
		spawnSession:     cfg.SpawnSession,
		transcriber:      cfg.Transcriber,
		interruptMode:    interruptMode,
		queueSendMode:    settings.GetBusySendMode() == "queue",
		settingsSave:     userconfig.Update,
		soundEnabled:     settings.GetSound(),
		soundThreshold:   time.Duration(settings.GetSoundThreshold()) * time.Second,
		playSound:        sound.Play,
		majorEvents:      &messagebar.Aggregator{},
		term:             term,
		r:                ui.NewRenderer(term.Writer(), w, h),
		width:            w,
		height:           h,
		screen:           ui.NewScreen(cfg.WorkingDir, branch, "Type a message, / for commands", cfg.History),
		status:           ui.StatusModel{WorkingDir: cfg.WorkingDir, Branch: branch},
		sessionState:     sessionState,
		usage:            ui.NewUsageTracker(),
		appName:          appName,
		banner:           cfg.Banner,
		disabledCommands: disabled,
		renderImages:     renderImages,
		hideBanner:       cfg.ShowBanner != nil && !*cfg.ShowBanner,
	}
}

// render assembles the full frame and reconciles it with the terminal.
func (m *model) render() {
	lines, cursorLine, cursorCol := m.buildLines()
	m.r.Frame(lines, cursorLine, cursorCol)
}

// renderFinal repaints the current state, then erases the input box and footer
// so only the conversation remains once the program exits.
func (m *model) renderFinal() {
	m.screen.Transcript.FlushPending()
	m.render()
	m.r.EraseBelow(len(m.screen.Transcript.Lines(m.width, m.spinnerFrame, m.busy(), m.sessionState, m.pendingUsers)))
}

func (m *model) commitWelcome() {
	banner := m.banner
	if len(banner) == 0 {
		banner = bannerLines
	}
	if m.hideBanner {
		banner = nil
	}
	m.screen.Transcript.AddBlock(func(int) []string {
		lines := make([]string, 0, bannerTopPadding+len(banner)+2)
		for range bannerTopPadding {
			lines = append(lines, "")
		}

		leftPad := strings.Repeat(" ", bannerLeftPadding)
		for _, l := range banner {
			lines = append(lines, ui.StAccent().Render(leftPad+l))
		}
		lines = append(lines,
			"",
			ui.StMuted().Render(leftPad+"Type a message, press / for commands, Ctrl+C to quit."),
		)
		return lines
	})
}
