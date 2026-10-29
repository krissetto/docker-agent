import SwiftUI
import AgentKit

struct TranscriptView: View {
    let sessionRef: SessionRef
    let items: [TranscriptItem]
    var canRetry: Bool
    var onRetry: () -> Void
    var resolveMedia: (GeneratedMediaReference) async throws -> Data
    var matchIDs: Set<String> = []
    var currentMatchID: String?
    var onContinueFromHere: ((TranscriptItem) -> Void)?
    var onBranchHere: ((TranscriptItem) -> Void)?
    /// Rows the client dropped beyond its retention cap; they exist on the server but aren't loaded here.
    var omittedCount = 0
    /// Server-side coverage of the loaded history (bounded tail): what is older, what was too large, what is complete.
    var history = TranscriptWindow()
    var isLoadingOlder = false
    var loadOlderError: String?
    /// Asks the server for the previous history page; nil when paging isn't supported.
    var onLoadOlder: (() async -> Void)?

    /// Rows laid out at once; older rows stay loaded but are revealed on request so long sessions open instantly.
    static let initialWindow = 150
    static let windowStep = 200

    @State private var position = ScrollPosition(edge: .bottom)
    @State private var atBottom = true
    @State private var followsLatest = true
    @State private var userScrollInProgress = false
    @State private var window = TranscriptView.initialWindow
    /// Pending older-page request: where the reader was and how many rows existed, so a prepend can grow the
    /// window by exactly the fetched rows and restore the same turn — unless the reader moved meanwhile.
    @State private var pendingOlder: OlderLoadAnchor?
    @State private var olderTask: Task<Void, Never>?
    @State private var loadedOlderOnce = false

    private struct Row: Identifiable {
        let item: TranscriptItem
        var id: String { item.id }
    }

    /// Consecutive items with one owner; user messages and delegations always start their own turn.
    private struct Turn: Identifiable {
        let id: String
        let owner: String?
        let isUser: Bool
        var rows: [Row]
    }

    private var visible: [TranscriptItem] {
        let shown = items.filter { $0.origin != "runtime" }
        return Array(shown.suffix(window))
    }

    private var hiddenCount: Int {
        max(0, items.count(where: { $0.origin != "runtime" }) - window)
    }

    /// Find matches may sit above the window; widen it so the current match can be scrolled to.
    private func revealIfNeeded(_ id: String?) {
        guard let id else { return }
        let shown = items.filter { $0.origin != "runtime" }
        guard let index = shown.firstIndex(where: { $0.id == id }) else { return }
        let needed = shown.count - index
        if needed > window { window = needed + Self.windowStep / 2 }
    }

    private static func turns(_ items: [TranscriptItem]) -> [Turn] {
        var result: [Turn] = []
        for item in items {
            let isUser = item.kind == .message && item.role == "user"
            let standalone = isUser || item.kind == .delegation
            let owner = standalone ? nil : item.owner
            if !standalone, let last = result.last, !last.isUser, last.owner == owner, last.rows.first?.item.kind != .delegation {
                result[result.count - 1].rows.append(Row(item: item))
            } else {
                result.append(Turn(id: item.id, owner: owner, isUser: isUser, rows: [Row(item: item)]))
            }
        }
        return result
    }

    var body: some View {
        let visible = visible
        let turns = Self.turns(visible)
        let lastErrorID = items.last(where: { $0.kind == .error })?.id
        ScrollView {
            LazyVStack(alignment: .leading, spacing: Metrics.xl) {
                historyControl
                ForEach(turns) { turn in
                    turnView(turn, lastErrorID: lastErrorID)
                }
            }
            .scrollTargetLayout()
            .padding(.horizontal, Metrics.m)
            .padding(.vertical, Metrics.l)
            .frame(maxWidth: Metrics.readingWidth)
            .frame(maxWidth: .infinity)
        }
        .scrollPosition($position)
        .defaultScrollAnchor(.bottom)
        .onScrollGeometryChange(for: TranscriptScroll.Geometry.self) { geometry in
            TranscriptScroll.Geometry(visibleRect: geometry.visibleRect, contentSize: geometry.contentSize)
        } action: { previous, current in
            atBottom = current.isAtBottom
            if followsLatest && currentMatchID == nil && TranscriptScroll.shouldFollowLatest(from: previous, to: current) {
                jumpToLatest(animated: false)
            }
        }
        .onScrollPhaseChange { _, phase, context in
            if phase == .tracking || phase == .interacting {
                userScrollInProgress = true
                followsLatest = false
                pendingOlder?.userMoved = true
            } else if phase == .idle, userScrollInProgress {
                atBottom = TranscriptScroll.isAtBottom(visibleRect: context.geometry.visibleRect, contentSize: context.geometry.contentSize)
                followsLatest = atBottom
                userScrollInProgress = false
            }
        }
        .onChange(of: items.last) {
            if followsLatest && currentMatchID == nil { jumpToLatest(animated: false) }
        }
        .onChange(of: items.count) { _, count in
            // Rows arrived while an older page was pending: show exactly the fetched rows and keep the reader's
            // turn in place. Live appends during a pending load also pass here; growth only, never a jump.
            guard let pending = pendingOlder else { return }
            let shown = items.count(where: { $0.origin != "runtime" })
            let growth = pending.windowGrowth(nowShown: shown)
            guard growth > 0 else { return }
            window += growth
            if pending.shouldRestore(nowShown: shown), let anchor = pending.anchorID {
                followsLatest = false
                var transaction = Transaction(animation: nil)
                transaction.disablesAnimations = true
                withTransaction(transaction) { position.scrollTo(id: anchor, anchor: .top) }
            }
            // One restore per request; later live updates must never snap back to this anchor.
            pendingOlder = nil
            _ = count
        }
        .onChange(of: history.olderCursor) { old, new in
            // Coverage changed on the server side: that, not row count, is what "loaded older" means.
            if old != nil, old != new { loadedOlderOnce = true }
        }
        .onDisappear { olderTask?.cancel() }
        .onChange(of: currentMatchID) { _, id in
            guard let id else { return }
            followsLatest = false
            revealIfNeeded(id)
            withAnimation(.snappy) { position.scrollTo(id: id, anchor: .center) }
        }
        // An inset, not an overlay: the control reserves its own height so it can never cover the last row,
        // whatever the Dynamic Type size.
        .safeAreaInset(edge: .bottom, spacing: 0) {
            ZStack {
                Button {
                    jumpToLatest(animated: true)
                } label: {
                    Label("Jump to Latest", systemImage: "arrow.down")
                        .font(.callout.weight(.medium))
                }
                .buttonStyle(.glass)
                .frame(width: 180, height: Metrics.target)
                .contentShape(.rect)
                .opacity(atBottom ? 0 : 1)
                .animation(.snappy, value: atBottom)
                .disabled(atBottom)
                .accessibilityHidden(atBottom)
            }
            .frame(maxWidth: .infinity)
            .frame(height: Metrics.target)
        }
    }

    /// Targets the last row rather than the bottom edge: lazy layout estimates can leave an edge scroll
    /// short of the true end, which kept the control on screen after a tap.
    private func jumpToLatest(animated: Bool) {
        followsLatest = true
        userScrollInProgress = false
        // Only the top-level Turn views are scroll targets; item IDs nested inside a turn are not registered.
        let scroll = {
            if let target = TranscriptScroll.latestTargetID(turns: Self.turns(visible).map { $0.rows.map(\.id) }) {
                position.scrollTo(id: target, anchor: .bottom)
            } else {
                position.scrollTo(edge: .bottom)
            }
        }
        if animated {
            withAnimation(.snappy, scroll)
        } else {
            var transaction = Transaction(animation: nil)
            transaction.disablesAnimations = true
            withTransaction(transaction, scroll)
        }
    }

    /// Two different actions, never merged: "Show earlier" reveals rows already on this device; "Load older
    /// from server" fetches the previous history page. Captions say exactly what is where.
    @ViewBuilder
    private var historyControl: some View {
        let state = HistoryControlState(hiddenLocally: hiddenCount, omittedLocally: omittedCount, history: history,
                                        loading: isLoadingOlder, error: loadOlderError, canLoad: onLoadOlder != nil,
                                        loadedOlderOnce: loadedOlderOnce)
        if state.showsAnything {
            VStack(spacing: Metrics.s) {
                if state.showsServerControl {
                    serverControl(state)
                }
                if hiddenCount > 0 {
                    Button {
                        window += Self.windowStep
                    } label: {
                        Label("Show \(min(Self.windowStep, hiddenCount).formatted()) earlier", systemImage: "arrow.up")
                            .font(.callout.weight(.medium))
                    }
                    .buttonStyle(.bordered)
                    .buttonBorderShape(.capsule)
                    .controlSize(.large)
                }
                ForEach(state.captions, id: \.self) { caption in
                    Text(caption)
                        .font(.meta)
                        .foregroundStyle(.secondary)
                        .multilineTextAlignment(.center)
                }
            }
            .frame(maxWidth: .infinity)
            .padding(.bottom, Metrics.s)
        }
    }

    @ViewBuilder
    private func serverControl(_ state: HistoryControlState) -> some View {
        if isLoadingOlder {
            HStack(spacing: Metrics.s) {
                ProgressView().controlSize(.small)
                Text("Loading older…").font(.callout.weight(.medium)).foregroundStyle(.secondary)
            }
            .frame(minHeight: Metrics.target)
            .accessibilityElement(children: .combine)
        } else if let onLoadOlder, history.hasOlderOnServer {
            Button {
                // The reader's current top-level turn: the scroll position's tracked id when it has one, else the
                // first rendered turn, which sits directly under this control and is on screen whenever it is.
                let anchor = position.viewID(type: String.self) ?? Self.turns(visible).first?.id
                pendingOlder = OlderLoadAnchor(anchorID: anchor, shownAtRequest: items.count(where: { $0.origin != "runtime" }))
                olderTask?.cancel()
                olderTask = Task {
                    await onLoadOlder()
                    // Finished without new rows (nothing older, error, cancelled): never let a stale anchor
                    // snap the view on a later live update.
                    if !Task.isCancelled { pendingOlder = nil }
                }
            } label: {
                Label(loadOlderError == nil ? "Load older from server" : "Try loading older again", systemImage: "icloud.and.arrow.down")
                    .font(.callout.weight(.medium))
            }
            .buttonStyle(.bordered)
            .buttonBorderShape(.capsule)
            .controlSize(.large)
        }
        if let loadOlderError {
            Label(loadOlderError, systemImage: "exclamationmark.triangle.fill")
                .font(.meta)
                .foregroundStyle(Palette.signalText)
                .multilineTextAlignment(.center)
        }
    }

    private func turnView(_ turn: Turn, lastErrorID: String?) -> some View {
        VStack(alignment: .leading, spacing: Metrics.m) {
            if let owner = turn.owner {
                AgentLabel(name: owner, font: .subheadline.weight(.semibold))
                    .accessibilityAddTraits(.isHeader)
            }
            ForEach(turn.rows) { row in
                itemView(row.item, retry: canRetry && row.id == lastErrorID)
                    .modifier(TranscriptRowContextMenu(item: row.item, onContinueFromHere: onContinueFromHere, onBranchHere: onBranchHere))
                    .padding(matchIDs.contains(row.id) ? Metrics.xs : 0)
                    .background {
                        if matchIDs.contains(row.id) {
                            RoundedRectangle(cornerRadius: Metrics.chipRadius, style: .continuous)
                                .fill(Color.yellow.opacity(row.id == currentMatchID ? 0.3 : 0.12))
                        }
                    }
                    .accessibilityIdentifier(row.id == visible.last?.id ? "transcript-latest-row" : "transcript-row")
                    .id(row.id)
            }
        }
        .modifier(OwnershipGutter(owner: turn.owner))
    }

    @ViewBuilder
    private func itemView(_ item: TranscriptItem, retry: Bool) -> some View {
        switch item.kind {
        case .message where item.role == "user":
            UserTurnCard(item: item)
        case .message:
            VStack(alignment: .leading, spacing: Metrics.s) {
                if !item.text.isEmpty || item.isStreaming {
                    AssistantTextView(text: item.text, isStreaming: item.isStreaming)
                }
                ForEach(item.generatedMedia) { reference in
                    // Owner/root/path can repeat across servers, so identity includes the full session ref.
                    let identity = "\(sessionRef.serverID.uuidString)|\(sessionRef.sessionID)|\(reference.id)"
                    GeneratedMediaTile(identity: identity, name: reference.name, mimeType: reference.mimeType) {
                        try await resolveMedia(reference)
                    }
                    .id(identity)
                }
            }
        case .tool:
            ToolRow(item: item, onBranch: item.branchAnchor == nil ? nil : { onBranchHere?(item) })
        case .reasoning:
            ReasoningRow(item: item)
        case .delegation:
            DelegationDivider(item: item)
        case .error:
            ErrorRow(item: item, onRetry: retry ? onRetry : nil)
        case .activity:
            NoticeRow(item: item)
        }
    }
}

/// A pending older-page load, pure so the rules are testable: grow the rendered window by exactly the rows that
/// arrived, restore the reader's turn only if they haven't scrolled since asking.
struct OlderLoadAnchor: Equatable {
    let anchorID: String?
    var shownAtRequest: Int
    var userMoved = false

    func windowGrowth(nowShown: Int) -> Int { max(0, nowShown - shownAtRequest) }

    func shouldRestore(nowShown: Int) -> Bool { !userMoved && anchorID != nil && nowShown > shownAtRequest }
}

/// Pure description of the transcript-top controls, shared with tests. Never claims completeness it can't prove.
struct HistoryControlState: Equatable {
    let hiddenLocally: Int
    let omittedLocally: Int
    let history: TranscriptWindow
    let loading: Bool
    let error: String?
    let canLoad: Bool
    let loadedOlderOnce: Bool

    var showsServerControl: Bool { canLoad && (history.hasOlderOnServer || loading || error != nil) }

    var showsAnything: Bool { hiddenLocally > 0 || showsServerControl || !captions.isEmpty }

    /// Short, plain sentences; each states one fact about where history is.
    var captions: [String] {
        var lines: [String] = []
        if hiddenLocally > 0 { lines.append("\(hiddenLocally.formatted()) earlier rows are loaded on this device.") }
        if omittedLocally > 0 { lines.append("\(omittedLocally.formatted()) older rows aren't retained on this device.") }
        if history.hasOlderOnServer {
            let older = history.omittedOlderItemCount
            lines.append(older > 0 ? "\(older.formatted()) older items are on the server." : "Older history is on the server.")
        } else if loadedOlderOnce || history.startPosition == 0 {
            // With omissions we only know that nothing older can be loaded; "beginning" would overclaim.
            lines.append(history.containsTruncatedItems
                         ? "No older history can be loaded."
                         : "This is the beginning of the history kept on the server.")
        }
        if history.containsTruncatedItems {
            let ranges = history.omittedRanges.count
            lines.append(ranges > 0
                         ? "\(ranges == 1 ? "One range" : "\(ranges) ranges") of items were too large for the server to send and are left out."
                         : "Some items were too large for the server to send and are left out.")
        }
        return lines
    }
}

enum TranscriptScroll {
    struct Geometry: Equatable {
        let visibleRect: CGRect
        let contentSize: CGSize

        var isAtBottom: Bool {
            TranscriptScroll.isAtBottom(visibleRect: visibleRect, contentSize: contentSize)
        }

        static func == (lhs: Self, rhs: Self) -> Bool {
            lhs.contentSize == rhs.contentSize && lhs.visibleRect.size == rhs.visibleRect.size && lhs.isAtBottom == rhs.isAtBottom
        }
    }

    /// Each top-level turn is one scroll target; row IDs within it belong to its lazy child stack.
    static func latestTargetID(turns: [[String]]) -> String? {
        turns.last?.first
    }

    /// Slack for lazy-layout rounding plus the stack's trailing padding; anything closer counts as "at the latest".
    static let tolerance: CGFloat = Metrics.l + Metrics.s

    /// `visibleRect` already accounts for safe-area and inset views (composer, banners, the jump control),
    /// so this holds regardless of chrome height. Content that fits is always at the bottom, including
    /// while bouncing (negative offsets), so the control never appears on a short transcript.
    static func isAtBottom(visibleRect: CGRect, contentSize: CGSize) -> Bool {
        guard contentSize.height > visibleRect.height else { return true }
        return visibleRect.maxY >= contentSize.height - tolerance
    }

    /// Follow changes that move the latest row, but ignore ordinary scroll-offset changes.
    static func shouldFollowLatest(from previous: Geometry, to current: Geometry) -> Bool {
        previous.contentSize != current.contentSize || previous.visibleRect.size != current.visibleRect.size
    }
}

/// Agent turns carry the owner's hue in the gutter; user turns and dividers keep the same inset so text aligns.

private struct TranscriptRowContextMenu: ViewModifier {
    let item: TranscriptItem
    let onContinueFromHere: ((TranscriptItem) -> Void)?
    let onBranchHere: ((TranscriptItem) -> Void)?

    func body(content: Content) -> some View {
        if item.kind == .tool {
            content
        } else {
            content.contextMenu {
                if !item.text.isEmpty {
                    Button("Copy", systemImage: "doc.on.doc") { Clipboard.copy(item.text) }
                }
                if let onContinueFromHere, item.role == "user", item.userMessageIndex != nil {
                    Button("Continue from Here in New Session…", systemImage: "arrow.uturn.backward") { onContinueFromHere(item) }
                }
                if let onBranchHere, item.branchAnchor != nil {
                    Button("New Session Up to Here…", systemImage: "arrow.triangle.branch") { onBranchHere(item) }
                }
            }
        }
    }
}

private struct OwnershipGutter: ViewModifier {
    let owner: String?

    func body(content: Content) -> some View {
        content.agentGutter(owner)
    }
}
