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
            }
        }
        .badge(server.sessionCount)
        .accessibilityElement(children: .combine)
        .accessibilityValue("\(server.state.label), \(server.sessionCount) sessions")
    }
}
