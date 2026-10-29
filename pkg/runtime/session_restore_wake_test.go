package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/api"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
)

func TestRestoredAuthorizedSendWakesChildAndReportsToRoot(t *testing.T) {
	store := coordinationSQLite(t)
	workerEntered := make(chan struct{})
	var firstWorkerCalls atomic.Int32
	firstWorker := coordinationReply("unused")
	firstWorker.call = func(ctx context.Context, _ []chat.Message) (chat.MessageStream, error) {
		if firstWorkerCalls.Add(1) == 1 {
			close(workerEntered)
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return newStreamBuilder().AddContent("unexpected resumed run").AddStopWithUsage(1, 1).Build(), nil
	}
	firstRuntime, firstOwner := restoredSendRuntime(t, store, coordinationReply("unused"), firstWorker)
	root, err := firstOwner.Runtime().CreateSession(t.Context(), session.New(session.WithID("root"), session.WithToolsApproved(true)), SessionBinding{AgentName: "root"})
	require.NoError(t, err)
	child := coordinationCreate(t, firstOwner.Runtime(), "child", root.ID())
	interrupted, err := child.Submit(t.Context(), TurnInput{Content: "interrupted before restore", RequestID: "interrupted"})
	require.NoError(t, err)
	coordinationWait(t, workerEntered)
	backlog, err := child.Submit(t.Context(), TurnInput{Content: "accepted backlog", RequestID: "backlog"})
	require.NoError(t, err)
	node, ok := firstRuntime.subagents.nodeForSession(child.ID())
	require.True(t, ok)
	require.NoError(t, firstOwner.Shutdown(t.Context()))

	backlogID, err := sessionInputID(child.ID(), "backlog")
	require.NoError(t, err)
	var restoredDriver atomic.Pointer[sessionDriver]
	var workerTurnIDs []string
	var workerMu sync.Mutex
	releaseWorker := make(chan struct{})
	workerEnteredAgain := make(chan struct{})
	var workerCalls atomic.Int32
	worker := coordinationReply("unused")
	worker.call = func(ctx context.Context, _ []chat.Message) (chat.MessageStream, error) {
		driver := restoredDriver.Load()
		if driver == nil {
			return nil, fmt.Errorf("restored child driver was not published before provider call")
		}
		turnID := driver.Status().TurnID
		call := workerCalls.Add(1)
		workerMu.Lock()
		workerTurnIDs = append(workerTurnIDs, turnID)
		workerMu.Unlock()
		if call == 1 {
			close(workerEnteredAgain)
			select {
			case <-releaseWorker:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		content := map[string]string{backlogID: "backlog child completed", "authorized-send": "authorized child completed"}[turnID]
		if content == "" {
			return nil, fmt.Errorf("unexpected child turn %q", turnID)
		}
		return newStreamBuilder().AddContent(content).AddStopWithUsage(1, 1).Build(), nil
	}
	var rootCalls atomic.Int32
	rootDriverRef := atomic.Pointer[sessionDriver]{}
	rootFollowup := make(chan string, 1)
	rootProvider := coordinationReply("unused")
	rootProvider.call = func(_ context.Context, messages []chat.Message) (chat.MessageStream, error) {
		call := rootCalls.Add(1)
		if call == 1 {
			return newStreamBuilder().AddToolCallName("send", subagent.ToolSendMessage).AddToolCallArguments("send", fmt.Sprintf(`{"to":%q,"message":"authorized followup","request_id":"authorized-send","delivery_mode":"new_turn"}`, node)).AddToolCallStopWithUsage(1, 1).Build(), nil
		}
		joined := make([]string, 0, len(messages))
		for _, message := range messages {
			joined = append(joined, message.Content)
		}
		if strings.Contains(strings.Join(joined, "\n"), "authorized child completed") {
			if driver := rootDriverRef.Load(); driver != nil {
				select {
				case rootFollowup <- driver.Status().TurnID:
				default:
				}
			}
			return newStreamBuilder().AddContent("root received child completion").AddStopWithUsage(1, 1).Build(), nil
		}
		return newStreamBuilder().AddContent("root continuation").AddStopWithUsage(1, 1).Build(), nil
	}
	_, owner := restoredSendRuntime(t, store, rootProvider, worker)
	rootHandle, rootSnapshot, err := owner.Runtime().(SessionLoader).LoadSession(t.Context(), root.ID())
	require.NoError(t, err)
	require.NoError(t, owner.Runtime().(TreeRestorer).RestoreSessionTree(t.Context(), rootSnapshot))
	childHandle, err := owner.Runtime().SessionByID(child.ID())
	require.NoError(t, err)
	childDriver := childHandle.(*sessionHandle).driver
	rootDriverRef.Store(rootHandle.(*sessionHandle).driver)
	require.True(t, childDriver.Status().Dormant)
	require.True(t, rootHandle.(*sessionHandle).driver.Status().Dormant)
	require.Equal(t, 1, childDriver.Status().Pending, "restore retains durable accepted backlog but does not execute it")
	require.EqualValues(t, 1, firstWorkerCalls.Load(), "real shutdown/reload did not replay active or queued input")
	restoredDriver.Store(childDriver)

	parentTurn := submitThroughSessionTransport(t, rootHandle, "message the restored child", "root-send-turn")
	coordinationAwait(t, rootHandle, parentTurn.TurnID)
	require.Eventually(t, func() bool { return rootCalls.Load() >= 2 }, 5*time.Second, time.Millisecond, "root completes the explicit tool turn through its ordinary provider continuation")
	require.Eventually(t, func() bool { return childDriver.Status().TurnID == backlogID }, 5*time.Second, time.Millisecond, "target-only send authorization starts the restored accepted FIFO")
	coordinationWait(t, workerEnteredAgain)
	close(releaseWorker)
	coordinationAwait(t, childHandle, backlogID)
	coordinationAwait(t, childHandle, "authorized-send")

	var reportTurnID string
	select {
	case reportTurnID = <-rootFollowup:
	case <-time.After(5 * time.Second):
		t.Fatalf("root did not invoke its provider with the distinct child completion report (provider calls=%d)", rootCalls.Load())
	}
	require.NotEmpty(t, reportTurnID)
	require.NotEqual(t, parentTurn.TurnID, reportTurnID, "the report follow-up is a separate root turn from the send tool call")
	coordinationAwait(t, rootHandle, reportTurnID)
	rootSnapshot, err = rootHandle.Snapshot(t.Context())
	require.NoError(t, err)
	require.Equal(t, string(TurnCompleted), rootSnapshot.TurnOutcome(reportTurnID), "the distinct report-triggered root turn reaches terminal settlement")
	assistantAck := false
	for _, item := range rootSnapshot.MessagesSnapshot() {
		if item.Message != nil && item.Message.Message.Role == chat.MessageRoleAssistant && strings.Contains(item.Message.Message.Content, "root received child completion") {
			assistantAck = true
		}
	}
	require.True(t, assistantAck, "root provider returns a completion response after observing the child report")
	workerMu.Lock()
	gotTurns := append([]string(nil), workerTurnIDs...)
	workerMu.Unlock()
	require.Equal(t, []string{backlogID, "authorized-send"}, gotTurns, "fresh admission resumes accepted durable FIFO without replaying the pre-restore active turn")
	require.Equal(t, 0, childDriver.Status().Pending)
	assert.NotContains(t, gotTurns, interrupted.TurnID)
	assert.Equal(t, backlogID, backlog.TurnID)
}

func TestRestoredAuthorizedSendWaitsForDescendantCompletion(t *testing.T) {
	store := coordinationSQLite(t)
	root := session.New(session.WithID("nested-root"), session.WithAgentName("root"), session.WithToolsApproved(true), session.WithAttributes(map[string]string{SessionAgentAttribute: "root"}))
	root.AgentName = "root"
	require.NoError(t, store.AddSession(t.Context(), root))
	child := session.New(session.WithID("nested-child"), session.WithParentID(root.ID), session.WithAgentName("worker"), session.WithAttributes(map[string]string{SessionAgentAttribute: "worker", SessionParentAgentAttribute: "root"}), session.WithAsyncSubagent(true), session.WithNonInteractive(true))
	child.AgentName = "worker"
	leaf := session.New(session.WithID("nested-leaf"), session.WithParentID(child.ID), session.WithAgentName("leaf"), session.WithAttributes(map[string]string{SessionAgentAttribute: "leaf", SessionParentAgentAttribute: "worker"}), session.WithAsyncSubagent(true), session.WithNonInteractive(true))
	leaf.AgentName = "leaf"
	childRecord := session.ChildRecord{RootSessionID: root.ID, ParentSessionID: root.ID, Node: subagent.Node{ID: "nested-child-node", SessionID: child.ID, Parent: subagent.SessionRootID(root.ID), Agent: "worker", State: subagent.NodeIdle}}
	leafRecord := session.ChildRecord{RootSessionID: root.ID, ParentSessionID: child.ID, Node: subagent.Node{ID: "nested-leaf-node", SessionID: leaf.ID, Parent: childRecord.Node.ID, Agent: "leaf", State: subagent.NodeIdle}}
	coord := store.(session.CoordinationStore)
	require.NoError(t, coord.AdmitChild(t.Context(), session.ChildAdmission{Child: child, Record: childRecord}))
	require.NoError(t, coord.AdmitChild(t.Context(), session.ChildAdmission{Child: leaf, Record: leafRecord}))

	leafEntered, releaseLeaf := make(chan struct{}), make(chan struct{})
	leafProvider := coordinationReply("unused")
	leafProvider.call = func(ctx context.Context, _ []chat.Message) (chat.MessageStream, error) {
		close(leafEntered)
		select {
		case <-releaseLeaf:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return newStreamBuilder().AddContent("leaf result after parent work").AddStopWithUsage(1, 1).Build(), nil
	}
	var workerCalls atomic.Int32
	workerSawLeaf := make(chan struct{}, 1)
	workerProvider := coordinationReply("unused")
	workerProvider.call = func(_ context.Context, messages []chat.Message) (chat.MessageStream, error) {
		call := workerCalls.Add(1)
		var joined []string
		for _, message := range messages {
			joined = append(joined, message.Content)
		}
		if strings.Contains(strings.Join(joined, "\n"), "leaf result after parent work") {
			select {
			case workerSawLeaf <- struct{}{}:
			default:
			}
			return newStreamBuilder().AddContent("nested child followed leaf completion").AddStopWithUsage(1, 1).Build(), nil
		}
		return newStreamBuilder().AddContent(fmt.Sprintf("worker turn %d finished while descendant is active", call)).AddStopWithUsage(1, 1).Build(), nil
	}
	var rootCalls atomic.Int32
	rootDriverRef := atomic.Pointer[sessionDriver]{}
	rootFollowup := make(chan string, 1)
	rootProvider := coordinationReply("unused")
	rootProvider.call = func(_ context.Context, messages []chat.Message) (chat.MessageStream, error) {
		if rootCalls.Add(1) == 1 {
			return newStreamBuilder().AddToolCallName("send", subagent.ToolSendMessage).AddToolCallArguments("send", `{"to":"nested-child-node","message":"run nested task","request_id":"nested-send","delivery_mode":"new_turn"}`).AddToolCallStopWithUsage(1, 1).Build(), nil
		}
		var joined []string
		for _, message := range messages {
			joined = append(joined, message.Content)
		}
		if strings.Contains(strings.Join(joined, "\n"), "nested child followed leaf completion") {
			if driver := rootDriverRef.Load(); driver != nil {
				select {
				case rootFollowup <- driver.Status().TurnID:
				default:
				}
			}
			return newStreamBuilder().AddContent("root received nested completion").AddStopWithUsage(1, 1).Build(), nil
		}
		return newStreamBuilder().AddContent("root continuation").AddStopWithUsage(1, 1).Build(), nil
	}
	tm := team.New(team.WithAgents(
		agent.New("root", "root", agent.WithModel(rootProvider), agent.WithAsyncSubagents(latest.SubagentRef{Agent: "worker"})),
		agent.New("worker", "worker", agent.WithModel(workerProvider), agent.WithAsyncSubagents(latest.SubagentRef{Agent: "leaf"})),
		agent.New("leaf", "leaf", agent.WithModel(leafProvider)),
	), team.WithAgentConfigs(map[string]latest.AgentConfig{"root": {Name: "root", SubagentAccess: &latest.SubagentAccessConfig{SendMessage: latest.SubagentAccessChildren}}}))
	_, owner := newRestoreSendRuntime(t, store, tm)
	rootHandle, rootSnapshot, err := owner.Runtime().(SessionLoader).LoadSession(t.Context(), root.ID)
	require.NoError(t, err)
	require.NoError(t, owner.Runtime().(TreeRestorer).RestoreSessionTree(t.Context(), rootSnapshot))
	childHandle, err := owner.Runtime().SessionByID(child.ID)
	require.NoError(t, err)
	leafHandle, err := owner.Runtime().SessionByID(leaf.ID)
	require.NoError(t, err)
	rootDriverRef.Store(rootHandle.(*sessionHandle).driver)
	require.True(t, childHandle.(*sessionHandle).driver.Status().Dormant)
	require.True(t, leafHandle.(*sessionHandle).driver.Status().Dormant)

	leafTurn, err := leafHandle.Submit(t.Context(), TurnInput{Content: "start leaf work", RequestID: "leaf-work"})
	require.NoError(t, err)
	coordinationWait(t, leafEntered)
	parentTurn := submitThroughSessionTransport(t, rootHandle, "dispatch to child", "nested-root-turn")
	coordinationAwait(t, rootHandle, parentTurn.TurnID)
	require.Eventually(t, func() bool { return rootCalls.Load() >= 2 }, 5*time.Second, time.Millisecond)
	coordinationAwait(t, childHandle, "nested-send")
	reports, err := coord.PendingReports(t.Context(), root.ID)
	require.NoError(t, err)
	require.Empty(t, reports, "the child cannot publish completion while its descendant is active")
	select {
	case <-workerSawLeaf:
		t.Fatal("child consumed the descendant result before the descendant completed")
	default:
	}

	close(releaseLeaf)
	coordinationAwait(t, leafHandle, leafTurn.TurnID)
	var reportTurnID string
	select {
	case reportTurnID = <-rootFollowup:
	case <-time.After(5 * time.Second):
		t.Fatalf("root did not run after descendant report (root calls=%d child calls=%d)", rootCalls.Load(), workerCalls.Load())
	}
	require.NotEqual(t, parentTurn.TurnID, reportTurnID)
	coordinationAwait(t, rootHandle, reportTurnID)
	rootAfter, err := rootHandle.Snapshot(t.Context())
	require.NoError(t, err)
	require.Equal(t, string(TurnCompleted), rootAfter.TurnOutcome(reportTurnID))
}

type causalReportAdmissionStore struct {
	*session.SQLiteSessionStore
	fail     atomic.Bool
	attempts atomic.Int32
}

func (s *causalReportAdmissionStore) AcceptReport(ctx context.Context, parentID, reportID string, message *session.Message) (session.ReportAcceptance, error) {
	if message != nil {
		s.attempts.Add(1)
		if s.fail.Load() {
			return session.ReportAcceptance{}, errors.New("synthetic causal report admission failure")
		}
	}
	return s.SQLiteSessionStore.AcceptReport(ctx, parentID, reportID, message)
}

func TestCausalReportWakeHonorsParentPauseWithoutClearingIt(t *testing.T) {
	store := coordinationSQLite(t)
	root := session.New(session.WithID("causal-pause-root"), session.WithAgentName("root"), session.WithAttributes(map[string]string{SessionAgentAttribute: "root"}))
	root.AgentName = "root"
	child := peerSession(root, "causal-pause-child")
	require.NoError(t, store.AddSession(t.Context(), root))
	record := session.ChildRecord{RootSessionID: root.ID, ParentSessionID: root.ID, Node: subagent.Node{ID: "causal-pause-node", SessionID: child.ID, Parent: subagent.SessionRootID(root.ID), Agent: "worker", State: subagent.NodeIdle}}
	require.NoError(t, store.(session.CoordinationStore).AdmitChild(t.Context(), session.ChildAdmission{Child: child, Record: record}))
	var rootCalls atomic.Int32
	rootProvider := coordinationReply("unused")
	rootProvider.call = func(context.Context, []chat.Message) (chat.MessageStream, error) {
		rootCalls.Add(1)
		return newStreamBuilder().AddContent("parent processed fresh report").AddStopWithUsage(1, 1).Build(), nil
	}
	tm := team.New(team.WithAgents(agent.New("root", "root", agent.WithModel(rootProvider), agent.WithAsyncSubagents(latest.SubagentRef{Agent: "worker"})), agent.New("worker", "worker", agent.WithModel(coordinationReply("child completed")))), team.WithAgentConfigs(map[string]latest.AgentConfig{"root": {Name: "root", SubagentAccess: &latest.SubagentAccessConfig{SendMessage: latest.SubagentAccessChildren}}}))
	rt, owner := newRestoreSendRuntime(t, store, tm)
	_, loadedRoot, err := owner.Runtime().(SessionLoader).LoadSession(t.Context(), root.ID)
	require.NoError(t, err)
	require.NoError(t, owner.Runtime().(TreeRestorer).RestoreSessionTree(t.Context(), loadedRoot))
	parent, err := owner.Runtime().SessionByID(root.ID)
	require.NoError(t, err)
	paused, err := parent.TogglePause(t.Context())
	require.NoError(t, err)
	require.True(t, paused)
	report := session.ChildReport{ID: childReportID(child.ID, "fresh-turn"), ParentSessionID: root.ID, ChildSessionID: child.ID, TurnID: "fresh-turn", Revision: 2, Content: "fresh completion", ReportOutcome: session.ReportOutcomeFinished}
	// Seed an exact durable old/new report state and a current committed child identity.
	historical := session.ChildReport{ID: "historical-pause-report", ParentSessionID: root.ID, ChildSessionID: child.ID, TurnID: "old-turn", Content: "old", ReportOutcome: session.ReportOutcomeFinished}
	current := record
	current.Revision, current.LastTurnID = 2, "fresh-turn"
	require.NoError(t, store.(session.CoordinationStore).CommitChild(t.Context(), session.ChildCommit{ExpectedRevision: 1, Record: current, Reports: []session.ChildReport{report, historical}}))
	rtChild := rt.subagents.children[record.Node.ID]
	require.NotNil(t, rtChild)
	rt.subagents.mu.Lock()
	rtChild.durable = current
	rt.subagents.mu.Unlock()
	parentDriver := parent.(*sessionHandle).driver
	require.True(t, rt.subagents.authorizeCompletionReport(t.Context(), root.ID, root.ID, child.ID, report))
	require.Eventually(t, func() bool {
		status := parentDriver.Status()
		return !status.Dormant && status.Paused && status.PauseArmed
	}, 5*time.Second, time.Millisecond)
	require.Zero(t, rootCalls.Load(), "fresh report admission cannot bypass the explicit pause")
	pending, err := store.(session.CoordinationStore).PendingReports(t.Context(), root.ID)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	require.Equal(t, historical.ID, pending[0].ID, "the causal gate excludes unrelated report while paused")
	paused, err = parent.TogglePause(t.Context())
	require.NoError(t, err)
	require.False(t, paused)
	require.Eventually(t, func() bool { return rootCalls.Load() == 1 }, 5*time.Second, time.Millisecond)
	require.True(t, parentDriver.Status().PauseArmed == false)
}

func TestCausalReportAdmissionFailureKeepsDormantParentUntilCommit(t *testing.T) {
	base := coordinationSQLite(t).(*session.SQLiteSessionStore)
	store := &causalReportAdmissionStore{SQLiteSessionStore: base}
	rootProvider := coordinationReply("root acknowledged child report")
	workerProvider := coordinationReply("fresh child completion")
	rootProvider.call = func(_ context.Context, messages []chat.Message) (chat.MessageStream, error) {
		for _, message := range messages {
			if strings.Contains(message.Content, "fresh child completion") {
				return newStreamBuilder().AddContent("root acknowledged child report").AddStopWithUsage(1, 1).Build(), nil
			}
		}
		return newStreamBuilder().AddContent("root idle").AddStopWithUsage(1, 1).Build(), nil
	}
	firstRuntime, firstOwner := restoredSendRuntime(t, store, coordinationReply("unused"), coordinationReply("unused"))
	root, err := firstOwner.Runtime().CreateSession(t.Context(), session.New(session.WithID("causal-root")), SessionBinding{AgentName: "root"})
	require.NoError(t, err)
	child := coordinationCreate(t, firstOwner.Runtime(), "causal-child", root.ID())
	require.NoError(t, firstOwner.Shutdown(t.Context()))
	_, attached := firstRuntime.subagents.nodeForSession(child.ID())
	require.True(t, attached)

	store.fail.Store(true)
	_, owner := restoredSendRuntime(t, store, rootProvider, workerProvider)
	rootHandle, snapshot, err := owner.Runtime().(SessionLoader).LoadSession(t.Context(), root.ID())
	require.NoError(t, err)
	require.NoError(t, owner.Runtime().(TreeRestorer).RestoreSessionTree(t.Context(), snapshot))
	childHandle, err := owner.Runtime().SessionByID(child.ID())
	require.NoError(t, err)
	require.True(t, rootHandle.(*sessionHandle).driver.Status().Dormant)
	fresh, err := childHandle.Submit(t.Context(), TurnInput{Content: "execute fresh authorized turn", RequestID: "fresh-turn"})
	require.NoError(t, err)
	coordinationAwait(t, childHandle, fresh.TurnID)
	require.Eventually(t, func() bool { return store.attempts.Load() > 0 }, 5*time.Second, time.Millisecond, "causal path attempted durable report admission")
	require.True(t, rootHandle.(*sessionHandle).driver.Status().Dormant, "failed durable report admission cannot clear dormancy")
	unaccepted, err := store.PendingReports(t.Context(), root.ID())
	require.NoError(t, err)
	require.Len(t, unaccepted, 1, "failed AcceptReport leaves the outbox report durable and unacknowledged")
	rootStored, err := store.GetSession(t.Context(), root.ID())
	require.NoError(t, err)
	for _, item := range rootStored.MessagesSnapshot() {
		assert.False(t, item.Message != nil && item.Message.InputOrigin == session.InputOriginRuntime && strings.Contains(item.Message.Message.Content, "fresh child completion"), "failed acknowledgement cannot project an input")
	}

	store.fail.Store(false)
	var reportTurn string
	require.Eventually(t, func() bool {
		stored, err := owner.Runtime().SessionByID(root.ID())
		if err != nil {
			return false
		}
		snapshot, err := stored.Snapshot(t.Context())
		if err != nil {
			return false
		}
		for _, item := range snapshot.MessagesSnapshot() {
			if item.Message != nil && item.Message.ReportOutcome != "" {
				reportTurn = item.Message.TurnID
				return true
			}
		}
		return false
	}, 5*time.Second, time.Millisecond, "same process-local causal report identity retries after storage recovers")
	require.NotEmpty(t, reportTurn)
	coordinationAwait(t, rootHandle, reportTurn)
	rootAfter, err := rootHandle.Snapshot(t.Context())
	require.NoError(t, err)
	require.Equal(t, string(TurnCompleted), rootAfter.TurnOutcome(reportTurn))
	pending, err := store.PendingReports(t.Context(), root.ID())
	require.NoError(t, err)
	require.Empty(t, pending)
}

func TestCausalReportWakePermissionDoesNotSurviveColdRestore(t *testing.T) {
	base := coordinationSQLite(t).(*session.SQLiteSessionStore)
	store := &causalReportAdmissionStore{SQLiteSessionStore: base}
	store.fail.Store(true)
	var rootCalls atomic.Int32
	rootProvider := coordinationReply("unused")
	rootProvider.call = func(context.Context, []chat.Message) (chat.MessageStream, error) {
		rootCalls.Add(1)
		return newStreamBuilder().AddContent("root should remain dormant").AddStopWithUsage(1, 1).Build(), nil
	}
	childProvider := coordinationReply("child result")
	_, owner1 := restoredSendRuntime(t, store, coordinationReply("unused"), coordinationReply("unused"))
	root, err := owner1.Runtime().CreateSession(t.Context(), session.New(session.WithID("cold-causal-root")), SessionBinding{AgentName: "root"})
	require.NoError(t, err)
	child := coordinationCreate(t, owner1.Runtime(), "cold-causal-child", root.ID())
	require.NoError(t, owner1.Shutdown(t.Context()))

	_, owner2 := restoredSendRuntime(t, store, rootProvider, childProvider)
	rootHandle, snapshot, err := owner2.Runtime().(SessionLoader).LoadSession(t.Context(), root.ID())
	require.NoError(t, err)
	require.NoError(t, owner2.Runtime().(TreeRestorer).RestoreSessionTree(t.Context(), snapshot))
	childHandle, err := owner2.Runtime().SessionByID(child.ID())
	require.NoError(t, err)
	fresh, err := childHandle.Submit(t.Context(), TurnInput{Content: "fresh completion before cold restart", RequestID: "cold-fresh"})
	require.NoError(t, err)
	coordinationAwait(t, childHandle, fresh.TurnID)
	require.Eventually(t, func() bool { return store.attempts.Load() > 0 }, 5*time.Second, time.Millisecond)
	require.True(t, rootHandle.(*sessionHandle).driver.Status().Dormant)
	pendingBefore, err := store.PendingReports(t.Context(), root.ID())
	require.NoError(t, err)
	require.Len(t, pendingBefore, 1)
	require.NoError(t, owner2.Shutdown(t.Context()))

	store.fail.Store(false)
	_, owner3 := restoredSendRuntime(t, store, rootProvider, childProvider)
	reopened, rootSnapshot, err := owner3.Runtime().(SessionLoader).LoadSession(t.Context(), root.ID())
	require.NoError(t, err)
	require.NoError(t, owner3.Runtime().(TreeRestorer).RestoreSessionTree(t.Context(), rootSnapshot))
	reopenedDriver := reopened.(*sessionHandle).driver
	require.True(t, reopenedDriver.Status().Dormant, "a new process has no causal report capability")
	owner3Runtime := owner3.Runtime().(*localSessionRuntimeView).runtime
	owner3Runtime.sessionDrivers.signalWork() // test a normal maintenance scan, not a manual resume
	require.Never(t, func() bool {
		return !reopenedDriver.Status().Dormant || reopenedDriver.Status().Pending != 0 || rootCalls.Load() != 0
	}, 200*time.Millisecond, 5*time.Millisecond, "historical outbox is not itself wake authority after restart")
	pendingAfter, err := store.PendingReports(t.Context(), root.ID())
	require.NoError(t, err)
	require.Len(t, pendingAfter, 1)
	require.Equal(t, pendingBefore[0].ID, pendingAfter[0].ID)
}

func TestCompletedCausalReportReceiptDoesNotReleaseCausalFilter(t *testing.T) {
	store := coordinationSQLite(t)
	root := session.New(session.WithID("duplicate-report-root"), session.WithAgentName("root"), session.WithAttributes(map[string]string{SessionAgentAttribute: "root"}))
	root.AgentName = "root"
	child := peerSession(root, "duplicate-report-child")
	require.NoError(t, store.AddSession(t.Context(), root))
	record := session.ChildRecord{RootSessionID: root.ID, ParentSessionID: root.ID, Node: subagent.Node{ID: "duplicate-report-node", SessionID: child.ID, Parent: subagent.SessionRootID(root.ID), Agent: "worker", State: subagent.NodeIdle}}
	require.NoError(t, store.(session.CoordinationStore).AdmitChild(t.Context(), session.ChildAdmission{Child: child, Record: record}))
	record.Revision = 1
	report := session.ChildReport{ID: "duplicate-causal-report", ParentSessionID: root.ID, ChildSessionID: child.ID, TurnID: "completed-turn", Content: "already accepted report", ReportOutcome: session.ReportOutcomeFinished}
	require.NoError(t, store.(session.CoordinationStore).CommitChild(t.Context(), session.ChildCommit{ExpectedRevision: 1, Record: record, Reports: []session.ChildReport{report}}))
	acceptedMessage := session.UserMessage(report.Content)
	acceptedMessage.TurnID, acceptedMessage.Pending, acceptedMessage.Accepted = "report:"+report.ID, true, true
	acceptedMessage.InputOrigin, acceptedMessage.InputMode = session.InputOriginRuntime, "steer"
	_, err := store.(session.CoordinationStore).AcceptReport(t.Context(), root.ID, report.ID, acceptedMessage)
	require.NoError(t, err)
	require.NoError(t, store.PromotePendingUserMessage(t.Context(), root.ID, "report:"+report.ID))
	storedRoot, err := store.GetSession(t.Context(), root.ID)
	require.NoError(t, err)
	storedRoot.SetTurnOutcome("report:"+report.ID, string(TurnCompleted))
	require.NoError(t, store.UpdateSession(t.Context(), storedRoot))
	var providerCalls atomic.Int32
	provider := coordinationReply("unused")
	provider.call = func(context.Context, []chat.Message) (chat.MessageStream, error) {
		providerCalls.Add(1)
		return newStreamBuilder().AddContent("explicit resume completed").AddStopWithUsage(1, 1).Build(), nil
	}
	_, owner := restoredSendRuntime(t, store, provider, coordinationReply("unused"))
	rootHandle, snapshot, err := owner.Runtime().(SessionLoader).LoadSession(t.Context(), root.ID)
	require.NoError(t, err)
	require.NoError(t, owner.Runtime().(TreeRestorer).RestoreSessionTree(t.Context(), snapshot))
	d := rootHandle.(*sessionHandle).driver
	require.True(t, d.Status().Dormant)
	report.Revision = 2
	require.NoError(t, d.ownerCall(t.Context(), func() error {
		d.causalReportOnly = true
		d.causalReports = map[string]session.ChildReport{report.ID: report}
		d.causalReportOrder = []string{report.ID}
		return nil
	}))
	require.NoError(t, d.acceptAuthorizedReport(t.Context(), report))
	require.True(t, d.Status().Dormant, "a completed report receipt does not wake dormant history")
	assertCausalReportOnly(t, d, true, "a completed report receipt does not release the causal filter")
	require.EqualValues(t, 0, providerCalls.Load())
	pending, err := store.(session.CoordinationStore).PendingReports(t.Context(), root.ID)
	require.NoError(t, err)
	require.Empty(t, pending, "the accepted report receipt was already acknowledged")
}

func TestStoppedDormantParentRejectsCausalReportPermission(t *testing.T) {
	store := coordinationSQLite(t)
	root := session.New(session.WithID("stopped-causal-root"), session.WithAgentName("root"), session.WithAttributes(map[string]string{SessionAgentAttribute: "root"}))
	root.AgentName = "root"
	child := peerSession(root, "stopped-causal-child")
	require.NoError(t, store.AddSession(t.Context(), root))
	record := session.ChildRecord{RootSessionID: root.ID, ParentSessionID: root.ID, Node: subagent.Node{ID: "stopped-causal-node", SessionID: child.ID, Parent: subagent.SessionRootID(root.ID), Agent: "worker", State: subagent.NodeIdle}}
	require.NoError(t, store.(session.CoordinationStore).AdmitChild(t.Context(), session.ChildAdmission{Child: child, Record: record}))
	rt, owner := newRestoreSendRuntime(t, store, team.New(team.WithAgents(agent.New("root", "root", agent.WithModel(coordinationReply("never")), agent.WithAsyncSubagents(latest.SubagentRef{Agent: "worker"})), agent.New("worker", "worker", agent.WithModel(coordinationReply("never"))))))
	rootHandle, rootSnapshot, err := owner.Runtime().(SessionLoader).LoadSession(t.Context(), root.ID)
	require.NoError(t, err)
	require.NoError(t, owner.Runtime().(TreeRestorer).RestoreSessionTree(t.Context(), rootSnapshot))
	require.NoError(t, rootHandle.(SessionTreeController).StopSubtree(t.Context()))
	parent := rootHandle.(*sessionHandle).driver
	causal := session.ChildReport{ID: "not-authorized-after-stop", ParentSessionID: root.ID, ChildSessionID: child.ID, TurnID: "child-turn", Revision: 2, Content: "no wake", ReportOutcome: session.ReportOutcomeFinished}
	require.False(t, rt.subagents.authorizeCompletionReport(t.Context(), root.ID, root.ID, child.ID, causal))
	require.True(t, parent.Status().Stopped)
	require.Empty(t, parent.pending)
}

func TestAuthorizedSendPreservesExplicitPause(t *testing.T) {
	store := coordinationSQLite(t)
	root := session.New(session.WithID("pause-root"), session.WithAgentName("root"), session.WithAttributes(map[string]string{SessionAgentAttribute: "root"}))
	root.AgentName = "root"
	require.NoError(t, store.AddSession(t.Context(), root))
	child := session.New(session.WithID("pause-child"), session.WithParentID(root.ID), session.WithAgentName("worker"), session.WithAttributes(map[string]string{SessionAgentAttribute: "worker", SessionParentAgentAttribute: "root"}))
	child.AgentName = "worker"
	child.AsyncSubagent, child.NonInteractive = true, true
	require.NoError(t, store.AddSession(t.Context(), child))
	record := session.ChildRecord{RootSessionID: root.ID, ParentSessionID: root.ID, Node: subagent.Node{ID: "pause-node", SessionID: child.ID, Parent: subagent.SessionRootID(root.ID), Agent: "worker", State: subagent.NodeIdle}}
	require.NoError(t, store.(session.CoordinationStore).AdmitChild(t.Context(), session.ChildAdmission{Child: child, Record: record}))
	entered := make(chan struct{})
	worker := coordinationReply("unused")
	worker.call = func(ctx context.Context, _ []chat.Message) (chat.MessageStream, error) {
		close(entered)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	tm := team.New(team.WithAgents(agent.New("root", "root", agent.WithModel(coordinationReply("unused")), agent.WithAsyncSubagents(latest.SubagentRef{Agent: "worker"})), agent.New("worker", "worker", agent.WithModel(worker))), team.WithAgentConfigs(map[string]latest.AgentConfig{"root": {Name: "root", SubagentAccess: &latest.SubagentAccessConfig{SendMessage: latest.SubagentAccessChildren}}}))
	rt, owner := newRestoreSendRuntime(t, store, tm)
	_, loadedRoot, err := owner.Runtime().(SessionLoader).LoadSession(t.Context(), root.ID)
	require.NoError(t, err)
	require.NoError(t, owner.Runtime().(TreeRestorer).RestoreSessionTree(t.Context(), loadedRoot))
	handle, err := owner.Runtime().SessionByID(child.ID)
	require.NoError(t, err)
	paused, err := handle.TogglePause(t.Context())
	require.NoError(t, err)
	require.True(t, paused)
	result, err := rt.handleSendMessage(t.Context(), loadedRoot, tools.ToolCall{Function: tools.FunctionCall{Name: subagent.ToolSendMessage, Arguments: `{"to":"pause-node","message":"paused task","request_id":"paused-send","delivery_mode":"new_turn"}`}}, nil, nil)
	require.NoError(t, err)
	require.False(t, result.IsError, result.Output)
	status, err := handle.Status(t.Context())
	require.NoError(t, err)
	require.True(t, status.PauseArmed)
	require.Eventually(t, func() bool { return handle.(*sessionHandle).driver.Status().Paused }, 5*time.Second, time.Millisecond)
	assert.Never(t, func() bool {
		select {
		case <-entered:
			return true
		default:
			return false
		}
	}, 50*time.Millisecond, time.Millisecond, "authorized send cannot override explicit pause")
}

func TestRestoredSendFailsClosedOnUnacceptedLegacyPending(t *testing.T) {
	store := coordinationSQLite(t)
	rootSession := session.New(session.WithID("legacy-root"), session.WithAgentName("root"), session.WithAttributes(map[string]string{SessionAgentAttribute: "root"}))
	rootSession.AgentName = "root"
	require.NoError(t, store.AddSession(t.Context(), rootSession))
	child := session.New(session.WithID("legacy-child"), session.WithParentID(rootSession.ID), session.WithAgentName("worker"), session.WithAttributes(map[string]string{SessionAgentAttribute: "worker", SessionParentAgentAttribute: "root"}))
	legacy := session.UserMessage("unaccepted legacy row")
	legacy.TurnID, legacy.Pending, legacy.Accepted = "unaccepted", true, false
	legacy.InputOrigin, legacy.InputMode = session.InputOriginAgent, "turn"
	child.AddMessage(legacy)
	record := session.ChildRecord{RootSessionID: rootSession.ID, ParentSessionID: rootSession.ID, Node: subagent.Node{ID: "legacy-node", SessionID: child.ID, Parent: subagent.SessionRootID(rootSession.ID), Agent: "worker", State: subagent.NodeIdle}}
	require.NoError(t, store.(session.CoordinationStore).AdmitChild(t.Context(), session.ChildAdmission{Child: child, Record: record}))
	tm := team.New(team.WithAgents(agent.New("root", "root", agent.WithModel(coordinationReply("unused")), agent.WithAsyncSubagents(latest.SubagentRef{Agent: "worker"})), agent.New("worker", "worker", agent.WithModel(coordinationReply("unused")))), team.WithAgentConfigs(map[string]latest.AgentConfig{"root": {Name: "root", SubagentAccess: &latest.SubagentAccessConfig{SendMessage: latest.SubagentAccessChildren}}}))
	rt, owner := newRestoreSendRuntime(t, store, tm)
	_, root, err := owner.Runtime().(SessionLoader).LoadSession(t.Context(), rootSession.ID)
	require.NoError(t, err)
	require.NoError(t, owner.Runtime().(TreeRestorer).RestoreSessionTree(t.Context(), root))
	handle, err := owner.Runtime().SessionByID(child.ID)
	require.NoError(t, err)
	result, err := rt.handleSendMessage(t.Context(), root, tools.ToolCall{Function: tools.FunctionCall{Name: subagent.ToolSendMessage, Arguments: `{"to":"legacy-node","message":"new request","request_id":"new-request"}`}}, nil, tools.NopRuntime{})
	require.NoError(t, err)
	require.True(t, result.IsError, "cannot start an inbox with legacy unaccepted pending rows")
	status, err := handle.Status(t.Context())
	require.NoError(t, err)
	require.True(t, status.Dormant)
	require.Equal(t, 0, status.Pending)
	snapshot, err := handle.Snapshot(t.Context())
	require.NoError(t, err)
	items := snapshot.MessagesSnapshot()
	require.Len(t, items, 1)
	require.True(t, items[0].Message.Pending)
	require.False(t, items[0].Message.Accepted, "fail-closed admission leaves legacy row untouched")
	_, resident := rt.sessionDrivers.Lookup(child.ID)
	assert.True(t, resident)
}

func TestDormantCompletedSendRetryDoesNotAuthorize(t *testing.T) {
	store := coordinationSQLite(t)
	rootSession := session.New(session.WithID("retry-root"), session.WithAgentName("root"), session.WithAttributes(map[string]string{SessionAgentAttribute: "root"}))
	rootSession.AgentName = "root"
	require.NoError(t, store.AddSession(t.Context(), rootSession))
	child := session.New(session.WithID("retry-child"), session.WithParentID(rootSession.ID), session.WithAgentName("worker"), session.WithAttributes(map[string]string{SessionAgentAttribute: "worker", SessionParentAgentAttribute: "root"}))
	message := session.UserMessage("completed send")
	message.TurnID, message.Pending, message.Accepted = "completed-send", false, true
	message.InputOrigin, message.InputMode, message.SenderID, message.SenderName = session.InputOriginAgent, "turn", rootSession.ID, "root"
	child.AddMessage(message)
	child.SetTurnOutcome("completed-send", string(TurnCompleted))
	record := session.ChildRecord{RootSessionID: rootSession.ID, ParentSessionID: rootSession.ID, Node: subagent.Node{ID: "retry-node", SessionID: child.ID, Parent: subagent.SessionRootID(rootSession.ID), Agent: "worker", State: subagent.NodeIdle}}
	require.NoError(t, store.(session.CoordinationStore).AdmitChild(t.Context(), session.ChildAdmission{Child: child, Record: record}))
	tm := team.New(team.WithAgents(agent.New("root", "root", agent.WithModel(coordinationReply("unused")), agent.WithAsyncSubagents(latest.SubagentRef{Agent: "worker"})), agent.New("worker", "worker", agent.WithModel(coordinationReply("unused")))), team.WithAgentConfigs(map[string]latest.AgentConfig{"root": {Name: "root", SubagentAccess: &latest.SubagentAccessConfig{SendMessage: latest.SubagentAccessChildren}}}))
	rt, owner := newRestoreSendRuntime(t, store, tm)
	_, root, err := owner.Runtime().(SessionLoader).LoadSession(t.Context(), rootSession.ID)
	require.NoError(t, err)
	require.NoError(t, owner.Runtime().(TreeRestorer).RestoreSessionTree(t.Context(), root))
	handle, err := owner.Runtime().SessionByID(child.ID)
	require.NoError(t, err)
	result, err := rt.handleSendMessage(t.Context(), root, tools.ToolCall{Function: tools.FunctionCall{Name: subagent.ToolSendMessage, Arguments: `{"to":"retry-node","message":"completed send","request_id":"completed-send","delivery_mode":"new_turn"}`}}, nil, tools.NopRuntime{})
	require.NoError(t, err)
	require.False(t, result.IsError, "an exact completed retry preserves inbox receipt success")
	var receipt subagent.DeliveryReceipt
	require.NoError(t, json.Unmarshal([]byte(result.Output), &receipt))
	require.True(t, receipt.Idempotent)
	status, err := handle.Status(t.Context())
	require.NoError(t, err)
	require.True(t, status.Dormant, "settled exact retry cannot re-arm the restored target")
	require.Equal(t, 0, status.Pending)
	snapshot, err := handle.Snapshot(t.Context())
	require.NoError(t, err)
	var matches int
	for _, item := range snapshot.MessagesSnapshot() {
		if item.Message != nil && item.Message.TurnID == "completed-send" {
			matches++
		}
	}
	require.Equal(t, 1, matches, "settled receipt probe cannot append again")
}

func TestDormantAuthorizedPendingSendRetryMatchesAndWakes(t *testing.T) {
	store := coordinationSQLite(t)
	root := session.New(session.WithID("retry-root-send"), session.WithAgentName("root"), session.WithAttributes(map[string]string{SessionAgentAttribute: "root"}))
	root.AgentName = "root"
	require.NoError(t, store.AddSession(t.Context(), root))
	child := session.New(session.WithID("retry-child-send"), session.WithParentID(root.ID), session.WithAgentName("worker"), session.WithAttributes(map[string]string{SessionAgentAttribute: "worker", SessionParentAgentAttribute: "root"}))
	child.AgentName = "worker"
	pending := session.UserMessage("durable peer request")
	pending.TurnID, pending.Pending, pending.Accepted = "repeat-send", true, true
	pending.InputOrigin, pending.InputMode, pending.SenderID, pending.SenderName = session.InputOriginAgent, "turn", root.ID, "root"
	child.AddMessage(pending)
	record := session.ChildRecord{RootSessionID: root.ID, ParentSessionID: root.ID, Node: subagent.Node{ID: "retry-node-send", SessionID: child.ID, Parent: subagent.SessionRootID(root.ID), Agent: "worker", State: subagent.NodeIdle}}
	require.NoError(t, store.(session.CoordinationStore).AdmitChild(t.Context(), session.ChildAdmission{Child: child, Record: record}))
	var calls atomic.Int32
	worker := coordinationReply("unused")
	worker.call = func(context.Context, []chat.Message) (chat.MessageStream, error) {
		calls.Add(1)
		return newStreamBuilder().AddContent("one response").AddStopWithUsage(1, 1).Build(), nil
	}
	tm := team.New(team.WithAgents(agent.New("root", "root", agent.WithModel(coordinationReply("unused")), agent.WithAsyncSubagents(latest.SubagentRef{Agent: "worker"})), agent.New("worker", "worker", agent.WithModel(worker))), team.WithAgentConfigs(map[string]latest.AgentConfig{"root": {Name: "root", SubagentAccess: &latest.SubagentAccessConfig{SendMessage: latest.SubagentAccessChildren}}}))
	rt, owner := newRestoreSendRuntime(t, store, tm)
	_, loadedRoot, err := owner.Runtime().(SessionLoader).LoadSession(t.Context(), root.ID)
	require.NoError(t, err)
	require.NoError(t, owner.Runtime().(TreeRestorer).RestoreSessionTree(t.Context(), loadedRoot))
	handle, err := owner.Runtime().SessionByID(child.ID)
	require.NoError(t, err)
	require.True(t, handle.(*sessionHandle).driver.Status().Dormant)
	send := tools.ToolCall{Function: tools.FunctionCall{Name: subagent.ToolSendMessage, Arguments: `{"to":"retry-node-send","message":"durable peer request","request_id":"repeat-send","delivery_mode":"new_turn"}`}}
	changedWhileDormant, err := rt.handleSendMessage(t.Context(), loadedRoot, tools.ToolCall{Function: tools.FunctionCall{Name: subagent.ToolSendMessage, Arguments: `{"to":"retry-node-send","message":"changed body","request_id":"repeat-send","delivery_mode":"new_turn"}`}}, nil, nil)
	require.NoError(t, err)
	require.True(t, changedWhileDormant.IsError)
	status, err := handle.Status(t.Context())
	require.NoError(t, err)
	require.True(t, status.Dormant)
	snapshot, err := handle.Snapshot(t.Context())
	require.NoError(t, err)
	require.Len(t, snapshot.MessagesSnapshot(), 1, "mismatched dormant retry adds no transcript input")
	result, err := rt.handleSendMessage(t.Context(), loadedRoot, send, nil, nil)
	require.NoError(t, err)
	require.False(t, result.IsError, result.Output)
	require.Eventually(t, func() bool { return calls.Load() == 1 && handle.(*sessionHandle).driver.Status().Pending == 0 }, 5*time.Second, time.Millisecond)
	retry, err := rt.handleSendMessage(t.Context(), loadedRoot, send, nil, nil)
	require.NoError(t, err)
	require.False(t, retry.IsError, retry.Output)
	var receipt subagent.DeliveryReceipt
	require.NoError(t, json.Unmarshal([]byte(retry.Output), &receipt))
	require.True(t, receipt.Idempotent)
	result, err = rt.handleSendMessage(t.Context(), loadedRoot, tools.ToolCall{Function: tools.FunctionCall{Name: subagent.ToolSendMessage, Arguments: `{"to":"retry-node-send","message":"changed body","request_id":"repeat-send","delivery_mode":"new_turn"}`}}, nil, nil)
	require.NoError(t, err)
	require.True(t, result.IsError, "changed payload under same request identity conflicts")
	snapshot, err = handle.Snapshot(t.Context())
	require.NoError(t, err)
	matches := 0
	for _, item := range snapshot.MessagesSnapshot() {
		if item.Message != nil && item.Message.TurnID == "repeat-send" {
			matches++
		}
	}
	require.Equal(t, 1, matches, "idempotent retry cannot duplicate the transcript row")
	require.EqualValues(t, 1, calls.Load())
}

func TestStoppedParentBlocksCausalReportWake(t *testing.T) {
	store := coordinationSQLite(t)
	root := session.New(session.WithID("stop-causal-root"), session.WithAgentName("root"), session.WithAttributes(map[string]string{SessionAgentAttribute: "root"}))
	root.AgentName = "root"
	child := peerSession(root, "stop-causal-child")
	require.NoError(t, store.AddSession(t.Context(), root))
	record := session.ChildRecord{RootSessionID: root.ID, ParentSessionID: root.ID, Node: subagent.Node{ID: "stop-causal-node", SessionID: child.ID, Parent: subagent.SessionRootID(root.ID), Agent: "worker", State: subagent.NodeIdle}}
	require.NoError(t, store.(session.CoordinationStore).AdmitChild(t.Context(), session.ChildAdmission{Child: child, Record: record}))
	record.Revision = 1
	record.LastTurnID = "finished"
	record.Result = "fresh result"
	require.NoError(t, store.(session.CoordinationStore).CommitChild(t.Context(), session.ChildCommit{ExpectedRevision: 1, Record: record}))
	tm := team.New(team.WithAgents(agent.New("root", "root", agent.WithModel(coordinationReply("unused")), agent.WithAsyncSubagents(latest.SubagentRef{Agent: "worker"})), agent.New("worker", "worker", agent.WithModel(coordinationReply("unused")))), team.WithAgentConfigs(map[string]latest.AgentConfig{"root": {Name: "root", SubagentAccess: &latest.SubagentAccessConfig{SendMessage: latest.SubagentAccessChildren}}}))
	rt, owner := newRestoreSendRuntime(t, store, tm)
	rootHandle, loadedRoot, err := owner.Runtime().(SessionLoader).LoadSession(t.Context(), root.ID)
	require.NoError(t, err)
	require.NoError(t, owner.Runtime().(TreeRestorer).RestoreSessionTree(t.Context(), loadedRoot))
	transition := rt.subagents.transition(root.ID)
	transition.Lock()
	var freshRecord session.ChildRecord
	records, err := store.(session.CoordinationStore).LoadChildren(t.Context(), root.ID)
	require.NoError(t, err)
	for _, candidate := range records {
		if candidate.Node.ID == record.Node.ID {
			freshRecord = candidate
			break
		}
	}
	transition.Unlock()
	require.Equal(t, uint64(2), freshRecord.Revision)
	report := session.ChildReport{ID: childReportID(child.ID, "finished"), ParentSessionID: root.ID, ChildSessionID: child.ID, TurnID: "finished", Revision: freshRecord.Revision, Content: "fresh", ReportOutcome: session.ReportOutcomeFinished}
	controller := rootHandle.(SessionTreeController)
	require.NoError(t, controller.StopSubtree(t.Context()))
	require.False(t, rt.subagents.authorizeCompletionReport(t.Context(), root.ID, root.ID, child.ID, report), "root stop fence blocks fresh causal report wake")
	status, err := rootHandle.Status(t.Context())
	require.NoError(t, err)
	require.True(t, status.Stopped)
	assert.Zero(t, status.Pending)
	require.NoError(t, rootHandle.(*sessionHandle).driver.ownerCall(t.Context(), func() error {
		assert.Empty(t, rootHandle.(*sessionHandle).driver.causalReports)
		return nil
	}))
}

func TestSessionStatusStoppedIsExplicitAndDormancyIndependent(t *testing.T) {
	d := newSessionDriver(newDriverTestRuntime(t), session.New(session.WithID("stopped-status")))
	require.NoError(t, d.ownerCall(t.Context(), func() error { d.viewDormant = true; return nil }))
	status := d.Status()
	require.True(t, status.Dormant)
	require.False(t, status.Stopped)
	require.NoError(t, d.ownerCall(t.Context(), func() error { d.stopped, d.explicitStopped = true, true; return nil }))
	status = d.Status()
	require.True(t, status.Dormant)
	require.True(t, status.Stopped)
}

func submitThroughSessionTransport(t *testing.T, local SessionHandle, content, requestID string) Submission {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != api.SessionAPIPath+"/"+local.ID()+"/messages" {
			http.NotFound(w, r)
			return
		}
		var input api.SessionInputRequest
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		assert.Equal(t, "submit", input.Mode)
		submission, err := local.Submit(r.Context(), TurnInput{Content: input.Content, RequestID: input.RequestID})
		if err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		_ = json.NewEncoder(w).Encode(api.SessionSubmission[SubmissionDisposition](submission))
	}))
	t.Cleanup(server.Close)
	client, err := NewClient(server.URL, WithHTTPClient(server.Client()))
	require.NoError(t, err)
	transport, err := NewSessionTransport(client)
	require.NoError(t, err)
	remote, err := transport.SessionByID(local.ID())
	require.NoError(t, err)
	submission, err := remote.Submit(t.Context(), TurnInput{Content: content, RequestID: requestID})
	require.NoError(t, err)
	return submission
}

func restoredSendRuntime(t *testing.T, store session.Store, rootProvider, workerProvider *coordinationProvider) (*LocalRuntime, SessionRuntimeSupervisor) {
	t.Helper()
	tm := team.New(team.WithAgents(
		agent.New("root", "prompt", agent.WithModel(rootProvider), agent.WithAsyncSubagents(latest.SubagentRef{Agent: "worker"})),
		agent.New("worker", "prompt", agent.WithModel(workerProvider)),
	), team.WithAgentConfigs(map[string]latest.AgentConfig{
		"root": {Name: "root", SubagentAccess: &latest.SubagentAccessConfig{SendMessage: latest.SubagentAccessChildren}},
	}))
	return newRestoreSendRuntime(t, store, tm)
}

func newRestoreSendRuntime(t *testing.T, store session.Store, tm *team.Team) (*LocalRuntime, SessionRuntimeSupervisor) {
	t.Helper()
	rt, err := NewLocalRuntime(t.Context(), tm, WithSessionStore(store), WithSessionCompaction(false), WithModelStore(mockModelStore{}))
	require.NoError(t, err)
	owner := NewSessionRuntimeSupervisor(rt)
	t.Cleanup(func() { require.NoError(t, owner.Shutdown(context.WithoutCancel(t.Context()))) })
	return rt, owner
}

func TestOptInPeerSendRearmsRestoredDormantTarget(t *testing.T) {
	store := coordinationSQLite(t)
	root := session.New(session.WithID("peer-root"), session.WithAgentName("root"), session.WithAttributes(map[string]string{SessionAgentAttribute: "root"}))
	root.AgentName = "root"
	rootBacklogID, err := sessionInputID(root.ID, "root-backlog")
	require.NoError(t, err)
	rootBacklog := session.UserMessage("parent durable backlog")
	rootBacklog.TurnID, rootBacklog.Pending, rootBacklog.Accepted = rootBacklogID, true, true
	rootBacklog.InputOrigin, rootBacklog.InputMode = session.InputOriginUser, "turn"
	root.AddMessage(rootBacklog)
	require.NoError(t, store.AddSession(t.Context(), root))
	sender := peerSession(root, "peer-sender")
	target := peerSession(root, "peer-target")
	historical := peerSession(root, "peer-historical")
	coord := store.(session.CoordinationStore)
	senderNode := subagent.Node{ID: "sender-node", SessionID: sender.ID, Parent: subagent.SessionRootID(root.ID), Agent: "worker", State: subagent.NodeIdle}
	targetNode := subagent.Node{ID: "target-node", SessionID: target.ID, Parent: subagent.SessionRootID(root.ID), Agent: "worker", State: subagent.NodeIdle}
	historicalNode := subagent.Node{ID: "historical-node", SessionID: historical.ID, Parent: subagent.SessionRootID(root.ID), Agent: "worker", State: subagent.NodeIdle}
	require.NoError(t, coord.AdmitChild(t.Context(), session.ChildAdmission{Child: sender, Record: session.ChildRecord{RootSessionID: root.ID, ParentSessionID: root.ID, Node: senderNode}}))
	require.NoError(t, coord.AdmitChild(t.Context(), session.ChildAdmission{Child: target, Record: session.ChildRecord{RootSessionID: root.ID, ParentSessionID: root.ID, Node: targetNode}}))
	require.NoError(t, coord.AdmitChild(t.Context(), session.ChildAdmission{Child: historical, Record: session.ChildRecord{RootSessionID: root.ID, ParentSessionID: root.ID, Node: historicalNode}}))
	records, err := coord.LoadChildren(t.Context(), root.ID)
	require.NoError(t, err)
	var historicalRecord session.ChildRecord
	for _, record := range records {
		if record.Node.ID == historicalNode.ID {
			historicalRecord = record
		}
	}
	require.NotZero(t, historicalRecord.Revision)
	oldReport := session.ChildReport{ID: "peer-historical-report", ParentSessionID: root.ID, ChildSessionID: historical.ID, TurnID: "old-turn", Content: "historical unaccepted report", ReportOutcome: session.ReportOutcomeFinished}
	historicalRecord.LastTurnID = oldReport.TurnID
	require.NoError(t, coord.CommitChild(t.Context(), session.ChildCommit{ExpectedRevision: historicalRecord.Revision, Record: historicalRecord, Reports: []session.ChildReport{oldReport}}))

	targetEntered := make(chan struct{})
	targetRelease := make(chan struct{})
	worker := coordinationReply("unused")
	worker.call = func(ctx context.Context, _ []chat.Message) (chat.MessageStream, error) {
		close(targetEntered)
		select {
		case <-targetRelease:
			return newStreamBuilder().AddContent("peer target completed").AddStopWithUsage(1, 1).Build(), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	var rootCalls atomic.Int32
	rootTurnSeen := make(chan string, 1)
	rootBacklogRun := make(chan string, 1)
	rootDriverRef := atomic.Pointer[sessionDriver]{}
	var rootSawFresh, rootSawOld atomic.Bool
	rootProvider := coordinationReply("unused")
	rootProvider.call = func(_ context.Context, messages []chat.Message) (chat.MessageStream, error) {
		rootCalls.Add(1)
		var joined []string
		for _, message := range messages {
			joined = append(joined, message.Content)
		}
		transcript := strings.Join(joined, "\n")
		if strings.Contains(transcript, "parent durable backlog") {
			if driver := rootDriverRef.Load(); driver != nil {
				select {
				case rootBacklogRun <- driver.Status().TurnID:
				default:
				}
			}
		}
		if strings.Contains(transcript, "peer target completed") {
			rootSawFresh.Store(true)
			rootSawOld.Store(strings.Contains(transcript, "historical unaccepted report"))
			if driver := rootDriverRef.Load(); driver != nil {
				select {
				case rootTurnSeen <- driver.Status().TurnID:
				default:
				}
			}
		}
		return newStreamBuilder().AddContent("root acknowledged authorized peer completion").AddStopWithUsage(1, 1).Build(), nil
	}
	tm := team.New(team.WithAgents(
		agent.New("root", "root", agent.WithModel(rootProvider), agent.WithAsyncSubagents(latest.SubagentRef{Agent: "worker"})),
		agent.New("worker", "worker", agent.WithModel(worker)),
	), team.WithAgentConfigs(map[string]latest.AgentConfig{
		"root":   {Name: "root"},
		"worker": {Name: "worker", SubagentAccess: &latest.SubagentAccessConfig{SendMessage: latest.SubagentAccessTree}},
	}))
	rt, owner := newRestoreSendRuntime(t, store, tm)
	_, loadedRoot, err := owner.Runtime().(SessionLoader).LoadSession(t.Context(), root.ID)
	require.NoError(t, err)
	require.NoError(t, owner.Runtime().(TreeRestorer).RestoreSessionTree(t.Context(), loadedRoot))
	targetHandle, err := owner.Runtime().SessionByID(target.ID)
	require.NoError(t, err)
	targetDriver := targetHandle.(*sessionHandle).driver
	rootHandle, err := owner.Runtime().SessionByID(root.ID)
	require.NoError(t, err)
	rootDriver := rootHandle.(*sessionHandle).driver
	rootDriverRef.Store(rootDriver)
	require.True(t, targetDriver.Status().Dormant)
	require.True(t, rootDriver.Status().Dormant)
	require.Equal(t, 1, rootDriver.Status().Pending, "restore retains accepted parent FIFO")
	oldBefore, err := coord.PendingReports(t.Context(), root.ID)
	require.NoError(t, err)
	require.Len(t, oldBefore, 1, "cold restore does not authorize historical reports")
	require.Equal(t, oldReport.ID, oldBefore[0].ID)
	senderHandle, err := owner.Runtime().SessionByID(sender.ID)
	require.NoError(t, err)
	senderSnapshot, err := senderHandle.Snapshot(t.Context())
	require.NoError(t, err)
	result, err := rt.handleSendMessage(t.Context(), senderSnapshot, tools.ToolCall{ID: "peer-send-call", Function: tools.FunctionCall{Name: subagent.ToolSendMessage, Arguments: `{"to":"target-node","message":"peer task","request_id":"peer-send","delivery_mode":"new_turn"}`}}, nil, tools.NopRuntime{})
	require.NoError(t, err)
	require.False(t, result.IsError, result.Output)
	coordinationWait(t, targetEntered)
	require.True(t, rootDriver.Status().Dormant, "peer send does not wake the untouched parent while the child turn is still running")
	close(targetRelease)
	coordinationAwait(t, targetHandle, "peer-send")
	select {
	case firstID := <-rootBacklogRun:
		require.Equal(t, rootBacklogID, firstID, "causal report must not skip accepted parent FIFO")
	case <-time.After(5 * time.Second):
		t.Fatal("accepted parent FIFO did not run after targeted causal authorization")
	}
	var reportTurnID string
	select {
	case reportTurnID = <-rootTurnSeen:
	case <-time.After(5 * time.Second):
		t.Fatalf("dormant root did not run for the fresh authorized report (calls=%d)", rootCalls.Load())
	}
	coordinationAwait(t, rootHandle, reportTurnID)
	rootSnapshot, err := rootHandle.Snapshot(t.Context())
	require.NoError(t, err)
	require.Equal(t, string(TurnCompleted), rootSnapshot.TurnOutcome(rootBacklogID), "accepted parent FIFO runs first")
	assert.True(t, rootSawFresh.Load())
	assert.False(t, rootSawOld.Load(), "causal-only root did not consume historical outbox report")
	pendingReports, err := coord.PendingReports(t.Context(), root.ID)
	require.NoError(t, err)
	require.Len(t, pendingReports, 1)
	require.Equal(t, oldReport.ID, pendingReports[0].ID)
	assertCausalReportOnly(t, rootDriver, true, "causal report keeps a filtering fence")
	freshResume, err := rootHandle.Submit(t.Context(), TurnInput{Content: "explicitly resume historical outbox", RequestID: "root-explicit-resume"})
	require.NoError(t, err)
	coordinationAwait(t, rootHandle, freshResume.TurnID)
	require.Eventually(t, func() bool {
		pending, err := coord.PendingReports(t.Context(), root.ID)
		return err == nil && len(pending) == 0
	}, 5*time.Second, time.Millisecond, "a fresh explicit root submit releases causal-only filtering")
	assertCausalReportOnly(t, rootDriver, false, "fresh explicit authorization releases causal-only filtering")
}

func peerSession(root *session.Session, id string) *session.Session {
	child := session.New(session.WithID(id), session.WithParentID(root.ID), session.WithAgentName("worker"), session.WithAttributes(map[string]string{SessionAgentAttribute: "worker", SessionParentAgentAttribute: "root"}))
	child.AgentName = "worker"
	return child
}

func TestSessionStatusStoppedWireIsAdditiveAndRemote(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want bool
	}{
		{name: "older peer", body: `{"metadata":{"session_id":"status"},"status":{"session_id":"status","agent_name":"worker","state":"settled","pending":0}}`, want: false},
		{name: "stopped peer", body: `{"metadata":{"session_id":"status"},"status":{"session_id":"status","agent_name":"worker","state":"settled","pending":0,"stopped":true}}`, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, http.MethodGet, r.Method)
				assert.Equal(t, api.SessionAPIPath+"/status/status", r.URL.Path)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			client, err := NewClient(server.URL, WithHTTPClient(server.Client()))
			require.NoError(t, err)
			transport, err := NewSessionTransport(client)
			require.NoError(t, err)
			handle, err := transport.SessionByID("status")
			require.NoError(t, err)
			status, err := handle.Status(t.Context())
			require.NoError(t, err)
			assert.Equal(t, tc.want, status.Stopped)
		})
	}
	encoded, err := json.Marshal(SessionStatus{SessionID: "s", Dormant: true})
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), `"stopped":true`, "view dormancy is never encoded as explicit stop")
	encoded, err = json.Marshal(SessionStatus{SessionID: "s", Stopped: true})
	require.NoError(t, err)
	assert.Contains(t, string(encoded), `"stopped":true`)
	var event DormancyChangedEvent
	require.NoError(t, json.Unmarshal([]byte(`{"type":"session_dormancy_changed","session_id":"s","dormant":false}`), &event))
	assert.False(t, event.Stopped, "old dormant event payload omits explicit stop")
	require.NoError(t, json.Unmarshal([]byte(`{"type":"session_dormancy_changed","session_id":"s","dormant":true,"stopped":true}`), &event))
	assert.True(t, event.Stopped)
}

func assertCausalReportOnly(t *testing.T, driver *sessionDriver, want bool, message string) {
	t.Helper()
	var got bool
	require.NoError(t, driver.ownerCall(t.Context(), func() error { got = driver.causalReportOnly; return nil }))
	assert.Equal(t, want, got, message)
}
