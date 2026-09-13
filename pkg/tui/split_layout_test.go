package tui

import (
	"reflect"
	"strconv"
	"testing"
)

func splitTestInsert(t *testing.T, layout splitLayout, source, target string, edge splitEdge) splitLayout {
	t.Helper()
	next, ok := layout.Insert(source, target, edge)
	if !ok {
		t.Fatalf("Insert(%q, %q, %d) rejected", source, target, edge)
	}
	return next
}

func splitTestSessions(t *testing.T, layout splitLayout, want ...string) {
	t.Helper()
	if got := layout.Sessions(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Sessions() = %v, want %v", got, want)
	}
	seen := make(map[string]bool)
	for _, session := range layout.Sessions() {
		if seen[session] || !layout.Contains(session) {
			t.Fatalf("duplicate or unfindable session %q", session)
		}
		seen[session] = true
	}
}

// Every drawable cell belongs to exactly one pane or one one-cell divider.
func splitTestCoverage(t *testing.T, geometry splitGeometry, bounds splitRect) {
	t.Helper()
	cells := make(map[[2]int]bool)
	add := func(rect splitRect) {
		t.Helper()
		if rect.W <= 0 || rect.H <= 0 || rect.X < bounds.X || rect.Y < bounds.Y || rect.X+rect.W > bounds.X+bounds.W || rect.Y+rect.H > bounds.Y+bounds.H {
			t.Fatalf("invalid rectangle %+v within %+v", rect, bounds)
		}
		for y := rect.Y; y < rect.Y+rect.H; y++ {
			for x := rect.X; x < rect.X+rect.W; x++ {
				cell := [2]int{x, y}
				if cells[cell] {
					t.Fatalf("overlap at %v", cell)
				}
				cells[cell] = true
			}
		}
	}
	for _, rect := range geometry.Panes {
		add(rect)
	}
	ids := make(map[string]bool)
	for _, divider := range geometry.Dividers {
		if ids[divider.ID] || divider.ID == "" {
			t.Fatalf("invalid divider ID %q", divider.ID)
		}
		ids[divider.ID] = true
		if divider.Axis == splitColumns && divider.Rect.W != 1 || divider.Axis == splitRows && divider.Rect.H != 1 {
			t.Fatalf("divider is not one cell thick: %+v", divider)
		}
		add(divider.Rect)
	}
	if len(cells) != bounds.W*bounds.H {
		t.Fatalf("covered %d cells, want %d", len(cells), bounds.W*bounds.H)
	}
}

func splitTestIndependent(t *testing.T, before, after splitLayout) {
	t.Helper()
	nodes := make(map[*splitNode]bool)
	var collect func(*splitNode)
	collect = func(node *splitNode) {
		if node == nil {
			return
		}
		nodes[node] = true
		collect(node.first)
		collect(node.second)
	}
	collect(before.root)
	var check func(*splitNode)
	check = func(node *splitNode) {
		if node == nil {
			return
		}
		if nodes[node] {
			t.Fatal("successful candidate shares tree nodes with original")
		}
		check(node.first)
		check(node.second)
	}
	check(after.root)
}

func TestSplitLayoutSingleAndEmpty(t *testing.T) {
	bounds := splitRect{X: 4, Y: 7, W: 80, H: 20}
	var empty splitLayout
	if empty.Contains("") || empty.Contains("a") || len(empty.Sessions()) != 0 {
		t.Fatal("zero layout is not empty")
	}
	if got := empty.Compute(bounds, "", 24, 6); len(got.Panes) != 0 || len(got.Dividers) != 0 || got.Compact {
		t.Fatalf("empty geometry = %+v", got)
	}
	if newSplitLayout("").root != nil {
		t.Fatal("empty session must not create a leaf")
	}
	layout := newSplitLayout("a")
	splitTestSessions(t, layout, "a")
	geometry := layout.Compute(bounds, "missing", 24, 6)
	if geometry.Compact || geometry.Panes["a"] != bounds || len(geometry.Dividers) != 0 {
		t.Fatalf("single geometry = %+v", geometry)
	}
	if next, ok := layout.Remove("a"); ok || next.root != layout.root {
		t.Fatal("removing last session must be unchanged")
	}
	sessions := layout.Sessions()
	sessions[0] = "corrupted"
	splitTestSessions(t, layout, "a")
	single := layout.Single("b")
	splitTestSessions(t, single, "b")
	splitTestIndependent(t, layout, single)
	splitTestSessions(t, layout, "a")
}

func TestSplitLayoutInsertEdges(t *testing.T) {
	for _, tc := range []struct {
		name  string
		edge  splitEdge
		axis  splitAxis
		order []string
	}{
		{"left", splitLeft, splitColumns, []string{"b", "a"}},
		{"right", splitRight, splitColumns, []string{"a", "b"}},
		{"top", splitTop, splitRows, []string{"b", "a"}},
		{"bottom", splitBottom, splitRows, []string{"a", "b"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			original := newSplitLayout("a")
			layout := splitTestInsert(t, original, "b", "a", tc.edge)
			splitTestSessions(t, layout, tc.order...)
			splitTestSessions(t, original, "a")
			splitTestIndependent(t, original, layout)
			bounds := splitRect{X: 10, Y: 20, W: 101, H: 31}
			geometry := layout.Compute(bounds, "a", 24, 6)
			if geometry.Compact || len(geometry.Dividers) != 1 || geometry.Dividers[0].Axis != tc.axis {
				t.Fatalf("unexpected geometry %+v", geometry)
			}
			first, second := geometry.Panes[tc.order[0]], geometry.Panes[tc.order[1]]
			if tc.axis == splitColumns && (first.W != 50 || second.W != 50) || tc.axis == splitRows && (first.H != 15 || second.H != 15) {
				t.Fatalf("insert did not split 50:50: %+v, %+v", first, second)
			}
			splitTestCoverage(t, geometry, bounds)
		})
	}
}

func TestSplitLayoutInvalidEditsAreUnchanged(t *testing.T) {
	layout := newSplitLayout("a")
	for _, tc := range []struct {
		source, target string
		edge           splitEdge
	}{
		{"", "a", splitLeft},
		{"a", "a", splitRight},
		{"b", "missing", splitTop},
		{"b", "", splitBottom},
		{"b", "a", splitEdge(255)},
	} {
		if next, ok := layout.Insert(tc.source, tc.target, tc.edge); ok || next.root != layout.root {
			t.Fatalf("invalid insertion changed tree: %+v", tc)
		}
	}
	if next, ok := layout.Remove("missing"); ok || next.root != layout.root {
		t.Fatal("unknown removal changed tree")
	}
	if _, ok := (splitLayout{}).Insert("a", "b", splitLeft); ok {
		t.Fatal("insertion without target accepted")
	}
}

func TestSplitLayoutMoveAndCollapse(t *testing.T) {
	original := splitTestInsert(t, newSplitLayout("a"), "b", "a", splitRight)
	original = splitTestInsert(t, original, "c", "a", splitBottom)
	// Moving a across the root collapses its old rows parent.
	moved := splitTestInsert(t, original, "a", "b", splitTop)
	splitTestSessions(t, original, "a", "c", "b")
	splitTestSessions(t, moved, "c", "a", "b")
	splitTestIndependent(t, original, moved)
	if moved.root.first.session != "c" || moved.root.second.axis != splitRows {
		t.Fatal("moving visible source did not collapse its old parent")
	}
	// Moving a beside its own sibling also replaces, rather than duplicates, it.
	movedAgain := splitTestInsert(t, moved, "a", "b", splitRight)
	splitTestSessions(t, movedAgain, "c", "b", "a")
	removed, ok := movedAgain.Remove("b")
	if !ok {
		t.Fatal("remove rejected")
	}
	splitTestSessions(t, removed, "c", "a")
	splitTestIndependent(t, movedAgain, removed)
	last, ok := removed.Remove("c")
	if !ok || last.root.first != nil {
		t.Fatal("root parent did not collapse to last leaf")
	}
	splitTestSessions(t, last, "a")
	bounds := splitRect{W: 101, H: 31}
	for _, layout := range []splitLayout{original, moved, movedAgain, removed, last} {
		splitTestCoverage(t, layout.Compute(bounds, "a", 24, 6), bounds)
	}
}

func TestSplitLayoutNestedGeometry(t *testing.T) {
	layout := splitTestInsert(t, newSplitLayout("a"), "b", "a", splitRight)
	layout = splitTestInsert(t, layout, "c", "a", splitBottom)
	layout = splitTestInsert(t, layout, "d", "b", splitLeft)
	bounds := splitRect{X: 10, Y: 20, W: 103, H: 31}
	geometry := layout.Compute(bounds, "a", 24, 6)
	want := map[string]splitRect{
		"a": {X: 10, Y: 20, W: 51, H: 15},
		"c": {X: 10, Y: 36, W: 51, H: 15},
		"d": {X: 62, Y: 20, W: 25, H: 31},
		"b": {X: 88, Y: 20, W: 25, H: 31},
	}
	if geometry.Compact || !reflect.DeepEqual(geometry.Panes, want) {
		t.Fatalf("panes = %+v, want %+v", geometry, want)
	}
	wantDividers := []splitDivider{
		{ID: "root", Axis: splitColumns, Rect: splitRect{X: 61, Y: 20, W: 1, H: 31}},
		{ID: "root/0", Axis: splitRows, Rect: splitRect{X: 10, Y: 35, W: 51, H: 1}},
		{ID: "root/1", Axis: splitColumns, Rect: splitRect{X: 87, Y: 20, W: 1, H: 31}},
	}
	if !reflect.DeepEqual(geometry.Dividers, wantDividers) {
		t.Fatalf("dividers = %+v, want %+v", geometry.Dividers, wantDividers)
	}
	splitTestCoverage(t, geometry, bounds)
}

func TestSplitLayoutDeepTree(t *testing.T) {
	layout := newSplitLayout("0")
	for i := 1; i < 80; i++ {
		edge := splitRight
		if i%2 == 0 {
			edge = splitBottom
		}
		layout = splitTestInsert(t, layout, strconv.Itoa(i), strconv.Itoa(i-1), edge)
	}
	bounds := splitRect{X: -3, Y: 5, W: 160, H: 120}
	geometry := layout.Compute(bounds, "79", 1, 1)
	if geometry.Compact || len(geometry.Panes) != 80 || len(geometry.Dividers) != 79 {
		t.Fatalf("deep tree lost nodes: %d panes, %d dividers, compact=%v", len(geometry.Panes), len(geometry.Dividers), geometry.Compact)
	}
	splitTestCoverage(t, geometry, bounds)
	moved := splitTestInsert(t, layout, "0", "79", splitBottom)
	if len(moved.Sessions()) != 80 || !moved.Contains("0") {
		t.Fatal("deep move lost or duplicated leaves")
	}
	splitTestCoverage(t, moved.Compute(bounds, "0", 1, 1), bounds)
}

func TestSplitLayoutCompactIsReversible(t *testing.T) {
	layout := splitTestInsert(t, newSplitLayout("a"), "b", "a", splitRight)
	layout = splitTestInsert(t, layout, "c", "a", splitBottom)
	large := splitRect{X: 3, Y: 8, W: 121, H: 41}
	layout, ok := layout.Resize("root", large.X+80, large, 24, 6)
	if !ok {
		t.Fatal("initial resize rejected")
	}
	before := layout.Compute(large, "a", 24, 6)
	for _, bounds := range []splitRect{
		{X: 3, Y: 8, W: 48, H: 41},
		{X: 3, Y: 8, W: 121, H: 12},
		{X: 3, Y: 8, W: 1, H: 1},
	} {
		geometry := layout.Compute(bounds, "c", 24, 6)
		if !geometry.Compact || len(geometry.Panes) != 1 || geometry.Panes["c"] != bounds || len(geometry.Dividers) != 0 {
			t.Fatalf("focused compact geometry = %+v", geometry)
		}
		splitTestCoverage(t, geometry, bounds)
		if next, changed := layout.Resize("root", 30, bounds, 24, 6); changed || next.root != layout.root {
			t.Fatal("compact resize must not change preferred ratio")
		}
	}
	fallback := layout.Compute(splitRect{W: 10, H: 3}, "missing", 24, 6)
	if _, ok := fallback.Panes["a"]; !ok {
		t.Fatal("absent focus did not fall back to first leaf")
	}
	if after := layout.Compute(large, "a", 24, 6); !reflect.DeepEqual(after, before) {
		t.Fatalf("compact changed restored geometry: before %+v, after %+v", before, after)
	}
	splitTestSessions(t, layout, "a", "c", "b")
}

func TestSplitLayoutNonpositiveBoundsAndMinima(t *testing.T) {
	layout := splitTestInsert(t, newSplitLayout("a"), "b", "a", splitRight)
	for _, bounds := range []splitRect{{W: 0, H: 20}, {W: 30, H: 0}, {W: -2, H: 20}, {W: 30, H: -1}} {
		geometry := layout.Compute(bounds, "a", 0, -1)
		if !geometry.Compact || len(geometry.Panes) != 0 || len(geometry.Dividers) != 0 {
			t.Fatalf("nonpositive bounds drew geometry: %+v", geometry)
		}
	}
	bounds := splitRect{W: 3, H: 1}
	geometry := layout.Compute(bounds, "a", 0, -1)
	if geometry.Compact || len(geometry.Panes) != 2 {
		t.Fatalf("nonpositive minima should use drawable one-cell panes: %+v", geometry)
	}
	splitTestCoverage(t, geometry, bounds)
}

func TestSplitLayoutRecursiveMinimaAndResize(t *testing.T) {
	layout := splitTestInsert(t, newSplitLayout("a"), "b", "a", splitRight)
	layout = splitTestInsert(t, layout, "c", "a", splitRight)
	layout = splitTestInsert(t, layout, "d", "b", splitBottom)
	bounds := splitRect{X: 10, Y: 20, W: 100, H: 30}
	before := layout.Compute(bounds, "a", 24, 6)
	for _, tc := range []struct {
		name string
		id   string
		pos  int
		want int
	}{
		{"root minimum", "root", -1000, bounds.X + 49},
		{"root maximum", "root", 1000, bounds.X + 75},
		{"rows minimum", "root/1", -1000, bounds.Y + 6},
		{"rows maximum", "root/1", 1000, bounds.Y + 23},
	} {
		t.Run(tc.name, func(t *testing.T) {
			next, ok := layout.Resize(tc.id, tc.pos, bounds, 24, 6)
			if !ok {
				t.Fatal("resize rejected")
			}
			splitTestIndependent(t, layout, next)
			geometry := next.Compute(bounds, "a", 24, 6)
			for i, divider := range geometry.Dividers {
				if divider.ID != before.Dividers[i].ID {
					t.Fatal("resize changed topology-path ID")
				}
				if divider.ID == tc.id {
					position := divider.Rect.X
					if divider.Axis == splitRows {
						position = divider.Rect.Y
					}
					if position != tc.want {
						t.Fatalf("divider position = %d, want %d", position, tc.want)
					}
				}
			}
			for session, rect := range geometry.Panes {
				if rect.W < 24 || rect.H < 6 {
					t.Fatalf("pane %s violates minima: %+v", session, rect)
				}
			}
			if tc.id == "root/1" {
				if geometry.Panes["a"] != before.Panes["a"] || geometry.Panes["c"] != before.Panes["c"] || next.root.ratio != layout.root.ratio || !reflect.DeepEqual(next.root.first, layout.root.first) {
					t.Fatal("nested resize changed unaffected branch")
				}
			} else if !reflect.DeepEqual(next.root.first, layout.root.first) || !reflect.DeepEqual(next.root.second, layout.root.second) {
				t.Fatal("root resize changed descendant preferences")
			}
			splitTestCoverage(t, geometry, bounds)
			if !reflect.DeepEqual(layout.Compute(bounds, "a", 24, 6), before) {
				t.Fatal("candidate resize changed original")
			}
		})
	}
	// The whole tree needs 49+1+24 columns and max(6,6+1+6) rows.
	exact := splitRect{W: 74, H: 13}
	geometry := layout.Compute(exact, "a", 24, 6)
	if geometry.Compact {
		t.Fatal("exact recursive minimum should fit")
	}
	splitTestCoverage(t, geometry, exact)
	for _, tooSmall := range []splitRect{{W: 73, H: 13}, {W: 74, H: 12}} {
		if !layout.Compute(tooSmall, "a", 24, 6).Compact {
			t.Fatal("recursive minimum violation did not compact")
		}
	}
}

func TestSplitLayoutTerminalClampPreservesPreference(t *testing.T) {
	layout := splitTestInsert(t, newSplitLayout("a"), "b", "a", splitRight)
	large := splitRect{W: 201, H: 20}
	layout, ok := layout.Resize("root", 150, large, 24, 6)
	if !ok {
		t.Fatal("resize rejected")
	}
	before := layout.Compute(large, "a", 24, 6)
	narrow := splitRect{W: 61, H: 20}
	clamped := layout.Compute(narrow, "a", 24, 6)
	if clamped.Compact || clamped.Panes["a"].W != 36 || clamped.Panes["b"].W != 24 {
		t.Fatalf("terminal clamp = %+v", clamped)
	}
	if layout.root.ratio != 0.75 || !reflect.DeepEqual(before, layout.Compute(large, "a", 24, 6)) {
		t.Fatal("terminal clamp changed preferred ratio")
	}
	// Dragging without moving the rendered divider must not commit its clamp.
	if next, changed := layout.Resize("root", 36, narrow, 24, 6); changed || next.root != layout.root {
		t.Fatal("unmoved divider changed preferred ratio")
	}
}

func TestSplitLayoutResizeRoundTripAndRejections(t *testing.T) {
	layout := splitTestInsert(t, newSplitLayout("a"), "b", "a", splitRight)
	// Exercise non-binary fractions: a rendered drag must land on its exact cell.
	for width := 49; width <= 111; width++ {
		bounds := splitRect{X: -7, Y: 3, W: width, H: 6}
		current := layout.Compute(bounds, "a", 24, 6).Dividers[0].Rect.X
		for position := bounds.X + 24; position <= bounds.X+width-25; position++ {
			next, ok := layout.Resize("root", position, bounds, 24, 6)
			if position == current {
				if ok || next.root != layout.root {
					t.Fatal("unmoved resize changed layout")
				}
				continue
			}
			if !ok {
				t.Fatalf("resize rejected at width=%d position=%d", width, position)
			}
			if got := next.Compute(bounds, "a", 24, 6).Dividers[0].Rect.X; got != position {
				t.Fatalf("width=%d divider at %d, want %d", width, got, position)
			}
		}
	}
	bounds := splitRect{W: 101, H: 31}
	for _, id := range []string{"", "missing", "root/0", "root/1/0"} {
		if next, ok := layout.Resize(id, 30, bounds, 24, 6); ok || next.root != layout.root {
			t.Fatalf("unknown divider %q changed layout", id)
		}
	}
	for _, single := range []splitLayout{{}, newSplitLayout("a")} {
		if next, ok := single.Resize("root", 30, bounds, 24, 6); ok || next.root != single.root {
			t.Fatal("layout without divider accepted resize")
		}
	}
}

func TestSplitLayoutIndependentCandidatesAndGeometry(t *testing.T) {
	base := splitTestInsert(t, newSplitLayout("a"), "b", "a", splitRight)
	bounds := splitRect{W: 151, H: 41}
	before := base.Compute(bounds, "a", 24, 6)
	left := splitTestInsert(t, base, "c", "a", splitBottom)
	right := splitTestInsert(t, base, "d", "b", splitTop)
	splitTestIndependent(t, left, right)
	resized, ok := left.Resize("root/0", 10, bounds, 24, 6)
	if !ok {
		t.Fatal("candidate resize rejected")
	}
	removed, ok := right.Remove("b")
	if !ok {
		t.Fatal("candidate removal rejected")
	}
	splitTestIndependent(t, left, resized)
	splitTestIndependent(t, right, removed)
	splitTestSessions(t, left, "a", "c", "b")
	splitTestSessions(t, right, "a", "d", "b")
	splitTestSessions(t, removed, "a", "d")
	geometry := base.Compute(bounds, "a", 24, 6)
	delete(geometry.Panes, "a")
	geometry.Dividers[0].ID = "corrupted"
	geometry.Dividers[0].Rect.W = 999
	if !reflect.DeepEqual(base.Compute(bounds, "a", 24, 6), before) {
		t.Fatal("editing candidates or returned geometry changed base")
	}
}
