package runtime

import (
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
)

// releaseRegressionFixture builds a valid root+child tree with IDs scoped to the
// test, since the session service registry is process-wide.
func releaseRegressionFixture(t *testing.T) (SessionRuntimeSupervisor, string) {
	t.Helper()
	prefix := strings.NewReplacer("/", "-", " ", "-").Replace(t.Name())
	rootID, childID, nodeID := prefix+"-root", prefix+"-child", prefix+"-node"
	store := session.NewInMemorySessionStore()
	root := session.New(session.WithID(rootID), session.WithAttributes(map[string]string{SessionAgentAttribute: "root"}))
	child := session.New(session.WithID(childID), session.WithParentID(rootID), session.WithAgentName("worker"),
		session.WithAttributes(map[string]string{SessionAgentAttribute: "worker", SessionParentAgentAttribute: "root"}))
	child.AddMessage(session.UserMessage("original message"))
	require.NoError(t, store.AddSession(t.Context(), root))
	require.NoError(t, store.AddSession(t.Context(), child))
	rt, owner := coordinationRuntime(t, store, coordinationReply("root"), coordinationReply("child"))
	rootNode := subagent.SessionRootID(rootID)
	tree := subagent.Snapshot{Version: subagent.SnapshotVersion, Root: rootNode, Nodes: []subagent.NodeSnapshot{{Node: subagent.Node{ID: rootNode, Agent: "root"}, Children: []subagent.NodeSnapshot{{Node: subagent.Node{ID: subagent.NodeID(nodeID), SessionID: childID, Parent: rootNode, Agent: "worker", State: subagent.NodeIdle}}}}}}
	require.NoError(t, rt.subagentStore.SaveTree(t.Context(), rootID, tree))
	return owner, childID
}

// After a successful commit the prepared view keeps only what Info and repeat
// Commit read; publication-only graphs are released and results stay identical.
func TestPreparedViewReleasesPublicationGraphsAfterCommit(t *testing.T) {
	owner, childID := releaseRegressionFixture(t)
	prepared, err := owner.Runtime().(SessionViewPreparer).PrepareSessionView(t.Context(), childID)
	require.NoError(t, err)
	p := prepared.(*preparedSessionView)
	before := prepared.Info()

	first, err := prepared.Commit(t.Context())
	require.NoError(t, err)
	assert.Nil(t, p.root, "publication-only root graph must be released")
	assert.Nil(t, p.nodes, "publication-only node graphs must be released")
	assert.Nil(t, p.records)
	assert.Nil(t, p.existing)
	assert.Equal(t, before, prepared.Info(), "Info keeps serving prepared metadata after commit")

	again, err := prepared.Commit(t.Context())
	require.NoError(t, err)
	assert.Same(t, first.SessionHandle, again.SessionHandle)
	assert.Equal(t, first.Info.SessionID, again.Info.SessionID)
	assert.Equal(t, first.Info.RootSessionID, again.Info.RootSessionID)

	prepared.Abort()
	assert.Equal(t, before, prepared.Info(), "abort after commit must not disturb prepared metadata")
}

// Terminal abort before commit releases the same graphs; Info still serves prepared metadata.
func TestPreparedViewAbortReleasesGraphsKeepsInfo(t *testing.T) {
	owner, childID := releaseRegressionFixture(t)
	prepared, err := owner.Runtime().(SessionViewPreparer).PrepareSessionView(t.Context(), childID)
	require.NoError(t, err)
	p := prepared.(*preparedSessionView)
	before := prepared.Info()

	prepared.Abort()
	assert.Nil(t, p.root, "aborted view must release its root graph")
	assert.Nil(t, p.nodes)
	assert.Nil(t, p.existing)
	assert.Equal(t, before, prepared.Info(), "Info keeps serving prepared metadata after abort")
	_, err = prepared.Commit(t.Context())
	require.Error(t, err, "commit after abort must not publish")
}

// Stored committed copies may share one private snapshot, but every public Info
// or Commit result must be independent, including under concurrent use.
func TestPreparedViewCommittedResultsAreIndependentUnderConcurrentUse(t *testing.T) {
	owner, childID := releaseRegressionFixture(t)
	prepared, err := owner.Runtime().(SessionViewPreparer).PrepareSessionView(t.Context(), childID)
	require.NoError(t, err)
	first, err := prepared.Commit(t.Context())
	require.NoError(t, err)
	require.NotNil(t, first.Info.Attach)
	assert.NotSame(t, first.Info.Session, first.Info.Attach.Session)
	baselineTitle := first.Info.Session.TitleSnapshot()
	baselineAttachTitle := first.Info.Attach.Session.TitleSnapshot()

	var wg sync.WaitGroup
	var errs [8]error
	for i := range errs {
		wg.Go(func() {
			result, err := prepared.Commit(t.Context())
			if err != nil {
				errs[i] = err
				return
			}
			result.Info.Session.SetTitle("mutated-session")
			result.Info.Attach.Session.SetAttribute("mutated", "yes")
			result.Info.Attach.Name = "mutated-name"
			info := prepared.Info()
			info.Session.SetTitle("mutated-info")
		})
	}
	wg.Wait()
	for _, err := range errs {
		require.NoError(t, err)
	}

	again, err := prepared.Commit(t.Context())
	require.NoError(t, err)
	assert.Equal(t, baselineTitle, again.Info.Session.TitleSnapshot(), "mutating one public result must not reach another")
	assert.Equal(t, baselineAttachTitle, again.Info.Attach.Session.TitleSnapshot())
	_, leaked := again.Info.Attach.Session.AttributesSnapshot()["mutated"]
	assert.False(t, leaked, "attach mutations must not reach the cached committed result")
	assert.NotEqual(t, "mutated-name", again.Info.Attach.Name)
	assert.NotSame(t, again.Info.Session, again.Info.Attach.Session)
}
