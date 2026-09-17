package sidebar

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/spinner"
	"github.com/docker/docker-agent/pkg/tui/service"
)

// newSubagentTestModel returns a sidebar whose subagent spinner is a fake, so
// tests stay off the process-global animation coordinator (which parallel
// tests would otherwise race on).
func newSubagentTestModel(t *testing.T) *model {
	t.Helper()
	m := New(animation.NewRuntime(), t.Context(), &service.SessionState{}).(*model)
	m.subagentSpinner = &fakeSpinner{m.subagentSpinner}
	return m
}

func subagentSnapshot() subagent.Snapshot {
	base := time.Now().Add(-10 * time.Minute)
	return subagent.Snapshot{
		Root: "root:sess",
		Nodes: []subagent.NodeSnapshot{{
			Node: subagent.Node{ID: "root:sess", Agent: "root", State: subagent.NodeRunning},
			Children: []subagent.NodeSnapshot{
				{Node: subagent.Node{ID: "a1b2c", Agent: "coder", Parent: "root:sess", State: subagent.NodeRunning, CreatedAt: base}},
				{Node: subagent.Node{ID: "d4e5f", Agent: "reviewer", Parent: "root:sess", State: subagent.NodeCompleted, CreatedAt: base.Add(time.Minute)}},
			},
		}},
	}
}

func TestSameSubagentNodesIncludesDisplayNameAndCreatedAt(t *testing.T) {
	base := time.Now()
	left := []subagent.NodeSnapshot{{Node: subagent.Node{ID: "node", Agent: "agent", Name: "left", CreatedAt: base}}}
	right := []subagent.NodeSnapshot{{Node: subagent.Node{ID: "node", Agent: "agent", Name: "right", CreatedAt: base}}}
	assert.False(t, sameSubagentNodes(left, right))

	right[0].Node.Name = "left"
	right[0].Node.CreatedAt = base.Add(time.Second)
	assert.False(t, sameSubagentNodes(left, right))
	right[0].Node.CreatedAt = base
	assert.True(t, sameSubagentNodes(left, right))
	right[0].Node.NeedsAttention = true
	assert.False(t, sameSubagentNodes(left, right), "attention-only snapshots are authoritative changes")
	right[0].Node.NeedsAttention = false
	right[0].Node.SessionID = "different-session"
	assert.False(t, sameSubagentNodes(left, right), "session ownership cannot retain stale identity attribution")
}

func TestSubagentsInfo_RendersRunningSubagents(t *testing.T) {
	t.Parallel()

	m := newSubagentTestModel(t)
	m.SetSubagentTree(subagentSnapshot())

	out := m.subagentsInfo(40)
	assert.NotContains(t, out, "Subagents")
	assert.Contains(t, out, "coder")
	assert.Contains(t, out, "reviewer")
	// The synthetic session root is not shown; its subagents are the top level.
	assert.NotContains(t, out, "root")
	// Right-aligned status column shows the node state.
	assert.Contains(t, out, "running")
	assert.Contains(t, out, "completed")
}

func TestSubagentsInfo_EmptyWhenNoSubagents(t *testing.T) {
	t.Parallel()

	m := newSubagentTestModel(t)
	assert.Empty(t, m.subagentsInfo(40))

	// A snapshot with only the synthetic root (no children) renders nothing.
	m.SetSubagentTree(subagent.Snapshot{Nodes: []subagent.NodeSnapshot{{
		Node: subagent.Node{ID: "root:sess", Agent: "root", State: subagent.NodeRunning},
	}}})
	assert.NotContains(t, m.subagentsInfo(40), "root")
	assert.Empty(t, m.subagentsInfo(40))
}

func TestSubagentsInfo_InRenderSections(t *testing.T) {
	t.Parallel()

	m := newSubagentTestModel(t)
	m.SetSize(40, 100)

	before := strings.Join(m.renderSections(35), "\n")
	assert.NotContains(t, before, "Subagents")

	m.SetSubagentTree(subagentSnapshot())
	after := strings.Join(m.renderSections(35), "\n")
	assert.NotContains(t, after, "Subagents")
	assert.Contains(t, after, "coder")
}

func TestSubagentsInfo_MostRecentFirstPerSubtree(t *testing.T) {
	t.Parallel()

	base := time.Now().Add(-time.Hour)
	snap := subagent.Snapshot{Nodes: []subagent.NodeSnapshot{{
		Node: subagent.Node{ID: "root:sess", Agent: "root", State: subagent.NodeRunning},
		Children: []subagent.NodeSnapshot{
			{
				Node: subagent.Node{ID: "old01", Agent: "older", State: subagent.NodeRunning, CreatedAt: base},
				Children: []subagent.NodeSnapshot{
					{Node: subagent.Node{ID: "gc001", Agent: "firstgrandchild", State: subagent.NodeRunning, CreatedAt: base.Add(time.Minute)}},
					{Node: subagent.Node{ID: "gc002", Agent: "lastgrandchild", State: subagent.NodeRunning, CreatedAt: base.Add(2 * time.Minute)}},
				},
			},
			{Node: subagent.Node{ID: "new01", Agent: "newer", State: subagent.NodeRunning, CreatedAt: base.Add(30 * time.Minute)}},
		},
	}}}

	m := newSubagentTestModel(t)
	m.SetSubagentTree(snap)

	out := m.subagentsInfo(60)
	// Siblings are most-recently-spawned first at every level, and children
	// stay nested under their parent (tree structure preserved).
	newer := strings.Index(out, "newer")
	older := strings.Index(out, "older")
	last := strings.Index(out, "lastgrandchild")
	first := strings.Index(out, "firstgrandchild")
	require.True(t, newer >= 0 && older >= 0 && last >= 0 && first >= 0)
	assert.Less(t, newer, older, "most recent sibling first")
	assert.Less(t, older, last, "children nested under their parent")
	assert.Less(t, last, first, "grandchildren ordered most recent first too")
}

func TestSubagentsInfo_HoverShowsTimeAgoAndID(t *testing.T) {
	t.Parallel()

	m := newSubagentTestModel(t)
	m.SetSubagentTree(subagentSnapshot())

	m.hoveredSubagent = "a1b2c"
	m.branchSpans = map[subagent.NodeID]branchSpans{"a1b2c": {idValue: 1, idTarget: 1}}
	out := ansi.Strip(m.subagentsInfo(40))
	assert.Contains(t, out, "(a1b2c)", "hovered row shows node id next to the agent name")
	assert.Contains(t, out, "10m ago", "hovered row shows spawn time instead of state")
	assert.NotContains(t, out, "running", "current root is excluded and hovered child shows time instead")
	assert.Contains(t, out, "reviewer ✓", "non-hovered rows show the glyph after the name")
	assert.NotContains(t, out, "completed ✓", "glyph no longer lives in the status column")
	assert.NotContains(t, out, "reviewer (d4e5f)", "non-hovered rows do not show ids")
	assert.Contains(t, out, "completed", "non-hovered rows keep their state")
}

func TestSubagentHoverZonesAndClear(t *testing.T) {
	t.Parallel()

	m := newSubagentTestModel(t)
	m.SetSize(40, 100)
	m.SetSubagentTree(subagentSnapshot())
	m.ReconcileLayout()
	m.CancelPresentation()
	m.View()

	require.Len(t, m.subagentHoverZone, 2)

	// Hovering a mapped line selects its node; a non-mapped line clears it.
	var line int
	for l, id := range m.subagentHoverZone {
		if id == "a1b2c" {
			line = l
		}
	}
	m.updateSubagentHover(line)
	assert.Equal(t, subagent.NodeID("a1b2c"), m.hoveredSubagent)
	m.updateSubagentHover(0)
	assert.Empty(t, m.hoveredSubagent)

	m.updateSubagentHover(line)
	m.ClearSubagentHover()
	assert.Empty(t, m.hoveredSubagent)
}

func TestSubagentSpinnerLifecycle(t *testing.T) {
	t.Parallel()

	m := newSubagentTestModel(t)

	cmd := sidebarOwnerCommand(m, m.SetSubagentTree(subagentSnapshot()))
	assert.NotNil(t, cmd, "running subagents start the spinner")
	assert.True(t, m.subagentSpinnerOn)

	done := subagent.Snapshot{Nodes: []subagent.NodeSnapshot{{
		Node: subagent.Node{ID: "root:sess", Agent: "root", State: subagent.NodeRunning},
		Children: []subagent.NodeSnapshot{
			{Node: subagent.Node{ID: "a1b2c", Agent: "coder", Parent: "root:sess", State: subagent.NodeCompleted}},
		},
	}}}
	m.SetSubagentTree(done)
	assert.False(t, m.subagentSpinnerOn, "current root is not rendered in its own descendant tree")
	done.Nodes[0].Node.State = subagent.NodeIdle
	m.SetSubagentTree(done)
	assert.False(t, m.subagentSpinnerOn, "spinner stops when root and children are idle or terminal")
}

// fakeSpinner overrides the animation-coordinator lifecycle with no-ops.
type fakeSpinner struct{ spinner.Spinner }

func (f *fakeSpinner) Init() tea.Cmd { return func() tea.Msg { return nil } }
func (f *fakeSpinner) Stop()         {}

// Visible state glyphs occupy exactly one bare cell so a state transition
// (spinner → ✓/✗) never shifts the row's text unexpectedly. Idle has no glyph.
func TestSubagentGlyphs(t *testing.T) {
	t.Parallel()

	m := newSubagentTestModel(t)
	states := []subagent.NodeState{
		subagent.NodeStarting, subagent.NodeRunning,
		subagent.NodeCompleted, subagent.NodeFailed, subagent.NodeStopped,
	}
	for _, s := range states {
		glyph := m.subagentGlyph(subagent.Node{Agent: "coder", State: s})
		assert.Equal(t, 1, lipgloss.Width(glyph), "state %s glyph must be a single cell", s)
	}
	assert.Empty(t, ansi.Strip(m.subagentGlyph(subagent.Node{Agent: "coder", State: subagent.NodeIdle})))
}

func TestTimeAgo(t *testing.T) {
	t.Parallel()

	now := time.Now()
	assert.Equal(t, "just now", timeAgo(now.Add(-30*time.Second)))
	assert.Equal(t, "5m ago", timeAgo(now.Add(-5*time.Minute)))
	assert.Equal(t, "3h ago", timeAgo(now.Add(-3*time.Hour)))
	assert.Equal(t, "2d ago", timeAgo(now.Add(-49*time.Hour)))
}

func TestLoadFromSessionRestoresSubagentTree(t *testing.T) {
	t.Parallel()

	m := newSubagentTestModel(t)
	sess := session.New(session.WithID("sess"))
	sess.SubagentTree = &subagent.Snapshot{Nodes: []subagent.NodeSnapshot{{
		Node: subagent.Node{ID: subagent.SessionRootID("sess"), Agent: "root", State: subagent.NodeRunning},
		Children: []subagent.NodeSnapshot{
			{Node: subagent.Node{ID: "a1b2c", Agent: "coder", Parent: subagent.SessionRootID("sess"), State: subagent.NodeIdle}},
			{Node: subagent.Node{ID: "d4e5f", Agent: "reviewer", Parent: subagent.SessionRootID("sess"), State: subagent.NodeStopped}},
		},
	}}}

	m.LoadFromSession(sess)

	out := m.subagentsInfo(40)
	assert.Contains(t, out, "coder")
	assert.Contains(t, out, "reviewer")
	// States are authoritative (the runtime adopts resumable subagents as
	// idle); the sidebar renders them as-is.
	assert.Contains(t, out, "idle")
	assert.Contains(t, out, "stopped")
	assert.False(t, m.subagentSpinnerOn, "only actual running descendants require tree spinner")
}

func TestLoadFromSessionClearsStaleSubagents(t *testing.T) {
	t.Parallel()

	m := newSubagentTestModel(t)
	m.SetSubagentTree(subagentSnapshot())
	require.NotEmpty(t, m.subagentNodes)

	// Loading a session without a persisted tree clears the previous view.
	m.LoadFromSession(session.New(session.WithID("other")))
	assert.Empty(t, m.subagentNodes)
	assert.Empty(t, m.subagentsInfo(40))
}

func TestSetSubagentTreeScopedToMainSession(t *testing.T) {
	t.Parallel()

	m := newSubagentTestModel(t)
	m.rootSessionID = "sess"

	snap := subagent.Snapshot{Nodes: []subagent.NodeSnapshot{
		{
			Node: subagent.Node{ID: subagent.SessionRootID("sess"), Agent: "root", State: subagent.NodeRunning},
			Children: []subagent.NodeSnapshot{
				{Node: subagent.Node{ID: "a1b2c", Agent: "coder", Parent: subagent.SessionRootID("sess"), State: subagent.NodeRunning}},
			},
		},
		{
			Node: subagent.Node{ID: subagent.SessionRootID("other"), Agent: "root", State: subagent.NodeRunning},
			Children: []subagent.NodeSnapshot{
				{Node: subagent.Node{ID: "ff00f", Agent: "intruder", Parent: subagent.SessionRootID("other"), State: subagent.NodeRunning}},
			},
		},
	}}
	m.SetSubagentTree(snap)

	out := m.subagentsInfo(40)
	assert.Contains(t, out, "coder")
	assert.NotContains(t, out, "intruder", "other sessions' subagents are filtered out")
}

// At process start the tree bridge publishes the runtime's initial (empty)
// snapshot right after the sidebar restored the persisted tree from the
// session store. A snapshot with no root for this session is not authoritative
// and must not wipe the restored view.
func TestRestoredSubagentsSurviveEmptyLiveSnapshot(t *testing.T) {
	t.Parallel()

	m := newSubagentTestModel(t)
	sess := session.New(session.WithID("sess"))
	sess.SubagentTree = &subagent.Snapshot{Nodes: []subagent.NodeSnapshot{{
		Node: subagent.Node{ID: subagent.SessionRootID("sess"), Agent: "root", State: subagent.NodeRunning},
		Children: []subagent.NodeSnapshot{
			{Node: subagent.Node{ID: "77c88", Agent: "planner", Parent: subagent.SessionRootID("sess"), State: subagent.NodeIdle}},
		},
	}}}
	m.LoadFromSession(sess)
	require.NotEmpty(t, m.subagentNodes)

	// Empty initial snapshot from the bridge: ignored.
	m.SetSubagentTree(subagent.Snapshot{})
	assert.NotEmpty(t, m.subagentNodes, "empty live snapshot must not wipe the restored view")
	assert.Contains(t, m.subagentsInfo(40), "planner")

	// Foreign-session snapshot: also ignored.
	m.SetSubagentTree(subagent.Snapshot{Nodes: []subagent.NodeSnapshot{{
		Node: subagent.Node{ID: subagent.SessionRootID("other"), Agent: "root", State: subagent.NodeRunning},
	}}})
	assert.Contains(t, m.subagentsInfo(40), "planner")

	// A live snapshot that does track this session is authoritative.
	m.SetSubagentTree(subagent.Snapshot{Nodes: []subagent.NodeSnapshot{{
		Node: subagent.Node{ID: subagent.SessionRootID("sess"), Agent: "root", State: subagent.NodeRunning},
		Children: []subagent.NodeSnapshot{
			{Node: subagent.Node{ID: "9ca01", Agent: "coder", Parent: subagent.SessionRootID("sess"), State: subagent.NodeRunning}},
		},
	}}})
	out := m.subagentsInfo(40)
	assert.Contains(t, out, "coder")
	assert.NotContains(t, out, "planner")
}

// An attached subagent tab roots the swarm section at the subagent's own node:
// only ITS children render, not the whole session tree.
func TestSubagentContextRootsViewAtNode(t *testing.T) {
	t.Parallel()

	m := newSubagentTestModel(t)
	m.rootSessionID = "sess"
	m.SetSubagentContext("a1b2c", "root", "sess")

	snap := subagent.Snapshot{
		Root: "root:sess",
		Nodes: []subagent.NodeSnapshot{{
			Node: subagent.Node{ID: "root:sess", Agent: "root", State: subagent.NodeRunning},
			Children: []subagent.NodeSnapshot{{
				Node: subagent.Node{ID: "a1b2c", Agent: "coder", Parent: "root:sess", State: subagent.NodeIdle},
				Children: []subagent.NodeSnapshot{
					{Node: subagent.Node{ID: "e1f2a", Agent: "tester", Parent: "a1b2c", State: subagent.NodeRunning}},
				},
			}},
		}},
	}
	m.SetSubagentTree(snap)

	out := m.subagentsInfo(40)
	assert.Contains(t, out, "tester", "the attached subagent's own children render")
	assert.NotContains(t, out, "coder", "attached current agent is excluded from its own children")
}

// The parent line renders for attached tabs and resolves clicks to the parent
// session; subagent rows resolve to their node id.
func TestSidebarSubagentClickZones(t *testing.T) {
	t.Parallel()

	m := newSubagentTestModel(t)
	m.SetSize(40, 50)
	m.mode = ModeVertical
	m.rootSessionID = "sess"
	m.SetSubagentContext("root:sess", "root", "parent-sess")
	m.SetSubagentTree(subagentSnapshot())

	view := m.View()
	assert.Contains(t, view, "parent: ")
	assert.Contains(t, view, "root")

	// Find the rendered lines to click on.
	parentY, coderY, reviewerY, headerY := -1, -1, -1, -1
	viewLines := strings.Split(view, "\n")
	for i, line := range viewLines {
		if strings.Contains(line, "Subagents") {
			headerY = i
		}
		if strings.Contains(line, "parent: ") {
			parentY = i
		}
		if strings.Contains(line, "coder") {
			coderY = i
		}
		if strings.Contains(line, "reviewer") {
			reviewerY = i
		}
	}
	require.GreaterOrEqual(t, parentY, 0)
	require.GreaterOrEqual(t, coderY, 0)
	require.GreaterOrEqual(t, reviewerY, 0)

	// The parent line lives inside the Subagents section, separated from the
	// children tree (newest sibling first) by exactly one blank line.
	assert.Equal(t, -1, headerY, "the tree has no section heading")
	assert.Equal(t, parentY+4, reviewerY, "parent gap, recap and expanded gap precede children")
	assert.Empty(t, strings.TrimSpace(ansi.Strip(viewLines[parentY+1])))
	assert.Contains(t, ansi.Strip(viewLines[parentY+2]), "2 subagents")
	assert.Empty(t, strings.TrimSpace(ansi.Strip(viewLines[parentY+3])))

	result, payload := m.HandleClickType(m.layoutCfg.PaddingLeft, parentY)
	assert.Equal(t, ClickSubagentParent, result)
	assert.Equal(t, "parent-sess", payload)

	result, payload = m.HandleClickType(m.layoutCfg.PaddingLeft+2, coderY)
	assert.Equal(t, ClickSubagent, result)
	assert.Equal(t, "a1b2c", payload)
}

// Nested subagents render with branch guides (├─/└─ plus │ continuation
// rails), not bare indentation, so deep trees read as connected.
func TestSubagentsInfoRendersBranchGuides(t *testing.T) {
	t.Parallel()

	m := newSubagentTestModel(t)
	m.rootSessionID = "sess"
	base := time.Now().Add(-time.Hour)
	m.SetSubagentTree(subagent.Snapshot{
		Root: "root:sess",
		Nodes: []subagent.NodeSnapshot{{
			Node: subagent.Node{ID: "root:sess", Agent: "root", State: subagent.NodeRunning},
			Children: []subagent.NodeSnapshot{{
				Node: subagent.Node{ID: "aaaaa", Agent: "planner", Parent: "root:sess", State: subagent.NodeIdle, CreatedAt: base},
				Children: []subagent.NodeSnapshot{
					// Two children: the newer renders first with ├, the older
					// last with └; the newer one's own child keeps a │ rail.
					{
						Node: subagent.Node{ID: "bbbbb", Agent: "coder", Parent: "aaaaa", State: subagent.NodeIdle, CreatedAt: base.Add(2 * time.Minute)},
						Children: []subagent.NodeSnapshot{
							{Node: subagent.Node{ID: "ccccc", Agent: "tester", Parent: "bbbbb", State: subagent.NodeIdle, CreatedAt: base.Add(3 * time.Minute)}},
						},
					},
					{Node: subagent.Node{ID: "ddddd", Agent: "reviewer", Parent: "aaaaa", State: subagent.NodeIdle, CreatedAt: base.Add(time.Minute)}},
				},
			}},
		}},
	})

	out := ansi.Strip(m.subagentsInfo(60))
	lines := strings.Split(out, "\n")
	find := func(name string) string {
		for _, line := range lines {
			if strings.Contains(line, name) {
				return line
			}
		}
		t.Fatalf("no line for %s in:\n%s", name, out)
		return ""
	}

	assert.NotContains(t, find("planner"), "─", "top-level rows are bare")
	assert.Contains(t, find("planner"), "planner")
	assert.NotContains(t, find("planner"), "planner ○")
	assert.Contains(t, find("planner"), "idle", "state text remains right-aligned")
	assert.Contains(t, find("coder"), "├ coder", "child branch starts under the parent name")
	assert.NotContains(t, find("coder"), "coder ○")
	assert.Contains(t, find("coder"), "idle", "child status remains right-aligned")
	assert.Contains(t, find("tester"), "│ └ tester", "nested child branch starts under its parent name")
	assert.Contains(t, find("reviewer"), "└ reviewer", "last sibling gets an elbow")
	assert.NotContains(t, find("reviewer"), "reviewer ○")

	assert.True(t, strings.HasPrefix(find("planner"), "  planner"),
		"top-level rows indent the agent name under the recap: %q", find("planner"))
	assert.True(t, strings.HasPrefix(find("reviewer"), "  └ reviewer"),
		"guides start under the parent name: %q", find("reviewer"))
	assert.True(t, strings.HasPrefix(find("tester"), "  │ └ tester"),
		"rails inherit the parent-name origin: %q", find("tester"))
}

func TestSubagentsInfoTruncatesDeepRowsWithoutWrapping(t *testing.T) {
	t.Parallel()

	m := newSubagentTestModel(t)
	longName := "planner-with-a-very-long-name"
	line := ansi.Strip(m.subagentLine(subagent.Node{
		ID:        "long1",
		Agent:     longName,
		Name:      longName,
		State:     subagent.NodeIdle,
		CreatedAt: time.Now(),
	}, "│││││││└", 24))

	assert.LessOrEqual(t, lipgloss.Width(line), 24)
	assert.NotContains(t, line, "\n")
	assert.Contains(t, line, "planner-with-a-ver", "idle rows use the freed glyph space for the name")
	assert.NotContains(t, line, "○", "idle rows do not render a state glyph")
	assert.NotContains(t, line, "│", "indentation recedes before the name truncates")
	assert.Contains(t, line, "idle", "status stays on the right when possible")
}

func TestSubagentGuideTailKeepsDeepRowsReadable(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "│ └ ", subagentGuideTail("│ │ │ │ └ ", 4))
	assert.Empty(t, subagentGuideTail("│ └ ", 0))
	assert.Equal(t, "│ └ ", subagentGuideTail("│ └ ", 8))
}

// Animation-only frames (a subagent spinner tick) must not re-render the
// static sidebar sections: they are served from sectionCache until a full
// invalidation. Without this, a working subagent re-renders the whole
// sidebar 14 times a second while nothing but one glyph changes.
func TestRenderSections_AnimationFramesServeStaticSectionsFromCache(t *testing.T) {
	t.Parallel()

	m := newSubagentTestModel(t)
	m.SetSubagentTree(subagentSnapshot())
	m.sessionTitle = "before"

	first := strings.Join(m.renderSections(40), "\n")
	require.Contains(t, first, "before")

	// Mutate section content behind the cache's back (no invalidation), as a
	// probe: an animation-only frame must serve the stale cached render.
	m.sessionTitle = "after"
	m.invalidateAnimation()
	assert.Contains(t, strings.Join(m.renderSections(40), "\n"), "before",
		"static session section must come from the cache on animation frames")

	// The animated subagents section stays live on the same frames.
	kept := m.subagentNodes[:0]
	for _, n := range m.subagentNodes {
		if n.Node.Agent != "reviewer" {
			kept = append(kept, n)
		}
	}
	m.subagentNodes = kept
	m.invalidateAnimation()
	assert.NotContains(t, strings.Join(m.renderSections(40), "\n"), "reviewer",
		"subagents section must re-render on animation frames")

	// A full invalidation (what every content setter does) drops the cache.
	m.invalidateCache()
	assert.Contains(t, strings.Join(m.renderSections(40), "\n"), "after")

	// A width change (e.g. the two-pass scrollbar probe) also drops it.
	m.sessionTitle = "resized"
	assert.Contains(t, strings.Join(m.renderSections(39), "\n"), "resized")
}
