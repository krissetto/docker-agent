package memory

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/memory/database/sqlite"
	"github.com/docker/docker-agent/pkg/paths"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/toolsetpath"
)

// CreateToolSet is used by the tools registry.
func CreateToolSet(toolset latest.Toolset, parentDir string, runConfig *config.RuntimeConfig, configName string) (tools.ToolSet, error) {
	return CreateToolSetWithBackend(toolset, parentDir, runConfig, configName, openSQLiteBackend)
}

// BackendFactory opens a backend for a resolved database path. Its caller owns its lifetime.
type BackendFactory func(path string) (DB, error)

// CreateToolSetWithBackend lets embedders use a portable backend without opening SQLite.
func CreateToolSetWithBackend(toolset latest.Toolset, parentDir string, runConfig *config.RuntimeConfig, configName string, open BackendFactory) (tools.ToolSet, error) {
	if open == nil {
		return nil, errors.New("memory backend factory is required")
	}
	var validatedMemoryPath string

	if toolset.Path != "" {
		var err error
		validatedMemoryPath, err = toolsetpath.Resolve(toolset.Path, parentDir, runConfig)
		if err != nil {
			return nil, fmt.Errorf("invalid memory database path: %w", err)
		}
	} else {
		if configName == "" {
			configName = "default"
		}
		validatedMemoryPath = filepath.Join(paths.GetDataDir(), "memory", sanitizePathSegment(configName), "memory.db")
	}

	db, err := open(validatedMemoryPath)
	if err != nil {
		return nil, fmt.Errorf("failed to create memory database: %w", err)
	}

	if db == nil {
		return nil, errors.New("memory backend factory returned nil")
	}

	return NewWithPath(db, validatedMemoryPath), nil
}

// sanitizePathSegment replaces characters that are illegal in a single path
// component on Windows with '_'. Agent sources loaded from an OCI reference
// (e.g. "namespace/repo:tag") produce config names that include the image
// tag's ':'; the colon causes os.MkdirAll to fail with ERROR_INVALID_NAME on
// NTFS. The replacement is lossy but safe — the hash suffix already in the
// config name preserves uniqueness, so collisions from sanitisation aren't a
// concern in practice.
func sanitizePathSegment(s string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case '<', '>', ':', '"', '|', '?', '*', '\\', '/':
			return '_'
		}
		if r < 0x20 {
			return '_'
		}
		return r
	}, s)
}

func openSQLiteBackend(path string) (DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("failed to create memory database directory: %w", err)
	}
	return sqlite.NewMemoryDatabase(path)
}
