package root

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/mattn/go-isatty"
	"github.com/spf13/cobra"
	"go.opentelemetry.io/otel"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/creator"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/telemetry"
	"github.com/docker/docker-agent/pkg/tui"
	tuiimage "github.com/docker/docker-agent/pkg/tui/image"
	tuiinput "github.com/docker/docker-agent/pkg/tui/input"
	viewhost "github.com/docker/docker-agent/pkg/tui/service/supervisor"
	"github.com/docker/docker-agent/pkg/tui/styles"
	"github.com/docker/docker-agent/pkg/userconfig"
)

type newFlags struct {
	modelParam         string
	maxIterationsParam int
	runConfig          config.RuntimeConfig
}

func newNewCmd() *cobra.Command {
	var flags newFlags

	cmd := &cobra.Command{
		Use:   "new [description]",
		Short: "Create a new agent configuration",
		Long: `Create a new agent configuration interactively.

The agent builder will ask questions about what you want the agent to do,
then generate a YAML configuration file you can use with 'docker-agent run'.

Optionally provide a description as an argument to skip the initial prompt.`,
		Example: `  docker-agent new
  docker-agent new "a web scraper that extracts product prices"
  docker-agent new --model openai/gpt-4o "a code reviewer agent"`,
		GroupID: "advanced",
		RunE:    flags.runNewCommand,
	}

	cmd.PersistentFlags().StringVar(&flags.modelParam, "model", "", "Model to use, optionally as provider/model where provider is one of: anthropic, openai, google, dmr, or a custom provider from `docker agent setup`. If omitted, provider is auto-selected based on available credentials or gateway")
	cmd.PersistentFlags().IntVar(&flags.maxIterationsParam, "max-iterations", 0, "Maximum number of agentic loop iterations to prevent infinite loops (default: 20 for DMR, unlimited for other providers)")
	addRuntimeConfigFlags(cmd, &flags.runConfig)

	return cmd
}

func (f *newFlags) runNewCommand(cmd *cobra.Command, args []string) (commandErr error) {
	ctx := cmd.Context()
	telemetry.TrackCommand(ctx, "new", args)
	defer func() { // do not inline this defer so that commandErr is not resolved early
		telemetry.TrackCommandError(ctx, "new", args, commandErr)
	}()

	loadResult, err := creator.Load(ctx, &f.runConfig, f.modelParam)
	if err != nil {
		return err
	}
	t := loadResult.Team
	defer stopToolSets(ctx, t)

	// Bind the creator's tools and runtime hooks to the same captured workspace.
	workingDir, err := session.CaptureLocalWorkingDir(f.runConfig.WorkingDir)
	if err != nil {
		return err
	}
	rt, err := runtime.NewLocalRuntime(ctx, t,
		runtime.WithWorkingDir(workingDir),
		runtime.WithUseSubagents(userconfig.Get().GetUseSubagents()),
		runtime.WithProviderRegistry(loadResult.ProviderRegistry),
		runtime.WithTracer(otel.Tracer(AppName)),
	)
	if err != nil {
		return err
	}

	supervisor := runtime.NewSessionRuntimeSupervisor(rt)
	defer func() { _ = supervisor.Shutdown(context.WithoutCancel(ctx)) }()

	var appOpts []app.Opt
	// The creator runs in the user's checkout and writes the generated agent
	// YAML there; retain the captured workspace as session provenance.
	sessOpts := []session.Opt{
		session.WithTitle("New agent"),
		session.WithMaxIterations(f.maxIterationsParam),
		session.WithToolsApproved(true),
		session.WithWorkingDir(workingDir),
	}
	if len(args) > 0 {
		arg := strings.Join(args, " ")
		sessOpts = append(sessOpts, session.WithUserMessage(arg))
		appOpts = append(appOpts, app.WithFirstMessage(arg))
	}

	sess := session.New(sessOpts...)

	// The agent builder shares the run TUI, so honour the user's configured
	// theme (including "auto") the same way `docker-agent run` does.
	applyTheme("")

	appOpts = append(appOpts, app.WithRuntimeServices(rt))
	sessions := supervisor.Runtime()
	viewSupervisor := viewhost.New(nil)
	scope := viewhost.NewViewOwnerScope()
	if err := viewSupervisor.ConfigureSessionViews(ctx, viewhost.HostViewConfig{
		Resolve:               localViewOwnerResolver(scope, "creator", rt.SessionStore()),
		MaxRetainedViewOwners: viewhost.DefaultMaxRetainedViewOwners,
	}); err != nil {
		return err
	}
	if err := viewSupervisor.RegisterSessionOwner(viewhost.ViewOwnerIdentity{
		Scope: scope, Source: "creator", RootSessionID: sess.ID, RootWorkingDir: workingDir,
		RootBinding: runtime.SessionBinding{AgentName: rt.CurrentAgentName(ctx)},
	}, viewhost.ViewOwnerResources{
		Services: rt, Sessions: sessions,
		NewApp:  resolvedViewBuilder(sessions, withTitleGenerator(ctx, rt, []app.Opt{app.WithRuntimeServices(rt)})),
		Cleanup: func() { _ = supervisor.Shutdown(context.WithoutCancel(ctx)) },
	}, true); err != nil {
		return err
	}
	defer viewSupervisor.Shutdown()
	return runTUI(ctx, rt, sessions, sess, runtime.SessionBinding{AgentName: rt.CurrentAgentName(ctx), Model: f.modelParam}, nil, viewSupervisor.Shutdown, []tui.Option{tui.WithSupervisor(viewSupervisor)}, appOpts...)
}

func runTUI(ctx context.Context, rt app.Services, sessions runtime.SessionRuntime, sess *session.Session, binding runtime.SessionBinding, spawner tui.SessionSpawner, cleanup func(), tuiOpts []tui.Option, opts ...app.Opt) error {
	return runTUIWrapped(ctx, rt, sessions, sess, binding, spawner, cleanup, tuiOpts, nil, opts...)
}

// runTUIWrapped is runTUI with an optional model wrapper, used by --record to
// interpose the input recorder between the terminal and the real model.
func runTUIWrapped(ctx context.Context, rt app.Services, sessions runtime.SessionRuntime, sess *session.Session, binding runtime.SessionBinding, spawner tui.SessionSpawner, cleanup func(), tuiOpts []tui.Option, wrap func(tea.Model) tea.Model, opts ...app.Opt) error {
	opts = withTitleGenerator(ctx, rt, opts)

	a := app.New(ctx, sessions, sess, binding, opts...)

	coalescer := tuiinput.NewMouseCoalescer()
	defer coalescer.Stop()
	filter := func(_ tea.Model, msg tea.Msg) tea.Msg {
		return coalescer.Filter(msg)
	}

	if cleanup == nil {
		cleanup = func() {}
	}
	// Prefer the session's working directory so the TUI (and features keyed
	// off it, like /shell) operate where the tools do — e.g. the worktree
	// created by --worktree, not the process CWD it was launched from.
	wd := sess.WorkingDir
	if wd == "" {
		wd, _ = os.Getwd()
	}
	imageWriter := tuiimage.NewWriter(os.Stdout)
	imageWriter.SetSupported(tuiimage.SupportsKittyGraphics(os.Stdin, os.Stdout))
	imageWriter.SetEnabled(userconfig.Get().GetRenderImages())
	tuiimage.SetRenderingEnabled(imageWriter.RenderingEnabled())
	if err := resetInheritedTerminalStyle(imageWriter, isatty.IsTerminal(os.Stdout.Fd())); err != nil {
		return err
	}
	tuiOpts = append(tuiOpts, tui.WithImageWriter(imageWriter))
	model := tui.New(ctx, spawner, a, wd, cleanup, tuiOpts...)
	if wrap != nil {
		model = wrap(model)
	}

	p := tea.NewProgram(model, tea.WithContext(ctx), tea.WithFilter(filter), tea.WithOutput(imageWriter))
	coalescer.SetSender(p.Send)

	if m, ok := model.(interface{ SetProgram(p *tea.Program) }); ok {
		m.SetProgram(p)
	}

	_, err := p.Run()
	resetLightDarkReports()
	return err
}

// Reset synchronously before Bubble Tea's first erase; its renderer starts
// with a default pen and cannot detect styles inherited from the shell.
func resetInheritedTerminalStyle(out io.Writer, terminal bool) error {
	if !terminal {
		return nil
	}
	n, err := io.WriteString(out, ansi.ResetStyle)
	if err != nil {
		return fmt.Errorf("resetting terminal styling: %w", err)
	}
	if n != len(ansi.ResetStyle) {
		return fmt.Errorf("resetting terminal styling: %w", io.ErrShortWrite)
	}
	return nil
}

// resetLightDarkReports clears DEC mode 2031 after the TUI program exits
// while the auto theme is active, covering exits that bypass the model's own
// quit path (context cancellation, forced shutdown). A duplicate reset is
// harmless and invisible, and nothing is written when the auto theme was
// never enabled.
func resetLightDarkReports() {
	if !styles.AutoThemeEnabled() {
		return
	}
	_, _ = os.Stdout.WriteString(ansi.ResetModeLightDark)
}
