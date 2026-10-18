import SwiftUI
import AgentKit

struct ActivityStrip: View {
    let session: OpenSessionProjection
    var onShowAgents: () -> Void

    private var activeSubagents: Int {
        func count(_ nodes: [AgentNodeProjection]) -> Int {
            nodes.reduce(0) { $0 + ($1.state.isActive ? 1 : 0) + count($1.children) }
        }
        return session.agentTree.reduce(0) { $0 + count($1.children) }
    }

    var body: some View {
        Button(action: onShowAgents) {
            HStack(spacing: Metrics.s) {
                AgentDot(name: session.agentName)
                Text(session.agentName).font(.callout.weight(.semibold))
                Text(session.currentActivity ?? (session.state == .waiting ? "Waiting for you" : "Working…"))
                    .font(.callout)
                    .foregroundStyle(.secondary)
                    .lineLimit(1)
                    .contentTransition(.opacity)
                Spacer(minLength: Metrics.s)
                if activeSubagents > 0 {
                    Text("+\(activeSubagents) agents")
                        .font(.caption.weight(.medium))
                        .padding(.horizontal, Metrics.s)
                        .padding(.vertical, Metrics.xxs)
                        .background(.fill.tertiary, in: .capsule)
                }
                if session.state == .running {
                    ProgressView().controlSize(.small)
                }
            }
            .padding(.horizontal, Metrics.m)
            .padding(.vertical, Metrics.s)
            .contentShape(.rect)
        }
        .buttonStyle(.plain)
        .animation(.easeInOut(duration: 0.2), value: session.currentActivity)
        .accessibilityLabel("\(session.agentName): \(session.currentActivity ?? session.state.label)")
        .accessibilityHint("Shows agents")
    }
}
