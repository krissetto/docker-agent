import Foundation

public struct SessionControlMetadata: Decodable, Sendable {
    public let sessionID: String
    public let agentName: String
    public let historical: Bool?
    public let executionAvailable: Bool?
    public let model: String?
    public let thinkingLevels: [String]?
    public let thinkingLevel: String?
    public let capabilities: [String: JSONValue]?
    enum CodingKeys: String, CodingKey {
        case sessionID = "session_id", agentName = "agent_name", historical
        case executionAvailable = "execution_available", model, thinkingLevels = "thinking_levels"
        case thinkingLevel = "thinking_level", capabilities
    }
    public func supports(_ capability: String) -> Bool { capabilities?[capability]?.boolValue ?? false }
    public var canExecute: Bool { historical != true && executionAvailable == true }
    public func requireExecution(for requestedSessionID: String, capability: String? = nil) throws {
        guard sessionID == requestedSessionID, canExecute,
              capability.map(supports) ?? true else { throw SessionControlsError.sessionNotExecutable }
    }
}

public struct SessionStatusResponse: Decodable, Sendable {
    public let metadata: SessionControlMetadata
    public let status: StatusWire
}

public struct SessionModelChoice: Decodable, Sendable, Equatable {
    public let name: String
    public let ref: String
    public let provider: String?
    public let model: String?
    public let modelID: String?
    public let modelName: String?
    public let isDefault: Bool?
    public let isCurrent: Bool?
    public let isCustom: Bool?
    public let isCatalog: Bool?
    public let family: String?
    public let inputCost: Double?
    public let outputCost: Double?
    enum CodingKeys: String, CodingKey {
        case name, ref, provider, model, modelID = "model_id", modelName = "model_name"
        case isDefault = "is_default", isCurrent = "is_current", isCustom = "is_custom", isCatalog = "is_catalog"
        case family, inputCost = "input_cost", outputCost = "output_cost"
    }
}

public struct ThinkingLevelResponse: Decodable, Sendable {
    public let levels: [String]
    public let current: String?
    public let metadata: SessionControlMetadata
}

public struct SessionTodoWire: Decodable, Sendable, Equatable, Identifiable {
    public let id: String
    public let description: String
    public let status: String
}

public struct RecentChatMessageWire: Decodable, Sendable, Equatable, Identifiable {
    public let id: Int64
    public let position: Int
    public let role: String
    public let agentName: String?
    public let text: String
    public let truncated: Bool?
    enum CodingKeys: String, CodingKey { case id, position, role, agentName = "agent_name", text, truncated }
}

public struct RecentChatResponse: Decodable, Sendable {
    public let version: Int
    public let sessionID: String
    public let rootSessionID: String
    public let source: String?
    public let agentID: String?
    public let agentName: String?
    public let runtimeStatus: String
    public let state: String?
    public let executionAvailable: Bool
    public let pending: Int
    public let attentionKnown: Bool
    public let approvalCount: Int
    public let elicitationCount: Int
    public let messages: [RecentChatMessageWire]
    public let truncatedBefore: Bool
    public let beforeCursor: String?
    enum CodingKeys: String, CodingKey {
        case version, sessionID = "session_id", rootSessionID = "root_session_id", source
        case agentID = "agent_id", agentName = "agent_name", runtimeStatus = "runtime_status"
        case state, executionAvailable = "execution_available", pending, attentionKnown = "attention_known"
        case approvalCount = "approval_count", elicitationCount = "elicitation_count", messages
        case truncatedBefore = "truncated_before", beforeCursor = "before_cursor"
    }
}

public struct CancelSessionResponse: Decodable, Sendable {
    public let sessionID: String
    public let turnID: String
    public let outcome: String
    enum CodingKeys: String, CodingKey { case sessionID = "session_id", turnID = "turn_id", outcome }
}

public struct SessionAgentConfigResponse: Decodable, Sendable {
    public let sessionID: String
    public let source: String?
    public let agentName: String
    public let historical: Bool?
    public let executionAvailable: Bool?
    public let historyReason: String?
    public let agentID: String?
    public let agentKey: String?
    public let definitionKnown: Bool?
    public let definition: JSONValue?
    public let info: JSONValue
    enum CodingKeys: String, CodingKey {
        case sessionID = "session_id", source, agentName = "agent_name", historical
        case executionAvailable = "execution_available", historyReason = "history_reason"
        case agentID = "agent_id", agentKey = "agent_key", definitionKnown = "definition_known"
        case definition, info
    }
}

public struct SessionPermissionsResponse: Decodable, Sendable {
    public let policy: String
    public let toolsApproved: Bool
    public let source: JSONValue?
    public let session: JSONValue?
    enum CodingKeys: String, CodingKey { case policy, toolsApproved = "tools_approved", source, session }
}

public struct SessionAgentTreeResponse: Decodable, Sendable {
    public let version: Int
    public let durability: String?
    public let root: String?
    public let nodes: [JSONValue]?
}

public struct SessionBranchRequest: Encodable, Sendable, Equatable {
    public let position: Int?
    public let expectedSnapshot: String?
    enum CodingKeys: String, CodingKey { case position, expectedSnapshot = "expected_snapshot" }
    public init(position: Int? = nil, expectedSnapshot: String? = nil) {
        self.position = position; self.expectedSnapshot = expectedSnapshot
    }
}

public struct SessionBranchResponse: Decodable, Sendable {
    public let metadata: SessionControlMetadata
    public let session: SessionHistoryWire?
}

public struct SessionSwitchAgentRequest: Encodable, Sendable, Equatable {
    public let agentName: String
    enum CodingKeys: String, CodingKey { case agentName = "agent_name" }
    public init(agentName: String) { self.agentName = agentName }
}

public struct SessionSwitchAgentResponse: Decodable, Sendable {
    public let metadata: SessionControlMetadata
    public let session: SessionHistoryWire?
}

public struct SessionTitleRequest: Encodable, Sendable, Equatable {
    public let title: String
    public init(title: String) { self.title = title }
}

public struct SessionStarredRequest: Encodable, Sendable, Equatable {
    public let starred: Bool
    public init(starred: Bool) { self.starred = starred }
}

public struct SessionModelRequest: Encodable, Sendable, Equatable {
    public let model: String
    public init(model: String) { self.model = model }
}

public struct ThinkingLevelRequest: Encodable, Sendable, Equatable {
    public let level: String
    public init(level: String) { self.level = level }
}

public struct SessionCompactRequest: Encodable, Sendable, Equatable {
    public let additionalPrompt: String
    enum CodingKeys: String, CodingKey { case additionalPrompt = "additional_prompt" }
    public init(additionalPrompt: String) { self.additionalPrompt = additionalPrompt }
}

public struct SessionCancelRequest: Encodable, Sendable, Equatable {
    public let pendingOnly: Bool
    public let turnID: String?
    enum CodingKeys: String, CodingKey { case pendingOnly = "pending_only", turnID = "turn_id" }
    public init(pendingOnly: Bool = false, turnID: String? = nil) { self.pendingOnly = pendingOnly; self.turnID = turnID }
}

public struct SessionTodoPatch: Encodable, Sendable, Equatable {
    public let status: String?
    public let description: String?
    public let expectedDescription: String?
    enum CodingKeys: String, CodingKey { case status, description, expectedDescription = "expected_description" }
    public init(status: String? = nil, description: String? = nil, expectedDescription: String? = nil) {
        self.status = status; self.description = description; self.expectedDescription = expectedDescription
    }
}

public struct PendingMessageEditPayload: Encodable, Sendable, Equatable {
    public let turnID: String
    public let content: String
    public let expectedContent: String?
    enum CodingKeys: String, CodingKey { case turnID = "turn_id", content, expectedContent = "expected_content" }
    public init(turnID: String, content: String, expectedContent: String? = nil) {
        self.turnID = turnID; self.content = content; self.expectedContent = expectedContent
    }
}

public struct SessionEditPendingMessageRequest: Encodable, Sendable, Equatable {
    public let kind = "pending_message"
    public let pendingMessage: PendingMessageEditPayload
    enum CodingKeys: String, CodingKey { case kind, pendingMessage = "pending_message" }
    public init(pendingMessage: PendingMessageEditPayload) { self.pendingMessage = pendingMessage }
}

public enum SessionTodoStatus: String, Codable, Sendable, CaseIterable {
    case pending
    case inProgress = "in-progress"
    case completed
}

public enum SessionControlsError: Error, LocalizedError, Sendable, Equatable {
    case invalidPageLimit
    case identityMismatch
    case invalidTodoPatch
    case invalidSessionID
    case sessionNotExecutable
    public var errorDescription: String? {
        switch self {
        case .invalidPageLimit: "Recent chat page size must be between 1 and 24."
        case .identityMismatch: "The server response belongs to a different session."
        case .invalidTodoPatch: "A todo patch must change exactly one field and include the old description for compare-and-swap."
        case .invalidSessionID: "A session ID is required."
        case .sessionNotExecutable: "This session is read-only or does not support this operation."
        }
    }
}

public extension APIClient {
    func sessionStatus(_ sessionID: String) async throws -> SessionStatusResponse {
        try await request(SessionStatusResponse.self, path: Self.sessionPath(sessionID) + "/status")
    }

    func setSessionTitle(_ sessionID: String, title: String, metadata: SessionControlMetadata) async throws {
        try metadata.requireExecution(for: sessionID, capability: "session_editing")
        try await noContent(method: "PATCH", path: Self.sessionPath(sessionID) + "/title", body: SessionTitleRequest(title: title), accepted: [204])
    }

    func setSessionStarred(_ sessionID: String, starred: Bool, metadata: SessionControlMetadata) async throws {
        try metadata.requireExecution(for: sessionID, capability: "session_editing")
        try await noContent(method: "PATCH", path: Self.sessionPath(sessionID) + "/starred", body: SessionStarredRequest(starred: starred), accepted: [204])
    }

    func sessionModels(_ sessionID: String) async throws -> [SessionModelChoice] {
        try await request([SessionModelChoice].self, path: Self.sessionPath(sessionID) + "/models")
    }

    func refreshSessionModels(_ sessionID: String, metadata: SessionControlMetadata) async throws -> SessionControlMetadata {
        try metadata.requireExecution(for: sessionID, capability: "model_catalog_refresh")
        let response = try await request(SessionControlMetadata.self, method: "POST", path: Self.sessionPath(sessionID) + "/models/refresh")
        guard response.sessionID == sessionID else { throw SessionControlsError.identityMismatch }
        return response
    }

    func setSessionModel(_ sessionID: String, model: String, metadata: SessionControlMetadata) async throws -> SessionControlMetadata {
        try metadata.requireExecution(for: sessionID, capability: "model_switching")
        let response = try await request(SessionControlMetadata.self, method: "PATCH", path: Self.sessionPath(sessionID) + "/model", body: SessionModelRequest(model: model))
        guard response.sessionID == sessionID else { throw SessionControlsError.identityMismatch }
        return response
    }

    func thinkingLevels(_ sessionID: String) async throws -> ThinkingLevelResponse {
        try await request(ThinkingLevelResponse.self, path: Self.sessionPath(sessionID) + "/thinking-level")
    }

    func setThinkingLevel(_ sessionID: String, level: String, metadata: SessionControlMetadata) async throws -> ThinkingLevelResponse {
        try metadata.requireExecution(for: sessionID, capability: "thinking_levels")
        return try await request(ThinkingLevelResponse.self, method: "PATCH", path: Self.sessionPath(sessionID) + "/thinking-level", body: ThinkingLevelRequest(level: level))
    }

    func cycleThinkingLevel(_ sessionID: String, metadata: SessionControlMetadata) async throws -> ThinkingLevelResponse {
        try metadata.requireExecution(for: sessionID, capability: "thinking_levels")
        return try await request(ThinkingLevelResponse.self, method: "POST", path: Self.sessionPath(sessionID) + "/thinking-level/cycle")
    }

    func compactSession(_ sessionID: String, additionalPrompt: String? = nil, metadata: SessionControlMetadata) async throws {
        try metadata.requireExecution(for: sessionID, capability: "compaction")
        let body = additionalPrompt.map { SessionCompactRequest(additionalPrompt: $0) }
        try await noContent(method: "POST", path: Self.sessionPath(sessionID) + "/compact", body: body, accepted: [202])
    }

    func cancelSession(_ sessionID: String, turnID: String? = nil, pendingOnly: Bool = false, metadata: SessionControlMetadata) async throws -> CancelSessionResponse {
        try metadata.requireExecution(for: sessionID, capability: pendingOnly ? "pending_message_removal" : nil)
        let response = try await request(CancelSessionResponse.self, method: "POST", path: Self.sessionPath(sessionID) + "/cancel",
                                         body: SessionCancelRequest(pendingOnly: pendingOnly, turnID: turnID))
        guard response.sessionID == sessionID else { throw SessionControlsError.identityMismatch }
        return response
    }

    func sessionTodos(_ sessionID: String) async throws -> [SessionTodoWire] {
        try await request([SessionTodoWire].self, path: Self.sessionPath(sessionID) + "/todos")
    }

    func setTodoStatus(_ sessionID: String, todoID: String, status: SessionTodoStatus, metadata: SessionControlMetadata) async throws -> [SessionTodoWire] {
        try metadata.requireExecution(for: sessionID, capability: "todo_editing")
        return try await request([SessionTodoWire].self, method: "PATCH", path: Self.sessionPath(sessionID) + "/todos/" + Self.pathSegment(todoID), body: SessionTodoPatch(status: status.rawValue))
    }

    func updateTodoDescription(_ sessionID: String, todoID: String, expectedDescription: String, description: String, metadata: SessionControlMetadata) async throws -> [SessionTodoWire] {
        try metadata.requireExecution(for: sessionID, capability: "todo_editing")
        guard !expectedDescription.isEmpty, !description.isEmpty else { throw SessionControlsError.invalidTodoPatch }
        return try await request([SessionTodoWire].self, method: "PATCH", path: Self.sessionPath(sessionID) + "/todos/" + Self.pathSegment(todoID), body: SessionTodoPatch(description: description, expectedDescription: expectedDescription))
    }

    func removeTodo(_ sessionID: String, todoID: String, metadata: SessionControlMetadata) async throws -> [SessionTodoWire] {
        try metadata.requireExecution(for: sessionID, capability: "todo_editing")
        return try await request([SessionTodoWire].self, method: "DELETE", path: Self.sessionPath(sessionID) + "/todos/" + Self.pathSegment(todoID))
    }

    func editPendingSessionInput(_ sessionID: String, turnID: String, content: String, expectedContent: String? = nil, metadata: SessionControlMetadata) async throws -> SessionHistoryWire {
        try metadata.requireExecution(for: sessionID, capability: "session_editing")
        let updated = try await request(SessionHistoryWire.self, method: "PATCH", path: Self.sessionPath(sessionID),
                                        body: SessionEditPendingMessageRequest(pendingMessage: PendingMessageEditPayload(turnID: turnID, content: content, expectedContent: expectedContent)))
        guard updated.id == sessionID else { throw SessionControlsError.identityMismatch }
        return updated
    }

    func recentChat(_ sessionID: String, limit: Int = 12, cursor: String? = nil, source: String? = nil) async throws -> RecentChatResponse {
        guard (1...24).contains(limit) else { throw SessionControlsError.invalidPageLimit }
        var query = [URLQueryItem(name: "limit", value: String(limit))]
        if let cursor, !cursor.isEmpty { query.append(URLQueryItem(name: "cursor", value: cursor)) }
        if let source, !source.isEmpty { query.append(URLQueryItem(name: "source", value: source)) }
        let response = try await request(RecentChatResponse.self, path: Self.sessionPath(sessionID) + "/recent-chat", query: query)
        guard response.version == 1, response.sessionID == sessionID else { throw SessionControlsError.identityMismatch }
        return response
    }

    func sessionTree(_ sessionID: String) async throws -> SessionAgentTreeResponse {
        try await request(SessionAgentTreeResponse.self, path: Self.sessionPath(sessionID) + "/tree")
    }

    func sessionAgentInfo(_ sessionID: String) async throws -> AgentInfoResponse {
        try await request(AgentInfoResponse.self, path: Self.sessionPath(sessionID) + "/agent-info")
    }

    func sessionAgentConfig(_ sessionID: String, agent: String) async throws -> SessionAgentConfigResponse {
        guard !agent.isEmpty else { throw SessionControlsError.identityMismatch }
        let response = try await request(SessionAgentConfigResponse.self, path: Self.sessionPath(sessionID) + "/agent-config", query: [URLQueryItem(name: "agent", value: agent)])
        guard response.sessionID == sessionID, response.agentName == agent else { throw SessionControlsError.identityMismatch }
        return response
    }

    func sessionPermissions(_ sessionID: String) async throws -> SessionPermissionsResponse {
        try await request(SessionPermissionsResponse.self, path: Self.sessionPath(sessionID) + "/permissions")
    }

    func branchSession(_ sessionID: String, request body: SessionBranchRequest = .init(), metadata: SessionControlMetadata) async throws -> SessionBranchResponse {
        try metadata.requireExecution(for: sessionID, capability: "branching")
        let response = try await request(SessionBranchResponse.self, method: "POST", path: Self.sessionPath(sessionID) + "/branches", body: body, accepted: [201])
        guard let newID = response.session?.id, response.metadata.sessionID == newID else { throw SessionControlsError.identityMismatch }
        return response
    }

    func switchSessionAgent(_ sessionID: String, agentName: String, metadata: SessionControlMetadata) async throws -> SessionSwitchAgentResponse {
        try metadata.requireExecution(for: sessionID, capability: "session_editing")
        guard !agentName.isEmpty else { throw SessionControlsError.identityMismatch }
        let response = try await request(SessionSwitchAgentResponse.self, method: "POST", path: Self.sessionPath(sessionID) + "/switch-agent", body: SessionSwitchAgentRequest(agentName: agentName), accepted: [201])
        guard let newID = response.session?.id, response.metadata.sessionID == newID else { throw SessionControlsError.identityMismatch }
        return response
    }

    private func freshControlMetadata(for sessionID: String) async throws -> SessionControlMetadata {
        let response = try await sessionStatus(sessionID)
        guard response.metadata.sessionID == sessionID else { throw SessionControlsError.identityMismatch }
        return response.metadata
    }

    func setSessionTitle(_ sessionID: String, title: String) async throws {
        try await setSessionTitle(sessionID, title: title, metadata: freshControlMetadata(for: sessionID))
    }

    func setSessionStarred(_ sessionID: String, starred: Bool) async throws {
        try await setSessionStarred(sessionID, starred: starred, metadata: freshControlMetadata(for: sessionID))
    }

    func refreshSessionModels(_ sessionID: String) async throws -> SessionControlMetadata {
        try await refreshSessionModels(sessionID, metadata: freshControlMetadata(for: sessionID))
    }

    func setSessionModel(_ sessionID: String, model: String) async throws -> SessionControlMetadata {
        try await setSessionModel(sessionID, model: model, metadata: freshControlMetadata(for: sessionID))
    }

    func setThinkingLevel(_ sessionID: String, level: String) async throws -> ThinkingLevelResponse {
        try await setThinkingLevel(sessionID, level: level, metadata: freshControlMetadata(for: sessionID))
    }

    func cycleThinkingLevel(_ sessionID: String) async throws -> ThinkingLevelResponse {
        try await cycleThinkingLevel(sessionID, metadata: freshControlMetadata(for: sessionID))
    }

    func compactSession(_ sessionID: String, additionalPrompt: String? = nil) async throws {
        try await compactSession(sessionID, additionalPrompt: additionalPrompt, metadata: freshControlMetadata(for: sessionID))
    }

    func cancelSession(_ sessionID: String, turnID: String? = nil, pendingOnly: Bool = false) async throws -> CancelSessionResponse {
        try await cancelSession(sessionID, turnID: turnID, pendingOnly: pendingOnly, metadata: freshControlMetadata(for: sessionID))
    }

    func setTodoStatus(_ sessionID: String, todoID: String, status: SessionTodoStatus) async throws -> [SessionTodoWire] {
        try await setTodoStatus(sessionID, todoID: todoID, status: status, metadata: freshControlMetadata(for: sessionID))
    }

    func updateTodoDescription(_ sessionID: String, todoID: String, expectedDescription: String, description: String) async throws -> [SessionTodoWire] {
        try await updateTodoDescription(sessionID, todoID: todoID, expectedDescription: expectedDescription,
                                        description: description, metadata: freshControlMetadata(for: sessionID))
    }

    func removeTodo(_ sessionID: String, todoID: String) async throws -> [SessionTodoWire] {
        try await removeTodo(sessionID, todoID: todoID, metadata: freshControlMetadata(for: sessionID))
    }

    func editPendingSessionInput(_ sessionID: String, turnID: String, content: String, expectedContent: String? = nil) async throws -> SessionHistoryWire {
        try await editPendingSessionInput(sessionID, turnID: turnID, content: content, expectedContent: expectedContent,
                                          metadata: freshControlMetadata(for: sessionID))
    }

    func editPendingMessage(_ sessionID: String, turnID: String, content: String, expectedContent: String? = nil) async throws -> SessionHistoryWire {
        try await editPendingSessionInput(sessionID, turnID: turnID, content: content, expectedContent: expectedContent)
    }

    func branchSession(_ sessionID: String, request body: SessionBranchRequest = .init()) async throws -> SessionBranchResponse {
        try await branchSession(sessionID, request: body, metadata: freshControlMetadata(for: sessionID))
    }

    func switchSessionAgent(_ sessionID: String, agentName: String) async throws -> SessionSwitchAgentResponse {
        try await switchSessionAgent(sessionID, agentName: agentName, metadata: freshControlMetadata(for: sessionID))
    }

    private static func sessionPath(_ sessionID: String) -> String { "api/v2/sessions/" + pathSegment(sessionID) }
    private static func pathSegment(_ value: String) -> String {
        value.addingPercentEncoding(withAllowedCharacters: CharacterSet(charactersIn: "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-._~")) ?? value
    }
}
