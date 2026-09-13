package tui

import (
	"context"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/browser"
	"github.com/docker/docker-agent/pkg/tui/components/notification"
)

func workingDirectoryURL(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("working directory is not an absolute local path: %q", path)
	}
	normalized := filepath.ToSlash(path)
	if network, ok := strings.CutPrefix(normalized, "//"); ok {
		host, rest, ok := strings.Cut(network, "/")
		if !ok || host == "" {
			return "", fmt.Errorf("invalid network directory path: %q", path)
		}
		return (&url.URL{Scheme: "file", Host: host, Path: "/" + rest}).String(), nil
	}
	if !strings.HasPrefix(normalized, "/") {
		normalized = "/" + normalized
	}
	return (&url.URL{Scheme: "file", Path: normalized}).String(), nil
}

func openWorkingDirectory(ctx context.Context, path string) error {
	target, err := workingDirectoryURL(path)
	if err != nil {
		return err
	}
	return browser.Open(ctx, target)
}

func (m *appModel) handleOpenWorkingDirectory(path string) tea.Cmd {
	opener := m.openWorkingDirectory
	if opener == nil {
		opener = openWorkingDirectory
	}
	ctx := m.ctx()
	return func() tea.Msg {
		if err := opener(ctx, path); err != nil {
			return notification.ShowMsg{Text: "Failed to open working directory: " + err.Error(), Type: notification.TypeError}
		}
		return nil
	}
}
