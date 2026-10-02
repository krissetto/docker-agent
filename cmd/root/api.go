package root

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"time"

	"github.com/spf13/cobra"
	"go.opentelemetry.io/otel"

	"github.com/docker/docker-agent/pkg/cli"
	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/snapshot"
	"github.com/docker/docker-agent/pkg/config/sources"
	"github.com/docker/docker-agent/pkg/host"
	pathx "github.com/docker/docker-agent/pkg/path"
	"github.com/docker/docker-agent/pkg/profiling"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/server"
	"github.com/docker/docker-agent/pkg/session/sqlitestore"
	"github.com/docker/docker-agent/pkg/teamloader"
	loaderdefaults "github.com/docker/docker-agent/pkg/teamloader/defaults"
	"github.com/docker/docker-agent/pkg/telemetry"
)

type apiFlags struct {
	listenAddr            string
	sessionDB             string
	pullIntervalMins      int
	fakeResponses         string
	recordPath            string
	authToken             string
	authTokenFile         string
	managedState          string
	managedManifest       string
	manifest              *snapshot.Manifest
	corsOrigin            string
	pprofAddr             string
	maxRequestSize        int64
	sessionWorkingDirRoot string
	runConfig             config.RuntimeConfig
}

func newAPICmd() *cobra.Command {
	var flags apiFlags

	cmd := &cobra.Command{
		Use:   "api <agent-file>|<agents-dir>",
		Short: "Start the API server",
		Args:  cobra.ExactArgs(1),
		RunE:  flags.runAPICommand,
	}

	cmd.PersistentFlags().StringVarP(&flags.listenAddr, "listen", "l", "127.0.0.1:8080", "Address to listen on")
	cmd.PersistentFlags().StringVarP(&flags.sessionDB, "session-db", "s", "session.db", "Path to the session database")
	cmd.PersistentFlags().IntVar(&flags.pullIntervalMins, "pull-interval", 0, "Auto-pull OCI reference every N minutes (0 = disabled)")
	cmd.PersistentFlags().StringVar(&flags.fakeResponses, "fake", "", "Replay AI responses from cassette file (for testing)")
	cmd.PersistentFlags().StringVar(&flags.recordPath, "record", "", "Record AI API interactions to cassette file")
	cmd.PersistentFlags().StringVar(&flags.authToken, "auth-token", "", "Bearer token required for API requests (empty = no authentication)")
	cmd.PersistentFlags().StringVar(&flags.authTokenFile, "auth-token-file", "", "Private file containing the API bearer token")
	cmd.MarkFlagsMutuallyExclusive("auth-token", "auth-token-file")
	cmd.PersistentFlags().StringVar(&flags.managedState, "managed-state", "", "Managed daemon state")
	cmd.PersistentFlags().StringVar(&flags.managedManifest, "managed-manifest", "", "Managed daemon candidate manifest")
	_ = cmd.PersistentFlags().MarkHidden("managed-state")
	_ = cmd.PersistentFlags().MarkHidden("managed-manifest")
	cmd.PersistentFlags().StringVar(&flags.corsOrigin, "cors-origin", "", "Exact browser origin allowed to call the API (empty disables CORS)")
	cmd.PersistentFlags().Int64Var(&flags.maxRequestSize, "max-request-size", 1<<20, "Maximum request body size in bytes (default 1 MiB). Requests exceeding this limit are rejected with HTTP 413.")
	cmd.PersistentFlags().StringVar(&flags.sessionWorkingDirRoot, "session-workingdir-root", "", "Restrict the working_dir of sessions created via POST /api/v2/sessions to this directory and its descendants (empty = no restriction; recommended for multi-user deployments)")
	cmd.PersistentFlags().StringVar(&flags.pprofAddr, "pprof-addr", "", "TCP host:port to expose Go pprof endpoints at /debug/pprof/ (e.g. 127.0.0.1:6060); also set via CAGENT_PPROF_ADDR")
	_ = cmd.PersistentFlags().MarkHidden("pprof-addr")
	cmd.MarkFlagsMutuallyExclusive("fake", "record")
	addRuntimeConfigFlags(cmd, &flags.runConfig)

	return cmd
}

func (f *apiFlags) runAPICommand(cmd *cobra.Command, args []string) (commandErr error) {
	ctx := cmd.Context()
	telemetry.TrackCommand(ctx, "serve", append([]string{"api"}, args...))
	defer func() { // do not inline this defer so that commandErr is not resolved early
		telemetry.TrackCommandError(ctx, "serve", append([]string{"api"}, args...), commandErr)
	}()

	out := cli.NewPrinter(cmd.OutOrStdout())
	agentsPath := args[0]
	releaseOwner, err := f.prepareManagedDaemon()
	if err != nil {
		return err
	}
	defer releaseOwner() // registered before store.Close: release ownership last
	if f.authTokenFile != "" {
		f.authToken, err = readAPIAuthToken(f.authTokenFile)
		if err != nil {
			return err
		}
	}

	if err := validateSessionWorkingDirRoot(f.sessionWorkingDirRoot); err != nil {
		return err
	}
	if f.corsOrigin != "" {
		if err := server.ValidateCORSOrigin(f.corsOrigin); err != nil {
			return fmt.Errorf("invalid --cors-origin: %w", err)
		}
	}

	// Make sure no question is ever asked to the user in api mode.
	os.Stdin = nil

	cleanup, err := setupFakeProxy(ctx, f.fakeResponses, 0, &f.runConfig)
	if err != nil {
		return err
	}
	defer func() {
		if err := cleanup(); err != nil {
			slog.ErrorContext(ctx, "Failed to cleanup fake proxy", "error", err)
		}
	}()

	// Start recording proxy if --record is specified
	_, recordCleanup, err := setupRecordingProxy(ctx, f.recordPath, &f.runConfig)
	if err != nil {
		return err
	}
	defer func() {
		if err := recordCleanup(); err != nil {
			slog.ErrorContext(ctx, "Failed to cleanup recording proxy", "error", err)
		}
	}()

	if f.pullIntervalMins > 0 && !config.IsOCIReference(agentsPath) && !config.IsURLReference(agentsPath) {
		return errors.New("--pull-interval flag can only be used with OCI or URL references, not local files")
	}

	if pprofAddr := cmp.Or(f.pprofAddr, os.Getenv("CAGENT_PPROF_ADDR")); pprofAddr != "" {
		if err := profiling.StartPprofServer(ctx, pprofAddr); err != nil {
			return err
		}
	}

	ln, lnCleanup, err := newListener(ctx, f.listenAddr)
	if err != nil {
		return err
	}
	defer lnCleanup()

	out.Println("Listening on", ln.Addr().String())
	if f.authToken == "" {
		warnIfNotLoopback(out, ln.Addr())
	}

	slog.DebugContext(ctx, "Starting server", "agents", agentsPath, "addr", ln.Addr().String())

	// Expand tilde in session database path
	sessionDB, err := pathx.ExpandHomeDir(f.sessionDB)
	if err != nil {
		return err
	}

	sessionStore, err := sqlitestore.New(ctx, sessionDB)
	if err != nil {
		return fmt.Errorf("creating session store: %w", err)
	}
	defer func() {
		if err := sessionStore.Close(); err != nil {
			slog.ErrorContext(ctx, "Failed to close session store", "error", err)
		}
	}()

	var agentSources config.Sources
	if f.manifest != nil {
		agentSources = config.Sources{f.manifest.Startup().SourceKey: f.manifest.FrozenSource()}
	} else {
		agentSources, err = sources.ResolveSources(agentsPath, f.runConfig.EnvProvider())
	}
	if err != nil {
		return fmt.Errorf("resolving agent sources: %w", err)
	}

	// Session runtimes are built by the server on demand — per source, per
	// session working directory, and again after a source refresh — and shut
	// down with it. Nothing is loaded before the first session needs it.
	loaderOptions := loaderdefaults.Opts()
	if f.manifest != nil {
		loaderOptions = append(loaderOptions, teamloader.WithSourceResolver(f.manifest.SourceResolver), teamloader.WithModelOverrides(f.manifest.Startup().ModelOverrides))
	}
	serverOpts := []server.SessionManagerOpt{
		server.WithSessionWorkingDirRoot(f.sessionWorkingDirRoot),
		server.WithSessionRuntimeFactory(func(ctx context.Context, source config.Source, workingDir string) (runtime.SessionRuntimeSupervisor, error) {
			return host.NewSessionRuntime(ctx, source, &f.runConfig, sessionStore, host.RuntimeOptions{LoaderOptions: loaderOptions, WorkingDir: workingDir, ManagedOAuth: false, UnmanagedOAuthRedirectURI: f.runConfig.MCPOAuthRedirectURI, Tracer: otel.Tracer(AppName)})
		}),
	}

	identity, err := f.managedIdentity()
	if err != nil {
		return err
	}
	sm := server.NewSessionManager(ctx, agentSources, sessionStore, time.Duration(f.pullIntervalMins)*time.Minute, &f.runConfig, serverOpts...)
	s := server.NewWithManager(sm, f.authToken, server.WithCORSOrigin(f.corsOrigin), server.WithMaxRequestBytes(f.maxRequestSize), server.WithServerInfo(identity))
	return f.serveManaged(ctx, s, ln, identity)
}

// warnIfNotLoopback prints a security warning when the API server is bound to
// an address other than loopback. The default --listen value is 127.0.0.1, so
// reaching this code path means the operator was explicit about exposing the
// API; we just remind them that the API has no authentication.
func warnIfNotLoopback(out *cli.Printer, addr net.Addr) {
	tcpAddr, ok := addr.(*net.TCPAddr)
	if !ok {
		// Unix sockets and named pipes rely on filesystem permissions.
		return
	}
	if tcpAddr.IP.IsLoopback() {
		return
	}
	out.Println("WARNING: API server is listening on a non-loopback address.")
	out.Println("         The API has no authentication; anyone able to reach")
	out.Println("         this address can run agents and access all sessions.")
	slog.Warn("API server bound to non-loopback address", "addr", tcpAddr.String())
}
