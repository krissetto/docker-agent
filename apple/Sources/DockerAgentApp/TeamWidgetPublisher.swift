import Foundation
import AgentKit
import WidgetKit

struct TeamWidgetSummary: Codable, Sendable, Equatable {
    let agentCount: Int
    let attentionCount: Int
    let activeCount: Int
    let liveSessions: Int
    let serverCount: Int
    let updatedAt: Date?
}

@MainActor
enum TeamWidgetPublisher {
    private static let suiteName = "group.dev.dockeragent.apple"
    private static let key = "team-summary-v1"
    private static var lastPublished: TeamWidgetSummary?

    static func summary(sessions: [OpenSessionProjection]) -> TeamWidgetSummary {
        var agentKeys = Set<String>()
        var activeKeys = Set<String>()
        var attentionKeys = Set<String>()
        var serverIDs = Set<UUID>()
        var agents = 0
        var active = 0
        var attention = 0
        var liveCount = 0
        var newest = Date.distantPast
        for session in sessions where session.connection == .live {
            liveCount += 1
            serverIDs.insert(session.ref.serverID)
            newest = max(newest, session.refreshedAt ?? .distantPast)
            let sessionKey = "session:\(session.ref.serverID.uuidString):\(session.ref.sessionID)"
            if agentKeys.insert(sessionKey).inserted { agents += 1 }
            if session.state.isActive, activeKeys.insert(sessionKey).inserted { active += 1 }
            if !session.interactions.isEmpty, attentionKeys.insert(sessionKey).inserted { attention += 1 }
            var stack = Array(session.agentTree)
            while let node = stack.popLast() {
                let key: String
                if let ref = node.session {
                    key = "session:\(ref.serverID.uuidString):\(ref.sessionID)"
                } else {
                    key = "node:\(session.ref.serverID.uuidString):\(node.id)"
                }
                if agentKeys.insert(key).inserted { agents += 1 }
                if node.state.isActive, activeKeys.insert(key).inserted { active += 1 }
                if node.needsAttention, attentionKeys.insert(key).inserted { attention += 1 }
                stack.append(contentsOf: node.children)
            }
        }
        return TeamWidgetSummary(agentCount: agents, attentionCount: attention, activeCount: active,
                                 liveSessions: liveCount, serverCount: serverIDs.count,
                                 updatedAt: liveCount > 0 ? newest : nil)
    }

    static func publish(_ payload: TeamWidgetSummary) {
        guard FileManager.default.containerURL(forSecurityApplicationGroupIdentifier: suiteName) != nil,
              let defaults = UserDefaults(suiteName: suiteName), payload != lastPublished,
              let data = try? JSONEncoder().encode(payload) else { return }
        lastPublished = payload
        defaults.set(data, forKey: key)
        WidgetCenter.shared.reloadTimelines(ofKind: "TeamSummaryWidget")
    }
}
