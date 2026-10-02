package root

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/mattn/go-isatty"
	"github.com/spf13/cobra"
	"go.opentelemetry.io/otel"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/app"
	boardtui "github.com/docker/docker-agent/pkg/board/tui"
	"github.com/docker/docker-agent/pkg/cli"
	"github.com/docker/docker-agent/pkg/config"
	latestcfg "github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/hooks"
	"github.com/docker/docker-agent/pkg/hooks/builtins"
	"github.com/docker/docker-agent/pkg/input"
	"github.com/docker/docker-agent/pkg/leantui"
	"github.com/docker/docker-agent/pkg/permissions"
	"github.com/docker/docker-agent/pkg/profiling"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/server"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/sessiontitle"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/teamloader"
	loaderdefaults "github.com/docker/docker-agent/pkg/teamloader/defaults"
	"github.com/docker/docker-agent/pkg/telemetry"
	"github.com/docker/docker-agent/pkg/tour"
	"github.com/docker/docker-agent/pkg/tui"
	tuiimage "github.com/docker/docker-agent/pkg/tui/image"
	"github.com/docker/docker-agent/pkg/tui/recorder"
	viewhost "github.com/docker/docker-agent/pkg/tui/service/supervisor"
	"github.com/docker/docker-agent/pkg/tui/service/tuistate"
	"github.com/docker/docker-agent/pkg/tui/styles"
	"github.com/docker/docker-agent/pkg/userconfig"
	"github.com/docker/docker-agent/pkg/worktree"
)

// worktreeAutoName is the value stored when --worktree is given without an
// explicit name (cobra's NoOptDefVal). It also doubles as the reserved name a
// user can pass explicitly (--worktree=auto) to request a generated name; with
// cobra's optional-value flags the two are indistinguishable by design.
const worktreeAutoName = "auto"

var projectDefaultAgentFiles = []string{"docker-agent.yaml", "docker-agent.yml", "docker-agent.hcl"}

type runExecFlags struct {
	agentName   string
	autoApprove bool
	safety      string
	// safetyChanged / yoloChanged record whether --safety / --yolo were
	// explicitly passed on the command line. Explicit flags are the only
	// safety sources allowed to override a resumed session's stored mode;
	// alias options and user settings are defaults that never do.
	safetyChanged bool
	yoloChanged   bool
	// defaultSafety is the user-owned safety default resolved from alias
	// options and user settings (alias wins; within each scope safety wins
	// over the legacy yolo/YOLO flag). Never populated from CLI flags or
	// author YAML.
	defaultSafety session.SafetyPolicy
	// workingDirChanged records whether a non-empty --working-dir was
	// explicitly passed on the command line. Captured before worktree and
	// resume handling mutate runConfig.WorkingDir; only an explicit flag
	// makes generic new-session actions in the TUI reuse the initial
	// session's directory instead of opening the picker.
	workingDirChanged bool
	attachmentPath    string
	remoteAddress     string
	modelOverrides    []string
	promptFiles       []string
	dryRun            bool
	runConfig         config.RuntimeConfig
	sessionDB         string
	sessionID         string
	recordPath        string
	fakeResponses     string
	fakeStreamDelay   int
	exitAfterResponse bool
	cpuProfile        string
	memProfile        string
	forceTUI          bool
	tour              bool
	sandbox           bool
	sandboxTemplate   string
	sbx               bool
	noKit             bool
	agentPickerSpec   string
	worktree          bool
	worktreeName      string
	worktreePR        string
	worktreeBase      string
	sessionReadOnly   bool

	// Exec only
	exec          bool
	hideToolCalls bool
	outputJSON    bool

	// Run only
	hideToolResults bool
	lean            bool
	leanChanged     bool
	appName         string
	sidebar         bool
	listenAddr      string
	// sessionWorkingDirRoot confines the working_dir of sessions created
	// through the --listen control plane; empty means unrestricted (see
	// server.WithSessionWorkingDirRoot).
	sessionWorkingDirRoot string
	onEventSpecs          []string
	disabledCommands      []string
	theme                 string

	// globalPermissions holds the user-level global permission checker built
	// from user config settings. Nil when no global permissions are configured.
	globalPermissions *permissions.Checker
	snapshotsEnabled  bool

	// snapshotController is the [builtins.SnapshotController] for the
	// initial App: it is wired into the initial runtime as an
	// auto-injector and into the App via app.WithSnapshotController so
	// /undo, /snapshots, /reset drive the same instance that captures
	// the checkpoints. Sub-runtimes created by [createSessionSpawner]
	// build their own controller (and registry) so each spawned
	// session has independent snapshot state; that controller is local
	// to the spawner closure and never reaches this field.
	snapshotController builtins.SnapshotController

	// listenSM is the SessionManager behind the --listen control plane, set
	// by startSessionCoordinator. Nil when --listen is off.
	listenSM *server.SessionManager
	// listenSessions is the registry listenSM serves. Tabs that run on their
	// own runtime (another working directory) register it here so
	// control-plane clients can observe and drive every tab of the run — not
	// just the sessions of the shared runtime. Nil when --listen is off.
	listenSessions *controlPlaneSessions

	// One lifecycle host and source/auth scope serve every UI and control-plane
	// admission in this run. Execution remains in its existing runtime owners.
	sessionViewHost       *viewhost.Supervisor
	sessionViewScope      *viewhost.ViewOwnerScope
	sessionViewSource     string
	sessionViewWorkingDir string
	sessionViewStore      session.Store
}

func newRunCmd() *cobra.Command {
	var flags runExecFlags

	cmd := &cobra.Command{
		Use:   "run [<agent-file>|<registry-ref>] [message]...",
		Short: "Run an agent",
		Long:  "Run an agent with the specified configuration and prompt",
		Example: `  docker-agent run ./agent.yaml
  docker-agent run ./team.yaml --agent root
  docker-agent run # project config or built-in default agent
  docker-agent run coder # built-in coding agent
  docker-agent run ./echo.yaml "INSTRUCTIONS"
  docker-agent run ./echo.yaml "First question" "Follow-up question"
  echo "INSTRUCTIONS" | docker-agent run ./echo.yaml -
  docker-agent run ./agent.yaml --record  # Records session + generates a TUI e2e test`,
		GroupID:           "core",
		ValidArgsFunction: completeRunExec,
		Args:              cobra.ArbitraryArgs,
		RunE:              flags.runRunCommand,
	}

	addRunOrExecFlags(cmd, &flags)
	addRuntimeConfigFlags(cmd, &flags.runConfig)

	return cmd
}

func addRunOrExecFlags(cmd *cobra.Command, flags *runExecFlags) {
	cmd.PersistentFlags().StringVarP(&flags.agentName, "agent", "a", "", "Name of the agent to run (defaults to the team's first agent)")
	cmd.PersistentFlags().BoolVar(&flags.autoApprove, "yolo", false, "Automatically approve all tool calls without prompting (same as --safety autonomous)")
	cmd.PersistentFlags().StringVar(&flags.safety, "safety", "", "Safety mode for tool approval: strict (ask for everything), balanced (auto-approve safe calls), restricted (auto-approve safe calls, deny the rest — for unattended runs), or autonomous (approve everything)")
	cmd.PersistentFlags().BoolVar(&flags.hideToolResults, "hide-tool-results", false, "Hide tool call results")
	cmd.PersistentFlags().StringVar(&flags.attachmentPath, "attach", "", "Attach an image file to the message")
	cmd.PersistentFlags().StringArrayVar(&flags.promptFiles, "prompt-file", nil, "Append file contents to the prompt (repeatable)")
	cmd.PersistentFlags().StringArrayVar(&flags.modelOverrides, "model", nil, "Override agent model: [agent=]provider/model (repeatable)")
	cmd.PersistentFlags().BoolVar(&flags.dryRun, "dry-run", false, "Initialize the agent without executing anything")
	cmd.PersistentFlags().StringVar(&flags.remoteAddress, "remote", "", "Use remote runtime with specified address")
	cmd.PersistentFlags().StringVarP(&flags.sessionDB, "session-db", "s", "", "Path to the session database (default: <data-dir>/session.db)")
	cmd.PersistentFlags().StringVar(&flags.sessionID, "session", "", "Continue from a previous session by ID or relative offset (e.g., -1 for last session). An explicit ID that does not exist yet is created with that ID.")
	cmd.PersistentFlags().StringVar(&flags.fakeResponses, "fake", "", "Replay AI responses from cassette file (for testing)")
	cmd.PersistentFlags().IntVar(&flags.fakeStreamDelay, "fake-stream", 0, "Simulate streaming with delay in ms between chunks (default 15ms if no value given)")
	cmd.Flag("fake-stream").NoOptDefVal = "15" // --fake-stream without value uses 15ms
	cmd.PersistentFlags().StringVar(&flags.recordPath, "record", "", "Record AI API interactions to cassette file and generate a TUI e2e test from the session (auto-generates filename if empty)")
	cmd.PersistentFlags().Lookup("record").NoOptDefVal = "true"
	cmd.PersistentFlags().BoolVar(&flags.exitAfterResponse, "exit-after-response", false, "Exit TUI after first assistant response completes")
	_ = cmd.PersistentFlags().MarkHidden("exit-after-response")
	cmd.PersistentFlags().StringVar(&flags.listenAddr, "listen", "", "Expose this run's control plane on the given address (e.g. 127.0.0.1:0)")
	_ = cmd.PersistentFlags().MarkHidden("listen")
	cmd.PersistentFlags().StringVar(&flags.sessionWorkingDirRoot, "session-workingdir-root", "", "Restrict the working_dir of sessions created via the --listen control plane to this directory and its descendants (empty = no restriction)")
	_ = cmd.PersistentFlags().MarkHidden("session-workingdir-root")
	cmd.PersistentFlags().StringArrayVar(&flags.onEventSpecs, "on-event", nil, "Run shell command on event: --on-event <type>=<cmd> (or *=<cmd> for any). Repeatable.")
	cmd.PersistentFlags().StringVar(&flags.cpuProfile, "cpuprofile", "", "Write CPU profile to file")
	_ = cmd.PersistentFlags().MarkHidden("cpuprofile")
	cmd.PersistentFlags().StringVar(&flags.memProfile, "memprofile", "", "Write memory profile to file")
	_ = cmd.PersistentFlags().MarkHidden("memprofile")
	cmd.PersistentFlags().BoolVar(&flags.forceTUI, "force-tui", false, "Force TUI mode even when not in a terminal")
	_ = cmd.PersistentFlags().MarkHidden("force-tui")
	cmd.PersistentFlags().BoolVar(&flags.tour, "tour", false, "Start the interactive getting-started tour in the TUI")
	_ = cmd.PersistentFlags().MarkHidden("tour")
	cmd.PersistentFlags().BoolVar(&flags.lean, "lean", false, "Use a simplified TUI with minimal chrome")
	cmd.PersistentFlags().StringVar(&flags.appName, "app-name", "", "Application name shown in the TUI in place of \"docker agent\"")
	cmd.PersistentFlags().StringSliceVar(&flags.disabledCommands, "disable-commands", nil, "Comma-separated list of slash commands to hide and disable in the TUI (e.g. /cost,/eval,/model)")
	cmd.PersistentFlags().BoolVar(&flags.sidebar, "sidebar", true, "Show the sidebar in the TUI (set --sidebar=false to hide it)")
	cmd.PersistentFlags().StringVar(&flags.theme, "theme", "", "Preselect a TUI theme by name, or \"auto\" to match the terminal's light/dark background (overrides the theme from user config; ignored outside the interactive TUI)")
	_ = cmd.RegisterFlagCompletionFunc("theme", completeTheme)
	cmd.PersistentFlags().BoolVar(&flags.sandbox, "sandbox", false, "Run the agent inside a Docker sandbox (requires Docker Desktop with sandbox support)")
	cmd.PersistentFlags().StringVar(&flags.sandboxTemplate, "template", "docker/docker-agent-sbx-templates:latest", "Template image for the sandbox (passed to docker sandbox create -t)")
	cmd.PersistentFlags().BoolVar(&flags.sbx, "sbx", true, "Prefer the sbx CLI backend when available (set --sbx=false to force docker sandbox)")
	cmd.PersistentFlags().BoolVar(&flags.noKit, "no-kit", false, "Do not stage a docker-agent kit (skills, prompt files) when running in a sandbox")
	cmd.PersistentFlags().StringVar(&flags.agentPickerSpec, "agent-picker", "", "Show a full-screen picker to choose an agent before launching. Optional comma-separated list of agent refs; \"defaults\" (or no value) offers the built-in agents plus any configs in ~/.agents")
	cmd.PersistentFlags().Lookup("agent-picker").NoOptDefVal = agentPickerDefaultsSpec
	cmd.PersistentFlags().StringVarP(&flags.worktreeName, "worktree", "w", "", "Run the agent in a fresh git worktree of the working directory (isolates changes from your checkout). Optionally name it: --worktree=my-name")
	cmd.PersistentFlags().Lookup("worktree").NoOptDefVal = worktreeAutoName
	cmd.PersistentFlags().StringVar(&flags.worktreePR, "worktree-pr", "", "Run the agent in a git worktree checked out on an existing GitHub pull request (number or URL). Continues the PR's branch; requires the GitHub CLI (gh).")
	cmd.PersistentFlags().StringVar(&flags.worktreeBase, "worktree-base", "", "Branch the --worktree from this ref instead of the current HEAD (e.g. main, origin/main). A remote-tracking ref is fetched first so the worktree starts from the latest remote state.")
	cmd.PersistentFlags().BoolVar(&flags.sessionReadOnly, "session-read-only", false, "Open the session in read-only mode (view conversation history but prevent new messages)")
	cmd.MarkFlagsMutuallyExclusive("fake", "record")
	cmd.MarkFlagsMutuallyExclusive("remote", "sandbox")
	cmd.MarkFlagsMutuallyExclusive("remote", "session-db")
	cmd.MarkFlagsMutuallyExclusive("remote", "session")
	cmd.MarkFlagsMutuallyExclusive("remote", "record")
	cmd.MarkFlagsMutuallyExclusive("remote", "fake")
	// A worktree is a local directory: it has no meaning for a remote runtime
	// and is not wired through the sandbox boundary.
	cmd.MarkFlagsMutuallyExclusive("remote", "worktree")
	cmd.MarkFlagsMutuallyExclusive("sandbox", "worktree")
	cmd.MarkFlagsMutuallyExclusive("remote", "worktree-pr")
	cmd.MarkFlagsMutuallyExclusive("sandbox", "worktree-pr")
	cmd.MarkFlagsMutuallyExclusive("worktree", "worktree-pr")
	// --worktree-base picks the start-point of the branch --worktree creates,
	// so it is meaningless for a PR worktree (which continues the PR's branch)
	// or a remote/sandbox run (which has no local worktree).
	cmd.MarkFlagsMutuallyExclusive("worktree-base", "worktree-pr")
	cmd.MarkFlagsMutuallyExclusive("remote", "worktree-base")
	cmd.MarkFlagsMutuallyExclusive("sandbox", "worktree-base")

	// --exec only
	cmd.PersistentFlags().BoolVar(&flags.exec, "exec", false, "Execute without a TUI")
	cmd.PersistentFlags().BoolVar(&flags.hideToolCalls, "hide-tool-calls", false, "Hide the tool calls in the output")
	cmd.PersistentFlags().BoolVar(&flags.outputJSON, "json", false, "Output results in JSON format")
}

func (f *runExecFlags) runRunCommand(cmd *cobra.Command, args []string) (commandErr error) {
	ctx := cmd.Context()

	if f.exec {
		telemetry.TrackCommand(ctx, "exec", args)
		defer func() { // do not inline this defer so that commandErr is not resolved early
			telemetry.TrackCommandError(ctx, "exec", args, commandErr)
		}()
	} else {
		telemetry.TrackCommand(ctx, "run", args)
		defer func() { // do not inline this defer so that commandErr is not resolved early
			telemetry.TrackCommandError(ctx, "run", args, commandErr)
		}()
	}

	// Validate an explicit --theme value early so a typo fails fast with a
	// helpful message instead of silently falling back to the default theme
	// once the TUI starts.
	if f.theme != "" {
		if err := validateTheme(f.theme); err != nil {
			return err
		}
	}

	if err := validateSafetyFlag(f.safety); err != nil {
		return err
	}
	f.safetyChanged = cmd.Flags().Changed("safety")
	f.yoloChanged = cmd.Flags().Changed("yolo")
	// Captured here because runOrExec later overwrites runConfig.WorkingDir
	// for worktrees and resumed sessions.
	f.workingDirChanged = cmd.Flags().Changed("working-dir") && f.runConfig.WorkingDir != ""

	// A --session-workingdir-root that trims to empty (e.g. an unresolved
	// shell variable) must fail loudly instead of silently disabling the
	// containment the operator asked for.
	if err := validateSessionWorkingDirRoot(f.sessionWorkingDirRoot); err != nil {
		return err
	}

	useTUI := !f.exec && (f.forceTUI || isatty.IsTerminal(os.Stdout.Fd()))
	f.leanChanged = cmd.Flags().Changed("lean")

	// When --agent-picker is set, show a full-screen picker up front and use
	// the chosen ref as the agent to run. Resolving it here (before sandbox
	// and alias resolution) means the selected agent's own sandbox/alias
	// defaults are honoured exactly as if it had been passed positionally.
	// The picker is interactive, so it requires a TUI.
	if cmd.Flags().Changed("agent-picker") {
		if !useTUI {
			return errors.New("--agent-picker requires an interactive terminal and cannot be used with --exec")
		}
		refs := parseAgentPickerRefs(f.agentPickerSpec)
		applyTheme(f.theme)
		// Seed the picker's "Lean Mode" checkbox with the lean state the run
		// would otherwise use (--lean, or the user-config default applied in
		// applyUserSettings) so the checkbox never lies about the run mode.
		initialLean := f.lean || (!f.leanChanged && userconfig.Get().Lean && !f.tour)
		chosen, lean, err := selectAgentRef(ctx, refs, f.runConfig.EnvProvider(), initialLean, f.runConfig.Flavors)
		if err != nil {
			if errors.Is(err, errAgentPickerCancelled) {
				cli.NewPrinter(cmd.OutOrStdout()).Println("Agent selection cancelled.")
				return nil
			}
			if errors.Is(err, errAgentPickerStartBoard) {
				// Intentionally tracked in addition to the "run" event above:
				// the user asked for a run but ended up on the board.
				telemetry.TrackCommand(ctx, "board", nil)
				return boardtui.Run(ctx)
			}
			return err
		}
		// The checkbox is the user's explicit choice: it wins over both the
		// --lean flag and the user-config lean default.
		f.lean = lean
		f.leanChanged = true
		// With --agent-picker the agent comes from the picker, so any
		// positional args are messages. Prepend the chosen ref so the rest
		// of the pipeline (which expects args[0] to be the agent) is happy.
		args = prependAgentRef(chosen, args)
	}

	var discoveredProjectConfig bool
	args, discoveredProjectConfig = f.discoverRunAgentArgs(args)

	// Resolve alias / runtime-declared sandbox opt-in before dispatch.
	// An explicit --sandbox=<bool> on the CLI always wins, so we only
	// consult the lower-priority sources when the flag wasn't set.
	var agentCfg *latestcfg.Config
	if !cmd.Flags().Changed("sandbox") {
		var agentRef string
		if len(args) > 0 {
			agentRef = args[0]
		}
		f.sandbox, agentCfg = resolveSandboxDefault(ctx, agentRef, f.sandbox, f.runConfig.Flavors)
	}

	out := cli.NewPrinter(cmd.OutOrStdout())
	if discoveredProjectConfig {
		out.Println("Using project config: " + args[0])
	}

	if f.sandbox {
		if cmd.Flags().Changed("worktree") || cmd.Flags().Changed("worktree-pr") {
			return errors.New("--worktree/--worktree-pr cannot be combined with a sandboxed run")
		}
		return runInSandbox(ctx, cmd, args, &f.runConfig, f.sandboxTemplate, f.sbx, f.noKit, agentCfg)
	}

	// --worktree was provided (with or without a value). The string flag lets
	// users name the worktree (--worktree=my-name); without a value cobra
	// stores the sentinel that triggers a random name.
	f.worktree = cmd.Flags().Changed("worktree")

	// --worktree-base only selects the start-point of the branch --worktree
	// creates; on its own it would silently do nothing, so reject it.
	if f.worktreeBase != "" && !f.worktree {
		return errors.New("--worktree-base requires --worktree")
	}

	// When an interactive run cannot find any usable model, offer the setup
	// wizard instead of leaving the user alone with the error.
	err := f.runOrExec(ctx, out, args, useTUI)
	return f.offerSetupOnNoModel(ctx, cmd, out, args, useTUI, err)
}

func (f *runExecFlags) runOrExec(ctx context.Context, out *cli.Printer, args []string, useTUI bool) error {
	slog.DebugContext(ctx, "Starting agent", "agent", f.agentName)

	// Start profiling if requested
	stopProfiling, err := profiling.Start(f.cpuProfile, f.memProfile)
	if err != nil {
		return err
	}
	defer func() {
		if err := stopProfiling(); err != nil {
			slog.ErrorContext(ctx, "Profiling cleanup failed", "error", err)
		}
	}()

	agentFileName := f.resolveRunAgentFileName(args)

	// Load the user config once and fail loudly: falling back to defaults
	// here would silently drop the user's settings (safety, permissions,
	// hooks) and alias options, and an invalid settings/alias safety value
	// must surface as a clear validation error, not a quiet reset.
	userCfg, err := userconfig.Load()
	if err != nil {
		return fmt.Errorf("loading user config: %w", err)
	}

	// Apply global user settings first (lowest priority)
	// User settings only apply if the flag wasn't explicitly set by the user
	userSettings := userCfg.GetSettings()
	f.applyUserSettings(ctx, userSettings)
	f.runConfig.GlobalHooks = config.MergeHooks(userSettings.GlobalHooks(), config.LoadHookDropIns())

	// Apply alias options if this is an alias reference
	// Alias options only apply if the flag wasn't explicitly set by the user
	if alias := aliasOptions(userCfg, agentFileName); alias != nil {
		f.applyAliasOptions(ctx, alias)
	}

	// Build global permissions checker from user config settings.
	if userSettings.Permissions != nil {
		f.globalPermissions = permissions.NewChecker(userSettings.Permissions)
	}

	// Start fake proxy if --fake is specified
	fakeCleanup, err := setupFakeProxy(ctx, f.fakeResponses, f.fakeStreamDelay, &f.runConfig)
	if err != nil {
		return err
	}
	defer func() {
		if err := fakeCleanup(); err != nil {
			slog.ErrorContext(ctx, "Failed to cleanup fake proxy", "error", err)
		}
	}()

	// Record AI API interactions to a cassette file if --record flag is specified.
	cassettePath, recordCleanup, err := setupRecordingProxy(ctx, f.recordPath, &f.runConfig)
	if err != nil {
		return err
	}
	if cassettePath != "" {
		defer func() {
			if err := recordCleanup(); err != nil {
				slog.ErrorContext(ctx, "Failed to cleanup recording proxy", "error", err)
			}
		}()
		out.Println("Recording mode enabled, cassette: " + cassettePath)
	}

	b, err := f.selectBackend(agentFileName)
	if err != nil {
		return err
	}
	defer func() {
		if err := b.Close(); err != nil {
			slog.ErrorContext(ctx, "Failed to close backend", "error", err)
		}
	}()

	if f.dryRun {
		// A dry run initializes the team but runs nothing, so it never
		// creates a worktree.
		loadResult, err := b.LoadTeam(ctx, b.LoadTeamRequest())
		if err != nil {
			return err
		}
		if loadResult != nil {
			stopToolSets(ctx, loadResult.Team)
		}
		out.Println("Dry run mode enabled. Agent initialized but will not execute.")
		return nil
	}

	// Create the worktree BEFORE loading the team. Toolsets capture the
	// working directory when they are built, so the worktree must already be
	// the working directory by then for every tool — the shell included — to
	// operate inside it rather than the user's checkout.
	//
	// The base directory is the process working directory, which already
	// reflects --working-dir: addGatewayFlags' PersistentPreRunE chdirs there
	// before the run. That is what lets --worktree and --working-dir compose —
	// --working-dir selects the repository the worktree is branched from.
	baseDir, _ := os.Getwd()

	// Resuming a session that ran in a worktree: reattach to that worktree's
	// directory so the shell and every other tool operate inside it again,
	// without the caller re-passing --worktree (which would fail because the
	// worktree already exists). An explicit --worktree/--worktree-pr on this
	// run takes precedence and creates a new one as usual.
	if !f.worktree && f.worktreePR == "" {
		if resumeDir, ok := b.ResumeWorkingDir(ctx); ok {
			baseDir = resumeDir
			f.runConfig.WorkingDir = resumeDir
		}
	}

	loadResult, createdWorktree, wd, err := f.loadTeamInWorktree(ctx, b, baseDir)
	if err != nil {
		return err
	}
	if createdWorktree != nil {
		out.Println("Using git worktree: " + createdWorktree.Dir + " (branch " + createdWorktree.Branch + ")")
		// loadResult is nil for the remote backend; worktrees are mutually
		// exclusive with --remote so this is belt-and-suspenders, matching
		// the nil-guard used for cleanup throughout this function.
		if loadResult != nil {
			if err := f.dispatchWorktreeCreate(ctx, out, loadResult.Team, createdWorktree); err != nil {
				stopToolSets(ctx, loadResult.Team)
				return err
			}
		}
	}
	rt, sessions, sess, cleanup, err := b.CreateSession(ctx, loadResult, b.CreateSessionRequest(wd))
	if err != nil {
		return err
	}
	defer cleanup()

	if !useTUI {
		// Non-interactive (--exec) runs never clean up the worktree: there
		// is no safe moment to prompt, and silently discarding work would
		// be surprising. The worktree is left in place for later inspection.
		return f.handleExecMode(ctx, out, rt, sessions, sess, args)
	}

	binding := sessionSessionBinding(ctx, rt, sess)
	spawner := b.Spawner(rt, sessions)
	if err := f.configureSessionViewHost(ctx, b, rt, sessions, sess, spawner, cleanup); err != nil {
		return err
	}
	defer f.sessionViewHost.Shutdown()
	cleanup = f.sessionViewHost.Shutdown

	if err := f.startSessionCoordinator(ctx, out, sessions, rt.SessionStore(), sess); err != nil {
		return err
	}

	applyTheme(f.theme)
	opts, err := f.buildAppOpts(args)
	if err != nil {
		return err
	}
	opts = append(opts, app.WithRuntimeServices(rt))

	eventHooks, err := parseOnEventFlags(f.onEventSpecs)
	if err != nil {
		return err
	}
	if hookOpt := withEventHooks(eventHooks); hookOpt != nil {
		opts = append(opts, hookOpt)
	}

	var rec *recorder.Recorder
	tuiOptions := append(f.tuiOpts(args), tui.WithSupervisor(f.sessionViewHost))
	if dir := f.explicitDefaultWorkingDir(sess); dir != "" {
		tuiOptions = append(tuiOptions, tui.WithDefaultWorkingDir(dir))
	}
	runErr := func() error {
		if f.lean {
			return f.runLeanTUI(ctx, rt, sessions, sess, binding, spawner, cleanup, args, opts...)
		}
		if cassettePath != "" {
			// Record keystrokes and clicks alongside the model traffic so a
			// complete tuitest e2e test can be generated when the session ends.
			wrap := func(m tea.Model) tea.Model {
				rec = recorder.New(m)
				return rec
			}
			return runTUIWrapped(ctx, rt, sessions, sess, binding, spawner, cleanup, tuiOptions, wrap, opts...)
		}
		return runTUI(ctx, rt, sessions, sess, binding, spawner, cleanup, tuiOptions, opts...)
	}()
	if rec != nil && rec.HasInput() {
		writeGeneratedTUITest(ctx, out, rec, cassettePath, agentFileName)
	}
	if runErr != nil {
		// On a TUI error we deliberately leave the worktree in place rather
		// than risk discarding work after an abnormal exit.
		return runErr
	}

	// The interactive session is over. Offer to clean up the worktree we
	// created for it (never a pre-existing one, since Create only makes new
	// worktrees). Shut the session down first so tools release any file
	// handles inside the worktree before we try to remove it; cleanup is
	// idempotent, so the deferred call above becomes a no-op.
	//
	// A fresh context is used because the TUI may have exited via a
	// canceled ctx (Ctrl-C), which would otherwise abort the prompt and
	// the git commands.
	if createdWorktree != nil {
		cleanup()
		f.cleanupWorktree(context.WithoutCancel(ctx), out, createdWorktree)
	}
	return nil
}

func (f *runExecFlags) resolveRunAgentFileName(args []string) string {
	if len(args) > 0 {
		return args[0]
	}
	return ""
}

func (f *runExecFlags) discoverRunAgentArgs(args []string) ([]string, bool) {
	if len(args) > 0 || f.remoteAddress != "" {
		return args, false
	}
	if name, ok := discoverProjectDefaultAgentFile(); ok {
		return prependAgentRef(name, args), true
	}
	return args, false
}

func discoverProjectDefaultAgentFile() (string, bool) {
	for _, name := range projectDefaultAgentFiles {
		info, err := os.Stat(name)
		if err == nil && !info.IsDir() {
			return name, true
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			// Surface unreadable or otherwise broken project configs instead of
			// silently falling through to a later candidate or the built-in default.
			return name, true
		}
	}
	return "", false
}

// loadTeamInWorktree creates the requested worktree (if any) and then loads
// the team with the worktree as its working directory. The ordering matters:
// toolsets capture runConfig.WorkingDir when they are constructed during
// LoadTeam, so the worktree must be settled first for every tool — the shell
// included — to operate inside the worktree instead of the user's checkout.
//
// It returns the loaded team, the created worktree (nil when neither
// --worktree nor --worktree-pr was given) and the working directory to run in
// (the worktree's directory when one was created, otherwise wd unchanged).
func (f *runExecFlags) applyUserSettings(ctx context.Context, userSettings *userconfig.Settings) {
	if userSettings.HideToolResults && !f.hideToolResults {
		f.hideToolResults = true
		slog.DebugContext(ctx, "Applying user settings", "hide_tool_results", true)
	}
	if userSettings.YOLO && !f.yoloChanged && !f.autoApprove {
		f.autoApprove = true
		slog.DebugContext(ctx, "Applying user settings", "YOLO", true)
	}
	if s := f.scopedSafetyDefault(userSettings.Safety, userSettings.YOLO); s != "" {
		f.defaultSafety = s
		slog.DebugContext(ctx, "Applying user settings", "safety", string(s))
	}
	// The tour needs the full TUI's overlay support, so a lean default from
	// user config is not applied when the tour was requested.
	if userSettings.Lean && !f.leanChanged && !f.lean && !f.tour {
		f.lean = true
		slog.DebugContext(ctx, "Applying user settings", "lean", true)
	}
	if userSettings.SnapshotsEnabled() {
		f.snapshotsEnabled = true
		slog.DebugContext(ctx, "Applying user settings", "snapshot", true)
	}
}

// applyAliasOptions applies an alias's bundled runtime options. Like user
// settings they are defaults: an explicitly-passed flag wins. They are
// applied after applyUserSettings so an alias's own choices (including its
// safety default) take priority over the global settings.
func (f *runExecFlags) applyAliasOptions(ctx context.Context, alias *userconfig.Alias) {
	slog.DebugContext(ctx, "Applying alias options", "yolo", alias.Yolo, "safety", string(alias.Safety), "model", alias.Model, "hide_tool_results", alias.HideToolResults, "sandbox", alias.Sandbox)
	if alias.Yolo && !f.yoloChanged && !f.autoApprove {
		f.autoApprove = true
	}
	// The alias's safety default (safety option, or legacy yolo →
	// autonomous) outranks the user-settings default applied before it.
	if s := f.scopedSafetyDefault(alias.Safety, alias.Yolo); s != "" {
		f.defaultSafety = s
	}
	if alias.Model != "" && len(f.modelOverrides) == 0 {
		f.modelOverrides = append(f.modelOverrides, alias.Model)
	}
	if alias.HideToolResults && !f.hideToolResults {
		f.hideToolResults = true
	}
	// alias.Sandbox is consumed earlier in runRunCommand before
	// dispatch; reaching runOrExec means the sandbox decision
	// resolved to false (or the user opted out via --sandbox=false),
	// so flipping it here would be a no-op.
}

// aliasOptions resolves the alias options for an agent reference from an
// already-loaded user config, mirroring sources.ResolveAlias: the empty
// reference maps to the "default" alias, and an alias without options is
// not returned.
func aliasOptions(cfg *userconfig.Config, agentFileName string) *userconfig.Alias {
	alias, ok := cfg.GetAlias(cmp.Or(agentFileName, "default"))
	if !ok || !alias.HasOptions() {
		return nil
	}
	return alias
}

func (f *runExecFlags) loadTeamInWorktree(ctx context.Context, b backend, wd string) (*teamloader.LoadResult, *worktree.Worktree, string, error) {
	createdWorktree, err := f.setupWorktree(ctx, wd)
	if err != nil {
		return nil, nil, wd, err
	}
	if createdWorktree != nil {
		wd = createdWorktree.Dir
		f.runConfig.WorkingDir = createdWorktree.Dir
	}

	loadResult, err := b.LoadTeam(ctx, b.LoadTeamRequest())
	if err != nil {
		// The worktree was created before the load; tear it down so a load
		// failure doesn't leave an orphaned worktree behind. A fresh context
		// is used because the load may have failed on a cancelled ctx (Ctrl-C),
		// which would otherwise kill the git removal subprocess and orphan the
		// worktree.
		if createdWorktree != nil {
			if rmErr := createdWorktree.Remove(context.WithoutCancel(ctx)); rmErr != nil {
				slog.WarnContext(ctx, "Failed to remove worktree after load error", "dir", createdWorktree.Dir, "error", rmErr)
			}
		}
		return nil, nil, wd, err
	}
	return loadResult, createdWorktree, wd, nil
}

// setupWorktree creates the git worktree requested by --worktree or
// --worktree-pr, returning nil when neither was given. The returned worktree
// (when non-nil) becomes the session's working directory and is cleaned up
// when an interactive run ends.
func (f *runExecFlags) setupWorktree(ctx context.Context, wd string) (*worktree.Worktree, error) {
	switch {
	case f.worktreePR != "":
		wt, err := worktree.CreatePR(ctx, wd, f.worktreePR)
		if err != nil {
			switch {
			case errors.Is(err, worktree.ErrNotGitRepository):
				return nil, fmt.Errorf("--worktree-pr requires %s to be inside a git repository", wd)
			case errors.Is(err, worktree.ErrInvalidPRRef):
				return nil, fmt.Errorf("invalid --worktree-pr value: %w", err)
			case errors.Is(err, worktree.ErrGHNotFound):
				return nil, fmt.Errorf("--worktree-pr requires the GitHub CLI: %w", err)
			default:
				return nil, err
			}
		}
		return wt, nil

	case f.worktree:
		name := f.worktreeName
		if name == worktreeAutoName {
			name = ""
		}
		wt, err := worktree.Create(ctx, wd, name, worktree.WithBase(f.worktreeBase))
		if err != nil {
			switch {
			case errors.Is(err, worktree.ErrNotGitRepository):
				return nil, fmt.Errorf("--worktree requires %s to be inside a git repository", wd)
			case errors.Is(err, worktree.ErrInvalidName):
				return nil, fmt.Errorf("invalid --worktree name: %w", err)
			case errors.Is(err, worktree.ErrInvalidBase):
				return nil, fmt.Errorf("invalid --worktree-base: %w", err)
			default:
				return nil, err
			}
		}
		return wt, nil

	default:
		return nil, nil
	}
}

// cleanupWorktree removes a worktree created for an interactive run once it
// ends. A clean worktree (no uncommitted changes, untracked files, or new
// commits) is removed automatically. A dirty one is kept unless the user
// explicitly asks to remove it, so work is never discarded silently.
// Failures are reported but never abort the command — the run already
// succeeded.
func (f *runExecFlags) cleanupWorktree(ctx context.Context, out *cli.Printer, wt *worktree.Worktree) {
	st, err := wt.Status(ctx)
	if err != nil {
		out.Println("Could not inspect git worktree " + wt.Dir + ": " + err.Error())
		out.Println("Leaving it in place. Remove it manually with: git -C " + wt.SourceDir + " worktree remove " + wt.Dir)
		return
	}

	if st.IsDirty() {
		if !promptRemoveDirtyWorktree(ctx, out, wt, st) {
			out.Println("Keeping git worktree " + wt.Dir + " (branch " + wt.Branch + ").")
			return
		}
	}

	if err := wt.Remove(ctx); err != nil {
		out.Println("Failed to remove git worktree " + wt.Dir + ": " + err.Error())
		return
	}
	out.Println("Removed git worktree " + wt.Dir + " (branch " + wt.Branch + ").")
}

// promptRemoveDirtyWorktree asks the user whether to discard a worktree that
// still holds work. It defaults to keeping (returns false) on any non-yes
// answer or read error, so uncommitted work is never lost by accident.
func promptRemoveDirtyWorktree(ctx context.Context, out *cli.Printer, wt *worktree.Worktree, st worktree.Status) bool {
	var held []string
	if st.Modified {
		held = append(held, "uncommitted changes")
	}
	if st.Untracked {
		held = append(held, "untracked files")
	}
	if st.NewCommits {
		held = append(held, "new commits")
	}

	out.Println("\nThe git worktree " + wt.Dir + " (branch " + wt.Branch + ") still has " + strings.Join(held, ", ") + ".")
	out.Println("  y: delete the worktree, its branch, and all of this work")
	out.Println("  N: keep them so you can return later")
	out.Print("Delete this worktree and discard the work? (y/N) ")

	response, err := input.ReadLine(ctx, os.Stdin)
	if err != nil {
		return false
	}
	response = strings.TrimSpace(strings.ToLower(response))
	return response == "y" || response == "yes"
}

// dispatchWorktreeCreate fires the worktree_create hooks of the agent the
// run targets, just after the worktree is created and before the session
// exists. Unlike every other event, this is dispatched from the CLI rather
// than the run loop: the worktree (and the working directory the runtime,
// session, tools and snapshot machinery all capture) must be settled first.
// Hooks run inside the new worktree so setup commands (copy .env, install
// deps) operate on the fresh checkout. A blocking verdict aborts the run.
func (f *runExecFlags) dispatchWorktreeCreate(ctx context.Context, out *cli.Printer, t *team.Team, wt *worktree.Worktree) error {
	agt, err := t.AgentOrDefault(f.agentName)
	if err != nil {
		return err
	}
	hooksCfg := agt.Hooks()
	if hooksCfg == nil {
		return nil
	}

	executor := hooks.NewExecutor(hooksCfg, wt.Dir, os.Environ())
	if !executor.Has(hooks.EventWorktreeCreate) {
		return nil
	}

	result, err := executor.Dispatch(ctx, hooks.EventWorktreeCreate, &hooks.Input{
		AgentName:         agt.Name(),
		Cwd:               wt.Dir,
		WorktreePath:      wt.Dir,
		WorktreeBranch:    wt.Branch,
		WorktreeSourceDir: wt.SourceDir,
	})
	if err != nil {
		return fmt.Errorf("running worktree_create hooks: %w", err)
	}
	if result.SystemMessage != "" {
		out.Println(result.SystemMessage)
	}
	if result.AdditionalContext != "" {
		out.Println(result.AdditionalContext)
	}
	if !result.Allowed {
		msg := result.Message
		if msg == "" {
			msg = "a worktree_create hook blocked the run"
		}
		return fmt.Errorf("worktree_create hook aborted the run: %s", msg)
	}
	return nil
}

func (f *runExecFlags) loadAgentFrom(ctx context.Context, req runtime.LoadTeamRequest) (*teamloader.LoadResult, error) {
	opts := append(loaderdefaults.Opts(), teamloader.WithModelOverrides(req.ModelOverrides))
	if len(req.PromptFiles) > 0 {
		opts = append(opts, teamloader.WithPromptFiles(req.PromptFiles))
	}
	return teamloader.LoadWithConfig(ctx, req.Source, req.RunConfig, opts...)
}

// runtimeOpts returns the runtime options derived from the current flags,
// the loaded team and the runtime configuration. The session store and the
// current agent name are passed in because they're resolved by callers from
// different sources (e.g. the spawner uses the same store as the parent).
func (f *runExecFlags) runtimeOpts(loadResult *teamloader.LoadResult, runConfig *config.RuntimeConfig, sessStore session.Store, agentName string) []runtime.Opt {
	modelSwitcherCfg := &runtime.ModelSwitcherConfig{
		Models:             loadResult.Models,
		Providers:          loadResult.Providers,
		ModelsGateway:      runConfig.ModelsGateway,
		EncryptedConfig:    loadResult.EncryptedConfig,
		EnvProvider:        runConfig.EnvProvider(),
		ProviderRegistry:   loadResult.ProviderRegistry,
		AgentDefaultModels: loadResult.AgentDefaultModels,
	}
	// Share the models.dev store the team loader already warmed (parsing the
	// multi-MB catalog once) so the runtime doesn't build its own cold store
	// and re-pay the parse on the first /model open. On error we leave it unset
	// and the runtime falls back to its lazy default.
	if store, err := runConfig.ModelsDevStore(); err == nil {
		modelSwitcherCfg.ModelsStore = store
	} else {
		slog.Warn("Failed to obtain shared models.dev store; runtime will use its own", "error", err)
	}
	opts := []runtime.Opt{
		runtime.WithSessionStore(sessStore),
		runtime.WithCurrentAgent(agentName),
		// Read the saved preference for every owner, including new and restored tabs.
		runtime.WithUseSubagents(userconfig.Get().GetUseSubagents()),
		runtime.WithWorkingDir(runConfig.WorkingDir),
		runtime.WithTracer(otel.Tracer(AppName)),
		runtime.WithModelSwitcherConfig(modelSwitcherCfg),
		runtime.WithBudget(loadResult.Budget),
		runtime.WithNamedBudgets(loadResult.Budgets, loadResult.AgentBudgets),
	}
	return opts
}

// snapshotRuntimeOpts wires the snapshot builtin into a runtime.
// Returns the [runtime.Opt]s that hand the registry and the
// [builtins.SnapshotController] auto-injector to the runtime, plus
// the controller itself for the embedder to pass to the App via
// [app.WithSnapshotController]. When snapshots aren't enabled,
// returns no opts and a nil controller so callers don't have to
// branch on f.snapshotsEnabled themselves.
//
// A fresh registry is created here rather than reused across runtimes
// so the spawner-created sub-runtimes get their own snapshot state
// (each spawned session has independent /undo history).
func (f *runExecFlags) snapshotRuntimeOpts() ([]runtime.Opt, builtins.SnapshotController, error) {
	if !f.snapshotsEnabled {
		return nil, nil, nil
	}
	reg := hooks.NewRegistry()
	ctrl, err := builtins.RegisterSnapshot(reg, true)
	if err != nil {
		return nil, nil, fmt.Errorf("register snapshot builtin: %w", err)
	}
	return []runtime.Opt{
		runtime.WithHooksRegistry(reg),
		runtime.WithAutoInjector(ctrl),
	}, ctrl, nil
}

func (f *runExecFlags) createLocalRuntimeAndSession(ctx context.Context, loadResult *teamloader.LoadResult, req runtime.CreateSessionRequest, sessStore session.Store) (*runtime.LocalRuntime, *session.Session, error) {
	t := loadResult.Team

	// Merge user-level global permissions into the team's checker so the
	// runtime receives a single, already-merged permission set.
	if req.GlobalPermissions != nil && !req.GlobalPermissions.IsEmpty() {
		t.SetPermissions(permissions.Merge(t.Permissions(), req.GlobalPermissions))
	}

	agt, err := t.AgentOrDefault(req.AgentName)
	if err != nil {
		return nil, nil, err
	}
	agentName := agt.Name()

	rtOpts, ctrl, err := f.snapshotRuntimeOpts()
	if err != nil {
		return nil, nil, err
	}
	runtimeOpts := append(f.runtimeOpts(loadResult, &f.runConfig, sessStore, agentName), rtOpts...)
	localRt, err := runtime.New(ctx, t, runtimeOpts...)
	if err != nil {
		return nil, nil, fmt.Errorf("creating runtime: %w", err)
	}
	f.snapshotController = ctrl

	var sess *session.Session
	if req.ResumeSessionID != "" {
		// Resolve relative session references (e.g., "-1" for last session)
		resolvedID, err := session.ResolveSessionID(ctx, sessStore, req.ResumeSessionID)
		if err != nil {
			return nil, nil, fmt.Errorf("resolving session %q: %w", req.ResumeSessionID, err)
		}

		// Load existing session
		sess, err = sessStore.GetSession(ctx, resolvedID)
		switch {
		case err == nil:
			// Only an explicit CLI flag (--safety / --yolo, both resolved
			// into req.SafetyPolicy with --safety winning) may override the
			// mode a resumed session carries: alias options, user settings
			// and author-declared YAML defaults are defaults for NEW
			// sessions and must never replace persisted state. The override
			// goes through the option (not a raw field write) so the legacy
			// ToolsApproved flag and the mode stay in sync. Without an
			// explicit flag the stored state is left untouched — a plain
			// resume must not reset ToolsApproved out from under a stored
			// autonomous policy.
			if req.SafetyExplicit && req.SafetyPolicy != "" {
				session.WithSafetyPolicy(req.SafetyPolicy)(sess)
			}
			sess.HideToolResults = req.HideToolResults
			// Stored model overrides are applied per session when its session
			// is bound (see sessionSessionBinding), so other tabs sharing this
			// runtime keep their own models.

			slog.DebugContext(ctx, "Loaded existing session", "session_id", resolvedID, "session_ref", req.ResumeSessionID, "agent", agentName)
		case errors.Is(err, session.ErrNotFound) && !session.IsRelativeSessionRef(req.ResumeSessionID):
			// An explicit, caller-chosen ID that doesn't exist yet: create the
			// session with that ID rather than failing. This lets a supervisor
			// (e.g. a board reconnecting to a dead agent) own the ID up front and
			// reuse it across runs — the first run creates, later runs resume.
			// A relative ref (-1, -2, ...) never lands here: it must resolve
			// against existing sessions.
			sess = session.New(append(f.buildSessionOpts(agt, t, req), session.WithID(resolvedID))...)
			slog.DebugContext(ctx, "Creating session with caller-supplied ID", "session_id", resolvedID, "agent", agentName)
		default:
			return nil, nil, fmt.Errorf("loading session %q: %w", resolvedID, err)
		}
	} else {
		sess = session.New(f.buildSessionOpts(agt, t, req)...)
		// Session is stored lazily on first UpdateSession call (when content is added)
		// This avoids creating empty sessions in the database
		slog.DebugContext(ctx, "Using local runtime", "agent", agentName)
	}

	return localRt, sess, nil
}

func (f *runExecFlags) handleExecMode(ctx context.Context, out *cli.Printer, rt app.Services, sessions runtime.SessionRuntime, sess *session.Session, args []string) error {
	if f.sessionReadOnly {
		return errors.New("--session-read-only cannot be used with --exec: there is nothing to display without a TUI")
	}

	// args[0] is the agent file; args[1:] are user messages for multi-turn conversation
	var userMessages []string
	if len(args) > 1 {
		userMessages = args[1:]
	}

	commandSource, ok := rt.(runtime.CommandSource)
	if !ok {
		return errors.New("runtime services do not provide immutable commands")
	}
	err := cli.Run(ctx, out, f.execCLIConfig(sess), commandSource, sessions, sess, userMessages)
	if cliErr, ok := errors.AsType[cli.RuntimeError](err); ok {
		return RuntimeError{Err: cliErr.Err}
	}
	return err
}

// execCLIConfig builds the cli.Config for a non-TUI (--exec) run.
// AutoApprove is derived from the session's resolved safety mode rather
// than the raw --yolo flag: when a legacy yolo boolean and a typed safety
// default coexist (settings/alias), the typed mode wins for the session,
// and max-iteration auto-extension must match that resolved mode.
func (f *runExecFlags) execCLIConfig(sess *session.Session) cli.Config {
	return cli.Config{
		AppName:        AppName,
		AttachmentPath: f.attachmentPath,
		HideToolCalls:  f.hideToolCalls,
		OutputJSON:     f.outputJSON,
		AutoApprove:    sess.GetSafetyPolicy() == session.SafetyPolicyAutonomous,
	}
}

func readInitialMessage(args []string) (*string, error) {
	if len(args) < 2 {
		return nil, nil
	}

	if args[1] == "-" {
		buf, err := io.ReadAll(os.Stdin)
		if err != nil {
			return nil, fmt.Errorf("failed to read from stdin: %w", err)
		}
		text := string(buf)
		return &text, nil
	}

	return &args[1], nil
}

// tuiOpts returns the TUI options derived from the current flags. args are
// the run command's positional arguments, used to detect an initial message.
func (f *runExecFlags) tuiOpts(args []string) []tui.Option {
	var opts []tui.Option
	if f.lean {
		opts = append(opts, tui.WithLeanMode())
	}
	if f.appName != "" {
		opts = append(opts, tui.WithAppName(f.appName))
	}
	if len(f.disabledCommands) > 0 {
		opts = append(opts, tui.WithDisabledCommands(f.disabledCommands))
	}
	if !f.sidebar {
		opts = append(opts, tui.WithHideSidebar())
	}
	switch {
	case f.tour:
		opts = append(opts, tui.WithTourStart())
	case f.shouldOfferTour(args):
		opts = append(opts, tui.WithTourOffer(telemetry.GetTelemetryEnabled()))
	}
	return opts
}

// explicitDefaultWorkingDir returns the working directory generic new-session
// actions in the TUI should default to instead of opening the picker, or ""
// when --working-dir was not explicitly supplied on this invocation. It
// mirrors runTUIWrapped's resolution of the initial tab's directory (session
// working dir, then process CWD) so new tabs open exactly where the initial
// session did — the worktree or resume directory, not the raw flag value.
func (f *runExecFlags) explicitDefaultWorkingDir(sess *session.Session) string {
	if !f.workingDirChanged {
		return ""
	}
	if sess.WorkingDir != "" {
		return sess.WorkingDir
	}
	wd, _ := os.Getwd()
	return wd
}

// shouldOfferTour reports whether this interactive run should show the
// first-run dialog offering the getting-started tour. Automation and
// replay/record contexts never see the offer, and neither does a run that
// already carries an initial message (the user clearly has a task in mind);
// DOCKER_AGENT_NO_TOUR=1 suppresses it for scripted environments. The
// explicit --tour flag (or the getting-started command) bypasses the offer
// and starts the tour directly.
func (f *runExecFlags) shouldOfferTour(args []string) bool {
	if f.lean || f.exitAfterResponse || f.sessionReadOnly || f.recordPath != "" || f.fakeResponses != "" {
		return false
	}
	if len(args) > 1 {
		return false
	}
	return tour.ShouldOffer(os.Getenv)
}

func withTitleGenerator(ctx context.Context, services app.Services, opts []app.Opt) []app.Opt {
	provider, ok := services.(interface {
		TitleGenerator(ctx context.Context) *sessiontitle.Generator
	})
	if !ok {
		return opts
	}
	if gen := provider.TitleGenerator(ctx); gen != nil {
		return append(opts, app.WithTitleGenerator(gen))
	}
	return opts
}

// runLeanTUI builds the App and drives the standalone lean TUI, used when
// --lean is set. Unlike the full TUI it renders to the normal terminal buffer
// (no alternate screen) and sends the first/queued messages itself rather than
// through the App's bubbletea command pipeline.
func (f *runExecFlags) runLeanTUI(ctx context.Context, rt app.Services, sessions runtime.SessionRuntime, sess *session.Session, binding runtime.SessionBinding, spawner tui.SessionSpawner, cleanup func(), args []string, opts ...app.Opt) error {
	opts = withTitleGenerator(ctx, rt, opts)
	a := app.New(ctx, sessions, sess, binding, opts...)

	firstMessage, err := readInitialMessage(args)
	if err != nil {
		return err
	}
	var queued []string
	if len(args) > 2 {
		queued = args[2:]
	}
	if cleanup == nil {
		cleanup = func() {}
	}

	// Prefer the session's working directory so the lean status bar shows
	// where the tools operate — e.g. the worktree from --worktree, not the
	// process CWD it was launched from.
	wd := sess.WorkingDir
	if wd == "" {
		wd, _ = os.Getwd()
	}
	renderImages := userconfig.Get().GetRenderImages() && tuiimage.SupportsKittyGraphics(os.Stdin, os.Stdout)
	showBanner := userconfig.Get().GetShowBanner()
	var tabStore leantui.TabStore
	if ts, err := tuistate.New(ctx); err != nil {
		slog.WarnContext(ctx, "Failed to open TUI state store, tabs won't persist", "error", err)
	} else {
		tabStore = ts
		defer func() {
			if err := ts.Close(); err != nil {
				slog.WarnContext(ctx, "Failed to close TUI state store", "error", err)
			}
		}()
	}
	restoreSession := leanSessionRestorer(spawner, rt.SessionStore(), a)
	if f.sessionViewHost != nil {
		restoreSession = func(ctx context.Context, id, _ string) (*app.App, func(), error) {
			if id == a.Session().ID {
				return a, nil, nil
			}
			restored, err := f.restoreHostedSession(ctx, id)
			return restored, nil, err
		}
	}
	spawnSession := leanSessionSpawner(spawner, rt.SessionStore())
	if f.sessionViewHost != nil {
		spawnSession = leanSessionSpawner(spawner, rt.SessionStore(), f.restoreHostedSession)
	}
	return leantui.Run(ctx, leantui.Config{
		App:                    a,
		SessionViews:           f.sessionViewHost,
		TabStore:               tabStore,
		RestoreTabs:            userconfig.Get().GetRestoreTabs(),
		RestoreSession:         restoreSession,
		SpawnSession:           spawnSession,
		WorkingDir:             wd,
		Cleanup:                cleanup,
		FirstMessage:           firstMessage,
		FirstMessageAttachment: f.attachmentPath,
		QueuedMessages:         queued,
		AppName:                f.appName,
		DisabledCommands:       f.disabledCommands,
		RenderImages:           &renderImages,
		ShowBanner:             &showBanner,
	})
}

// leanSessionSpawner adapts the canonical host spawner without sharing a
// directory-dependent runtime across workspaces. A source requests the same
// persisted branch used by the full TUI; it is never modified by this adapter.
func leanSessionSpawner(spawner tui.SessionSpawner, store session.Store, restore ...func(context.Context, string) (*app.App, error)) func(context.Context, string, *session.Session) (*app.App, func(), error) {
	return func(ctx context.Context, workingDir string, source *session.Session) (*app.App, func(), error) {
		if spawner == nil {
			return nil, nil, fmt.Errorf("session spawning not available: %w", runtime.ErrUnsupported)
		}
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		var fork *session.Session
		if source != nil {
			if store == nil {
				return nil, nil, errors.New("no session store configured")
			}
			var err error
			fork, err = session.BranchSession(source, len(source.MessagesSnapshot()))
			if err != nil {
				return nil, nil, fmt.Errorf("failed to fork session: %w", err)
			}
			// BranchSession drops transient delegation identity, including
			// AgentName. This top-level fork retains the selected agent so
			// its persisted model override binds to the same agent, not the
			// runtime's default. The new branch is not yet shared.
			fork.AgentName = source.AgentName
			fork.SetAttribute(runtime.SessionAgentAttribute, source.AgentName)
			if err := store.AddSession(ctx, fork); err != nil {
				return nil, nil, fmt.Errorf("failed to save forked session: %w", err)
			}
			if len(restore) != 0 {
				application, err := restore[0](ctx, fork.ID)
				return application, nil, err
			}
			workingDir = fork.WorkingDir
		}
		spawned, err := spawner(ctx, workingDir)
		return leanBindSpawnedSession(ctx, spawned, err, fork)
	}
}

// leanSessionRestorer hydrates one selected persisted tab. Merely installing
// this callback does not load sessions, start Apps, or create runtimes.
func leanSessionRestorer(spawner tui.SessionSpawner, store session.Store, initial *app.App) func(context.Context, string, string) (*app.App, func(), error) {
	return func(ctx context.Context, sessionID, workingDir string) (*app.App, func(), error) {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		if initial != nil && initial.Session() != nil && initial.Session().ID == sessionID {
			return initial, nil, nil
		}
		if spawner == nil {
			return nil, nil, fmt.Errorf("session restoring not available: %w", runtime.ErrUnsupported)
		}
		if store == nil {
			return nil, nil, errors.New("no session store configured")
		}
		sess, err := store.GetSession(ctx, sessionID)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to load restored session: %w", err)
		}
		if sess == nil || sess.ID != sessionID {
			return nil, nil, errors.New("session store returned a different restored session")
		}
		// The session's recorded workspace wins over stale tab metadata.
		// Never reinterpret missing provenance as the process workspace.
		workingDir = cmp.Or(sess.WorkingDir, workingDir)
		if strings.TrimSpace(workingDir) == "" {
			return nil, nil, session.ErrWorkingDirUnavailable
		}
		workingDir, err = session.CaptureLocalWorkingDir(workingDir)
		if err != nil {
			return nil, nil, err
		}
		info, err := os.Stat(workingDir)
		if err != nil {
			return nil, nil, fmt.Errorf("restored session working directory: %w", err)
		}
		if !info.IsDir() {
			return nil, nil, errors.New("restored session working directory is not a directory")
		}
		// GetSession returns the store snapshot, not a running session owner.
		// Pin legacy tab provenance before creating its immutable handle.
		sess.WorkingDir = workingDir
		if sess.AgentName == "" {
			sess.AgentName = sess.AttributesSnapshot()[runtime.SessionAgentAttribute]
		}
		spawned, err := spawner(ctx, workingDir)
		if sess.AgentName == "" && spawned.App != nil {
			sess.AgentName = spawned.App.Binding().AgentName
		}
		return leanBindSpawnedSession(ctx, spawned, err, sess)
	}
}

// leanBindSpawnedSession is the shared admission/ownership boundary for fresh,
// forked, and restored lean viewers. The caller owns presentation lifecycle;
// only a genuinely owned runtime receives a shutdown callback.
func leanBindSpawnedSession(ctx context.Context, spawned tui.SpawnedSession, spawnErr error, target *session.Session) (*app.App, func(), error) {
	var cleanup func()
	if spawned.Ownership == tui.RuntimeOwned && spawned.Cleanup != nil {
		cleanup = sync.OnceFunc(spawned.Cleanup)
	}
	fail := func(err error) (*app.App, func(), error) {
		if spawned.App != nil {
			spawned.App.Close()
		}
		if cleanup != nil {
			cleanup()
		}
		return nil, nil, err
	}
	if spawnErr != nil {
		return fail(spawnErr)
	}
	if spawned.App == nil {
		return fail(errors.New("session spawner returned no app"))
	}
	if target != nil {
		spawned.App.ReplaceSession(ctx, target)
	}
	// ReplaceSession has no error return. Do not admit an App whose
	// immutable handle failed to bind the requested session or agent.
	sess := spawned.App.Session()
	handle := spawned.App.SessionHandle()
	if sess == nil || handle == nil || handle.ID() != sess.ID || (target != nil && (sess.ID != target.ID || handle.AgentName() != target.AgentName)) {
		return fail(errors.New("failed to bind spawned session"))
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	return spawned.App, cleanup, nil
}

func (f *runExecFlags) buildAppOpts(args []string) ([]app.Opt, error) {
	firstMessage, err := readInitialMessage(args)
	if err != nil {
		return nil, err
	}

	var opts []app.Opt
	if firstMessage != nil {
		opts = append(opts, app.WithFirstMessage(*firstMessage))
	} else if f.attachmentPath != "" {
		// When --attach is used without an explicit message, provide a default
		// so that first-message preparation includes the attachment in
		// InitialEventCommands.
		defaultMsg := ""
		opts = append(opts, app.WithFirstMessage(defaultMsg))
	}
	if len(args) > 2 {
		opts = append(opts, app.WithQueuedMessages(args[2:]))
	}
	if f.attachmentPath != "" {
		opts = append(opts, app.WithFirstMessageAttachment(f.attachmentPath))
	}
	if f.exitAfterResponse {
		opts = append(opts, app.WithExitAfterFirstResponse())
	}
	if f.snapshotController != nil {
		opts = append(opts, app.WithSnapshotController(f.snapshotController))
	}
	if f.sessionReadOnly {
		opts = append(opts, app.WithReadOnly())
	}
	return opts, nil
}

// buildSessionOpts returns the canonical set of session options derived from
// CLI flags and agent configuration. Both the initial session and spawned
// sessions use this method so their options never drift apart.
func (f *runExecFlags) buildSessionOpts(agt *agent.Agent, t *team.Team, req runtime.CreateSessionRequest) []session.Opt {
	return []session.Opt{
		session.WithAgentName(agt.Name()),
		session.WithMaxIterations(agt.MaxIterations()),
		session.WithMaxConsecutiveToolCalls(agt.MaxConsecutiveToolCalls()),
		session.WithMaxOldToolCallTokens(agt.MaxOldToolCallTokens()),
		session.WithMaxToolResultTokens(agt.MaxToolResultTokens()),
		// WithToolsApproved before WithSafetyPolicy so a resolved safety
		// policy wins over the legacy yolo boolean; an empty policy is a
		// no-op and leaves the yolo-derived state alone.
		session.WithToolsApproved(req.ToolsApproved),
		session.WithSafetyPolicy(effectiveNewSessionSafety(req, agt, t)),
		session.WithHideToolResults(req.HideToolResults),
		session.WithWorkingDir(req.WorkingDir),
	}
}

// effectiveNewSessionSafety resolves the safety mode a FRESH session starts
// with. The user-owned request value (explicit CLI flags, alias options,
// user settings — already resolved in that order by createSessionRequest)
// always wins; only when the user expressed no preference at all do the
// author-declared YAML defaults apply: the selected agent's safety first,
// then the config-wide runtime.safety. Author configs — local, URL, or OCI
// — can therefore never override a user choice. Resumed sessions never go
// through this resolution; their stored mode is handled separately.
func effectiveNewSessionSafety(req runtime.CreateSessionRequest, agt *agent.Agent, t *team.Team) session.SafetyPolicy {
	if req.SafetyPolicy != "" {
		return req.SafetyPolicy
	}
	if req.ToolsApproved {
		// Legacy user-owned yolo without a resolved policy: WithToolsApproved
		// already pins autonomous; author defaults must not downgrade it.
		return ""
	}
	if agt != nil {
		if s := agt.Safety(); s != "" {
			return session.SafetyPolicy(s)
		}
	}
	if t != nil {
		if s := t.RuntimeSafety(); s != "" {
			return session.SafetyPolicy(s)
		}
	}
	return ""
}

// explicitCLISafety returns the safety mode explicitly requested on the
// command line, or empty when neither --safety nor --yolo was passed.
// --safety wins over --yolo; an explicit --yolo=false expresses no mode
// (it only suppresses yolo defaults from alias options and user settings).
func (f *runExecFlags) explicitCLISafety() session.SafetyPolicy {
	if f.safetyChanged && f.safety != "" {
		return session.SafetyPolicy(f.safety)
	}
	if f.yoloChanged && f.autoApprove {
		return session.SafetyPolicyAutonomous
	}
	return ""
}

// userSafetyPolicy resolves the user-owned safety mode for this run:
// explicit CLI flags first, then the alias/settings default. Empty means
// the user expressed no preference and author-declared YAML defaults may
// apply to fresh sessions.
func (f *runExecFlags) userSafetyPolicy() session.SafetyPolicy {
	if s := f.explicitCLISafety(); s != "" {
		return s
	}
	return f.defaultSafety
}

// scopedSafetyDefault resolves one scope's (user settings or alias)
// safety default: the typed safety field wins over the legacy yolo
// boolean at the same scope, and the legacy boolean is ignored entirely
// once --yolo was explicitly passed — --yolo=true is already an explicit
// CLI mode, and --yolo=false must not be resurrected by a lower-scope
// yolo default.
func (f *runExecFlags) scopedSafetyDefault(safety latestcfg.SafetyMode, legacyYolo bool) session.SafetyPolicy {
	if safety != "" {
		return session.SafetyPolicy(safety)
	}
	if legacyYolo && !f.yoloChanged {
		return session.SafetyPolicyAutonomous
	}
	return ""
}

// createSessionSpawner creates a function that can spawn new sessions with
// different working directories. A tab that stays in the running runtime's
// working directory borrows the shared session registry (one subagent swarm,
// one set of started toolsets). A tab in another directory gets its own
// runtime, because toolsets capture their working directory when they are
// built: the shell and filesystem tools of that tab must operate there, not
// in the directory the initial session was started in.
func (f *runExecFlags) createSessionSpawner(agentSource config.Source, services app.Services, sessions runtime.SessionRuntime) tui.SessionSpawner {
	if services == nil || sessions == nil {
		return nil
	}
	return func(spawnCtx context.Context, workingDir string) (tui.SpawnedSession, error) {
		// Pin workspace provenance before selecting the shared or owned runtime.
		workingDir, err := session.CaptureLocalWorkingDir(workingDir)
		if err != nil {
			return tui.SpawnedSession{}, err
		}
		if sameWorkingDir(workingDir, f.runConfig.WorkingDir) {
			return f.spawnBorrowedSession(spawnCtx, services, sessions, workingDir)
		}
		return f.spawnOwnedSession(spawnCtx, agentSource, services.SessionStore(), workingDir)
	}
}

// spawnBorrowedSession opens a fresh session on the shared session registry.
func (f *runExecFlags) spawnBorrowedSession(ctx context.Context, services app.Services, sessions runtime.SessionRuntime, workingDir string) (tui.SpawnedSession, error) {
	agentName := services.CurrentAgentInfo(ctx).Name
	spawnReq := f.createSessionRequest(workingDir)
	spawnReq.AgentName = agentName
	newSess := session.New(
		session.WithAgentName(agentName),
		session.WithToolsApproved(spawnReq.ToolsApproved),
		session.WithSafetyPolicy(spawnReq.SafetyPolicy),
		session.WithHideToolResults(spawnReq.HideToolResults),
		session.WithWorkingDir(workingDir),
	)
	binding := sessionSessionBinding(ctx, services, newSess)
	opts := withTitleGenerator(ctx, services, []app.Opt{app.WithRuntimeServices(services)})
	if f.sessionViewHost != nil {
		resources := viewhost.ViewOwnerResources{Services: services, Sessions: sessions, NewApp: resolvedViewBuilder(sessions, opts)}
		if err := f.sessionViewHost.RegisterSessionOwner(f.freshViewOwnerIdentity(newSess), resources, false); err != nil {
			return tui.SpawnedSession{}, err
		}
	}
	a := app.New(ctx, sessions, newSess, binding, opts...)
	return tui.SpawnedSession{App: a, Session: newSess, Ownership: tui.RuntimeBorrowed}, nil
}

// ownedSessionResources is one workspace-bound runtime, before any session is
// admitted or the runtime is exposed to the control plane. Ownership admission
// decides when Activate may publish it and when Cleanup may release it.
type ownedSessionResources struct {
	services *runtime.LocalRuntime
	sessions runtime.SessionRuntime
	team     *team.Team
	agent    *agent.Agent
	opts     []app.Opt
	activate func() error
	cleanup  func()
}

// spawnOwnedSession retains the ordinary fresh-session behavior on top of the
// same private resource construction used by host-owned view acquisition.
func (f *runExecFlags) spawnOwnedSession(ctx context.Context, agentSource config.Source, store session.Store, workingDir string) (tui.SpawnedSession, error) {
	resources, err := f.buildOwnedSessionResources(ctx, agentSource, store, workingDir)
	if err != nil {
		return tui.SpawnedSession{}, err
	}
	spawnReq := f.createSessionRequest(workingDir)
	spawnReq.AgentName = resources.agent.Name()
	newSess := session.New(f.buildSessionOpts(resources.agent, resources.team, spawnReq)...)
	cleanup := resources.cleanup
	if f.sessionViewHost != nil {
		if err := f.sessionViewHost.RegisterSessionOwner(f.freshViewOwnerIdentity(newSess), resources.viewResources(), false); err != nil {
			resources.cleanup()
			return tui.SpawnedSession{}, err
		}
		cleanup = f.sessionViewHost.SessionOwnerCleanup(resources.sessions)
	}
	if err := resources.activate(); err != nil {
		cleanup()
		return tui.SpawnedSession{}, err
	}
	a := app.New(ctx, resources.sessions, newSess, sessionSessionBinding(ctx, resources.services, newSess), resources.opts...)
	return tui.SpawnedSession{App: a, Session: newSess, Ownership: tui.RuntimeOwned, Cleanup: cleanup}, nil
}

func (f *runExecFlags) buildOwnedSessionResources(ctx context.Context, agentSource config.Source, store session.Store, workingDir string) (*ownedSessionResources, error) {
	runConfigCopy := f.runConfig.Clone()
	runConfigCopy.WorkingDir = workingDir

	// Honour every flag the initial load already honours (model overrides
	// AND prompt files).
	loadReq := f.loadTeamRequest(agentSource)
	loadReq.RunConfig = runConfigCopy
	loadResult, err := f.loadAgentFrom(ctx, loadReq)
	if err != nil {
		return nil, err
	}
	return f.buildLoadedSessionResources(ctx, loadResult, runConfigCopy, store)
}

// buildLoadedSessionResources binds tools already constructed for this workspace
// and runtime hooks to the same immutable config, without admitting a session.
func (f *runExecFlags) buildLoadedSessionResources(ctx context.Context, loadResult *teamloader.LoadResult, runConfigCopy *config.RuntimeConfig, store session.Store) (*ownedSessionResources, error) {
	workingDir := runConfigCopy.WorkingDir
	t := loadResult.Team

	agt, err := t.AgentOrDefault(f.agentName)
	if err != nil {
		stopToolSets(ctx, t)
		return nil, err
	}
	if f.globalPermissions != nil && !f.globalPermissions.IsEmpty() {
		t.SetPermissions(permissions.Merge(t.Permissions(), f.globalPermissions))
	}
	if ignoreRules := permissions.FromAgentsIgnore(workingDir); ignoreRules != nil {
		t.SetPermissions(permissions.Merge(t.Permissions(), ignoreRules))
	}

	rtOpts, ctrl, err := f.snapshotRuntimeOpts()
	if err != nil {
		stopToolSets(ctx, t)
		return nil, err
	}
	localRt, err := runtime.New(ctx, t, append(f.runtimeOpts(loadResult, runConfigCopy, store, agt.Name()), rtOpts...)...)
	if err != nil {
		stopToolSets(ctx, t)
		return nil, fmt.Errorf("creating runtime: %w", err)
	}
	supervisor := runtime.NewSessionRuntimeSupervisor(localRt)
	sessions := supervisor.Runtime()
	var publicationMu sync.Mutex
	closed, published := false, false
	unregister := func(context.Context) error { return nil }
	activate := func() error {
		publicationMu.Lock()
		defer publicationMu.Unlock()
		if closed {
			return runtime.ErrSessionClosed
		}
		if !published {
			if f.listenSessions != nil {
				unregister = f.listenSessions.Add(sessions)
			}
			published = true
		}
		return nil
	}

	opts := withTitleGenerator(ctx, localRt, []app.Opt{app.WithRuntimeServices(localRt)})
	if ctrl != nil {
		opts = append(opts, app.WithSnapshotController(ctrl))
	}
	if f.sessionReadOnly {
		opts = append(opts, app.WithReadOnly())
	}

	var once sync.Once
	cleanup := func() {
		once.Do(func() {
			publicationMu.Lock()
			closed = true
			unregister := unregister
			publicationMu.Unlock()
			unregisterCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			if err := unregister(unregisterCtx); err != nil {
				slog.ErrorContext(ctx, "Timed out draining spawned session runtime registration", "working_dir", workingDir, "error", err)
				cancel()
				go func() {
					if drainErr := unregister(context.WithoutCancel(ctx)); drainErr != nil {
						slog.ErrorContext(ctx, "Failed to drain spawned session runtime registration", "working_dir", workingDir, "error", drainErr)
					}
					stopToolSets(ctx, t)
					if shutdownErr := supervisor.Shutdown(context.WithoutCancel(ctx)); shutdownErr != nil {
						slog.ErrorContext(ctx, "Failed to shut down spawned session runtime", "working_dir", workingDir, "error", shutdownErr)
					}
				}()
				return
			}
			cancel()
			stopToolSets(ctx, t)
			if err := supervisor.Shutdown(context.WithoutCancel(ctx)); err != nil {
				slog.ErrorContext(ctx, "Failed to shut down spawned session runtime", "working_dir", workingDir, "error", err)
			}
		})
	}
	return &ownedSessionResources{
		services: localRt,
		sessions: sessions,
		team:     t,
		agent:    agt,
		opts:     opts,
		activate: activate,
		cleanup:  cleanup,
	}, nil
}

// sameWorkingDir reports whether two directories resolve to the same path;
// an empty candidate means "the current directory", which is what an empty
// runtime working dir denotes too.
func sameWorkingDir(a, b string) bool {
	norm := func(dir string) string {
		if dir == "" {
			dir, _ = os.Getwd()
		}
		if abs, err := filepath.Abs(dir); err == nil {
			dir = abs
		}
		if resolved, err := filepath.EvalSymlinks(dir); err == nil {
			dir = resolved
		}
		return filepath.Clean(dir)
	}
	return norm(a) == norm(b)
}

func sessionSessionBinding(ctx context.Context, rt app.Services, sess *session.Session) runtime.SessionBinding {
	agentName := sess.AgentName
	if agentName == "" {
		agentName = rt.CurrentAgentInfo(ctx).Name
		sess.AgentName = agentName
	}
	return runtime.SessionBinding{AgentName: agentName, Model: sess.AgentModelOverrides[agentName]}
}

// toolStopper is the subset of *team.Team needed by stopToolSets.
type toolStopper interface {
	StopToolSets(ctx context.Context) error
}

// stopToolSets gracefully stops all tool sets with a bounded timeout so
// that cleanup cannot block indefinitely. It detaches from ctx's
// cancellation (cleanup often runs after the caller's ctx is already done,
// e.g. on shutdown or tab close) while keeping ctx's trace context.
func stopToolSets(ctx context.Context, t toolStopper) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	if err := t.StopToolSets(ctx); err != nil {
		slog.ErrorContext(ctx, "Failed to stop tool sets", "error", err)
	}
}

// validateSafetyFlag rejects non-canonical --safety values consistently across commands.
func validateSafetyFlag(value string) error {
	switch session.SafetyPolicy(value) {
	case "", session.SafetyPolicyStrict, session.SafetyPolicyBalanced, session.SafetyPolicyRestricted, session.SafetyPolicyAutonomous:
		return nil
	default:
		return fmt.Errorf("invalid --safety value %q (valid: strict, balanced, restricted, autonomous)", value)
	}
}

// validateTheme reports whether ref names a loadable theme. It is used to
// fail fast on an explicit --theme value, listing the available themes so the
// user can correct a typo. The "auto" sentinel is accepted as-is: it resolves
// to a concrete theme at apply time.
func validateTheme(ref string) error {
	if ref == styles.AutoThemeRef {
		return nil
	}
	if _, err := styles.LoadTheme(ref); err != nil {
		if refs, listErr := styles.ListThemeRefs(); listErr == nil && len(refs) > 0 {
			refs = append([]string{styles.AutoThemeRef}, refs...)
			return fmt.Errorf("unknown theme %q; available themes: %s", ref, strings.Join(refs, ", "))
		}
		return fmt.Errorf("unknown theme %q: %w", ref, err)
	}
	return nil
}

// applyTheme applies the theme, resolving it from the --theme flag, then the
// user config, then the built-in default. The "auto" sentinel resolves to the
// configured light/dark theme pair based on the terminal background, queried
// synchronously (OSC 11) before the TUI starts so the first frame already has
// the right polarity.
func applyTheme(themeOverride string) {
	// Resolve theme from --theme flag > user config > built-in default
	themeRef := styles.DefaultThemeRef
	if userSettings := userconfig.Get(); userSettings.Theme != "" {
		themeRef = userSettings.Theme
	}
	if themeOverride != "" {
		themeRef = themeOverride
	}

	if themeRef == styles.AutoThemeRef {
		styles.SetAutoThemeEnabled(true)
		styles.SetTerminalDark(detectDarkTerminalBackground())
		themeRef = styles.ResolveThemeRef(themeRef)
	} else {
		styles.SetAutoThemeEnabled(false)
	}

	theme, err := styles.LoadTheme(themeRef)
	if err != nil {
		slog.Warn("Failed to load theme, using default", "theme", themeRef, "error", err)
		theme = styles.DefaultTheme()
	}

	styles.ApplyTheme(theme)
	slog.Debug("Applied theme", "theme_ref", themeRef, "theme_name", theme.Name)
}

// detectDarkTerminalBackground reports whether the terminal background is
// dark, falling back to dark when it cannot tell (non-TTY, pipes, CI, or a
// terminal that never answers). The query is TTY-gated so non-interactive
// runs emit no escape sequences at all; lipgloss bounds the wait internally
// and terminates early on the DA1 fallback answer that virtually every
// terminal sends.
func detectDarkTerminalBackground() bool {
	return terminalHasDarkBackground(os.Stdin, os.Stdout)
}

// terminalHasDarkBackground implements detectDarkTerminalBackground against
// explicit files so tests can drive the OSC 11 round-trip with a pty pair.
func terminalHasDarkBackground(in, out *os.File) bool {
	if !isatty.IsTerminal(in.Fd()) || !isatty.IsTerminal(out.Fd()) {
		return true
	}
	return lipgloss.HasDarkBackground(in, out)
}
