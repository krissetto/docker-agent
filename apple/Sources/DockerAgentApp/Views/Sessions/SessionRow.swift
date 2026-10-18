import SwiftUI
import AgentKit

struct SessionRow: View {
    let session: SessionProjection
    var reason: String?
    var serverName: String?

    private var detail: String? {
        reason ?? session.activity
    }

    var body: some View {
        HStack(alignment: .firstTextBaseline, spacing: Metrics.s) {
            StateIcon(state: reason != nil && session.state != .failed ? .waiting : session.state)
                .font(.caption)
            VStack(alignment: .leading, spacing: Metrics.xxs) {
                HStack(alignment: .firstTextBaseline) {
                    Text(session.title.isEmpty ? "Untitled session" : session.title)
                        .font(.body.weight(.medium))
                        .lineLimit(1)
                    Spacer(minLength: Metrics.s)
                    if session.starred {
                        Image(systemName: "star.fill")
                            .font(.caption2)
                            .foregroundStyle(.yellow)
                            .accessibilityLabel("Starred")
                    }
                    if let when = Format.relative(session.updatedAt) {
                        Text(when)
                            .font(.caption)
                            .foregroundStyle(.secondary)
                            .monospacedDigit()
                    }
                }
                HStack(spacing: Metrics.xs) {
                    AgentDot(name: session.agentName, size: 6)
                    Text(session.agentName)
                    if let serverName {
                        Text("· \(serverName)")
                    }
                    if let detail {
                        Text("· \(detail)")
                            .foregroundStyle(reason != nil ? AnyShapeStyle(.orange) : AnyShapeStyle(.secondary))
                    }
                }
                .font(.caption)
                .foregroundStyle(.secondary)
                .lineLimit(1)
            }
        }
        .padding(.vertical, Metrics.xxs)
        .accessibilityElement(children: .combine)
    }
}
