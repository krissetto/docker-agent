package tui

import "math"

// splitRect uses terminal cells, including each pane's title row.
type splitRect struct{ X, Y, W, H int }

type splitEdge uint8

const (
	splitLeft splitEdge = iota
	splitRight
	splitTop
	splitBottom
)

type splitAxis uint8

const (
	splitColumns splitAxis = iota
	splitRows
)

type splitDivider struct {
	ID   string
	Axis splitAxis
	Rect splitRect
}

type splitGeometry struct {
	Panes    map[string]splitRect
	Dividers []splitDivider
	Compact  bool
}

// splitLayout is an immutable binary tree. Successful edits own all their nodes;
// callers may retain any earlier layout to roll back a proposed edit.
type splitLayout struct {
	root *splitNode
}

type splitNode struct {
	session string
	axis    splitAxis
	ratio   float64 // Preferred share of the space remaining after the divider.
	first   *splitNode
	second  *splitNode
}

func newSplitLayout(sessionID string) splitLayout {
	if sessionID == "" {
		return splitLayout{}
	}
	return splitLayout{root: &splitNode{session: sessionID}}
}

// Sessions returns a fresh slice in spatial tree order (left/top first).
func (l splitLayout) Sessions() []string {
	var sessions []string
	var visit func(*splitNode)
	visit = func(node *splitNode) {
		if node == nil {
			return
		}
		if node.first == nil {
			sessions = append(sessions, node.session)
			return
		}
		visit(node.first)
		visit(node.second)
	}
	visit(l.root)
	return sessions
}

func (l splitLayout) Contains(sessionID string) bool {
	return sessionID != "" && splitContains(l.root, sessionID)
}

func splitContains(node *splitNode, sessionID string) bool {
	if node == nil {
		return false
	}
	if node.first == nil {
		return node.session == sessionID
	}
	return splitContains(node.first, sessionID) || splitContains(node.second, sessionID)
}

func (l splitLayout) Insert(source, target string, edge splitEdge) (splitLayout, bool) {
	if source == "" || source == target || edge > splitBottom || !l.Contains(target) {
		return l, false
	}
	root := splitClone(l.root)
	// Detach before locating the target: collapsing an ancestor can move it.
	root = splitRemove(root, source)
	var insert func(*splitNode) *splitNode
	insert = func(node *splitNode) *splitNode {
		if node.first == nil {
			if node.session != target {
				return node
			}
			axis := splitColumns
			if edge == splitTop || edge == splitBottom {
				axis = splitRows
			}
			first, second := node, &splitNode{session: source}
			if edge == splitLeft || edge == splitTop {
				first, second = second, first
			}
			return &splitNode{axis: axis, ratio: 0.5, first: first, second: second}
		}
		node.first = insert(node.first)
		node.second = insert(node.second)
		return node
	}
	return splitLayout{root: insert(root)}, true
}

func (l splitLayout) Remove(sessionID string) (splitLayout, bool) {
	if !l.Contains(sessionID) || l.root.first == nil {
		return l, false
	}
	return splitLayout{root: splitRemove(splitClone(l.root), sessionID)}, true
}

func (l splitLayout) Single(sessionID string) splitLayout {
	return newSplitLayout(sessionID)
}

func splitClone(node *splitNode) *splitNode {
	if node == nil {
		return nil
	}
	clone := *node
	clone.first = splitClone(node.first)
	clone.second = splitClone(node.second)
	return &clone
}

// splitRemove operates only on an edit's private clone.
func splitRemove(node *splitNode, sessionID string) *splitNode {
	if node == nil {
		return nil
	}
	if node.first == nil {
		if node.session == sessionID {
			return nil
		}
		return node
	}
	node.first = splitRemove(node.first, sessionID)
	node.second = splitRemove(node.second, sessionID)
	if node.first == nil {
		return node.second
	}
	if node.second == nil {
		return node.first
	}
	return node
}

type splitMinimum struct{ w, h int }

func splitMinima(root *splitNode, minW, minH int) map[*splitNode]splitMinimum {
	minima := make(map[*splitNode]splitMinimum)
	var visit func(*splitNode) splitMinimum
	visit = func(node *splitNode) splitMinimum {
		if node.first == nil {
			size := splitMinimum{max(1, minW), max(1, minH)}
			minima[node] = size
			return size
		}
		first, second := visit(node.first), visit(node.second)
		size := splitMinimum{max(first.w, second.w), first.h + 1 + second.h}
		if node.axis == splitColumns {
			size = splitMinimum{first.w + 1 + second.w, max(first.h, second.h)}
		}
		minima[node] = size
		return size
	}
	visit(root)
	return minima
}

// splitPartition clamps the rendered size, never the stored preferred ratio.
// The caller guarantees that bounds can contain both recursive minima.
func splitPartition(node *splitNode, bounds splitRect, minima map[*splitNode]splitMinimum) (first, divider, second splitRect) {
	first, divider, second = bounds, bounds, bounds
	if node.axis == splitColumns {
		available := bounds.W - 1
		width := max(minima[node.first].w, min(available-minima[node.second].w, int(math.Round(float64(available)*node.ratio))))
		first.W = width
		divider.X, divider.W = bounds.X+width, 1
		second.X, second.W = divider.X+1, available-width
	} else {
		available := bounds.H - 1
		height := max(minima[node.first].h, min(available-minima[node.second].h, int(math.Round(float64(available)*node.ratio))))
		first.H = height
		divider.Y, divider.H = bounds.Y+height, 1
		second.Y, second.H = divider.Y+1, available-height
	}
	return first, divider, second
}

// Compute falls back to the focused leaf when the entire tree cannot fit. An
// absent focus falls back to the first leaf. Nonpositive bounds draw nothing.
// Divider IDs are paths (root, root/0, root/1, ...), valid only for this topology.
func (l splitLayout) Compute(bounds splitRect, focused string, minW, minH int) splitGeometry {
	geometry := splitGeometry{Panes: make(map[string]splitRect)}
	if l.root == nil {
		return geometry
	}
	minima := splitMinima(l.root, minW, minH)
	minimum := minima[l.root]
	if bounds.W < minimum.w || bounds.H < minimum.h {
		geometry.Compact = true
		if bounds.W > 0 && bounds.H > 0 {
			if !l.Contains(focused) {
				node := l.root
				for node.first != nil {
					node = node.first
				}
				focused = node.session
			}
			geometry.Panes[focused] = bounds
		}
		return geometry
	}
	var visit func(*splitNode, splitRect, string)
	visit = func(node *splitNode, rect splitRect, path string) {
		if node.first == nil {
			geometry.Panes[node.session] = rect
			return
		}
		first, divider, second := splitPartition(node, rect, minima)
		geometry.Dividers = append(geometry.Dividers, splitDivider{ID: path, Axis: node.axis, Rect: divider})
		visit(node.first, first, path+"/0")
		visit(node.second, second, path+"/1")
	}
	visit(l.root, bounds, "root")
	return geometry
}

// Resize interprets absolutePosition as a terminal X (columns) or Y (rows).
// Hidden, unknown, and unmoved dividers are rejected. Only the selected node's
// preferred ratio changes; both recursive subtree minima constrain its position.
func (l splitLayout) Resize(dividerID string, absolutePosition int, bounds splitRect, minW, minH int) (splitLayout, bool) {
	if l.root == nil {
		return l, false
	}
	minima := splitMinima(l.root, minW, minH)
	minimum := minima[l.root]
	if bounds.W < minimum.w || bounds.H < minimum.h {
		return l, false
	}
	var find func(*splitNode, splitRect, string) (*splitNode, float64)
	find = func(node *splitNode, rect splitRect, path string) (*splitNode, float64) {
		if node.first == nil {
			return nil, 0
		}
		first, divider, second := splitPartition(node, rect, minima)
		if path == dividerID {
			origin, available, current := rect.X, rect.W-1, divider.X
			firstMin, secondMin := minima[node.first].w, minima[node.second].w
			if node.axis == splitRows {
				origin, available, current = rect.Y, rect.H-1, divider.Y
				firstMin, secondMin = minima[node.first].h, minima[node.second].h
			}
			position := max(origin+firstMin, min(origin+available-secondMin, absolutePosition))
			if position == current {
				return nil, 0
			}
			return node, float64(position-origin) / float64(available)
		}
		if found, ratio := find(node.first, first, path+"/0"); found != nil {
			return found, ratio
		}
		return find(node.second, second, path+"/1")
	}
	node, ratio := find(l.root, bounds, "root")
	if node == nil {
		return l, false
	}
	var clone func(*splitNode) *splitNode
	clone = func(original *splitNode) *splitNode {
		if original == nil {
			return nil
		}
		cloneNode := *original
		if original == node {
			cloneNode.ratio = ratio
		}
		cloneNode.first, cloneNode.second = clone(original.first), clone(original.second)
		return &cloneNode
	}
	return splitLayout{root: clone(l.root)}, true
}
