package environment

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os/exec"
	"path/filepath"
	"strings"
)

// runCommand executes a command and returns its trimmed stdout.
// Returns ("", false) if the command fails or is not found.
// The name parameter should be a fully-qualified path to the binary
// (as returned by lookupBinary) to prevent PATH hijacking (CWE-426).
func runCommand(ctx context.Context, logLabel, name string, args ...string) (string, bool) {
	var stdout, stderr bytes.Buffer

	cmd := exec.CommandContext(ctx, name, args...)
	cmd.SysProcAttr = secretCommandSysProcAttr()
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		slog.DebugContext(ctx, "Failed to find secret in "+logLabel, "error", err)
		return "", false
	}

	return strings.TrimSpace(stdout.String()), true
}

// lookupBinary checks if a binary is available on the system PATH and returns
// its absolute path. Returns a non-nil error if the binary is not found.
// The returned path should be stored and reused (rather than the bare name)
// to avoid TOCTOU races and PATH hijacking.
func lookupBinary(name string, notFoundErr error) (string, error) {
	path, err := exec.LookPath(name)
	if err != nil {
		// exec.ErrDot means the binary was only found via an unsafe relative
		// PATH entry (or "."). Treat it like "not found" so we never run an
		// attacker-controlled binary from the working directory (CWE-426).
		if !errors.Is(err, exec.ErrNotFound) && !errors.Is(err, exec.ErrDot) {
			slog.Warn("failed to lookup `"+name+"` binary", "error", err)
		}
		return "", notFoundErr
	}
	// Defensively require an absolute path so the resolved binary cannot be
	// hijacked via PATH or the current working directory.
	if !filepath.IsAbs(path) {
		return "", notFoundErr
	}
	return path, nil
}
