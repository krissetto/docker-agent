package messages

import (
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/animation"
	tuimessages "github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/styles"
	"github.com/docker/docker-agent/pkg/tui/types"
)

// A clear of empty URL caches must not reallocate: the tick and status paths
// drop entries many times per frame, and most drops find no URL spans.
func TestVKIDClearEmptyURLSpanCacheAllocatesNothing(t *testing.T) {
	c := newURLSpanCache()
	allocs := testing.AllocsPerRun(100, c.clear)
	require.Zero(t, allocs)
	require.NotNil(t, c.spans)
	require.NotNil(t, c.references)
}

func TestVKIDClearNonEmptyURLSpanCacheResetsBothMaps(t *testing.T) {
	c := newURLSpanCache()
	c.spans[3] = []urlSpan{{}}
	c.references[3] = renderedReference{}
	c.clear()
	require.Empty(t, c.spans)
	require.Empty(t, c.references)
	require.NotNil(t, c.spans)
	require.NotNil(t, c.references)
}

// A zero-value cache must stay writable after a clear: the early return cannot
// leave a nil map behind.
func TestVKIDClearZeroURLSpanCacheStaysWritable(t *testing.T) {
	var c urlSpanCache
	c.clear()
	c.spans[1] = nil
	c.references[1] = renderedReference{}
	require.Len(t, c.spans, 1)
	require.Len(t, c.references, 1)
}

// Dropping a materialized slot whose URL caches are empty allocates nothing
// beyond the entry bookkeeping. The entry is put back inside the measured loop
// so each run drops a real materialized entry.
func TestVKIDDropMaterializedEntryAllocatesNothing(t *testing.T) {
	m := buildWorkStatusSessionHistory(t, false, 20)
	var slot int
	var entry renderedItemRange
	found := false
	for i, e := range m.renderedItems {
		slot, entry, found = i, e, true
		break
	}
	require.True(t, found, "fixture must hold a materialized cacheable entry")
	require.Empty(t, m.urlSpans.spans)
	require.Empty(t, m.urlSpans.references)
	allocs := testing.AllocsPerRun(100, func() {
		m.renderedItems[slot] = entry
		m.dropEntry(slot)
	})
	require.Zero(t, allocs)
}

// SetWorkStatus must leave membership and anchors unchanged (status changes
// header text, not grouping), invalidate every anchor, and keep plain rows that
// are not affected by the status change.
func TestVKIDSetWorkStatusKeepsMembershipAndInvalidatesAnchors(t *testing.T) {
	m := buildWorkStatusSessionHistory(t, true, 40)
	before := maps.Clone(m.workSourceToHeader)
	anchors := make([]int, 0, len(m.workHeaderBySource))
	for anchor := range m.workHeaderBySource {
		anchors = append(anchors, anchor)
	}
	require.NotEmpty(t, anchors, "fixture must have a work block")

	plain := -1
	for i := range m.views {
		if _, grouped := m.workSourceToHeader[i]; grouped {
			continue
		}
		if _, ok := m.entryFor(i); ok {
			plain = i
			break
		}
	}
	require.GreaterOrEqual(t, plain, 0, "fixture must hold a materialized plain row")

	m.SetWorkStatus(false, time.Second)

	require.True(t, maps.Equal(before, m.workSourceToHeader), "membership must not change on status")
	for _, anchor := range anchors {
		require.NotNil(t, m.workHeaderBySource[anchor])
		_, ok := m.entryFor(anchor)
		require.False(t, ok, "anchor %d must be invalidated by a status change", anchor)
	}
	_, ok := m.entryFor(plain)
	require.True(t, ok, "unaffected plain row %d keeps its lines", plain)
}

// The old membership map survives a structural rebuild unmutated: the
// projection assigns a fresh map, so SetWorkStatus may hold the old reference.
func TestVKIDStructuralRebuildLeavesOldMembershipUnmutated(t *testing.T) {
	m := buildWorkStatusSessionHistory(t, true, 40)
	old := m.workSourceToHeader
	snapshot := maps.Clone(old)
	require.NotEmpty(t, snapshot)

	call := tools.ToolCall{ID: "vkid-rebuild-call", Function: tools.FunctionCall{Name: "shell", Arguments: `{"cmd":"true"}`}}
	m.addMessage(types.User("vkid rebuild request"))
	m.addMessage(types.ToolCallMessage("root", call, tools.Tool{Name: "shell"}, types.ToolStatusPending))
	m.SetWorkStatus(true, 0)
	_ = m.View()

	require.NotEqual(t, reflect.ValueOf(old).Pointer(), reflect.ValueOf(m.workSourceToHeader).Pointer(),
		"a rebuild must publish a new membership map")
	require.True(t, maps.Equal(snapshot, old), "the old membership map must not be mutated by a rebuild")
}

// Repeated clears of a non-empty cache reuse its storage: a render cycle that
// refills and clears the cache must not allocate a fresh pair of maps each time.
func TestVKIDRepeatedNonEmptyClearDoesNotAccumulate(t *testing.T) {
	c := newURLSpanCache()
	cycle := func() {
		for i := range 64 {
			c.spans[i] = nil
			c.references[i] = renderedReference{}
		}
		c.clear()
	}
	cycle()
	require.Zero(t, testing.AllocsPerRun(200, cycle))
	require.Empty(t, c.spans)
	require.Empty(t, c.references)
}

// A compact-off transcript with no Work state rebuilds nothing on each
// sync: the disabled reset is idempotent once it has nothing to clear.
func TestVKIDSyncWorkProjectionDisabledIdleAllocatesNothing(t *testing.T) {
	m := buildWorkStatusSessionHistory(t, false, 40)
	m.syncWorkProjection()
	require.Empty(t, m.workSourceToHeader)
	require.Empty(t, m.workHeaderBySource)
	require.Empty(t, m.workBlocks)
	require.Zero(t, testing.AllocsPerRun(100, m.syncWorkProjection))
}

// The idempotent reset still clears the projection dirty flag when nothing else
// needs resetting, so a later sync does not repeat it.
func TestVKIDSyncWorkProjectionDisabledClearsDirtyFlag(t *testing.T) {
	m := buildWorkStatusSessionHistory(t, false, 40)
	m.workProjectionDirty = true
	m.syncWorkProjection()
	require.False(t, m.workProjectionDirty)
}

// The unindexed projection gives the same exchanges as the canonical one, and it
// builds no owner or boundary index.
func TestVKIDUnindexedProjectionMatchesCanonicalExchanges(t *testing.T) {
	m := buildWorkStatusSessionHistory(t, true, 40)
	opener := m.currentSessionOpener()
	canonical := projectWorkWithVisibleReplies(m.messages, m.responseGroups, m.visibleReplies, opener)
	local := projectWorkWith(m.messages, m.responseGroups, m.visibleReplies, opener, false)
	require.NotEmpty(t, canonical.exchanges)
	require.Equal(t, canonical.exchanges, local.exchanges)
	require.Equal(t, canonical.bySource, local.bySource)
	require.Nil(t, local.owner)
	require.Nil(t, local.byBoundary)
	require.NotNil(t, canonical.owner)
	require.NotNil(t, canonical.byBoundary)
}

// AST guard: the unindexed projection is called only from SetWorkStatus, and only
// into a local variable. Nothing can store or publish it. Settled labels no longer
// need it, so it must not be called at all.
func TestVKIDUnindexedProjectionStaysLocal(t *testing.T) {
	entries, err := os.ReadDir(".")
	require.NoError(t, err)
	fset := token.NewFileSet()
	var localCalls, indexedOutside int
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		require.NoError(t, err)
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				ident, ok := call.Fun.(*ast.Ident)
				if !ok || ident.Name != "projectWorkWith" || len(call.Args) != 5 {
					return true
				}
				flag, ok := call.Args[4].(*ast.Ident)
				if !ok {
					return true
				}
				if flag.Name == "true" {
					if fn.Name.Name != "projectWorkWithVisibleReplies" {
						indexedOutside++
					}
					return true
				}
				require.Equal(t, "false", flag.Name)
				require.Equal(t, "SetWorkStatus", fn.Name.Name, "unindexed projection escaped to %s", fn.Name.Name)
				localCalls++
				// The result must land in a local identifier, never a field or element.
				assigned := false
				ast.Inspect(fn.Body, func(p ast.Node) bool {
					assign, ok := p.(*ast.AssignStmt)
					if !ok || len(assign.Rhs) != 1 || assign.Rhs[0] != call {
						return true
					}
					_, assigned = assign.Lhs[0].(*ast.Ident)
					return false
				})
				require.True(t, assigned, "unindexed projection must be assigned to a local variable")
				return true
			})
		}
	}
	require.Zero(t, localCalls, "settled durations come from turn timing, so no unindexed projection is needed")
	require.Equal(t, 0, indexedOutside, "projectWorkWith(..., true) may only be the canonical wrapper")
}

// referenceProjectionIndex rebuilds the owner and boundary indices by a direct
// walk over the final exchanges. It shares no code with the fused emit path.
func referenceProjectionIndex(p workProjection, messages []*types.Message) (map[int]int, map[*types.Message][]int) {
	owner := make(map[int]int)
	for source := range messages {
		for e, exchange := range p.exchanges {
			if slices.Contains(exchange.work, source) || slices.Contains(exchange.response, source) {
				owner[source] = e
				break
			}
		}
	}
	byBoundary := make(map[*types.Message][]int)
	for e, exchange := range p.exchanges {
		var boundary *types.Message
		if exchange.boundary >= 0 && exchange.boundary < len(messages) {
			boundary = messages[exchange.boundary]
		}
		byBoundary[boundary] = append(byBoundary[boundary], e)
	}
	return owner, byBoundary
}

// The fused indices equal the independent reference on a multi-exchange fixture,
// for two visible-reply policies and with and without an opener. The retention
// move (a source leaving work for response inside its exchange) leaves owner and
// boundary unchanged. This is an equivalence refactor, so there is no before-fail.
func TestVKIDFusedIndexMatchesIndependentReference(t *testing.T) {
	m := buildWorkStatusSessionHistory(t, true, 40)
	openers := []*types.Message{m.currentSessionOpener(), m.messages[0]}
	for _, policy := range []tuimessages.VisibleReplies{tuimessages.VisibleRepliesLatest, tuimessages.VisibleRepliesThree} {
		for _, opener := range openers {
			p := projectWorkWithVisibleReplies(m.messages, m.responseGroups, policy, opener)
			require.GreaterOrEqual(t, len(p.exchanges), 2, "fixture must hold several exchanges")
			owner, byBoundary := referenceProjectionIndex(p, m.messages)
			require.Equal(t, owner, p.owner)
			require.Equal(t, byBoundary, p.byBoundary)
			require.Equal(t, referenceWorkOwner(p), p.workOwner)

			moved := projectWorkWithVisibleReplies(m.messages, m.responseGroups, policy, opener)
			for e := range moved.exchanges {
				if n := len(moved.exchanges[e].work); n > 0 {
					source := moved.exchanges[e].work[n-1]
					moved.exchanges[e].work = moved.exchanges[e].work[:n-1]
					moved.exchanges[e].response = append(moved.exchanges[e].response, source)
					break
				}
			}
			movedOwner, movedBoundary := referenceProjectionIndex(moved, m.messages)
			require.Equal(t, movedOwner, p.owner)
			require.Equal(t, movedBoundary, p.byBoundary)
		}
	}
}

// Every published projection carries non-nil indices, including the empty one.
func TestVKIDPublishedProjectionsAreIndexed(t *testing.T) {
	m := buildWorkStatusSessionHistory(t, true, 40)
	requireIndexed := func(stage string) {
		t.Helper()
		require.NotNil(t, m.workProjection.owner, stage)
		require.NotNil(t, m.workProjection.byBoundary, stage)
	}
	requireIndexed("work on")
	m.SetWorkStatus(true, 0)
	_ = m.View()
	requireIndexed("status on")
	m.SetWorkStatus(false, time.Second)
	_ = m.View()
	requireIndexed("status off")
	m.SetCompactWorkLog(false)
	_ = m.View()
	requireIndexed("compact off")
	m.SetCompactWorkLog(true)
	_ = m.View()
	requireIndexed("compact on")
}

// Every active event (view, resize, theme, tick, scroll, mouse, hover, wheel, copy,
// compact toggles, stream and reply handoff) leaves the fallback counter at 0. The
// counter is checked after each event, so a regression names its event.
func TestVKIDHotPathsNeverScanProjection(t *testing.T) {
	m := buildWorkStatusSessionHistory(t, true, 40)
	step := func(name string, fn func()) {
		t.Helper()
		fn()
		require.Zero(t, m.stats.fullProjectionLookupScans, "fallback scan during %s", name)
	}
	step("view", func() { _ = m.View() })
	step("resize", func() { m.SetSize(90, 40); _ = m.View() })
	step("theme", func() { styles.ApplyTheme(styles.CurrentTheme()); _ = m.View() })
	step("status on", func() { m.SetWorkStatus(true, 0); _ = m.View() })
	step("status off", func() { m.SetWorkStatus(false, time.Second); _ = m.View() })
	step("tick", func() {
		m.AddOrUpdateToolCall("root", tools.ToolCall{ID: "vkid-spinner", Function: tools.FunctionCall{Name: "bench_spinner", Arguments: "{}"}}, tools.Tool{}, types.ToolStatusPending)
		cmd := m.ar.Continue()
		require.NotNil(t, cmd, "a pending tool must keep an animation tick")
		tick, ok := m.ar.Accept(cmd().(animation.TickMsg))
		require.True(t, ok)
		_, _ = m.Update(tick)
		_ = m.View()
	})
	step("wheel", func() { _, _ = m.Update(tuimessages.WheelCoalescedMsg{Delta: 1, X: 5, Y: 5}); _ = m.View() })
	step("mouse motion", func() { _, _ = m.Update(tea.MouseMotionMsg{X: 5, Y: 5}); _ = m.View() })
	step("mouse click", func() { _, _ = m.Update(tea.MouseClickMsg{X: 5, Y: 5}); _ = m.View() })
	step("mouse release", func() { _, _ = m.Update(tea.MouseReleaseMsg{X: 5, Y: 5}); _ = m.View() })
	step("copy", func() { _ = m.copyMessageToClipboard(1); _ = m.View() })
	step("compact off", func() { m.SetCompactWorkLog(false); _ = m.View() })
	step("compact on", func() { m.SetCompactWorkLog(true); _ = m.View() })
	step("stream", func() { m.AppendToLastMessage("root", "streamed text"); _ = m.View() })
	step("reply handoff", func() {
		m.addMessage(types.Agent(types.MessageTypeAssistant, "root", ""))
		m.AppendToLastMessage("root", "first prose of a new reply")
		_ = m.View()
	})
}

// AST guards over the production (non-test) sources of this package.
func TestVKIDProjectionPublishGuards(t *testing.T) {
	entries, err := os.ReadDir(".")
	require.NoError(t, err)
	fset := token.NewFileSet()
	// Direct legacy constructions, listed exactly. projectWork is test-only and
	// indexes its exchanges at the end (indexProjectionOwners).
	legacyLiterals := map[string]bool{"emptyWorkProjection": true, "projectWork": true, "projectWorkWith": true}
	var files []*ast.File
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		require.NoError(t, err)
		files = append(files, file)
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				// No unindexed workProjection literal outside the allowed constructors.
				if lit, ok := n.(*ast.CompositeLit); ok {
					if ident, ok := lit.Type.(*ast.Ident); ok && ident.Name == "workProjection" {
						require.True(t, legacyLiterals[fn.Name.Name], "workProjection literal in %s (%s)", fn.Name.Name, name)
					}
				}
				// Published or snapshotted projections come only from the allowed sources.
				if assign, ok := n.(*ast.AssignStmt); ok {
					for i, lhs := range assign.Lhs {
						sel, ok := lhs.(*ast.SelectorExpr)
						if !ok || (sel.Sel.Name != "workProjection" && sel.Sel.Name != "pendingReplyHandoffBefore") {
							continue
						}
						require.Len(t, assign.Rhs, len(assign.Lhs))
						allowed := false
						switch rhs := assign.Rhs[i].(type) {
						case *ast.CallExpr:
							id, ok := rhs.Fun.(*ast.Ident)
							allowed = ok && id.Name == "emptyWorkProjection"
						case *ast.Ident:
							allowed = rhs.Name == "projection"
						case *ast.SelectorExpr:
							allowed = rhs.Sel.Name == "workProjection"
						}
						require.True(t, allowed, "projection publish in %s (%s) must use an indexed source", fn.Name.Name, name)
					}
				}
				return true
			})
		}
	}
	require.NotEmpty(t, files)

	// The unindexed local in SetWorkStatus reads only exchanges and bySource, and it
	// is never passed to a lookup.
	for _, file := range files {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil || fn.Name.Name != "SetWorkStatus" {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if sel, ok := n.(*ast.SelectorExpr); ok {
					if id, ok := sel.X.(*ast.Ident); ok && id.Name == "projection" {
						require.Contains(t, []string{"exchanges", "bySource"}, sel.Sel.Name, "unindexed projection read %s", sel.Sel.Name)
					}
				}
				if call, ok := n.(*ast.CallExpr); ok {
					for _, arg := range call.Args {
						if id, ok := arg.(*ast.Ident); ok {
							require.NotEqual(t, "projection", id.Name, "unindexed projection passed to a call")
						}
					}
				}
				return true
			})
		}
	}
}

// referenceWorkOwner maps each work source to the first exchange that holds it as
// work, by a direct walk over the final exchanges.
func referenceWorkOwner(p workProjection) map[int]int {
	owner := make(map[int]int)
	for e, exchange := range p.exchanges {
		for _, source := range exchange.work {
			if _, taken := owner[source]; !taken {
				owner[source] = e
			}
		}
	}
	return owner
}

// Retention (a selected reply pinned out of work) removes that source from the
// published work-exchange index, the same as a rebuild over the final exchanges.
func TestVKIDPublishedWorkOwnerFollowsRetention(t *testing.T) {
	m := newFourthReplyFixture(t)
	m.suppressReplyHandoff = true
	m.addMessage(types.Agent(types.MessageTypeAssistant, "root", "fourth reply prose"))
	m.suppressReplyHandoff = false
	_ = m.View()
	_, wasWork := m.workExchangeBySource[1]
	require.True(t, wasWork, "the reply must start in work for retention to move it")

	m.selectedMessageIndex = 1
	m.workRetained[m.messages[1]] = struct{}{}
	m.workProjectionDirty = true
	m.renderDirty = true
	_ = m.View()

	require.Contains(t, fourthReplyResponseSources(m), 1, "retention must move the reply to response")
	_, stillWork := m.workExchangeBySource[1]
	require.False(t, stillWork)
	require.Equal(t, referenceWorkOwner(m.workProjection), m.workExchangeBySource)
	require.NotNil(t, m.workProjection.owner)
	require.NotNil(t, m.workProjection.byBoundary)
}
