import SwiftUI
import AgentKit

/// One live root session on the board, with whatever is known about the agents under it.
struct BoardEntry: Identifiable, Equatable {
    struct Member: Identifiable, Equatable {
        let name: String
        let state: SessionState
        let needsAttention: Bool
        let id: String

        /// Shared vocabulary with the team map and widget. Only a reported attention flag means a person
        /// is needed; `.waiting` alone is waiting on an agent, a tool or a pause.
        var word: String {
            if needsAttention { return "needs you" }
            switch state {
            case .running: return "working"
            case .cancelling: return "stopping"
            case .queued: return "queued"
            case .waiting: return "waiting"
            case .idle: return "idle"
            case .settled: return "done"
            case .failed: return "failed"
            case .unknown: return "unknown"
            }
        }
    }

    let session: SessionProjection
    /// Why a person is needed, when they are.
    let reason: String?
    /// Latest activity: the open session's live value when the session is open, else the catalog's.
    let activity: String?
    /// Known sub-agents: the live tree while the session is open, otherwise child sessions from the catalog.
    /// Neither is a promise of the complete hierarchy; the server may hold descendants this client hasn't loaded.
    let members: [Member]
    let membersAreLive: Bool
    let cost: Double

    var id: SessionRef { session.ref }
    var needsYou: Bool { reason != nil }
    /// Running only; waiting agents are not "working".
    var activeMembers: Int { members.count { $0.state == .running } }

    static func make(session: SessionProjection, reason: String?, open: OpenSessionProjection?, catalogChildren: [SessionProjection]) -> BoardEntry {
        var members: [Member] = []
        var live = false
        if let open, !open.agentTree.isEmpty {
            live = true
            func walk(_ node: AgentNodeProjection) {
                members.append(Member(name: node.agentName, state: node.state, needsAttention: node.needsAttention, id: node.id))
                node.children.forEach(walk)
            }
            // The root node is this session itself; only its descendants are "the team".
            open.agentTree.forEach { root in root.children.forEach(walk) }
        } else {
            members = catalogChildren
                .sorted { ($0.updatedAt ?? .distantPast) > ($1.updatedAt ?? .distantPast) }
                .map { Member(name: $0.agentName, state: $0.state, needsAttention: $0.needsYouReason != nil && $0.state != .failed, id: $0.ref.sessionID) }
        }
        return BoardEntry(
            session: session,
            reason: reason,
            activity: open?.currentActivity ?? session.activity,
            members: members,
            membersAreLive: live,
            cost: max(open?.cost ?? 0, session.cost)
        )
    }
}

/// Recency buckets for the log. Anything undated sorts last.
enum LogBucket: Int, CaseIterable, Identifiable {
    case today, yesterday, thisWeek, earlier

    var id: Int { rawValue }

    var title: String {
        switch self {
        case .today: "Today"
        case .yesterday: "Yesterday"
        case .thisWeek: "This week"
        case .earlier: "Earlier"
        }
    }

    static func bucket(for date: Date?, now: Date = .now, calendar: Calendar = .current) -> LogBucket {
        guard let date else { return .earlier }
        if calendar.isDate(date, inSameDayAs: now) { return .today }
        if let yesterday = calendar.date(byAdding: .day, value: -1, to: now), calendar.isDate(date, inSameDayAs: yesterday) { return .yesterday }
        if let weekAgo = calendar.date(byAdding: .day, value: -7, to: now), date > weekAgo { return .thisWeek }
        return .earlier
    }
}

/// A live root session and its team, shown on the home board. Tap opens the session.
struct BoardTag: View {
    let entry: BoardEntry
    @Environment(\.dynamicTypeSize) private var typeSize

    private var state: SessionState { entry.session.state }
    private var shownMembers: ArraySlice<BoardEntry.Member> { entry.members.prefix(typeSize.isAccessibilitySize ? 2 : 4) }

    var body: some View {
        VStack(alignment: .leading, spacing: Metrics.xs) {
            HStack(alignment: .firstTextBaseline, spacing: Metrics.s) {
                Text(entry.session.title.nonEmpty ?? "Untitled session")
                    .font(.boardTitle)
                    .lineLimit(2)
                Spacer(minLength: Metrics.xs)
                if entry.session.starred {
                    Image(systemName: "star.fill").font(.caption2).foregroundStyle(.yellow).accessibilityLabel("Starred")
                }
                if let age = Format.age(entry.session.updatedAt) {
                    Text(age).font(.figure).foregroundStyle(.tertiary)
                }
            }
            HStack(alignment: .firstTextBaseline, spacing: Metrics.s - 2) {
                AgentDot(name: entry.session.agentName)
                    .alignmentGuide(.firstTextBaseline) { $0[VerticalAlignment.center] + 3 }
                Text(entry.session.agentName.nonEmpty ?? "Agent")
                    .font(.footnote.weight(.semibold))
                if let team = entry.session.teamLabel {
                    Text(team)
                        .font(.footnote)
                        .foregroundStyle(.tertiary)
                }
                if let line = entry.reason ?? entry.activity ?? (state == .running ? "working" : state == .waiting ? "waiting" : nil) {
                    Text(line)
                        .font(.footnote)
                        .foregroundStyle(entry.needsYou ? AnyShapeStyle(Palette.signalText) : AnyShapeStyle(.secondary))
                        .layoutPriority(-1)
                }
                Spacer(minLength: Metrics.xs)
                if entry.cost > 0 {
                    Text(Format.cost(entry.cost)).font(.figure).foregroundStyle(.secondary)
                }
            }
            .lineLimit(1)
            if !entry.members.isEmpty {
                members
            }
        }
        .padding(.vertical, Metrics.s)
        .padding(.trailing, Metrics.m)
        .agentGutter(entry.session.agentName, inset: 0, attention: entry.needsYou)
        .padding(.leading, Metrics.s)
        .background(.fill.quaternary, in: .rect(cornerRadius: Metrics.boardRadius, style: .continuous))
        .contentShape(.rect(cornerRadius: Metrics.boardRadius, style: .continuous))
        .accessibilityElement(children: .combine)
        .accessibilityLabel(accessibilityText)
    }

    /// The team under this root: hue dot, name, state word. Idle members read quieter than working ones.
    private var members: some View {
        FlowLayout(spacing: Metrics.m, lineSpacing: Metrics.xs) { memberChips }
            .padding(.top, Metrics.xxs)
    }

    @ViewBuilder
    private var memberChips: some View {
        ForEach(shownMembers) { member in
            HStack(alignment: .firstTextBaseline, spacing: Metrics.xs) {
                AgentDot(name: member.name, size: 6)
                    .alignmentGuide(.firstTextBaseline) { $0[VerticalAlignment.center] + 2 }
                Text(member.name.nonEmpty ?? "agent")
                    .font(.caption.weight(member.state.isActive ? .semibold : .regular))
                    .lineLimit(1)
                    .truncationMode(.middle)
                Text(member.word)
                    .font(.caption)
                    .foregroundStyle(member.needsAttention ? AnyShapeStyle(Palette.signalText) : AnyShapeStyle(.secondary))
                    .lineLimit(1)
                    .layoutPriority(1)
            }
            .opacity(member.state.isActive || member.needsAttention ? 1 : 0.7)
        }
        if entry.members.count > shownMembers.count {
            Text("+\(entry.members.count - shownMembers.count)")
                .font(.figureSmall)
                .foregroundStyle(.secondary)
        }
    }

    private var accessibilityText: String {
        var parts = [entry.session.title.nonEmpty ?? "Untitled session", entry.session.agentName]
        if let team = entry.session.teamLabel { parts.append("team \(team)") }
        if let reason = entry.reason { parts.append("needs you: \(reason)") } else if let activity = entry.activity { parts.append(activity) } else { parts.append(state.label) }
        if !entry.members.isEmpty {
            let scope = entry.membersAreLive ? "sub-agents" : "known sub-agents"
            parts.append("\(entry.members.count) \(scope), \(entry.activeMembers) running")
        }
        if entry.cost > 0 { parts.append(Format.cost(entry.cost)) }
        if let age = Format.relative(entry.session.updatedAt) { parts.append(age) }
        return parts.joined(separator: ", ")
    }
}

/// Wraps chips onto as many lines as the width needs; the only layout here that isn't a stack.
/// Each chip is proposed at most the full width, so a long name wraps inside its chip instead of overflowing.
struct FlowLayout: Layout {
    var spacing: CGFloat = Metrics.s
    var lineSpacing: CGFloat = Metrics.xs

    private func size(of subview: LayoutSubview, width: CGFloat) -> CGSize {
        let ideal = subview.sizeThatFits(.unspecified)
        return ideal.width <= width ? ideal : subview.sizeThatFits(ProposedViewSize(width: width, height: nil))
    }

    func sizeThatFits(proposal: ProposedViewSize, subviews: Subviews, cache: inout ()) -> CGSize {
        let width = proposal.width ?? .infinity
        var x: CGFloat = 0, y: CGFloat = 0, lineHeight: CGFloat = 0, maxX: CGFloat = 0
        for subview in subviews {
            let size = size(of: subview, width: width)
            if x > 0, x + size.width > width {
                x = 0
                y += lineHeight + lineSpacing
                lineHeight = 0
            }
            x += size.width + spacing
            lineHeight = max(lineHeight, size.height)
            maxX = max(maxX, x - spacing)
        }
        return CGSize(width: width == .infinity ? maxX : width, height: y + lineHeight)
    }

    func placeSubviews(in bounds: CGRect, proposal: ProposedViewSize, subviews: Subviews, cache: inout ()) {
        var x = bounds.minX, y = bounds.minY, lineHeight: CGFloat = 0
        for subview in subviews {
            let size = size(of: subview, width: bounds.width)
            if x > bounds.minX, x + size.width > bounds.maxX {
                x = bounds.minX
                y += lineHeight + lineSpacing
                lineHeight = 0
            }
            subview.place(at: CGPoint(x: x, y: y), proposal: ProposedViewSize(size))
            x += size.width + spacing
            lineHeight = max(lineHeight, size.height)
        }
    }
}
