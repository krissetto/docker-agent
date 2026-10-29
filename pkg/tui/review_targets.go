package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tui/components/notification"
	view "github.com/docker/docker-agent/pkg/tui/components/review"
	"github.com/docker/docker-agent/pkg/tui/core"
	"github.com/docker/docker-agent/pkg/tui/dialog"
	"github.com/docker/docker-agent/pkg/tui/reviewdraft"
)

const (
	gitReviewTargetPage     = 20
	gitReviewTargetMaxPages = 10
	gitReviewNewTarget      = "\x00new-session"
	gitReviewAgentDialogID  = "git-review-agent"
)

// gitReviewCatalogPager fetches one bounded page of session metadata (no
// transcripts). next is empty on the last page.
type gitReviewCatalogPager func(ctx context.Context, cursor string, limit int) (entries []runtime.SessionSummaryEntry, next string, err error)

type gitReviewCatalogResult struct {
	choice  *gitReviewTargetChoice
	entries []runtime.SessionSummaryEntry
	next    string
	err     error
}

// gitReviewLease borrows a session view without a tab. Releasing it closes
// only the borrowed view; the recipient's runtime keeps running.
type gitReviewLease struct {
	release  func()
	released bool
}

func (l *gitReviewLease) Release() {
	if l != nil && !l.released {
		l.released = true
		l.release()
	}
}

type gitReviewResolved struct {
	choice *gitReviewTargetChoice
	target dialog.ReviewTarget
	app    *app.App
	lease  *gitReviewLease
	err    error
}

// gitReviewResolveFunc loads an existing session's view for sending; tests replace it.
type gitReviewResolveFunc func(ctx context.Context, id string) (*app.App, func(), error)

type gitReviewAgents struct {
	choice *gitReviewTargetChoice
	agents []ReviewSessionAgent
	err    error
}

// openGitReviewTargetPicker lists recipients: open sessions first, then
// bounded pages of other sessions, then New session when the host offers it.
func (m *appModel) openGitReviewTargetPicker(msg view.PreviewDraftMsg) tea.Cmd {
	ws := m.gitReviews[msg.Owner]
	if ws == nil || (len(ws.draft.Snapshot().Comments) == 0 && ws.draft.Snapshot().General == "") || ws.draft.Snapshot().Status == reviewdraft.StatusSending {
		return nil
	}
	if attempt, pending := ws.draft.Attempt(); pending {
		// An unconfirmed send keeps its exact recipient, request and payload:
		// the picker never offers a different target until it is resolved.
		if ws.preview == nil || ws.preview.target.SessionID != attempt.Target.SessionID {
			if ws.sent == nil || ws.sent.target.SessionID != attempt.Target.SessionID {
				return notification.ErrorCmd("The previous send is unresolved and its recipient is unavailable; draft kept")
			}
			retry := *ws.sent
			retry.payload = attempt.Payload
			ws.preview = &retry
		}
		ws.pushDraft()
		return nil
	}
	if ws.mode == view.ModeChanges {
		return m.previewCurrentAgent(ws, msg.Revision)
	}
	m.closeGitReviewPicker()
	ctx, cancel := context.WithCancel(m.ctx())
	choice := &gitReviewTargetChoice{owner: msg.Owner, revision: msg.Revision, options: map[string]gitReviewCandidate{}, ctx: ctx, cancel: cancel}
	m.gitReviewPicker = choice
	var targets []dialog.ReviewTarget
	for _, candidate := range m.gitReviewCandidates(msg.Owner) {
		choice.options[candidate.sessionID] = candidate
		targets = append(targets, dialog.ReviewTarget{ID: candidate.sessionID, Label: candidate.label, Detail: candidate.detail})
	}
	if m.reviewSessionFactory != nil {
		targets = append(targets, dialog.ReviewTarget{ID: gitReviewNewTarget, Label: "New session…", Detail: "choose an agent", New: true})
	}
	open := core.CmdHandler(dialog.OpenDialogMsg{Model: dialog.NewReviewTargetDialog(dialog.ReviewTargetConfig{
		DialogID: gitReviewTargetDialogID, Title: "Send review to…", Targets: targets, Default: m.gitReviewDefaultRecipient(msg.Owner), More: m.gitReviewCatalogPager() != nil, Context: choice,
	})})
	return open
}

// gitReviewDefaultRecipient is the session (and attached agent) the user is
// focused on, falling back to the review's own workspace.
func (m *appModel) gitReviewDefaultRecipient(owner string) string {
	if focus := m.paneFocus(); focus != "" {
		if runner := m.supervisor.GetRunner(focus); runner != nil && runner.App != nil && !runner.App.IsReadOnly() {
			return focus
		}
	}
	return owner
}

func (m *appModel) closeGitReviewPicker() {
	if m.gitReviewPicker != nil {
		m.gitReviewPicker.cancel()
		m.gitReviewPicker = nil
	}
}

// gitReviewCatalogPager returns the bounded metadata pager, if the session
// runtime offers one. A full unpaged listing is never used for this picker.
func (m *appModel) gitReviewCatalogPager() gitReviewCatalogPager {
	if m.gitReviewCatalog != nil {
		return m.gitReviewCatalog
	}
	if m.application == nil {
		return nil
	}
	return reviewPagerFor(m.application.SessionRuntime())
}

func reviewPagerFor(rt runtime.SessionRuntime) gitReviewCatalogPager {
	pager, ok := rt.(runtime.SessionSummaryPager)
	if !ok {
		return nil
	}
	return func(ctx context.Context, cursor string, limit int) ([]runtime.SessionSummaryEntry, string, error) {
		page, err := pager.ListSessionSummaryPage(ctx, runtime.SessionSummaryPageOptions{IncludeChildren: true, Limit: limit, Cursor: cursor})
		return page.Entries, page.NextCursor, err
	}
}

func (m *appModel) loadGitReviewCatalogPage(choice *gitReviewTargetChoice) tea.Cmd {
	pager := m.gitReviewCatalogPager()
	if pager == nil || choice.loading || choice.pages >= gitReviewTargetMaxPages || (choice.pages > 0 && choice.next == "") {
		return nil
	}
	choice.loading = true
	ctx, cursor := choice.ctx, choice.next
	return func() tea.Msg {
		entries, next, err := pager(ctx, cursor, gitReviewTargetPage)
		return gitReviewCatalogResult{choice: choice, entries: entries, next: next, err: err}
	}
}

func (m *appModel) applyGitReviewCatalog(result gitReviewCatalogResult) tea.Cmd {
	choice := result.choice
	if choice == nil || choice != m.gitReviewPicker {
		return nil // a cancelled or replaced picker never receives late pages
	}
	choice.loading = false
	msg := dialog.ReviewTargetPageMsg{DialogID: gitReviewTargetDialogID}
	if result.err != nil {
		msg.Partial = "Couldn't load more sessions"
		return m.updateDialogCmd(msg)
	}
	choice.next, choice.pages = result.next, choice.pages+1
	for _, e := range result.entries {
		if e.SessionID == "" {
			continue
		}
		if _, open := choice.options[e.SessionID]; open {
			continue
		}
		msg.Targets = append(msg.Targets, catalogTarget(e))
	}
	msg.More = choice.next != "" && choice.pages < gitReviewTargetMaxPages
	if choice.next != "" && !msg.More {
		msg.Partial = fmt.Sprintf("Showing the first %d sessions", gitReviewTargetPage*gitReviewTargetMaxPages)
	}
	return m.updateDialogCmd(msg)
}

func catalogTarget(e runtime.SessionSummaryEntry) dialog.ReviewTarget {
	var detail []string
	if e.AgentName != "" {
		detail = append(detail, e.AgentName)
	}
	if e.ParentID != "" {
		detail = append(detail, "subagent session")
	}
	detail = append(detail, fmt.Sprintf("%d msgs · not open", e.NumMessages))
	t := dialog.ReviewTarget{ID: e.SessionID, Label: "◇ " + fallbackString(e.Title, "untitled session"), Detail: strings.Join(detail, " · ")}
	switch {
	case e.RouteError != "":
		t.Disabled = "unavailable"
	case !e.Loadable:
		t.Disabled = "not loadable"
	case e.RequiresConfirmation:
		// A cold child candidate is not an access grant.
		t.Disabled = "open it to confirm access"
	}
	return t
}

func (m *appModel) handleGitReviewTargetMessage(msg tea.Msg) tea.Cmd {
	switch x := msg.(type) {
	case gitReviewCatalogResult:
		return m.applyGitReviewCatalog(x)
	case dialog.ReviewTargetMoreMsg:
		if choice, ok := x.Context.(*gitReviewTargetChoice); ok && choice == m.gitReviewPicker {
			return m.loadGitReviewCatalogPage(choice)
		}
	case dialog.ReviewTargetResultMsg:
		choice, ok := x.Context.(*gitReviewTargetChoice)
		if !ok || choice != m.gitReviewPicker {
			return nil
		}
		if x.DialogID == gitReviewAgentDialogID {
			m.closeGitReviewPicker()
			if x.Cancelled {
				return nil
			}
			return m.applyGitReviewNewSession(choice, x.Target.ID)
		}
		if x.Cancelled || x.Target.Disabled != "" {
			m.closeGitReviewPicker()
			return nil
		}
		if x.Target.New {
			return m.loadGitReviewAgents(choice)
		}
		if _, open := choice.options[x.Target.ID]; open {
			m.closeGitReviewPicker()
			return m.applyGitReviewTarget(choice, x.Target.ID)
		}
		return m.resolveGitReviewTarget(choice, x.Target)
	case gitReviewResolved:
		return m.applyGitReviewResolved(x)
	case gitReviewAgents:
		return m.openGitReviewAgentPicker(x)
	}
	return nil
}

// defaultGitReviewResolve uses the canonical hosted-session load: a committed
// view lease and an unadopted App, never a tab. Release closes that borrowed
// view only, as an abandoned hosted load does.
func (m *appModel) defaultGitReviewResolve(ctx context.Context, id string) (*app.App, func(), error) {
	owner, lifetime := m.supervisor, m.ctx()
	data, err := loadHostedSession(ctx, owner, id)
	if err != nil {
		return nil, nil, err
	}
	a, err := data.newApp(lifetime, data.committed)
	if err != nil {
		data.prepared.Abort()
		return nil, nil, err
	}
	return a, func() { a.Close(); data.prepared.Abort() }, nil
}

func (m *appModel) resolveGitReviewTarget(choice *gitReviewTargetChoice, target dialog.ReviewTarget) tea.Cmd {
	resolve := m.gitReviewResolve
	if resolve == nil {
		if m.supervisor == nil {
			return nil
		}
		resolve = m.defaultGitReviewResolve
	}
	ctx := choice.ctx
	return func() tea.Msg {
		a, release, err := resolve(ctx, target.ID)
		result := gitReviewResolved{choice: choice, target: target, app: a, err: err}
		if err == nil {
			result.lease = &gitReviewLease{release: release}
		}
		return result
	}
}

func (m *appModel) applyGitReviewResolved(r gitReviewResolved) tea.Cmd {
	stale := r.choice != m.gitReviewPicker
	if !stale {
		m.closeGitReviewPicker()
	}
	if r.err != nil {
		if stale {
			return nil
		}
		return notification.ErrorCmd("Can't reach that session; draft kept: " + r.err.Error())
	}
	ws := m.gitReviews[r.choice.owner]
	if stale || ws == nil || r.app == nil || r.app.Session() == nil || r.app.Session().ID != r.target.ID || r.app.IsReadOnly() {
		r.lease.Release()
		if stale {
			return nil
		}
		return notification.ErrorCmd("That session can't receive this review")
	}
	r.choice.options[r.target.ID] = gitReviewCandidate{label: r.target.Label, detail: r.target.Detail, sessionID: r.target.ID, app: r.app, lease: r.lease}
	cmd := m.applyGitReviewTarget(r.choice, r.target.ID)
	if ws.preview == nil || ws.preview.lease != r.lease {
		r.lease.Release()
	}
	return cmd
}

// New session ---------------------------------------------------------------

func (m *appModel) loadGitReviewAgents(choice *gitReviewTargetChoice) tea.Cmd {
	factory := m.reviewSessionFactory
	if factory == nil {
		m.closeGitReviewPicker()
		return nil
	}
	ctx := choice.ctx
	return func() tea.Msg {
		agents, err := factory.ListAgents(ctx)
		return gitReviewAgents{choice: choice, agents: agents, err: err}
	}
}

func (m *appModel) openGitReviewAgentPicker(r gitReviewAgents) tea.Cmd {
	if r.choice != m.gitReviewPicker {
		return nil
	}
	if r.err != nil || len(r.agents) == 0 {
		m.closeGitReviewPicker()
		if r.err != nil {
			return notification.ErrorCmd("Can't list agents: " + r.err.Error())
		}
		return notification.InfoCmd("No agents are available for a new session")
	}
	ws := m.gitReviews[r.choice.owner]
	if ws == nil {
		m.closeGitReviewPicker()
		return nil
	}
	r.choice.agents = map[string]ReviewSessionAgent{}
	var targets []dialog.ReviewTarget
	for _, a := range r.agents {
		r.choice.agents[a.Name] = a
		targets = append(targets, dialog.ReviewTarget{ID: a.Name, Label: a.Name, Detail: a.Description})
	}
	return core.CmdHandler(dialog.OpenDialogMsg{Model: dialog.NewReviewTargetDialog(dialog.ReviewTargetConfig{
		DialogID: gitReviewAgentDialogID, Title: "New session in " + ws.workingDir, Targets: targets, Context: r.choice,
	})})
}

// applyGitReviewNewSession previews a send to a session that does not exist
// yet. Its ID is reserved now and reused for every retry; creation happens
// only after explicit confirmation.
func (m *appModel) applyGitReviewNewSession(choice *gitReviewTargetChoice, agentName string) tea.Cmd {
	ws := m.gitReviews[choice.owner]
	agent, ok := choice.agents[agentName]
	if ws == nil || !ok {
		return nil
	}
	snap := ws.draft.Snapshot()
	if snap.Revision != choice.revision {
		return notification.InfoCmd("The review draft changed; preview again")
	}
	sessionID, err := newGitReviewSessionID()
	if err != nil {
		return notification.ErrorCmd("Review preview failed: " + err.Error())
	}
	token, err := newGitReviewToken()
	if err != nil {
		return notification.ErrorCmd("Review preview failed: " + err.Error())
	}
	source := reviewAgentSource(agent)
	if source == "" {
		return notification.ErrorCmd("That agent's source is unknown; nothing was created")
	}
	target := reviewdraft.Target{Kind: reviewdraft.TargetNewSession, SessionID: sessionID, AgentName: agent.Name, WorkingDir: ws.workingDir, Source: source}
	label := "new " + agent.Name + " session in " + ws.workingDir + " · " + source + " (opens in background)"
	ws.setPreview(&gitReviewPreview{token: token, revision: snap.Revision, label: label, target: target, payload: reviewdraft.SerializeWithGeneral(snap.Comments, snap.General), general: snap.General})
	ws.pushDraft()
	m.viewCacheValid = false
	return nil
}

func newGitReviewSessionID() (string, error) {
	token, err := newGitReviewToken()
	if err != nil {
		return "", err
	}
	return "review-" + token, nil
}

// submitGitReviewNewSession creates the session and admits the review as its
// first input through the host factory; retries reuse both IDs.
func (m *appModel) submitGitReviewNewSession(owner string, attempt reviewdraft.Attempt, commentInput *reviewCommentInput) tea.Cmd {
	factory := m.reviewSessionFactory
	if factory == nil {
		return nil
	}
	ctx := m.ctx()
	t := attempt.Target
	return func() tea.Msg {
		if t.Source == "" {
			return gitReviewSendResult{owner: owner, requestID: attempt.RequestID, commentInput: commentInput, err: errors.New("new session source is unknown")}
		}
		result, err := factory.CreateAndSubmit(ctx, ReviewSessionRequest{SessionID: t.SessionID, RequestID: attempt.RequestID, AgentName: t.AgentName, WorkingDir: t.WorkingDir, Source: t.Source, Body: attempt.Payload})
		if err == nil && (!result.Accepted || result.SessionID != t.SessionID) {
			err = errors.New("new session did not accept the review")
		}
		return gitReviewSendResult{owner: owner, requestID: attempt.RequestID, commentInput: commentInput, receipt: app.ReviewSubmitReceipt{Submission: runtime.Submission{SessionID: result.SessionID, TurnID: result.TurnID}, Durable: result.Durable}, newTitle: result.Title, err: err}
	}
}

// reviewAgentSource is the authoritative source the factory listed the agent
// under; it is never guessed from paths.
func reviewAgentSource(agent ReviewSessionAgent) string { return agent.Source }
