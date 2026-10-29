import SwiftUI
import AgentKit

/// A log row: quiet by design. Live sessions are shown on the board instead, so this row only needs
/// to carry recognition (title, agent) and the facts you compare history by (cost, age).
struct SessionRow: View {
    let session: SessionProjection
    var reason: String?
    var serverName: String?

    private var needsYou: Bool { reason != nil && session.state != .failed }
    private var detail: String? { reason ?? session.activity }

    var body: some View {
        VStack(alignment: .leading, spacing: 3) {
            HStack(alignment: .firstTextBaseline, spacing: Metrics.s) {
                Text(session.title.nonEmpty ?? "Untitled session")
                    .font(.rowTitle)
                    .lineLimit(1)
                Spacer(minLength: Metrics.xs)
                if session.starred {
                    Image(systemName: "star.fill")
                        .font(.caption2)
                        .foregroundStyle(.yellow)
                        .accessibilityLabel("Starred")
                }
                if let age = Format.age(session.updatedAt) {
                    Text(age)
                        .font(.figure)
                        .foregroundStyle(.tertiary)
                }
            }
            HStack(alignment: .firstTextBaseline, spacing: 6) {
                if needsYou {
                    NeedsYouIcon().font(.caption2).imageScale(.small)
                } else if session.state.isActive || session.state == .failed {
                    StateIcon(state: session.state)
                        .font(.caption2)
                        .imageScale(.small)
                }
                Text(session.agentName.nonEmpty ?? "Agent")
                    .font(.rowDetail.weight(.medium))
                    .foregroundStyle(.secondary)
                if let team = session.teamLabel {
                    Text(team)
                        .font(.rowDetail)
                        .foregroundStyle(.tertiary)
                }
                if let serverName {
                    Text(serverName)
                        .font(.rowDetail)
                        .foregroundStyle(.tertiary)
                }
                if let detail, !detail.isEmpty {
                    Text(detail)
                        .font(.rowDetail)
                        .foregroundStyle(needsYou ? AnyShapeStyle(Palette.signalText) : AnyShapeStyle(.secondary))
                        .layoutPriority(-1)
                }
                Spacer(minLength: Metrics.xs)
                if session.cost > 0 {
                    Text(Format.cost(session.cost))
                        .font(.figure)
                        .foregroundStyle(.secondary)
                }
            }
            .lineLimit(1)
            .truncationMode(.tail)
        }
        .agentGutter(session.agentName, inset: 1, attention: needsYou, opacity: session.state.spineOpacity)
        .padding(.vertical, Metrics.xs)
        .accessibilityElement(children: .combine)
        .accessibilityLabel(accessibilityText)
    }

    private var accessibilityText: String {
        var parts = [session.title.nonEmpty ?? "Untitled session", session.agentName]
        if let team = session.teamLabel { parts.append("team \(team)") }
        if needsYou { parts.append("needs you") } else { parts.append(session.state.label) }
        if let detail { parts.append(detail) }
        if session.cost > 0 { parts.append(Format.cost(session.cost)) }
        if let age = Format.relative(session.updatedAt) { parts.append(age) }
        return parts.joined(separator: ", ")
    }
}
