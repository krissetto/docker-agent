import SwiftUI
import AgentKit

struct ServerRow: View {
    let server: ServerProjection

    var body: some View {
        HStack(spacing: Metrics.s) {
            HealthDot(state: server.state)
            VStack(alignment: .leading, spacing: 1) {
                Text(server.name).lineLimit(1)
                Text(URL(string: server.baseURL)?.host() ?? server.baseURL)
                    .font(.caption)
                    .foregroundStyle(.secondary)
                    .lineLimit(1)
                if server.connectionIssue == .identityChanged {
                    Text("Identity changed. Edit and test to reconnect.")
                        .font(.caption)
                        .foregroundStyle(Palette.signalText)
                        .lineLimit(2)
                }
            }
        }
        .badge(server.sessionCount)
        .accessibilityElement(children: .combine)
        .accessibilityValue(server.connectionIssue == .identityChanged
                            ? "Server identity changed. Edit and test to reconnect. \(server.sessionCount) sessions"
                            : "\(server.state.label), \(server.sessionCount) sessions")
    }
}
