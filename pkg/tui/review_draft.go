package tui

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/tui/components/messagebar"
	"github.com/docker/docker-agent/pkg/tui/components/notification"
	view "github.com/docker/docker-agent/pkg/tui/components/review"
	"github.com/docker/docker-agent/pkg/tui/core"
	"github.com/docker/docker-agent/pkg/tui/dialog"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/reviewdraft"
)

const gitReviewTargetDialogID = "git-review-target"

// gitReviewPreview is a captured send: the chosen recipient, its exact app and
// route generation, the draft revision and a token the confirm must echo.
type gitReviewPreview struct {
	lease      *gitReviewLease // set when the recipient has no tab
	token      string
	revision   uint64
	label      string
	target     reviewdraft.Target
	app        *app.App
	submit     app.ReviewSubmitTarget
	payload    string
	general    string
	activeOnly bool
}

// gitReviewTargetChoice is one open picker; results for any other picker are ignored.
type gitReviewTargetChoice struct {
	owner      string
	revision   uint64
	activeOnly bool
	options    map[string]gitReviewCandidate
	agents     map[string]ReviewSessionAgent
	next       string
	pages      int
	loading    bool
	ctx        context.Context //nolint:containedctx // bounds the picker's loads; cancelled when it closes
	cancel     context.CancelFunc
}

type gitReviewCandidate struct {
	lease     *gitReviewLease
	label     string
	detail    string
	sessionID string
	app       *app.App
	route     uint64
}

type gitReviewSendResult struct {
	owner        string
	requestID    string
	commentInput *reviewCommentInput
	receipt      app.ReviewSubmitReceipt
	newTitle     string
	err          error
}

// gitReviewSubmitFunc submits one literal review body; tests replace it.
type gitReviewSubmitFunc func(ctx context.Context, a *app.App, target app.ReviewSubmitTarget, requestID, body string) (app.ReviewSubmitReceipt, error)

func defaultGitReviewSubmit(ctx context.Context, a *app.App, target app.ReviewSubmitTarget, requestID, body string) (app.ReviewSubmitReceipt, error) {
	return a.SubmitReviewInput(ctx, target, requestID, body)
}

func (m *appModel) gitReviewSubmitter() gitReviewSubmitFunc {
	if m.gitReviewSubmit != nil {
		return m.gitReviewSubmit
	}
	return defaultGitReviewSubmit
}

func toDraftAnchor(a view.Anchor) reviewdraft.Anchor {
	return reviewdraft.Anchor{
		RepositoryID: a.RepositoryID, RepositoryName: a.RepositoryName, WorktreeID: a.WorktreeID, WorktreePath: a.CheckoutPath,
		Branch: fallbackString(a.Branch, a.Detached), Path: a.Path, OldPath: a.OldPath, Mode: reviewModeName(a.Mode), Group: reviewGroupName(a.Group),
		OldStart: a.OldStart, OldEnd: a.OldEnd, NewStart: a.NewStart, NewEnd: a.NewEnd, StartColumn: a.StartColumn, EndColumn: a.EndColumn, CharacterRange: a.CharacterRange, Context: append([]string(nil), a.Context...), Version: a.Version,
	}
}

func fromDraftAnchor(a reviewdraft.Anchor) view.Anchor {
	mode := view.ModeChanges
	if a.Mode == "files" {
		mode = view.ModeFiles
	}
	return view.Anchor{
		RepositoryID: a.RepositoryID, RepositoryName: a.RepositoryName, WorktreeID: a.WorktreeID, CheckoutPath: a.WorktreePath, Branch: a.Branch,
		Path: a.Path, OldPath: a.OldPath, Mode: mode, Group: reviewGroup(gitReviewGroup(groupFromName(a.Group))),
		OldStart: a.OldStart, OldEnd: a.OldEnd, NewStart: a.NewStart, NewEnd: a.NewEnd, StartColumn: a.StartColumn, EndColumn: a.EndColumn, CharacterRange: a.CharacterRange, Context: a.Context, Version: a.Version,
	}
}

func reviewModeName(m view.Mode) string {
	if m == view.ModeFiles {
		return "files"
	}
	return "changes"
}

func reviewGroupName(g view.Group) string { return string(gitReviewGroup(g)) }

func groupFromName(name string) view.Group {
	for _, g := range []view.Group{view.GroupConflicts, view.GroupStaged, view.GroupUnstaged, view.GroupUntracked} {
		if reviewGroupName(g) == name {
			return g
		}
	}
	return view.GroupNone
}

// draftView projects the workspace draft and any captured preview for display.
func (ws *gitReviewWorkspace) draftView() view.DraftView {
	snap := ws.draft.Snapshot()
	out := view.DraftView{Revision: snap.Revision, Err: snap.Err, General: snap.General}
	for _, c := range snap.Comments {
		out.Comments = append(out.Comments, view.Comment{ID: c.ID, Anchor: fromDraftAnchor(c.Anchor), Body: c.Body})
	}
	switch snap.Status {
	case reviewdraft.StatusSending:
		out.Status = view.DraftSending
	case reviewdraft.StatusFailed:
		out.Status = view.DraftFailed
	}
	if ws.preview != nil {
		if out.Status == view.DraftIdle {
			out.Status = view.DraftPreviewing
		}
		out.Target, out.Preview, out.Token = ws.preview.label, ws.preview.payload, ws.preview.token
	}
	return out
}

// setPreview swaps the captured send, releasing a borrowed view nobody needs:
// an unresolved attempt keeps its recipient's lease for exact retry.
func (ws *gitReviewWorkspace) setPreview(p *gitReviewPreview) {
	if old := ws.preview; old != nil && old.lease != nil && (p == nil || p.lease != old.lease) && (ws.sent == nil || ws.sent.lease != old.lease) {
		old.lease.Release()
	}
	ws.preview = p
}

func (ws *gitReviewWorkspace) releaseLeases() {
	if ws.sent != nil && ws.sent.lease != nil {
		ws.sent.lease.Release()
	}
	if ws.preview != nil && ws.preview.lease != nil {
		ws.preview.lease.Release()
	}
}

func (ws *gitReviewWorkspace) pushDraft() {
	d := ws.draftView()
	ws.each(func(model *view.Model) { model.SetDraft(d) })
}

func (m *appModel) handleReviewDraftMessage(msg tea.Msg) tea.Cmd {
	switch x := msg.(type) {
	case view.CommentInputRequestMsg:
		return m.startReviewComment(x)
	case view.AddCommentMsg:
		ws := m.gitReviews[x.Owner]
		if ws == nil {
			return nil
		}
		_, err := ws.draft.Add(toDraftAnchor(x.Anchor), x.Body)
		return m.finishInlineDraftMutation(ws, x.RequestID, err, "Comment not added")
	case view.EditCommentMsg:
		ws := m.gitReviews[x.Owner]
		if ws == nil {
			return nil
		}
		err := ws.draft.Edit(x.ID, x.Body)
		return m.finishInlineDraftMutation(ws, x.RequestID, err, "Comment not saved")
	case view.DeleteCommentMsg:
		ws := m.gitReviews[x.Owner]
		if ws == nil {
			return nil
		}
		if err := ws.draft.Delete(x.ID); err != nil {
			return notification.ErrorCmd("Comment not deleted: " + err.Error())
		}
		ws.setPreview(nil)
		ws.pushDraft()
	case view.PreviewDraftMsg:
		if cmd := m.captureReviewGeneral(x.Owner); cmd != nil {
			return cmd
		}
		if ws := m.gitReviews[x.Owner]; ws != nil {
			if _, pending := ws.draft.Attempt(); pending {
				return m.openGitReviewTargetPicker(x)
			}
			return m.previewCurrentAgent(ws, x.Revision)
		}
		return nil
	case dialog.ReviewTargetResultMsg, dialog.ReviewTargetMoreMsg, gitReviewCatalogResult, gitReviewResolved, gitReviewAgents:
		return m.handleGitReviewTargetMessage(x)
	case view.CancelPreviewMsg:
		if ws := m.gitReviews[x.Owner]; ws != nil && ws.draft.Snapshot().Status != reviewdraft.StatusSending {
			ws.setPreview(nil)
			ws.pushDraft()
		}
	case view.SendDraftMsg:
		return m.sendGitReviewDraft(x)
	case gitReviewSendResult:
		return m.applyGitReviewSend(x)
	}
	return nil
}

func (m *appModel) finishInlineDraftMutation(ws *gitReviewWorkspace, requestID string, err error, failure string) tea.Cmd {
	if err == nil {
		ws.setPreview(nil)
		ws.pushDraft()
	}
	var cmds []tea.Cmd
	if requestID != "" {
		message := ""
		if err != nil {
			message = err.Error()
		}
		cmds = append(cmds, core.CmdHandler(view.CommentResultMsg{Owner: ws.anchor, RequestID: requestID, Err: message}))
	}
	if err != nil {
		cmds = append(cmds, notification.ErrorCmd(failure+": "+err.Error()))
	}
	return tea.Batch(cmds...)
}

// gitReviewCandidates lists open sessions the user can already reach; the
// workspace's own session (with its attached agent) comes first.
func (m *appModel) gitReviewCandidates(owner string) []gitReviewCandidate {
	if m.supervisor == nil {
		return nil
	}
	var out []gitReviewCandidate
	add := func(id string) {
		runner := m.supervisor.GetRunner(id)
		if runner == nil || runner.App == nil || runner.App.Session() == nil || runner.App.IsReadOnly() {
			return
		}
		for _, c := range out {
			if c.sessionID == id {
				return
			}
		}
		sess := runner.App.Session()
		c := gitReviewCandidate{label: "◆ " + fallbackString(sess.TitleSnapshot(), "untitled session"), sessionID: id, app: runner.App}
		var detail []string
		if attached := runner.App.AttachedSubagent(); attached != nil {
			detail = append(detail, "attached "+string(attached.NodeID))
		}
		if id == owner {
			detail = append(detail, "this workspace")
		}
		if page := m.chatPages[id]; page != nil && page.IsWorking() {
			detail = append(detail, "working")
		}
		c.detail = strings.Join(detail, " · ")
		c.route, _ = m.supervisor.RouteGeneration(id)
		out = append(out, c)
	}
	add(owner)
	tabs, _ := m.supervisor.GetTabs()
	for _, tab := range tabs {
		add(tab.SessionID)
	}
	return out
}

func (m *appModel) applyGitReviewTarget(choice *gitReviewTargetChoice, id string) tea.Cmd {
	ws := m.gitReviews[choice.owner]
	c, found := choice.options[id]
	if ws == nil || !found {
		return nil
	}
	snap := ws.draft.Snapshot()
	if snap.Revision != choice.revision {
		return notification.InfoCmd("The review draft changed; preview again")
	}
	capture := m.gitReviewCapture
	if capture == nil {
		capture = (*app.App).CaptureReviewSubmitTarget
	}
	submit := capture(c.app)
	if submit.SessionID == "" || submit.Handle == nil {
		return notification.ErrorCmd("That session can't receive input right now")
	}
	token, err := newGitReviewToken()
	if err != nil {
		return notification.ErrorCmd("Review preview failed: " + err.Error())
	}
	target := reviewdraft.Target{
		Kind: reviewdraft.TargetSession, SessionID: submit.SessionID, AgentName: submit.Handle.AgentName(), WorkingDir: c.app.Session().WorkingDir,
		NodeID: string(submit.AttachedNodeID), ParentSessionID: submit.ParentSessionID, RouteID: c.sessionID, RouteGeneration: c.route,
	}
	if submit.AttachedNodeID != "" {
		target.Kind = reviewdraft.TargetAttached
	}
	ws.setPreview(&gitReviewPreview{lease: c.lease, token: token, revision: snap.Revision, label: c.label, target: target, app: c.app, submit: submit, payload: reviewdraft.SerializeWithGeneral(snap.Comments, snap.General), general: snap.General, activeOnly: choice.activeOnly})
	ws.pushDraft()
	m.viewCacheValid = false
	return nil
}

func newGitReviewToken() (string, error) {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// sendGitReviewDraft submits only the previewed recipient and revision. A
// changed draft, replaced session or new route never redirects the send.
func (m *appModel) sendGitReviewDraft(msg view.SendDraftMsg) tea.Cmd {
	ws := m.gitReviews[msg.Owner]
	if ws == nil || ws.preview == nil || ws.preview.token != msg.Token {
		return nil
	}
	p := ws.preview
	if p.activeOnly && p.target.Kind != reviewdraft.TargetNewSession && p.target.RouteID != m.paneFocus() {
		return m.failGitReviewTarget(ws, errors.New("the active session changed; review kept for the original target"))
	}
	if ws.draft.Snapshot().Status == reviewdraft.StatusSending {
		return nil
	}
	_, retry := ws.draft.Attempt()
	if !retry && ws.draft.Snapshot().Revision != p.revision {
		ws.setPreview(nil)
		ws.pushDraft()
		return notification.InfoCmd("The review draft changed; preview again")
	}
	switch {
	case p.target.Kind == reviewdraft.TargetNewSession:
		if m.reviewSessionFactory == nil {
			return m.failGitReviewTarget(ws, errors.New("new sessions aren't available here; draft kept"))
		}
	case p.lease != nil:
		if p.lease.released || p.app.Session() == nil || p.app.Session().ID != p.target.SessionID {
			return m.failGitReviewTarget(ws, errors.New("the chosen session is no longer reachable; draft kept"))
		}
	default:
		runner := m.supervisor.GetRunner(p.target.RouteID)
		route, _ := m.supervisor.RouteGeneration(p.target.RouteID)
		if runner == nil || runner.App != p.app || route != p.target.RouteGeneration {
			return m.failGitReviewTarget(ws, errors.New("the chosen session was closed or replaced; draft kept"))
		}
	}
	attempt, err := ws.draft.BeginSend(p.target)
	if err != nil {
		return notification.ErrorCmd("Review not sent: " + err.Error())
	}
	if attempt.Target.SessionID != p.target.SessionID {
		// Never let a new preview redirect an unresolved attempt.
		return m.failGitReviewTarget(ws, errors.New("an earlier send to another recipient is unresolved"))
	}
	sent := *p
	sent.payload = attempt.Payload
	ws.sent = &sent
	ws.pushDraft()
	var borrowedInput *reviewCommentInput
	if input := m.reviewCommentInput; input != nil && input.general && input.owner == msg.Owner {
		borrowedInput = input
	}
	if attempt.Target.Kind == reviewdraft.TargetNewSession {
		return m.submitGitReviewNewSession(msg.Owner, attempt, borrowedInput)
	}
	submit, a, owner := m.gitReviewSubmitter(), p.app, msg.Owner
	ctx := m.ctx()
	return func() tea.Msg {
		receipt, err := submit(ctx, a, p.submit, attempt.RequestID, attempt.Payload)
		return gitReviewSendResult{owner: owner, requestID: attempt.RequestID, commentInput: borrowedInput, receipt: receipt, err: err}
	}
}

func (m *appModel) failGitReviewTarget(ws *gitReviewWorkspace, err error) tea.Cmd {
	if attempt, ok := ws.draft.Attempt(); ok {
		_ = ws.draft.Fail(attempt.RequestID, err)
	}
	ws.pushDraft()
	return notification.ErrorCmd("Review not sent: " + err.Error())
}

func (m *appModel) applyGitReviewSend(result gitReviewSendResult) tea.Cmd {
	ws := m.gitReviews[result.owner]
	if ws == nil {
		return nil
	}
	if result.err != nil {
		_ = ws.draft.Fail(result.requestID, result.err)
		ws.pushDraft()
		return notification.ErrorCmd("Review not sent; draft kept: " + result.err.Error())
	}
	sub := result.receipt.Submission
	if err := ws.draft.MarkAccepted(reviewdraft.Receipt{RequestID: result.requestID, SessionID: sub.SessionID, TurnID: sub.TurnID, Accepted: true, Durable: result.receipt.Durable}); err != nil {
		ws.pushDraft()
		return notification.ErrorCmd("Review send could not be confirmed: " + err.Error())
	}
	label := ""
	general := ""
	if ws.preview != nil {
		label = strings.TrimSpace(ws.preview.label)
		general = ws.preview.general
	}
	clearBorrowed := false
	if result.commentInput != nil && m.reviewCommentInput == result.commentInput &&
		m.reviewCommentInput.general && m.reviewCommentInput.owner == result.owner {
		if input, ok := m.editor.(reviewCommentEditor); ok {
			clearBorrowed = strings.TrimSpace(input.CommentInputValue()) == strings.TrimSpace(general)
			if clearBorrowed {
				input.SetCommentInputValue("")
			}
		}
	}
	ws.releaseLeases()
	if clearBorrowed && m.reviewCommentInput != nil {
		m.reviewCommentInput.general = false
		m.finishReviewComment(false)
	}
	ws.setPreview(nil)
	ws.sent = nil
	ws.pushDraft()
	if result.newTitle != "" || strings.HasPrefix(label, "new ") {
		// Offer an explicit Open; the new session never takes focus by itself.
		text := "Review sent to new session “" + fallbackString(result.newTitle, sub.SessionID) + "”"
		if m.messageBar != nil {
			open := core.CmdHandler(messages.LoadSessionMsg{SessionID: sub.SessionID})
			_, cmd, _ := m.messageBar.SetNotice(messagebar.Message{Text: text, Severity: messagebar.Info, Category: messagebar.Hint, Owner: gitReviewHintOwner, Actions: []messagebar.Action{{Label: "Open", Command: open}}}, time.Time{})
			return tea.Batch(cmd, notification.SuccessCmd(text))
		}
		return notification.SuccessCmd(text + " · open it from /sessions")
	}
	return notification.SuccessCmd("Review sent to " + fallbackString(label, "the session"))
}

func (m *appModel) captureReviewGeneral(owner string) tea.Cmd {
	ws := m.gitReviews[owner]
	if ws == nil {
		return nil
	}
	if m.reviewCommentInput != nil && m.reviewCommentInput.general && m.reviewCommentInput.owner == owner {
		if err := ws.draft.SetGeneral(m.editor.(reviewCommentEditor).CommentInputValue()); err != nil {
			return notification.ErrorCmd("Review comment not saved: " + err.Error())
		}
	}
	ws.pushDraft()
	return nil
}

func (m *appModel) sendReviewFromComposer() tea.Cmd {
	ws := m.activeReview()
	if ws == nil || (ws.mode == view.ModeChanges && !ws.inspecting) {
		return nil
	}
	if m.reviewCommentInput != nil && m.reviewCommentInput.general && m.reviewCommentInput.owner != ws.anchor {
		return notification.ErrorCmd("The active review changed; draft kept")
	}
	if ws.draft.Snapshot().Status == reviewdraft.StatusSending {
		return nil
	}
	if cmd := m.captureReviewGeneral(ws.anchor); cmd != nil {
		return cmd
	}
	snap := ws.draft.Snapshot()
	if len(snap.Comments) == 0 && snap.General == "" {
		return notification.InfoCmd("Add a review comment or select lines before sending")
	}
	if ws.draft.Snapshot().Attempt != nil {
		if ws.sent == nil {
			return notification.ErrorCmd("The previous review send is unresolved; draft kept")
		}
		copy := *ws.sent
		copy.payload = ws.draft.Snapshot().Attempt.Payload
		ws.setPreview(&copy)
	} else {
		if cmd := m.previewCurrentAgent(ws, snap.Revision); cmd != nil {
			return cmd
		}
	}
	if ws.preview == nil {
		return notification.ErrorCmd("The current session can't receive this review")
	}
	return m.sendGitReviewDraft(view.SendDraftMsg{Owner: ws.anchor, Revision: ws.preview.revision, Token: ws.preview.token})
}
