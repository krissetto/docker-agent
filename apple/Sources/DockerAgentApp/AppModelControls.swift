import AgentKit
import Foundation

extension AppModel {
    public func loadSessionControls(_ ref: SessionRef) async throws -> SessionControlsProjection {
        guard let client = controlClient(for: ref) else { throw APIError("Server is not connected.") }
        let status = try await client.sessionStatus(ref.sessionID)
        guard status.metadata.sessionID == ref.sessionID else { throw SessionControlsError.identityMismatch }
        let raw = status.metadata.capabilities ?? [:]
        let capabilities = SessionCapabilities(raw: raw, availableModels: [], thinkingLevels: status.metadata.thinkingLevels ?? [])
        async let todosRequest: [SessionTodoWire]? = capabilities.supports("todos") ? try? await client.sessionTodos(ref.sessionID) : nil
        async let modelsRequest: [SessionModelChoice]? = capabilities.supports("model_switching") ? try? await client.sessionModels(ref.sessionID) : nil
        async let thinkingRequest: ThinkingLevelResponse? = capabilities.supports("thinking_levels") ? try? await client.thinkingLevels(ref.sessionID) : nil
        async let treeRequest: SessionAgentTreeResponse? = (capabilities.supports("stop_subtree") || capabilities.supports("branching")) ? try? await client.sessionTree(ref.sessionID) : nil
        let snapshot: SessionSnapshotWire? = try? await client.request(SessionSnapshotWire.self, path: "api/v2/sessions/\(Self.controlPath(ref.sessionID))/snapshot")
        let models = await modelsRequest ?? []
        let thinking = await thinkingRequest
        let todos = await todosRequest ?? []
        let tree = await treeRequest
        let agentInfo: AgentInfoResponse? = try? await client.sessionAgentInfo(ref.sessionID)
        let availableAgents: [TeamAgentLeaf] = agentInfo?.team?.availableAgents ?? []
        let agentTree: [AgentNodeProjection] = availableAgents.isEmpty
            ? Self.projectTree(tree, serverID: ref.serverID)
            : availableAgents.map { (details: TeamAgentLeaf) in AgentNodeProjection(id: details.name, agentName: details.name, state: .unknown) }
        let projection = SessionControlsProjection(
            ref: ref,
            starred: snapshot?.session?.starred ?? sessions.first(where: { $0.ref == ref })?.starred ?? false,
            model: status.metadata.model,
            thinkingLevel: thinking?.current ?? status.metadata.thinkingLevel,
            thinkingLevels: thinking?.levels ?? status.metadata.thinkingLevels ?? [],
            historical: status.metadata.historical ?? false,
            executionAvailable: status.metadata.executionAvailable == true && status.metadata.historical != true,
            paused: status.status.paused ?? false,
            capabilities: SessionCapabilities(raw: raw, availableModels: models.map(\.ref), thinkingLevels: thinking?.levels ?? []),
            todos: todos,
            pendingInputs: (snapshot?.pendingInputs ?? []).map { PendingInputProjection(turnID: $0.turnID, content: $0.content, origin: $0.inputOrigin, senderName: $0.senderName) },
            agentTree: agentTree
        )
        sessionControls[ref] = projection
        return projection
    }

    public func setSessionTitle(_ ref: SessionRef, title: String) async throws {
        guard let client = controlClient(for: ref) else { throw APIError("Server is not connected.") }
        try await client.setSessionTitle(ref.sessionID, title: title)
        if let i = sessions.firstIndex(where: { $0.ref == ref }) { sessions[i].title = title }
        if var open = openSessions[ref] { open.title = title; openSessions[ref] = open }
    }

    public func setSessionStarred(_ ref: SessionRef, starred: Bool) async throws {
        guard let client = controlClient(for: ref) else { throw APIError("Server is not connected.") }
        try await client.setSessionStarred(ref.sessionID, starred: starred)
        if let i = sessions.firstIndex(where: { $0.ref == ref }) { sessions[i].starred = starred }
        if var controls = sessionControls[ref] { controls.starred = starred; sessionControls[ref] = controls }
    }

    public func sessionModels(_ ref: SessionRef) async throws -> [SessionModelChoice] {
        guard let client = controlClient(for: ref) else { throw APIError("Server is not connected.") }
        return try await client.sessionModels(ref.sessionID)
    }

    public func refreshSessionModels(_ ref: SessionRef) async throws -> [SessionModelChoice] {
        guard let client = controlClient(for: ref) else { throw APIError("Server is not connected.") }
        _ = try await client.refreshSessionModels(ref.sessionID)
        return try await client.sessionModels(ref.sessionID)
    }

    public func setSessionModel(_ ref: SessionRef, model: String) async throws {
        guard let client = controlClient(for: ref) else { throw APIError("Server is not connected.") }
        let current = try await client.sessionStatus(ref.sessionID)
        try current.metadata.requireExecution(for: ref.sessionID, capability: "model_switching")
        let updated = try await client.setSessionModel(ref.sessionID, model: model)
        if var controls = sessionControls[ref] { controls.model = updated.model; sessionControls[ref] = controls }
    }

    public func setThinkingLevel(_ ref: SessionRef, level: String) async throws {
        guard let client = controlClient(for: ref) else { throw APIError("Server is not connected.") }
        let current = try await client.sessionStatus(ref.sessionID)
        try current.metadata.requireExecution(for: ref.sessionID, capability: "thinking_levels")
        let updated = try await client.setThinkingLevel(ref.sessionID, level: level)
        if var controls = sessionControls[ref] { controls.thinkingLevel = updated.current; controls.thinkingLevels = updated.levels; sessionControls[ref] = controls }
    }

    public func compactSession(_ ref: SessionRef, additionalPrompt: String? = nil) async throws {
        guard let client = controlClient(for: ref) else { throw APIError("Server is not connected.") }
        let current = try await client.sessionStatus(ref.sessionID)
        try current.metadata.requireExecution(for: ref.sessionID, capability: "compaction")
        try await client.compactSession(ref.sessionID, additionalPrompt: additionalPrompt)
    }

    public func cancelPendingInputs(_ ref: SessionRef, turnID: String? = nil) async throws {
        guard let client = controlClient(for: ref) else { throw APIError("Server is not connected.") }
        _ = try await client.cancelSession(ref.sessionID, turnID: turnID, pendingOnly: true)
        _ = try await loadSessionControls(ref)
    }

    public func editPendingInput(_ ref: SessionRef, turnID: String, content: String, expectedContent: String? = nil) async throws {
        guard let client = controlClient(for: ref) else { throw APIError("Server is not connected.") }
        _ = try await client.editPendingMessage(ref.sessionID, turnID: turnID, content: content, expectedContent: expectedContent)
        _ = try await loadSessionControls(ref)
    }

    public func setTodoStatus(_ ref: SessionRef, todoID: String, status: SessionTodoStatus) async throws {
        guard let client = controlClient(for: ref) else { throw APIError("Server is not connected.") }
        let result = try await client.setTodoStatus(ref.sessionID, todoID: todoID, status: status)
        if var controls = sessionControls[ref] { controls.todos = result; sessionControls[ref] = controls }
    }

    public func updateTodoDescription(_ ref: SessionRef, todoID: String, expected: String, description: String) async throws {
        guard let client = controlClient(for: ref) else { throw APIError("Server is not connected.") }
        let result = try await client.updateTodoDescription(ref.sessionID, todoID: todoID, expectedDescription: expected, description: description)
        if var controls = sessionControls[ref] { controls.todos = result; sessionControls[ref] = controls }
    }

    public func removeTodo(_ ref: SessionRef, todoID: String) async throws {
        guard let client = controlClient(for: ref) else { throw APIError("Server is not connected.") }
        let result = try await client.removeTodo(ref.sessionID, todoID: todoID)
        if var controls = sessionControls[ref] { controls.todos = result; sessionControls[ref] = controls }
    }

    public func recentChat(_ ref: SessionRef, limit: Int = 12, cursor: String? = nil) async throws -> RecentChatResponse {
        guard let client = controlClient(for: ref) else { throw APIError("Server is not connected.") }
        let source = sessions.first(where: { $0.ref == ref })?.source
        let result = try await client.recentChat(ref.sessionID, limit: limit, cursor: cursor, source: source)
        recentChats[ref] = result
        return result
    }

    public func loadRecentHistory(_ ref: SessionRef, limit: Int = 24) async throws -> RecentHistoryProjection {
        guard let client = controlClient(for: ref) else { throw APIError("Server is not connected.") }
        guard (1...24).contains(limit) else { throw SessionControlsError.invalidPageLimit }
        let source = sessions.first(where: { $0.ref == ref })?.source
        let response = try await client.recentChat(ref.sessionID, limit: limit, source: source)
        guard clients[ref.serverID] === client else { throw SessionControlsError.identityMismatch }
        let result = RecentHistoryProjection(ref: ref, messages: response.messages, beforeCursor: response.beforeCursor, truncatedBefore: response.truncatedBefore)
        recentHistoryBySession[ref] = result
        return result
    }

    @discardableResult public func loadEarlierHistory(_ ref: SessionRef, limit: Int = 24) async throws -> RecentHistoryProjection {
        guard let client = controlClient(for: ref), let existing = recentHistoryBySession[ref] else { throw APIError("Load recent history before paging earlier.") }
        guard existing.hasMore, let cursor = existing.beforeCursor else { return existing }
        let source = sessions.first(where: { $0.ref == ref })?.source
        let page = try await client.recentChat(ref.sessionID, limit: limit, cursor: cursor, source: source)
        guard clients[ref.serverID] === client else { throw SessionControlsError.identityMismatch }
        let seen = Set(existing.messages.map(\.id))
        let appended = page.messages.filter { !seen.contains($0.id) }
        let result = RecentHistoryProjection(ref: ref, messages: existing.messages + appended, beforeCursor: page.beforeCursor, truncatedBefore: page.truncatedBefore)
        recentHistoryBySession[ref] = result
        return result
    }

    public func clearRecentHistory(_ ref: SessionRef) { recentHistoryBySession[ref] = nil }

    public func branchSession(_ ref: SessionRef, position: Int? = nil, expectedSnapshot: String? = nil) async throws -> SessionRef {
        guard let client = controlClient(for: ref) else { throw APIError("Server is not connected.") }
        if position != nil && (expectedSnapshot?.isEmpty ?? true) { throw APIError("Branching from an arbitrary position requires its expected snapshot proof.") }
        let result = try await client.branchSession(ref.sessionID, request: SessionBranchRequest(position: position, expectedSnapshot: expectedSnapshot))
        guard let id = result.session?.id else { throw SessionControlsError.identityMismatch }
        let branch = SessionRef(serverID: ref.serverID, sessionID: id)
        await refreshCatalog(ref.serverID)
        return branch
    }

    public func switchSessionAgent(_ ref: SessionRef, agentName: String) async throws -> SessionRef {
        guard let client = controlClient(for: ref) else { throw APIError("Server is not connected.") }
        let result = try await client.switchSessionAgent(ref.sessionID, agentName: agentName)
        guard let id = result.session?.id else { throw SessionControlsError.identityMismatch }
        let branch = SessionRef(serverID: ref.serverID, sessionID: id)
        await refreshCatalog(ref.serverID)
        return branch
    }

    private func controlClient(for ref: SessionRef) -> APIClient? { clients[ref.serverID] }
    private static func controlPath(_ id: String) -> String { id.addingPercentEncoding(withAllowedCharacters: CharacterSet(charactersIn: "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-._~")) ?? id }
    private static func projectTree(_ tree: SessionAgentTreeResponse?, serverID: UUID) -> [AgentNodeProjection] {
        guard let nodes = tree?.nodes else { return [] }
        func project(_ raw: JSONValue) -> AgentNodeProjection? {
            guard let object = raw.objectValue else { return nil }
            let node = object["node"]?.objectValue ?? object
            let id = node["id"]?.stringValue ?? UUID().uuidString
            let agent = node["agent"]?.stringValue ?? "Agent"
            let session = node["session_id"]?.stringValue.map { SessionRef(serverID: serverID, sessionID: $0) }
            let state: SessionState
            switch node["state"]?.stringValue { case "running": state = .running; case "failed": state = .failed; case "stopped", "idle", "completed": state = .settled; default: state = .unknown }
            let children = (object["children"]?.arrayValue ?? []).compactMap(project)
            return AgentNodeProjection(id: id, session: session, agentName: node["name"]?.stringValue ?? agent, state: state, needsAttention: node["needs_attention"]?.boolValue ?? false, waitingOn: node["waiting_on"]?.stringValue, cost: node["cost"]?.numberValue ?? 0, inputTokens: Int64(node["input_tokens"]?.numberValue ?? 0), outputTokens: Int64(node["output_tokens"]?.numberValue ?? 0), children: children)
        }
        return nodes.compactMap(project)
    }
}
