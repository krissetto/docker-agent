package root

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/docker/docker-agent/pkg/cli"
	"github.com/docker/docker-agent/pkg/runregistry"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/server"
	"github.com/docker/docker-agent/pkg/session"
)

// startSessionCoordinator exposes the shared session registry over HTTP when
// --listen is set. Local recall and event delivery are session-owned: the App
// retains its SessionHandle and observes the canonical session stream, while the
// server resolves the same session from the shared registry. Observer
// cancellation is therefore independent of session Stop.
func (f *runExecFlags) startSessionCoordinator(ctx context.Context, out *cli.Printer, sessions runtime.SessionRuntime, store session.Store, sess *session.Session) error {
	if f.listenAddr == "" {
		return nil
	}

	registry := newControlPlaneSessions(sessions, f)
	sm := server.NewSessionManager(ctx, nil, store, 0, &f.runConfig,
		server.WithSessionWorkingDirRoot(f.sessionWorkingDirRoot),
		server.WithSessionRuntime(registry),
	)
	f.listenSM = sm
	f.listenSessions = registry

	ln, err := server.Listen(ctx, f.listenAddr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", f.listenAddr, err)
	}
	context.AfterFunc(ctx, func() { _ = ln.Close() })

	cleanup, err := runregistry.Default().Write(runregistry.Record{
		PID:       os.Getpid(),
		Addr:      "http://" + ln.Addr().String(),
		SessionID: sess.ID,
		Agent:     f.agentName,
		StartedAt: time.Now(),
	})
	if err != nil {
		slog.WarnContext(ctx, "Could not write run registry record", "error", err)
	} else {
		context.AfterFunc(ctx, cleanup)
	}

	out.Println("Control plane listening on", ln.Addr().String())
	warnIfNotLoopback(out, ln.Addr())

	srv := server.NewWithManager(sm, "")
	go func() {
		if err := srv.Serve(ctx, ln); err != nil {
			slog.ErrorContext(ctx, "Control plane server stopped", "error", err)
		}
	}()
	return nil
}
