package tui

import (
	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/page/chat"
	"github.com/docker/docker-agent/pkg/tui/service/supervisor"
)

// sessionViewPolicy controls component installation only. Acquisition admission,
// persistence, layout, and focus remain explicit in each event-loop caller.
type sessionViewPolicy struct {
	replaceRoute string
	asyncReplay  bool
}

// adoptSessionView transfers a validated App to the supervisor and installs its
// component lifetime. On error the caller still owns and must close the App.
func (m *appModel) adoptSessionView(a *app.App, sess *session.Session, workingDir string, policy sessionViewPolicy) (string, tea.Cmd, error) {
	id := policy.replaceRoute
	if id == "" {
		var err error
		id, err = m.supervisor.AddSession(m.ctx(), a, sess, workingDir, nil)
		if err != nil {
			return "", nil, err
		}
	} else {
		m.supervisor.ReplaceRunnerApp(m.ctx(), id, supervisor.SpawnedSession{App: a, Session: sess, Ownership: supervisor.RuntimeBorrowed}, workingDir)
	}
	m.createSessionComponents(id, a, sess)
	page, editor := m.chatPages[id], m.editors[id]
	if policy.asyncReplay {
		chat.EnableAsyncReplay(page)
	}
	return id, m.routePaneCmd(id, tea.Batch(page.Init(), chat.WatchGitBranch(page), editor.Init())), nil
}

// disposeSessionComponents is event-loop-only, shared by replacement and close.
// In particular an overwritten editor must release its hover animations and temporary paste files.
func (m *appModel) disposeSessionComponents(id string) {
	if page := m.chatPages[id]; page != nil {
		chat.Cleanup(page)
	}
	if editor := m.editors[id]; editor != nil {
		editor.Cleanup()
	}
	delete(m.chatPages, id)
	delete(m.editors, id)
	delete(m.sessionStates, id)
}
