package acp

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"time"

	acpsdk "github.com/coder/acp-go-sdk"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/sources"
	"github.com/docker/docker-agent/pkg/session/sqlitestore"
)

func Run(ctx context.Context, agentFilename string, stdin io.Reader, stdout io.Writer, runConfig *config.RuntimeConfig, sessionDB string) error {
	slog.DebugContext(ctx, "Starting ACP server", "agent", agentFilename, "session_db", sessionDB)

	agentSource, err := sources.Resolve(agentFilename, nil)
	if err != nil {
		return err
	}

	// Create SQLite session store for persistent sessions
	sessStore, err := sqlitestore.New(ctx, sessionDB)
	if err != nil {
		return fmt.Errorf("creating session store: %w", err)
	}
	// Close the store on shutdown if it implements io.Closer
	if closer, ok := sessStore.(io.Closer); ok {
		defer closer.Close()
	}

	acpAgent := NewAgent(agentSource, runConfig, sessStore)
	conn := acpsdk.NewAgentSideConnection(acpAgent, stdout, stdin)
	conn.SetLogger(slog.Default())
	acpAgent.SetAgentConnection(conn)
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		acpAgent.Stop(cleanupCtx)
	}()

	slog.DebugContext(ctx, "acp started, waiting for conn")
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-conn.Done():
		return nil
	}
}
