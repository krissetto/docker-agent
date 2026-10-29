import SwiftUI
import AgentKit

/// One word per agent, read the same way in the list, the map and the inspector header.
/// Attention is orange and nothing else is; working is the app tint; everything quiet is secondary.
/// `needsAttention` is the server's word on whether a human is needed; a waiting state alone
/// (paused, waiting on a child or tool) stays "Waiting".
enum TeamState {
    case needsYou, working, stopping, queued, waiting, idle, done, failed, unknown

    init(_ node: AgentNodeProjection) {
        if node.needsAttention { self = .needsYou; return }
        switch node.state {
        case .running: self = .working
        case .cancelling: self = .stopping
        case .queued: self = .queued
        case .waiting: self = .waiting
        case .idle: self = .idle
        case .settled: self = .done
        case .failed: self = .failed
        case .unknown: self = .unknown
        }
    }

    var word: String {
        switch self {
        case .needsYou: "Needs you"
        case .working: "Working"
        case .stopping: "Stopping"
        case .queued: "Queued"
        case .waiting: "Waiting"
        case .idle: "Idle"
        case .done: "Done"
        case .failed: "Failed"
        case .unknown: "Not reported"
        }
    }

    /// Harbor is used directly: the accent colour does not pick up the app's `.tint()` inside a
    /// Canvas shading on iOS (verified on device: current card and live edges rendered grey).
    var tint: Color {
        switch self {
        case .needsYou: Palette.signal
        case .working, .stopping, .queued: Palette.harbor
        case .failed: .red
        case .waiting, .idle, .done, .unknown: .secondary
        }
    }

    /// Quiet agents recede so the eye lands on who is acting or asking.
    var isQuiet: Bool {
        switch self {
        case .idle, .done, .unknown: true
        default: false
        }
    }
}

/// Counts partition the tree the app was given, so the sentence is honest about every shown agent
/// and never shows a zero. The tree may include the root; it is only what the server reported for
/// this session, never a claim about every agent on the server.
/// `working` is strictly the running state; stopping and queued agents are counted on their own,
/// and `unknown` is the part of the shown tree whose state the server has not reported.
struct TeamTally {
    var needsYou = 0
    var working = 0
    var stopping = 0
    var queued = 0
    var waiting = 0
    var failed = 0
    var done = 0
    var idle = 0
    var unknown = 0
    /// Shown agents with a session that can be opened from this device.
    var attachable = 0
    /// Shown agents other than the roots that are strictly running.
    var runningSubagents = 0
    /// Shown agents other than the roots.
    var subagents = 0
    /// Every agent in the shown tree, roots included.
    var total: Int { needsYou + working + stopping + queued + waiting + failed + done + idle + unknown }
    /// Shown agents whose state the server reported; not server-wide coverage.
    var withReportedState: Int { total - unknown }

    init(_ roots: [AgentNodeProjection]) {
        var stack = roots.map { ($0, true) }
        while let (node, isRoot) = stack.popLast() {
            switch TeamState(node) {
            case .needsYou: needsYou += 1
            case .working: working += 1
            case .stopping: stopping += 1
            case .queued: queued += 1
            case .waiting: waiting += 1
            case .failed: failed += 1
            case .done: done += 1
            case .idle: idle += 1
            case .unknown: unknown += 1
            }
            if node.session != nil { attachable += 1 }
            if !isRoot {
                subagents += 1
                if node.state == .running { runningSubagents += 1 }
            }
            stack.append(contentsOf: node.children.map { ($0, false) })
        }
    }

    /// For the session activity line: "2 of 4 sub-agents running", counting only the shown tree.
    var subagentLine: String? {
        guard subagents > 0 else { return nil }
        return "\(runningSubagents) of \(subagents) sub-agent\(subagents == 1 ? "" : "s") running"
    }

    var title: String { total == 1 ? "1 agent" : "\(total) agents" }

    var sentence: String {
        var parts: [String] = []
        if needsYou > 0 { parts.append(needsYou == 1 ? "1 needs you" : "\(needsYou) need you") }
        if working > 0 { parts.append("\(working) working") }
        if stopping > 0 { parts.append("\(stopping) stopping") }
        if queued > 0 { parts.append("\(queued) queued") }
        if waiting > 0 { parts.append("\(waiting) waiting") }
        if failed > 0 { parts.append("\(failed) failed") }
        if done > 0 { parts.append("\(done) done") }
        if idle > 0 { parts.append("\(idle) idle") }
        if unknown > 0 { parts.append(unknown == 1 ? "1 not reported" : "\(unknown) not reported") }
        return parts.isEmpty ? "No agents yet" : parts.joined(separator: ", ")
    }
}

extension Array where Element == AgentNodeProjection {
    /// Which shown node stands for the open session. A node whose session matches wins; otherwise the
    /// server's root often carries no session id, so a single root with this session's agent name is
    /// accepted. Ambiguous (several matching roots) or no match returns nil. Display only, never an attach right.
    func currentNodeID(for ref: SessionRef, agentName: String) -> String? {
        if let match = first(where: { $0.session == ref }) { return match.id }
        // The server names a tree's root node "root:<session id>" (subagent.SessionRootID), an exact identity.
        if let match = first(where: { $0.id == "root:\(ref.sessionID)" }) { return match.id }
        let unnamedRoots = filter { $0.session == nil && $0.agentName == agentName }
        return unnamedRoots.count == 1 ? unnamedRoots[0].id : nil
    }
}

extension AgentNodeProjection {
    /// Second line for rows and the selection bar: the state word, with the server's reason when it gives one.
    var teamDetail: String {
        let word = TeamState(self).word
        guard let waitingOn, !waitingOn.isEmpty else { return word }
        return "\(word): \(waitingOn)"
    }
}

struct TeamTreeLayout {
    struct Item {
        let node: AgentNodeProjection
        let parent: Int?
        let depth: Int
        var children: [Int] = []
        var leafStart = 0
        var leafEnd = 0
        var centerLeaf = 0.0
    }

    let nodes: [Item]
    let points: [CGPoint]
    let size: CGSize
    let cardSize: CGSize

    static func build(roots: [AgentNodeProjection], cardSize: CGSize, columnSpacing: CGFloat, rowSpacing: CGFloat, inset: CGFloat) -> TeamTreeLayout {
        var nodes: [Item] = []
        var stack = roots.reversed().map { ($0, Optional<Int>.none, 0) }
        while let (node, parent, depth) = stack.popLast() {
            let index = nodes.count
            nodes.append(Item(node: node, parent: parent, depth: depth))
            if let parent { nodes[parent].children.append(index) }
            for child in node.children.reversed() { stack.append((child, index, depth + 1)) }
        }

        var nextLeaf = 0
        for index in nodes.indices.reversed() {
            let children = nodes[index].children
            if children.isEmpty {
                nodes[index].leafStart = nextLeaf
                nodes[index].leafEnd = nextLeaf
                nodes[index].centerLeaf = Double(nextLeaf)
                nextLeaf += 1
            } else {
                nodes[index].leafStart = nodes[children[0]].leafStart
                nodes[index].leafEnd = nodes[children[children.count - 1]].leafEnd
                nodes[index].centerLeaf = (Double(nodes[index].leafStart) + Double(nodes[index].leafEnd)) / 2
            }
        }

        let leaves = max(1, nextLeaf)
        let depth = max(0, nodes.map(\.depth).max() ?? 0)
        let size = CGSize(width: inset * 2 + CGFloat(leaves - 1) * columnSpacing + cardSize.width,
                          height: inset * 2 + CGFloat(depth) * rowSpacing + cardSize.height)
        let points = nodes.map { item in
            CGPoint(x: inset + CGFloat(item.centerLeaf) * columnSpacing + cardSize.width / 2,
                    y: inset + CGFloat(item.depth) * rowSpacing + cardSize.height / 2)
        }
        return TeamTreeLayout(nodes: nodes, points: points, size: size, cardSize: cardSize)
    }

    /// Screen-space frame of a card at `zoom`, grown to `minimum` so assistive taps always land on it.
    /// Rows and columns are spaced wider than the minimum, so grown frames never cover a neighbour.
    func accessibilityFrame(at index: Int, zoom: CGFloat, minimum: CGFloat = 44) -> CGRect {
        let center = CGPoint(x: points[index].x * zoom, y: points[index].y * zoom)
        let size = CGSize(width: max(minimum, cardSize.width * zoom), height: max(minimum, cardSize.height * zoom))
        return CGRect(x: center.x - size.width / 2, y: center.y - size.height / 2, width: size.width, height: size.height)
    }

    func hitTest(_ point: CGPoint, zoom: CGFloat) -> AgentNodeProjection? {
        guard zoom.isFinite, zoom > 0 else { return nil }
        let unscaled = CGPoint(x: point.x / zoom, y: point.y / zoom)
        guard unscaled.x.isFinite, unscaled.y.isFinite else { return nil }
        guard let index = nodes.indices.reversed().first(where: { index in
            let center = points[index]
            return abs(unscaled.x - center.x) <= cardSize.width / 2 && abs(unscaled.y - center.y) <= cardSize.height / 2
        }) else { return nil }
        return nodes[index].node
    }

    func node(id: String?) -> AgentNodeProjection? {
        guard let id else { return nil }
        return nodes.first { $0.node.id == id }?.node
    }
}

/// Team topology on one GPU canvas. Tapping a card selects it; the bar beneath carries the actions,
/// so opening or stopping an agent always goes through its confirmation.
struct TeamTreeMap: View {
    let roots: [AgentNodeProjection]
    let currentSession: SessionRef
    /// The node standing for this session when the server's root carries no session id.
    var currentID: String? = nil
    var canStop = false
    let onInspect: (AgentNodeProjection) -> Void
    let onOpen: (AgentNodeProjection) -> Void
    var onStop: ((AgentNodeProjection) -> Void)? = nil

    @Environment(\.accessibilityReduceMotion) private var reduceMotion
    @State private var zoom = 1.0
    @State private var pinchBase: Double?
    @State private var userZoomed = false
    @State private var viewport = CGSize.zero
    @State private var scrollPosition = ScrollPosition()
    @State private var selectedID: String?
    /// Agent under a long press; its actions open in a sheet, mirroring the selection bar.
    @State private var contextNode: AgentNodeProjection?

    private let cardSize = CGSize(width: 164, height: 58)
    private let columnSpacing: CGFloat = 184
    private let rowSpacing: CGFloat = 100
    private let inset: CGFloat = 20
    private let zoomRange = 0.35...1.6
    /// Fit never shrinks text past legibility; wider teams scroll, starting on this session's card.
    private let fitFloor = 0.55
    /// Height the zoom pill occupies at the bottom edge; kept clear of the tree.
    private static let controlReserve: CGFloat = 48

    private func isCurrent(_ node: AgentNodeProjection) -> Bool {
        node.session == currentSession || node.id == currentID
    }

    private var layout: TeamTreeLayout {
        TeamTreeLayout.build(roots: roots, cardSize: cardSize, columnSpacing: columnSpacing, rowSpacing: rowSpacing, inset: inset)
    }

    var body: some View {
        let map = layout
        let selected = map.node(id: selectedID)
        VStack(spacing: 0) {
            canvas(map)
                .overlay(alignment: .bottomTrailing) { zoomControls(map) }
            if let selected {
                Divider()
                SelectedAgentBar(node: selected, isCurrent: isCurrent(selected), canStop: canStop,
                                 onInspect: { onInspect(selected) }, onOpen: { onOpen(selected) }, onStop: onStop.map { stop in { stop(selected) } })
                    .transition(reduceMotion ? .opacity : .move(edge: .bottom).combined(with: .opacity))
            }
        }
        .animation(reduceMotion ? nil : .snappy(duration: 0.25), value: selectedID)
        .sensoryFeedback(.selection, trigger: selectedID) { (old: String?, new: String?) in new != nil && old != new }
        .sensoryFeedback(.impact(weight: .light), trigger: contextNode?.id) { (_: String?, new: String?) in new != nil }
        .onChange(of: map.size) { refit(map) }
        .confirmationDialog(contextNode?.agentName ?? "", isPresented: contextPresented, titleVisibility: .visible, presenting: contextNode,
                            actions: contextActions, message: contextMessage)
        .accessibilityAction(named: "Zoom to fit") { userZoomed = false; zoom = fitZoom(map) }
    }

    private func refit(_ map: TeamTreeLayout) {
        guard !userZoomed else { return }
        zoom = fitZoom(map)
        revealCurrent(map)
    }

    private var contextPresented: Binding<Bool> {
        Binding(get: { contextNode != nil }, set: { if !$0 { contextNode = nil } })
    }

    /// Long-press actions mirror the selection bar; opening and stopping still go through their confirmations.
    @ViewBuilder
    private func contextActions(_ node: AgentNodeProjection) -> some View {
        Button("Inspect") { onInspect(node) }
        if node.session != nil, !isCurrent(node) {
            Button("Open Session…") { onOpen(node) }
        }
        if canStop, let onStop, node.session != nil, node.state.isActive {
            Button("Stop…", role: .destructive) { onStop(node) }
        }
    }

    private func contextMessage(_ node: AgentNodeProjection) -> Text {
        Text(isCurrent(node) ? "This session" : node.teamDetail)
    }

    private func longPress(_ map: TeamTreeLayout) -> some Gesture {
        LongPressGesture(minimumDuration: 0.4)
            .sequenced(before: DragGesture(minimumDistance: 0, coordinateSpace: .local))
            .onEnded { value in
                guard case .second(true, let drag?) = value, let node = map.hitTest(drag.startLocation, zoom: zoom) else { return }
                selectedID = node.id
                contextNode = node
            }
    }

    private func canvas(_ map: TeamTreeLayout) -> some View {
        ScrollView([.horizontal, .vertical]) {
            Canvas(opaque: false, colorMode: .linear, rendersAsynchronously: true) { context, _ in
                draw(map, in: &context)
            }
            .frame(width: map.size.width, height: map.size.height)
            .scaleEffect(zoom, anchor: .topLeading)
            .frame(width: map.size.width * zoom, height: map.size.height * zoom, alignment: .topLeading)
            // Small trees hang from the header like a chart instead of floating mid-sheet; wide ones keep their own size.
            .frame(minWidth: viewport.width, minHeight: viewport.height, alignment: .top)
            .contentShape(Rectangle())
            .gesture(SpatialTapGesture().onEnded { tap in
                selectedID = map.hitTest(tap.location, zoom: zoom)?.id
            })
            .gesture(longPress(map))
            .simultaneousGesture(MagnifyGesture().onChanged { value in
                let base = pinchBase ?? zoom
                pinchBase = base
                userZoomed = true
                zoom = min(zoomRange.upperBound, max(zoomRange.lowerBound, base * value.magnification))
            }.onEnded { _ in pinchBase = nil })
            .accessibilityHidden(true)
            .overlay { accessibilityNodes(map) }
        }
        .accessibilityElement(children: .contain)
        .accessibilityLabel("Team map, \(TeamTally(roots).sentence)")
        .scrollIndicators(.hidden)
        .scrollPosition($scrollPosition)
        .contentMargins(.bottom, Self.controlReserve, for: .scrollContent)
        .onGeometryChange(for: CGSize.self) { $0.size } action: { size in
            viewport = CGSize(width: size.width, height: size.height - Self.controlReserve)
            refit(map)
        }
    }

    /// Beyond this many cards the list view is the assistive path; the map stays a single labelled element.
    private static let accessibilityNodeLimit = 150

    /// Invisible, non-hit-testing elements laid over the GPU canvas so VoiceOver and UI tests get real,
    /// zoom- and scroll-accurate frames per card. They never intercept touches: visible taps still go
    /// through the canvas hit test, so activating one selects exactly the node a finger would.
    @ViewBuilder
    private func accessibilityNodes(_ map: TeamTreeLayout) -> some View {
        if map.nodes.count <= Self.accessibilityNodeLimit {
            ForEach(map.nodes.indices, id: \.self) { index in
                let node = map.nodes[index].node
                let frame = map.accessibilityFrame(at: index, zoom: zoom)
                Color.clear
                    .frame(width: frame.width, height: frame.height)
                    .position(x: frame.midX, y: frame.midY)
                    .accessibilityElement()
                    .accessibilityLabel(node.agentName)
                    .accessibilityValue("\(TeamState(node).word)\(isCurrent(node) ? ", this session" : "")")
                    .accessibilityAddTraits(node.id == selectedID ? [.isButton, .isSelected] : .isButton)
                    .accessibilityIdentifier("teammap.node.\(node.id)")
                    .accessibilityAction { selectedID = node.id }
            }
            .allowsHitTesting(false)
        } else {
            Color.clear
                .accessibilityElement()
                .accessibilityLabel("\(map.nodes.count) agents on the map")
                .accessibilityHint("Switch to the list view to reach each agent.")
                .allowsHitTesting(false)
        }
    }

    private func zoomControls(_ map: TeamTreeLayout) -> some View {
        HStack(spacing: 0) {
            zoomButton("minus", "Zoom out") { nudge(-0.15) }
            zoomButton("arrow.down.left.and.arrow.up.right", "Zoom to fit") { userZoomed = false; zoom = fitZoom(map) }
            zoomButton("plus", "Zoom in") { nudge(0.15) }
        }
        .buttonStyle(.borderless)
        .background { Capsule().fill(.regularMaterial).padding(.vertical, 6) }
        .padding(.horizontal, Metrics.s)
        .padding(.bottom, Metrics.xxs)
    }

    /// The visible pill is 32pt; each glyph still owns a full 44pt touch target.
    private func zoomButton(_ symbol: String, _ label: String, action: @escaping () -> Void) -> some View {
        Button(action: action) {
            Image(systemName: symbol)
                .font(.footnote.weight(.medium))
                .frame(width: 44, height: 44)
                .contentShape(Rectangle())
        }
        .help(label)
        .accessibilityLabel(label)
    }

    private func nudge(_ delta: Double) {
        userZoomed = true
        zoom = min(zoomRange.upperBound, max(zoomRange.lowerBound, zoom + delta))
    }

    /// Fits the whole tree into the visible area, never enlarging past 1:1 or shrinking below legibility.
    private func fitZoom(_ map: TeamTreeLayout) -> Double {
        guard viewport.width > 0, viewport.height > 0 else { return 1 }
        let fit = min(viewport.width / map.size.width, viewport.height / map.size.height)
        guard fit.isFinite else { return 1 }
        return min(1, max(fitFloor, fit))
    }

    /// When the fitted tree still overflows, start with this session's card (or the first root) in view.
    private func revealCurrent(_ map: TeamTreeLayout) {
        guard viewport.width > 0, map.size.width * zoom > viewport.width || map.size.height * zoom > viewport.height else { return }
        let index = map.nodes.firstIndex { isCurrent($0.node) } ?? 0
        guard map.points.indices.contains(index) else { return }
        let center = map.points[index]
        let origin = CGPoint(x: max(0, center.x * zoom - viewport.width / 2), y: max(0, center.y * zoom - viewport.height / 2))
        scrollPosition.scrollTo(point: origin)
    }

    private func draw(_ layout: TeamTreeLayout, in context: inout GraphicsContext) {
        // Live branches carry weight and are drawn last so they sit on top of the quiet structure.
        let edges = layout.nodes.indices.filter { layout.nodes[$0].parent != nil }
        for index in edges.sorted(by: { TeamState(layout.nodes[$0].node).isQuiet && !TeamState(layout.nodes[$1].node).isQuiet }) {
            let from = layout.points[layout.nodes[index].parent!]
            let to = layout.points[index]
            let state = TeamState(layout.nodes[index].node)
            var path = Path()
            let elbow = (from.y + to.y) / 2
            path.move(to: CGPoint(x: from.x, y: from.y + cardSize.height / 2))
            path.addLine(to: CGPoint(x: from.x, y: elbow))
            path.addLine(to: CGPoint(x: to.x, y: elbow))
            path.addLine(to: CGPoint(x: to.x, y: to.y - cardSize.height / 2))
            let live = !state.isQuiet
            context.stroke(path, with: .color(live ? state.tint.opacity(0.55) : .secondary.opacity(0.28)), lineWidth: live ? 1.5 : 1)
        }

        for (index, item) in layout.nodes.enumerated() {
            let node = item.node
            let state = TeamState(node)
            let isCurrent = isCurrent(node)
            let isSelected = node.id == selectedID
            let center = layout.points[index]
            let rect = CGRect(x: center.x - cardSize.width / 2, y: center.y - cardSize.height / 2,
                              width: cardSize.width, height: cardSize.height)
            let shape = Path(roundedRect: rect, cornerRadius: Metrics.chipRadius, style: .continuous)
            let hue = AgentPalette.color(for: node.agentName)

            context.fill(shape, with: .color(isCurrent ? Palette.harbor.opacity(0.12) : Color.primary.opacity(state.isQuiet ? 0.025 : 0.05)))
            if state == .needsYou {
                context.stroke(shape, with: .color(.orange), lineWidth: 1.5)
            } else if isCurrent {
                context.stroke(shape, with: .color(Palette.harbor.opacity(0.6)), lineWidth: 1)
            } else {
                context.stroke(shape, with: .color(Color.primary.opacity(0.08)), lineWidth: 1)
            }
            if isSelected {
                context.stroke(Path(roundedRect: rect.insetBy(dx: -4, dy: -4), cornerRadius: Metrics.chipRadius + 4, style: .continuous),
                               with: .color(Palette.harbor), lineWidth: 2)
            }

            let spine = Path(roundedRect: CGRect(x: rect.minX + 7, y: rect.minY + 9, width: Metrics.gutterWidth, height: rect.height - 18),
                             cornerRadius: Metrics.gutterWidth / 2, style: .continuous)
            context.fill(spine, with: .color(hue.opacity(state.isQuiet ? 0.45 : 1)))

            let textX = rect.minX + 7 + Metrics.gutterWidth + 9
            let title = context.resolve(Text(node.agentName)
                .font(.system(size: 13, weight: isCurrent ? .semibold : .medium))
                .foregroundColor(state.isQuiet ? .secondary : .primary))
            context.draw(title, at: CGPoint(x: textX, y: rect.minY + 18), anchor: .leading)

            let detailText = context.resolve(Text(state.word).font(.system(size: 11)).foregroundColor(state.tint))
            context.draw(detailText, at: CGPoint(x: textX, y: rect.maxY - 17), anchor: .leading)

            if node.cost > 0 {
                let cost = context.resolve(Text(Format.cost(node.cost))
                    .font(.system(size: 10, design: .monospaced).monospacedDigit())
                    .foregroundColor(.secondary))
                context.draw(cost, at: CGPoint(x: rect.maxX - 10, y: rect.minY + 18), anchor: .trailing)
            }
        }
    }
}

private struct SelectedAgentBar: View {
    let node: AgentNodeProjection
    let isCurrent: Bool
    let canStop: Bool
    let onInspect: () -> Void
    let onOpen: () -> Void
    let onStop: (() -> Void)?

    private var state: TeamState { TeamState(node) }

    var body: some View {
        HStack(spacing: Metrics.s) {
            AgentDot(name: node.agentName, size: 10)
            VStack(alignment: .leading, spacing: 1) {
                Text(node.agentName).font(.rowTitle).lineLimit(1)
                Text(isCurrent ? "This session" : node.teamDetail)
                    .font(.rowDetail)
                    .foregroundStyle(isCurrent ? AnyShapeStyle(.secondary) : AnyShapeStyle(state.tint))
                    .lineLimit(1)
            }
            .accessibilityElement(children: .combine)
            Spacer(minLength: Metrics.s)
            if node.cost > 0 {
                Text(Format.cost(node.cost)).font(.monoSmall.monospacedDigit()).foregroundStyle(.secondary)
            }
            action("Inspect", "info.circle", help: "Inspect \(node.agentName)", action: onInspect)
                .accessibilityIdentifier("teammap.inspect.\(node.id)")
            if let ref = node.session, !isCurrent {
                action("Open session", "arrow.right.circle", help: "Open the \(node.agentName) session", action: onOpen)
                    .accessibilityIdentifier("teammap.open.\(node.id).session.\(ref.serverID.uuidString).\(ref.sessionID)")
            }
            if canStop, let onStop, node.session != nil, node.state.isActive {
                action("Stop", "stop.circle", role: .destructive, help: "Stop \(node.agentName)", action: onStop)
                    .accessibilityIdentifier("teammap.stop.\(node.id)")
            }
        }
        .accessibilityIdentifier("teammap.selected.\(node.id)")
        .buttonStyle(.borderless)
        .padding(.leading, Metrics.m)
        .padding(.trailing, Metrics.xs)
        .padding(.vertical, Metrics.xxs)
    }

    /// Icon-only so three actions fit beside a name on a 375pt screen; each one keeps a 44pt target.
    private func action(_ title: String, _ symbol: String, role: ButtonRole? = nil, help: String, action: @escaping () -> Void) -> some View {
        Button(role: role, action: action) {
            Image(systemName: symbol)
                .font(.body)
                .frame(width: 44, height: 44)
                .contentShape(Rectangle())
        }
        .help(help)
        .accessibilityLabel(title)
    }
}
