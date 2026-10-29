import SwiftUI
import WidgetKit
import AppIntents
import Foundation

private let teamSummarySuite = "group.dev.dockeragent.apple"
private let teamSummaryKey = "team-summary-v1"

struct TeamWidgetSnapshot: Codable, Sendable {
    let agentCount: Int
    let attentionCount: Int
    let activeCount: Int
    let liveSessions: Int
    let serverCount: Int
    let updatedAt: Date?
}

struct TeamEntry: TimelineEntry {
    let date: Date
    let snapshot: TeamWidgetSnapshot?
    let suiteAvailable: Bool

    var isFresh: Bool { snapshot?.updatedAt.map { Date().timeIntervalSince($0) <= 15 * 60 } ?? false }
}

struct TeamSummaryProvider: TimelineProvider {
    func placeholder(in context: Context) -> TeamEntry { TeamEntry(date: .now, snapshot: nil, suiteAvailable: true) }

    func getSnapshot(in context: Context, completion: @escaping (TeamEntry) -> Void) {
        completion(loadEntry())
    }

    func getTimeline(in context: Context, completion: @escaping (Timeline<TeamEntry>) -> Void) {
        let entry = loadEntry()
        completion(Timeline(entries: [entry], policy: .after(.now.addingTimeInterval(15 * 60))))
    }

    private func loadEntry() -> TeamEntry {
        guard FileManager.default.containerURL(forSecurityApplicationGroupIdentifier: teamSummarySuite) != nil,
              let defaults = UserDefaults(suiteName: teamSummarySuite) else {
            return TeamEntry(date: .now, snapshot: nil, suiteAvailable: false)
        }
        let snapshot = defaults.data(forKey: teamSummaryKey).flatMap { try? JSONDecoder().decode(TeamWidgetSnapshot.self, from: $0) }
        return TeamEntry(date: .now, snapshot: snapshot, suiteAvailable: true)
    }
}

struct TeamSummaryWidget: Widget {
    let kind = "TeamSummaryWidget"

    private var widgetFamilies: [WidgetFamily] {
        #if os(iOS)
        [.systemSmall, .systemMedium, .accessoryInline, .accessoryRectangular]
        #else
        [.systemSmall, .systemMedium]
        #endif
    }

    var body: some WidgetConfiguration {
        StaticConfiguration(kind: kind, provider: TeamSummaryProvider()) { entry in
            TeamSummaryView(entry: entry)
                .containerBackground(.background, for: .widget)
        }
        .configurationDisplayName("Team")
        .description("Who needs you and who is working, across your live sessions.")
        .supportedFamilies(widgetFamilies)
    }
}

/// The snapshot only carries counts, so the roster partitions them honestly:
/// agents asking first, then agents working, then everyone quiet; overlap folds into "needs you".
private struct TeamStatus {
    let needsYou: Int
    let working: Int
    let quiet: Int
    let agents: Int
    let servers: Int
    let updatedAt: Date?

    init(_ snapshot: TeamWidgetSnapshot) {
        needsYou = max(0, snapshot.attentionCount)
        working = max(0, snapshot.activeCount - needsYou)
        quiet = max(0, snapshot.agentCount - needsYou - working)
        agents = max(snapshot.agentCount, needsYou + working)
        servers = snapshot.serverCount
        updatedAt = snapshot.updatedAt
    }

    static let placeholder = TeamStatus(TeamWidgetSnapshot(agentCount: 5, attentionCount: 1, activeCount: 3, liveSessions: 2, serverCount: 1, updatedAt: .now))

    var headline: String {
        if needsYou > 0 { return needsYou == 1 ? "1 needs you" : "\(needsYou) need you" }
        if working > 0 { return "\(working) working" }
        return "Nothing running"
    }

    /// The rest of the partition, so the headline plus this line account for every agent.
    var remainder: String? {
        var parts: [String] = []
        if needsYou > 0, working > 0 { parts.append("\(working) working") }
        if quiet > 0 { parts.append("\(quiet) idle") }
        return parts.isEmpty ? nil : parts.joined(separator: ", ")
    }

    var scope: String {
        let agentText = agents == 1 ? "1 agent" : "\(agents) agents"
        let serverText = servers == 1 ? "1 server" : "\(servers) servers"
        return "\(agentText) on \(serverText)"
    }

    var tone: Color { needsYou > 0 ? .orange : working > 0 ? .accentColor : .secondary }

    var accessibilitySummary: String {
        [headline, remainder, scope].compactMap { $0 }.joined(separator: ". ")
    }
}

private struct TeamSummaryView: View {
    @Environment(\.widgetFamily) private var family
    @Environment(\.redactionReasons) private var redaction
    let entry: TeamEntry

    private var status: TeamStatus? {
        if redaction.contains(.placeholder) { return .placeholder }
        guard let snapshot = entry.snapshot, entry.isFresh, snapshot.liveSessions > 0 else { return nil }
        return TeamStatus(snapshot)
    }

    var body: some View {
        if let status {
            switch family {
            #if os(iOS)
            case .accessoryInline:
                Label(status.headline, systemImage: status.needsYou > 0 ? "hand.raised.fill" : "person.3.sequence.fill")
                    .privacySensitive()
                    .accessibilityLabel(status.accessibilitySummary)
            case .accessoryRectangular:
                VStack(alignment: .leading, spacing: 2) {
                    Text(status.headline).font(.headline).widgetAccentable()
                    if let remainder = status.remainder { Text(remainder).font(.caption) }
                    Text(status.scope).font(.caption).foregroundStyle(.secondary)
                }
                .privacySensitive()
                .accessibilityElement(children: .ignore)
                .accessibilityLabel(status.accessibilitySummary)
            #endif
            case .systemMedium:
                board(status, rosterLimit: 22, spacing: 10)
            default:
                board(status, rosterLimit: 8, spacing: 6)
            }
        } else {
            unavailable
        }
    }

    /// Spine, state sentence, roster: one leading bar in the team's tone and words for the rest.
    private func board(_ status: TeamStatus, rosterLimit: Int, spacing: CGFloat) -> some View {
        HStack(alignment: .top, spacing: 10) {
            RoundedRectangle(cornerRadius: 1.5, style: .continuous)
                .fill(status.tone)
                .frame(width: 3)
                .widgetAccentable()
                .accessibilityHidden(true)
            VStack(alignment: .leading, spacing: spacing) {
                Text(status.headline)
                    .font(family == .systemMedium ? .title2.weight(.semibold) : .title3.weight(.semibold))
                    .foregroundStyle(status.tone)
                    .lineLimit(1)
                    .minimumScaleFactor(0.8)
                    .widgetAccentable()
                if let remainder = status.remainder {
                    Text(remainder).font(.subheadline).foregroundStyle(.secondary).lineLimit(1)
                }
                Spacer(minLength: 0)
                TeamRoster(status: status, limit: rosterLimit)
                HStack(alignment: .firstTextBaseline) {
                    Text(status.scope).lineLimit(1)
                    if family == .systemMedium, let updatedAt = status.updatedAt {
                        Spacer(minLength: 6)
                        Text("Updated \(updatedAt.formatted(.relative(presentation: .named)))").lineLimit(1).foregroundStyle(.tertiary)
                    }
                }
                .font(.caption)
                .foregroundStyle(.secondary)
            }
        }
        .privacySensitive()
        .accessibilityElement(children: .ignore)
        .accessibilityLabel(status.accessibilitySummary)
    }

    private var unavailable: some View {
        VStack(alignment: .leading, spacing: 6) {
            Label("Team", systemImage: "person.3.sequence.fill").font(.headline)
            Text(unavailableMessage).font(.caption).foregroundStyle(.secondary)
        }
        .frame(maxWidth: .infinity, maxHeight: .infinity, alignment: .topLeading)
        .accessibilityElement(children: .combine)
    }

    private var unavailableMessage: String {
        guard entry.suiteAvailable else { return "Team status isn't shared with this widget yet. Open Docker Agent to check." }
        guard let snapshot = entry.snapshot else { return "Open Docker Agent once to start tracking your team." }
        if snapshot.liveSessions == 0 { return "No live sessions. Open the app to reconnect." }
        if let updatedAt = snapshot.updatedAt {
            return "Last update \(updatedAt.formatted(.relative(presentation: .named))). Open the app to refresh."
        }
        return "Open the app to refresh team status."
    }
}

/// One mark per agent in board order; beyond the limit the count stands in for the rest.
/// Limits are sized to the family's content width (small ≈126pt, medium ≈290pt) with room for a 3-digit "+N".
private struct TeamRoster: View {
    let status: TeamStatus
    let limit: Int

    var body: some View {
        let shown = min(status.agents, limit)
        let needsYou = min(status.needsYou, shown)
        let working = min(status.working, max(0, shown - needsYou))
        let quiet = max(0, shown - needsYou - working)
        HStack(spacing: 4) {
            marks(needsYou, color: .orange)
            marks(working, color: .accentColor)
            marks(quiet, color: .secondary.opacity(0.35))
            if status.agents > shown {
                Text("+\(status.agents - shown)").font(.caption2.monospacedDigit()).foregroundStyle(.secondary).lineLimit(1).fixedSize()
            }
        }
        .accessibilityHidden(true)
    }

    private func marks(_ count: Int, color: Color) -> some View {
        ForEach(0..<count, id: \.self) { _ in
            Circle().fill(color).frame(width: 7, height: 7)
        }
    }
}

@main
struct DockerAgentWidgetBundle: WidgetBundle {
    var body: some Widget { TeamSummaryWidget() }
}
