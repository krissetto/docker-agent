package messages

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// layoutFacadeFields are the compatibility facade (C) fields. A selector with one
// of these names is a facade touch on any base expression (I1, I2).
var layoutFacadeFields = []string{"totalHeight", "lineOffsets", "renderedLines"}

// layoutFacadeSegmentField is the facade part of activeSegments. The segment's
// index, height and segments are authority (A) and are not covered.
const layoutFacadeSegmentField = "start"

// layoutBaselineFields are the reader baseline (R). Only layoutBaselineWriters may
// assign them, increment them, or take their address (I3).
var layoutBaselineFields = []string{"followTotal", "epochPending"}

// layoutBaselineWriters are the only functions that write the reader baseline.
// applyGeometryDelta is the single delta writer; updateScrollState settles at its end.
var layoutBaselineWriters = []string{"applyGeometryDelta", "updateScrollState"}

// layoutFacadeFuncs are the only functions that may touch C. Each invariant is
// stated where the function is defined: publishFacade writes C from one authority
// snapshot; resetFacade and resetFacadeTotal zero it for test-visible invalidation;
// the legacy readers run only under legacyFacadeMode (direct-write tests with no
// views); ensureAllItemsRendered is the explicit full demand; clearRows and
// publishRowCapacity maintain the C row mirror's backing array and length and are
// never consulted for a layout decision (see layoutRowCapacityConditionUses).
var layoutFacadeFuncs = []string{
	"publishFacade", "ensureAllItemsRendered", "clearRows", "publishRowCapacity",
	"resetFacade", "resetFacadeTotal",
	"legacyTotal", "legacyOffset", "legacyOffsetCount", "legacyRow",
}

// layoutRowMirrorHelpers write the renderedLines mirror. layoutRowMirrorCallers
// bounds who may call them; it is an upper bound, so resetFacade and
// resetFacadeTotal are permitted but do not call the helpers today.
var layoutRowMirrorHelpers = []string{"clearRows", "publishRowCapacity"}

var layoutRowMirrorCallers = []string{"publishFacade", "resetFacade", "resetFacadeTotal", "dropEntry", "dropMaterialized"}

// layoutRowCapacityConditionUses pins how many production conditions per function
// may consult the publishRowCapacity result. publishFacade's one use is the mirror
// bound `off+len(e.lines) <= rows`, which guards a write into the mirror and is not
// a layout decision. Any other condition use fails the test.
var layoutRowCapacityConditionUses = map[string]int{"publishFacade": 1}

// layoutLegacyGuard is the only predicate under which a legacy reader may be called.
const layoutLegacyGuard = "legacyFacadeMode"

func layoutFacadeFuncAllowed(name string) bool {
	return slices.Contains(layoutFacadeFuncs, name)
}

// layoutBoundaryResult is what one scan of a set of files found.
type layoutBoundaryResult struct {
	violations []string
	// rowCallers maps each row-mirror helper to the functions that call it.
	rowCallers map[string]map[string]bool
	// baselineWriters are functions that write, increment or take the address of a
	// baseline field, whether or not they are allowed to.
	baselineWriters map[string]bool
	// legacyReads counts legacy reader call sites proven to sit under legacyFacadeMode.
	legacyReads int
}

// layoutBoundaryScan runs every boundary check over files. The checks are name
// based and lexical, so they over-approximate: a selector matches its field name on
// any base expression. They do not prove the following, which stay out of scope:
//   - writes through reflection, unsafe, or a pointer obtained inside an allowed
//     function and stored elsewhere (the address-of site is checked, its later
//     uses are not);
//   - aliases other than locals bound directly to x.activeSegments in the same
//     function (for example a copy made in another function);
//   - the value of publishRowCapacity once it leaves the local it is bound to;
//   - guards that are not a direct `if m.legacyFacadeMode()` or `&&` operand
//     (a flag computed earlier, or a negated guard, is flagged, not accepted).
//
// Closures are attributed to their enclosing top-level function. Package-level
// declarations are scanned as "<package>", which no allowlist includes.
func layoutBoundaryScan(fset *token.FileSet, files []*ast.File) layoutBoundaryResult {
	res := layoutBoundaryResult{
		rowCallers:      map[string]map[string]bool{},
		baselineWriters: map[string]bool{},
	}
	for _, file := range files {
		for _, decl := range file.Decls {
			layoutBoundaryScanDecl(fset, decl, &res)
		}
	}
	return res
}

func layoutBoundaryScanDecl(fset *token.FileSet, decl ast.Decl, res *layoutBoundaryResult) {
	fname := "<package>"
	var root ast.Node = decl
	if fn, ok := decl.(*ast.FuncDecl); ok {
		fname = fn.Name.Name
		if fname == "clearRows" && fn.Type.Results != nil {
			res.violations = append(res.violations, fmt.Sprintf("clearRows returns a value at %s", fset.Position(fn.Pos())))
		}
		if fn.Body == nil {
			return
		}
		// The body only: the declared name must not count as a reference.
		root = fn.Body
	}
	add := func(pos token.Pos, format string, args ...any) {
		p := fset.Position(pos)
		res.violations = append(res.violations, fmt.Sprintf(format, args...)+fmt.Sprintf(" at %s:%d %s", p.Filename, p.Line, fname))
	}

	// Pass 1 classifies every write, call, discarded call, bound result and alias
	// in the declaration before pass 2 inspects selectors against them.
	writes := map[ast.Expr]string{}
	calls := map[ast.Expr]bool{}
	discarded := map[*ast.CallExpr]bool{}
	bound := map[*ast.CallExpr]string{}
	aliases := map[string]bool{}
	mark := func(e ast.Expr, kind string) {
		if sel, ok := layoutUnparen(e).(*ast.SelectorExpr); ok {
			writes[sel] = kind
		}
	}
	ast.Inspect(root, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.AssignStmt:
			for _, lhs := range n.Lhs {
				mark(lhs, n.Tok.String())
			}
			if len(n.Lhs) == len(n.Rhs) {
				for i, rhs := range n.Rhs {
					id, isIdent := n.Lhs[i].(*ast.Ident)
					if !isIdent {
						continue
					}
					if call, ok := layoutUnparen(rhs).(*ast.CallExpr); ok && layoutCallName(call) == "publishRowCapacity" {
						bound[call] = id.Name
					}
					if layoutSelectorName(rhs) == "activeSegments" {
						aliases[id.Name] = true
					}
				}
			}
		case *ast.IncDecStmt:
			mark(n.X, n.Tok.String())
		case *ast.RangeStmt:
			if n.Tok == token.ASSIGN {
				mark(n.Key, "range assignment")
				mark(n.Value, "range assignment")
			}
		case *ast.UnaryExpr:
			if n.Op == token.AND {
				mark(n.X, "address-of")
			}
		case *ast.CallExpr:
			calls[layoutUnparen(n.Fun)] = true
		case *ast.ExprStmt:
			if call, ok := layoutUnparen(n.X).(*ast.CallExpr); ok {
				discarded[call] = true
			}
		case *ast.CompositeLit:
			typ := ""
			if id, ok := n.Type.(*ast.Ident); ok {
				typ = id.Name
			}
			for _, elt := range n.Elts {
				kv, ok := elt.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				key, ok := kv.Key.(*ast.Ident)
				if !ok {
					continue
				}
				facade := slices.Contains(layoutFacadeFields, key.Name) || (typ == "activeTranscriptSegments" && key.Name == layoutFacadeSegmentField)
				if facade && !layoutFacadeFuncAllowed(fname) {
					add(kv.Pos(), "facade field %s set in composite literal", key.Name)
				}
				if slices.Contains(layoutBaselineFields, key.Name) {
					res.baselineWriters[fname] = true
					if !slices.Contains(layoutBaselineWriters, fname) {
						add(kv.Pos(), "baseline field %s set in composite literal", key.Name)
					}
				}
			}
		}
		return true
	})

	// Pass 2 checks every reference against the pass-1 classification. It keeps a
	// stack so a legacy reader call can be checked for its enclosing guard.
	selIdents := map[*ast.Ident]bool{}
	var stack []ast.Node
	ast.Inspect(root, func(n ast.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return true
		}
		stack = append(stack, n)
		switch n := n.(type) {
		case *ast.Ident:
			if n.Name == "ensureAllItemsRendered" && !selIdents[n] {
				add(n.Pos(), "identifier reference to ensureAllItemsRendered")
			}
		case *ast.SelectorExpr:
			selIdents[n.Sel] = true
			name := n.Sel.Name
			allowed := layoutFacadeFuncAllowed(fname)
			if slices.Contains(layoutFacadeFields, name) && !allowed {
				add(n.Pos(), "facade field %s touched", name)
			}
			if name == layoutFacadeSegmentField && layoutIsActiveSegments(n.X, aliases) && !allowed {
				add(n.Pos(), "facade field activeSegments.start touched")
			}
			if kind, ok := writes[n]; ok && slices.Contains(layoutBaselineFields, name) {
				res.baselineWriters[fname] = true
				if !slices.Contains(layoutBaselineWriters, fname) {
					add(n.Pos(), "baseline field %s written (%s)", name, kind)
				}
			}
			if name == "ensureAllItemsRendered" {
				if calls[n] {
					add(n.Pos(), "production call of ensureAllItemsRendered")
				} else {
					add(n.Pos(), "method value of ensureAllItemsRendered")
				}
			}
			if slices.Contains(layoutRowMirrorHelpers, name) {
				switch {
				case !calls[n]:
					add(n.Pos(), "row mirror helper %s referenced without a call", name)
				case !slices.Contains(layoutRowMirrorCallers, fname):
					add(n.Pos(), "unexpected caller of %s", name)
				default:
					if res.rowCallers[name] == nil {
						res.rowCallers[name] = map[string]bool{}
					}
					res.rowCallers[name][fname] = true
				}
			}
			if layoutIsLegacyReader(name) && !calls[n] {
				add(n.Pos(), "legacy reader %s referenced as a value", name)
			}
		case *ast.CallExpr:
			name := layoutCallName(n)
			if layoutIsLegacyReader(name) {
				if layoutLegacyGuarded(stack, n) {
					res.legacyReads++
				} else {
					add(n.Pos(), "legacy reader %s called outside an if %s guard", name, layoutLegacyGuard)
				}
			}
			if name == "publishRowCapacity" && !discarded[n] && bound[n] == "" {
				add(n.Pos(), "publishRowCapacity result is not bound to a local")
			}
		}
		return true
	})

	// The bound result may reach production conditions only at the pinned count.
	names := map[string]bool{}
	for _, name := range bound {
		names[name] = true
	}
	for name := range names {
		if uses, want := layoutConditionUses(root, name), layoutRowCapacityConditionUses[fname]; uses != want {
			add(root.Pos(), "publishRowCapacity result %s used in %d production conditions, want %d", name, uses, want)
		}
	}
}

// layoutLegacyGuarded reports whether call sits in the body of an if whose
// condition requires legacyFacadeMode, or in the right operand of an && whose left
// operand does. stack holds the ancestors of call, outermost first.
func layoutLegacyGuarded(stack []ast.Node, call *ast.CallExpr) bool {
	for _, anc := range slices.Backward(stack) {
		switch p := anc.(type) {
		case *ast.IfStmt:
			if layoutWithin(call, p.Body) && layoutRequiresLegacyMode(p.Cond) {
				return true
			}
		case *ast.BinaryExpr:
			if p.Op == token.LAND && layoutWithin(call, p.Y) && layoutRequiresLegacyMode(p.X) {
				return true
			}
		}
	}
	return false
}

// layoutRequiresLegacyMode reports whether a true e implies legacyFacadeMode: the
// call itself, or an && chain with it as a conjunct.
func layoutRequiresLegacyMode(e ast.Expr) bool {
	switch e := layoutUnparen(e).(type) {
	case *ast.CallExpr:
		return layoutCallName(e) == layoutLegacyGuard
	case *ast.BinaryExpr:
		return e.Op == token.LAND && (layoutRequiresLegacyMode(e.X) || layoutRequiresLegacyMode(e.Y))
	}
	return false
}

// layoutConditionUses counts the distinct identifiers named name that sit inside a
// production condition: an if or for condition, a switch tag, a case list, or a
// comparison.
func layoutConditionUses(root ast.Node, name string) int {
	seen := map[token.Pos]bool{}
	mark := func(e ast.Node) {
		if e == nil {
			return
		}
		ast.Inspect(e, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok && id.Name == name {
				seen[id.Pos()] = true
			}
			return true
		})
	}
	ast.Inspect(root, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.IfStmt:
			mark(n.Cond)
		case *ast.ForStmt:
			mark(n.Cond)
		case *ast.SwitchStmt:
			mark(n.Tag)
		case *ast.CaseClause:
			for _, e := range n.List {
				mark(e)
			}
		case *ast.BinaryExpr:
			switch n.Op {
			case token.EQL, token.NEQ, token.LSS, token.LEQ, token.GTR, token.GEQ:
				mark(n)
			}
		}
		return true
	})
	return len(seen)
}

func layoutIsLegacyReader(name string) bool {
	return strings.HasPrefix(name, "legacy") && name != layoutLegacyGuard
}

// layoutIsActiveSegments reports whether x denotes the activeSegments record: a
// selector named activeSegments, or a local bound directly to one.
func layoutIsActiveSegments(x ast.Expr, aliases map[string]bool) bool {
	switch x := layoutUnparen(x).(type) {
	case *ast.SelectorExpr:
		return x.Sel.Name == "activeSegments"
	case *ast.Ident:
		return aliases[x.Name]
	}
	return false
}

func layoutWithin(n, outer ast.Node) bool {
	return outer.Pos() <= n.Pos() && n.End() <= outer.End()
}

func layoutUnparen(e ast.Expr) ast.Expr {
	for {
		p, ok := e.(*ast.ParenExpr)
		if !ok {
			return e
		}
		e = p.X
	}
}

// layoutSelectorName returns the selector name of e, or "" when e is not a selector.
func layoutSelectorName(e ast.Expr) string {
	if sel, ok := layoutUnparen(e).(*ast.SelectorExpr); ok {
		return sel.Sel.Name
	}
	return ""
}

func layoutCallName(call *ast.CallExpr) string {
	return layoutSelectorName(call.Fun)
}

// layoutBoundaryPackage parses every non-test source file of this package.
func layoutBoundaryPackage(t *testing.T) (*token.FileSet, []*ast.File) {
	t.Helper()
	names, err := filepath.Glob("*.go")
	require.NoError(t, err)
	fset := token.NewFileSet()
	var files []*ast.File
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		require.NoError(t, err)
		files = append(files, file)
	}
	require.NotEmpty(t, files)
	return fset, files
}

func TestLayoutBoundaryAllowlistsAreEnforced(t *testing.T) {
	fset, files := layoutBoundaryPackage(t)
	res := layoutBoundaryScan(fset, files)
	for _, v := range res.violations {
		t.Log(v)
	}
	require.Empty(t, res.violations)
}

// TestLayoutBoundaryDetectorSeesProduction checks that the scan is looking at the
// real code: the row helpers have the callers we expect, the baseline writers are
// seen writing, and legacy readers are proven guarded.
func TestLayoutBoundaryDetectorSeesProduction(t *testing.T) {
	fset, files := layoutBoundaryPackage(t)
	res := layoutBoundaryScan(fset, files)
	for helper, callers := range res.rowCallers {
		for caller := range callers {
			t.Logf("%s called from %s", helper, caller)
		}
	}
	require.True(t, res.rowCallers["clearRows"]["publishFacade"])
	require.True(t, res.rowCallers["publishRowCapacity"]["publishFacade"])
	require.True(t, res.baselineWriters["applyGeometryDelta"])
	require.True(t, res.baselineWriters["updateScrollState"])
	require.Positive(t, res.legacyReads)
	t.Logf("legacy reader call sites proven under %s: %d", layoutLegacyGuard, res.legacyReads)
}

// TestLayoutBoundaryDetectorSelfTest runs the same scan over inline snippets so
// each check is shown to fire on the pattern it targets and to stay quiet on the
// allowed form.
func TestLayoutBoundaryDetectorSelfTest(t *testing.T) {
	const head = "package messages\n\n"
	cases := []struct {
		name string
		src  string
		want int
		msg  string // substring of at least one violation, when want > 0
	}{
		// ensureAllItemsRendered: no call, no method value, no argument, no bare ident.
		{"ensure call", `func f(m *model) { m.ensureAllItemsRendered() }`, 1, "production call of ensureAllItemsRendered"},
		{"ensure defer", `func f(m *model) { defer m.ensureAllItemsRendered() }`, 1, "production call of ensureAllItemsRendered"},
		{"ensure method value", `func f(m *model) { g := m.ensureAllItemsRendered; g() }`, 1, "method value of ensureAllItemsRendered"},
		{"ensure as argument", `func f(m *model) { run(m.ensureAllItemsRendered) }`, 1, "method value of ensureAllItemsRendered"},
		{"ensure bare identifier", `func f() { ensureAllItemsRendered := 1; _ = ensureAllItemsRendered }`, 2, "identifier reference to ensureAllItemsRendered"},
		{"ensure definition only", `func (m *model) ensureAllItemsRendered() { m.publishFacade(true) }`, 0, ""},

		// Facade (C) touches, direct and through an activeSegments alias.
		{"facade read outside allowlist", `func f(m *model) int { return m.totalHeight }`, 1, "facade field totalHeight touched"},
		{"facade read in publishFacade", `func (m *model) publishFacade(full bool) { _ = m.totalHeight }`, 0, ""},
		{"facade write in updateScrollState", `func (m *model) updateScrollState() { m.lineOffsets = nil }`, 1, "facade field lineOffsets touched"},
		{"legacy reader not in list", `func (m *model) legacyExtra() int { return m.totalHeight }`, 1, "facade field totalHeight touched"},
		{"activeSegments start via alias", `func f(m *model) { a := m.activeSegments; a.start = 3 }`, 1, "activeSegments.start touched"},
		{"activeSegments start direct", `func f(m *model) int { return m.activeSegments.start }`, 1, "activeSegments.start touched"},
		{"activeSegments index via alias allowed", `func f(m *model) int { a := m.activeSegments; return a.index }`, 0, ""},
		{"facade composite start", `func f() *activeTranscriptSegments { return &activeTranscriptSegments{start: 1} }`, 1, "set in composite literal"},
		{"facade composite totalHeight", `func f() *model { return &model{totalHeight: 1} }`, 1, "facade field totalHeight set in composite literal"},

		// Reader baseline (R) writes: assignment, compound, increment, address, paren, range.
		{"baseline assign", `func f(m *model) { m.followTotal = 1 }`, 1, "baseline field followTotal written"},
		{"baseline compound", `func f(m *model) { m.followTotal += 1 }`, 1, "baseline field followTotal written"},
		{"baseline decrement", `func f(m *model) { m.followTotal-- }`, 1, "baseline field followTotal written"},
		{"baseline address", `func f(m *model) { p := &m.epochPending; _ = p }`, 1, "baseline field epochPending written"},
		{"baseline paren", `func f(m *model) { (m.epochPending) = true }`, 1, "baseline field epochPending written"},
		{"baseline range", `func f(m *model, xs []int) { for m.followTotal = range xs {} }`, 1, "baseline field followTotal written"},
		{"baseline composite", `func f() *model { return &model{followTotal: 1} }`, 1, "baseline field followTotal set in composite literal"},
		{"baseline package-level", `var _ = func(m *model) { m.followTotal = 1 }`, 1, "<package>"},
		{"baseline read", `func f(m *model) int { return m.followTotal }`, 0, ""},
		{"baseline in applyGeometryDelta", `func (m *model) applyGeometryDelta(r geometryReason, n int) { m.epochPending = true; m.followTotal += n }`, 0, ""},
		{"baseline in updateScrollState", `func (m *model) updateScrollState() { m.followTotal = m.layoutTotal() }`, 0, ""},

		// Legacy readers: every call under legacyFacadeMode, no method values.
		{"legacy unguarded", `func f(m *model) int { return m.legacyTotal() }`, 1, "legacy reader legacyTotal called outside"},
		{"legacy guarded if", `func f(m *model) int { if m.legacyFacadeMode() { return m.legacyTotal() }; return 0 }`, 0, ""},
		{"legacy guarded and", `func f(m *model) bool { return m.legacyFacadeMode() && m.legacyOffsetCount() > 0 }`, 0, ""},
		{"legacy else branch", `func f(m *model) int { if m.legacyFacadeMode() { return 0 } else { return m.legacyTotal() } }`, 1, "legacy reader legacyTotal called outside"},
		{"legacy negated guard", `func f(m *model) int { if !m.legacyFacadeMode() { return m.legacyTotal() }; return 0 }`, 1, "legacy reader legacyTotal called outside"},
		{"legacy or guard", `func f(m *model) bool { return m.legacyFacadeMode() || m.legacyOffsetCount() > 0 }`, 1, "legacy reader legacyOffsetCount called outside"},
		{"legacy method value", `func f(m *model) { g := m.legacyRow; _ = g }`, 1, "legacy reader legacyRow referenced as a value"},

		// Row mirror helpers: named callers only, no method values, result pinned.
		{"helper from other function", `func f(m *model) { m.clearRows(renderedItemRange{}) }`, 1, "unexpected caller of clearRows"},
		{"helper from dropEntry", `func (m *model) dropEntry(i int) { m.clearRows(renderedItemRange{}) }`, 0, ""},
		{"helper method value", `func (m *model) dropEntry(i int) { g := m.clearRows; _ = g }`, 1, "row mirror helper clearRows referenced without a call"},
		{"clearRows returns a value", `func (m *model) clearRows(e renderedItemRange) int { return 0 }`, 1, "clearRows returns a value"},
		{"publishRowCapacity result leaked", `func (m *model) publishFacade(full bool) { if m.publishRowCapacity(1, full) > 0 {} }`, 1, "not bound to a local"},
		{"publishRowCapacity discarded", `func (m *model) publishFacade(full bool) { m.publishRowCapacity(1, full) }`, 0, ""},
		{"publishRowCapacity pinned use", `func (m *model) publishFacade(full bool) { rows := m.publishRowCapacity(1, full); if rows > 0 { _ = rows } }`, 0, ""},
		{"publishRowCapacity extra use", `func (m *model) publishFacade(full bool) { rows := m.publishRowCapacity(1, full); if rows > 0 {}; if rows < 9 {} }`, 1, "publishRowCapacity result rows used in 2 production conditions"},
		{"publishRowCapacity in other caller", `func f(m *model) { rows := m.publishRowCapacity(1, true); if rows > 0 {} }`, 2, "unexpected caller of publishRowCapacity"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, "snippet.go", head+tc.src, parser.SkipObjectResolution)
			require.NoError(t, err)
			res := layoutBoundaryScan(fset, []*ast.File{file})
			require.Len(t, res.violations, tc.want, "violations: %v", res.violations)
			if tc.want > 0 {
				found := false
				for _, v := range res.violations {
					found = found || strings.Contains(v, tc.msg)
				}
				require.True(t, found, "no violation contains %q: %v", tc.msg, res.violations)
			}
		})
	}
}

// Hot-path guard for the work and identity indices. A hot function (View, layout,
// mouse, wheel, hover, tick, copy, selection, reply handoff) must find one block or
// one message through an index, never walk the work maps or rebuild an index.
var (
	// hotWorkRangeFields are the work maps whose range loops are all-history.
	hotWorkRangeFields = map[string]bool{"workHeaderBySource": true, "workSourceToHeader": true, "workRanges": true, "workAnchorByBlock": true}
	// workRangeStructural may range over those maps: the dirty-gated projection rebuild,
	// the status transition, the compact-off reset, the reverse-map builder, and the
	// explicit diagnostic materializer.
	workRangeStructural = map[string]bool{"syncWorkProjection": true, "SetWorkStatus": true, "restoreWorkPlain": true, "setWorkHeaders": true, "workRangeList": true, "PrependHistory": true, "ReplaceHistoryItem": true}
	// workRangeListCallers may call the full materializer. No production caller remains.
	workRangeListCallers = map[string]bool{}
	// identityBuilders are the full index builds. No epoch may reach them.
	identityBuilders = []string{"setWorkHeaders", "indexProjectionOwners", "indexMessages"}
	// epochRoots are the functions every width, theme or invalidation epoch runs.
	epochRoots = []string{"invalidateAllItems", "InvalidateRenderCaches", "applyGeometryDelta", "dropMaterialized"}
	// indexZeroRebuilds allows structural writes that already walk or shift the transcript.
	indexZeroRebuilds = map[string]bool{"removePendingToolCallMessages": true, "PrependHistory": true}
	// noIndexBuild are the hot functions that may never call indexMessages directly:
	// the identity lookup itself, and everything on the View, mouse, hover, scroll and
	// epoch paths.
	noIndexBuild = map[string]bool{
		"messagePointerIndex": true, "View": true, "layoutViewport": true, "trackPointerHover": true,
		"handleMouseMotion": true, "handleMouseClick": true, "handleMouseRelease": true,
		"globalLineToMessageLine": true, "globalLineToMessageLineCached": true, "globalLineToMessageLineCanonical": true,
		"settleHoverAnchor": true, "invalidateAllItems": true, "applyGeometryDelta": true, "dropMaterialized": true,
		"InvalidateRenderCaches": true, "renderedLineAt": true, "refreshRenderedItem": true, "measureSlot": true, "syncSlots": true,
	}
)

// messageWriteSites is the number of production assignments to m.messages. The test
// pins it, so a new unhooked write site cannot appear without a test change.
const messageWriteSites = 24

// workRangeGuardScan returns the hot-path violations of the package and the number of
// production writes to m.messages it checked. It flags all-history range loops over
// the work maps, workRangeList callers, index builds reachable from an epoch, direct
// indexMessages calls from hot functions, and message-list writes that are not
// followed by a re-index (or an explicit nil reset of the index) in their function.
func workRangeGuardScan(fset *token.FileSet, files []*ast.File) (violations []string, writeSites int) {
	var out []string
	calls := map[string]map[string]bool{}
	callPos := map[string][]token.Pos{}
	for _, file := range files {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			name := fn.Name.Name
			calls[name] = map[string]bool{}
			if name == "indexAppendedSuffix" && !startsAtMarker(fn.Body) {
				out = append(out, fmt.Sprintf("%s: indexAppendedSuffix does not start at messageIndexLen", fset.Position(fn.Pos())))
			}
			// The call set is complete before the write checks, so a hook that follows
			// the write in the same function counts.
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if x, ok := n.(*ast.CallExpr); ok {
					switch f := x.Fun.(type) {
					case *ast.Ident:
						calls[name][f.Name] = true
						callPos[name+"\x00"+f.Name] = append(callPos[name+"\x00"+f.Name], x.Pos())
					case *ast.SelectorExpr:
						calls[name][f.Sel.Name] = true
						callPos[name+"\x00"+f.Sel.Name] = append(callPos[name+"\x00"+f.Sel.Name], x.Pos())
						if f.Sel.Name == "workRangeList" && !workRangeListCallers[name] {
							out = append(out, fmt.Sprintf("%s: %s calls workRangeList (full work walk) outside the allowlist", fset.Position(x.Pos()), name))
						}
						if f.Sel.Name == "indexMessages" && noIndexBuild[name] {
							out = append(out, fmt.Sprintf("%s: %s calls indexMessages from a hot function", fset.Position(x.Pos()), name))
						}
						if f.Sel.Name == "indexMessages" && len(x.Args) == 1 {
							if lit, ok := x.Args[0].(*ast.BasicLit); ok && lit.Value == "0" && !indexZeroRebuilds[name] {
								out = append(out, fmt.Sprintf("%s: %s rebuilds the identity index from 0 outside the rebuild allowlist", fset.Position(x.Pos()), name))
							}
						}
						if f.Sel.Name == "indexAppendedSuffix" && name != "messagePointerIndex" {
							out = append(out, fmt.Sprintf("%s: %s calls indexAppendedSuffix; only messagePointerIndex may catch up the tail", fset.Position(x.Pos()), name))
						}
					}
				}
				return true
			})
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.RangeStmt:
					if sel, ok := x.X.(*ast.SelectorExpr); ok && hotWorkRangeFields[sel.Sel.Name] && !workRangeStructural[name] {
						out = append(out, fmt.Sprintf("%s: %s ranges over %s (all-history work walk) outside the structural allowlist", fset.Position(x.Pos()), name, sel.Sel.Name))
					}
				case *ast.AssignStmt:
					for _, lhs := range x.Lhs {
						sel, ok := lhs.(*ast.SelectorExpr)
						if !ok || sel.Sel.Name != "messages" {
							continue
						}
						if x.Tok != token.ASSIGN {
							continue
						}
						writeSites++
						if !writeFollowedByIndex(fn.Body, x.Pos(), name, callPos) {
							out = append(out, fmt.Sprintf("%s: %s writes m.messages with no re-index after it in the same function", fset.Position(x.Pos()), name))
						}
					}
				}
				return true
			})
		}
	}
	// Reachability from the epoch roots, by function name (an over-approximation).
	reached := map[string]bool{}
	queue := slices.Clone(epochRoots)
	for len(queue) > 0 {
		next := queue[0]
		queue = queue[1:]
		if reached[next] {
			continue
		}
		reached[next] = true
		for callee := range calls[next] {
			if _, known := calls[callee]; known {
				queue = append(queue, callee)
			}
		}
	}
	for _, builder := range identityBuilders {
		if reached[builder] {
			out = append(out, "epoch reaches index build "+builder)
		}
	}
	slices.Sort(out)
	return out, writeSites
}

// startsAtMarker reports whether the body's loop starts at messageIndexLen, so the
// catch-up never rewrites the indexed prefix.
func startsAtMarker(body ast.Node) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		if fs, ok := n.(*ast.ForStmt); ok {
			if as, ok := fs.Init.(*ast.AssignStmt); ok && len(as.Rhs) == 1 {
				if sel, ok := as.Rhs[0].(*ast.SelectorExpr); ok && sel.Sel.Name == "messageIndexLen" {
					found = true
				}
			}
		}
		return true
	})
	return found
}

// writeFollowedByIndex reports whether a write at pos is followed, in the same
// function, by a call to indexMessages, or by the explicit nil reset of the index
// (an assignment to messageIndex).
func writeFollowedByIndex(body ast.Node, pos token.Pos, name string, callPos map[string][]token.Pos) bool {
	for _, p := range callPos[name+"\x00indexMessages"] {
		if p > pos {
			return true
		}
	}
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		if as, ok := n.(*ast.AssignStmt); ok && as.Pos() > pos {
			for _, lhs := range as.Lhs {
				if s, ok := lhs.(*ast.SelectorExpr); ok && s.Sel.Name == "messageIndex" {
					found = true
				}
			}
		}
		return true
	})
	return found
}

func TestHotPathsDoNotWalkWorkIndices(t *testing.T) {
	fset, files := layoutBoundaryPackage(t)
	violations, sites := workRangeGuardScan(fset, files)
	for _, v := range violations {
		t.Log(v)
	}
	require.Empty(t, violations)
	require.Equal(t, messageWriteSites, sites, "every production write to m.messages is checked")
}

// TestHotPathGuardDetectsViolations proves the guard fires on each rule, so a clean
// result means the package is clean rather than the scan being blind.
func TestHotPathGuardDetectsViolations(t *testing.T) {
	src := `package messages
func (m *model) hover() {
	for k := range m.workSourceToHeader { _ = k }
	m.workRangeList()
}
func (m *model) invalidateAllItems() { m.setWorkHeaders(nil) }
func (m *model) setWorkHeaders(h map[int]int) {}
func (m *model) appendMessage(msg int) { m.messages = append(m.messages, msg) }
func (m *model) appendIndexed(msg int) { m.messages = append(m.messages, msg); m.indexMessages(len(m.messages) - 1) }
func (m *model) messagePointerIndex(msg int) int { m.indexMessages(0); m.indexAppendedSuffix(); return -1 }
func (m *model) indexAppendedSuffix() {
	for i := 0; i < len(m.messages); i++ { _ = i }
}
func (m *model) View() { m.indexAppendedSuffix() }
func (m *model) other() { m.indexMessages(0) }
func (m *model) workRangeList() {}
`
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "synthetic.go", src, parser.SkipObjectResolution)
	require.NoError(t, err)
	violations, _ := workRangeGuardScan(fset, []*ast.File{file})
	got := strings.Join(violations, "\n")
	require.Contains(t, got, "ranges over workSourceToHeader")
	require.Contains(t, got, "calls workRangeList")
	require.Contains(t, got, "epoch reaches index build setWorkHeaders")
	require.Contains(t, got, "appendMessage writes m.messages with no re-index")
	require.Contains(t, got, "messagePointerIndex calls indexMessages from a hot function")
	require.Contains(t, got, "rebuilds the identity index from 0")
	require.Contains(t, got, "only messagePointerIndex may catch up the tail")
	require.Contains(t, got, "indexAppendedSuffix does not start at messageIndexLen")
	require.NotContains(t, got, "appendIndexed writes", "a write followed by its re-index is clean")
}
