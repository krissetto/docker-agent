import SwiftUI
import AgentKit

/// The team line above the composer: who is working, on what, and which sub-agents are live.
/// Tap opens the Agents inspector; Stop sits here, next to the work it stops.
struct ActivityStrip: View {
    let session: OpenSessionProjection
    var onShowAgents: () -> Void
    var onStop: (() async throws -> Void)? = nil

    @State private var stopping = false
    @Environment(\.accessibilityReduceMotion) private var reduceMotion

    private struct LiveAgent: Identifiable {
        let id: String
        let name: String
        let needsAttention: Bool
    }

    /// Descendants that are running right now, in tree order; waiting or idle agents aren't counted as live.
    private var liveAgents: [LiveAgent] {
        var result: [LiveAgent] = []
        func walk(_ node: AgentNodeProjection) {
            if node.state == .running {
                result.append(LiveAgent(id: node.id, name: node.agentName, needsAttention: node.needsAttention))
            }
            node.children.forEach(walk)
        }
        session.agentTree.forEach { $0.children.forEach(walk) }
        return result
    }

    /// A person is needed when an interaction is open or an agent in the shown tree is flagged for attention;
    /// plain `.waiting` is waiting on an agent or tool and never counts.
    private var needsYou: Bool { !session.interactions.isEmpty || TeamTally(session.agentTree).needsYou > 0 }

    private var statusText: String {
        if needsYou { return session.interactions.isEmpty ? "Needs attention" : "Needs you" }
        if let activity = session.currentActivity { return activity }
        switch session.state {
        case .running: return "Working"
        default: return session.state.label
        }
    }

    var body: some View {
        let live = liveAgents
        let tally = TeamTally(session.agentTree)
        HStack(spacing: Metrics.s) {
            Button(action: onShowAgents) {
                HStack(spacing: Metrics.s) {
                    Circle()
                        .fill(needsYou ? Palette.signal : AgentPalette.color(for: session.agentName))
                        .frame(width: 8, height: 8)
                        .accessibilityHidden(true)
                    Text(session.agentName.nonEmpty ?? "Agent").font(.footnote.weight(.semibold))
                    Text(statusText)
                        .font(.footnote)
                        .foregroundStyle(needsYou ? AnyShapeStyle(Palette.signalText) : AnyShapeStyle(.secondary))
                        .lineLimit(1)
                        .contentTransition(.opacity)
                    Spacer(minLength: Metrics.s)
                    if tally.subagents > 0 {
                        teamDots(live, tally: tally)
                    }
                    if session.state == .running {
                        ProgressView().controlSize(.mini)
                    }
                }
                .contentShape(.rect)
            }
            .buttonStyle(.plain)
            .accessibilityLabel(accessibilityText(live, tally: tally))
            .accessibilityHint("Shows agents")
            if let onStop, session.state == .running {
                Button {
                    stopping = true
                    Task {
                        try? await onStop()
                        stopping = false
                    }
                } label: {
                    Image(systemName: "stop.circle.fill")
                        .font(.title3)
                        .symbolRenderingMode(.hierarchical)
                        .frame(width: Metrics.target, height: Metrics.target)
                        .contentShape(.rect)
                }
                .buttonStyle(.borderless)
                .disabled(stopping)
                .help("Stop the current turn")
                .accessibilityLabel("Stop")
            }
        }
        .padding(.leading, Metrics.m)
        .padding(.trailing, Metrics.xs)
        // The row itself is the 44pt target. It floats over scrolling content, so it wears glass like the composer.
        .frame(minHeight: Metrics.target)
        .floatingChip(radius: Metrics.target / 2)
        .animation(Motion.crossfade(reduceMotion), value: session.currentActivity)
        .animation(Motion.reveal(reduceMotion), value: live.count)
        // A new live agent is worth a nudge; activity text changes are not.
        .sensoryFeedback(.impact(weight: .light), trigger: live.count) { old, new in new > old }
    }

    /// One dot per running sub-agent in its hue, overlapping like a small crew, then "running of shown".
    /// The denominator is the tree this client has, not a promise of the server's whole tree.
    private func teamDots(_ live: [LiveAgent], tally: TeamTally) -> some View {
        HStack(spacing: Metrics.xs) {
            if !live.isEmpty {
                HStack(spacing: -3) {
                    ForEach(live.prefix(5)) { agent in
                        Circle()
                            .fill(agent.needsAttention ? Palette.signal : AgentPalette.color(for: agent.name))
                            .frame(width: 10, height: 10)
                            .overlay(Circle().strokeBorder(.background, lineWidth: 1.5))
                    }
                }
            }
            Text("\(tally.runningSubagents) of \(tally.subagents) running")
                .font(.figureSmall)
                .foregroundStyle(.secondary)
        }
        .accessibilityHidden(true)
    }

    private func accessibilityText(_ live: [LiveAgent], tally: TeamTally) -> String {
        var parts = ["\(session.agentName): \(statusText)"]
        if let line = tally.subagentLine {
            parts.append(live.isEmpty ? line : line + ": " + live.map(\.name).joined(separator: ", "))
        }
        return parts.joined(separator: ". ")
    }
}
