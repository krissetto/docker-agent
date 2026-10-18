import AgentKit
import Foundation

extension AppModel {
    public func loadSafety(_ ref: SessionRef) async throws -> SafetyProjection {
        let client = try await freshInspectorClient(ref, capability: "permissions_inspection")
        let wire: SessionPermissionsWireResponse = try await client.request(SessionPermissionsWireResponse.self, path: Self.inspectorPath(ref) + "/permissions")
        let projection = Self.safetyProjection(ref, wire: wire, metadata: try await freshMetadata(client, ref))
        safetyBySession[ref] = projection
        return projection
    }

    @discardableResult
    public func setSafetyPolicy(_ ref: SessionRef, policy: SafetyPolicyValue?) async throws -> SafetyProjection {
        let client = try await freshInspectorClient(ref, capability: "session_editing", execution: true)
        try await validateConnectedIdentity(serverID: ref.serverID)
        guard clients[ref.serverID] === client else { throw APIError("Server changed during the operation.") }
        try await client.noContent(method: "PATCH", path: Self.inspectorPath(ref) + "/safety-policy", body: SessionSafetyPolicyPatch(safetyPolicy: policy?.rawValue ?? ""), accepted: [200])
        return try await loadSafety(ref)
    }

    @discardableResult
    public func setPermissionRule(_ ref: SessionRef, list: PermissionList, pattern: String, enabled: Bool, expected: PermissionRules) async throws -> SafetyProjection {
        let normalized = pattern.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !normalized.isEmpty, normalized.utf8.count <= 256 else { throw APIError("Enter a permission pattern of at most 256 bytes.") }
        let client = try await freshInspectorClient(ref, capability: "session_editing", execution: true)
        try await validateConnectedIdentity(serverID: ref.serverID)
        guard clients[ref.serverID] === client else { throw APIError("Server changed during the operation.") }
        let currentWire: SessionPermissionsWireResponse = try await client.request(SessionPermissionsWireResponse.self, path: Self.inspectorPath(ref) + "/permissions")
        guard currentWire.session ?? PermissionRules() == expected else { throw APIError("Session permission rules changed. Refresh before editing.") }
        let latest = try await freshMetadata(client, ref)
        try latest.requireExecution(for: ref.sessionID, capability: "session_editing")
        var updated = expected
        var values = updated[list]
        if enabled {
            guard !values.contains(normalized) else { return Self.safetyProjection(ref, wire: currentWire, metadata: try await freshMetadata(client, ref)) }
            values.append(normalized)
        } else {
            guard values.contains(normalized) else { return Self.safetyProjection(ref, wire: currentWire, metadata: try await freshMetadata(client, ref)) }
            values.removeAll { $0 == normalized }
        }
        updated[list] = values
        try await client.noContent(method: "PATCH", path: Self.inspectorPath(ref) + "/permissions", body: SessionPermissionsPatch(permissions: updated), accepted: [200])
        return try await loadSafety(ref)
    }

    public func loadSessionTools(_ ref: SessionRef) async throws -> SessionToolsProjection {
        let client = try await freshInspectorClient(ref, capability: "tool_inspection")
        let wire: SessionToolsWireResponse = try await client.request(SessionToolsWireResponse.self, path: Self.inspectorPath(ref) + "/tools")
        let metadata = try await freshMetadata(client, ref)
        let projection = Self.toolsProjection(ref, wire: wire, canRestart: metadata.supports("toolset_restart") && metadata.canExecute)
        toolsBySession[ref] = projection
        return projection
    }

    @discardableResult
    public func restartToolset(_ ref: SessionRef, name: String) async throws -> SessionToolsProjection {
        let client = try await freshInspectorClient(ref, capability: "toolset_restart", execution: true)
        let wire: SessionToolsWireResponse = try await client.request(SessionToolsWireResponse.self, path: Self.inspectorPath(ref) + "/tools")
        guard let toolset = wire.statuses.first(where: { $0.name == name }), toolset.restartable else { throw APIError("This toolset is not currently restartable.") }
        try await client.noContent(method: "POST", path: Self.inspectorPath(ref) + "/toolsets/restart", body: SessionToolsetRestartRequest(name: name), accepted: [204])
        return try await loadSessionTools(ref)
    }

    public func loadTeamRoster(_ ref: SessionRef) async throws -> [TeamAgentProjection] {
        let client = try await freshInspectorClient(ref)
        let info = try await client.sessionAgentInfo(ref.sessionID)
        guard let team = info.team, let available = team.availableAgents else { return [] }
        let roster = available.map { TeamAgentProjection(name: $0.name, description: $0.description, model: $0.model, isCurrent: $0.name == team.currentAgent) }
        teamRosterBySession[ref] = roster
        return roster
    }

    public func loadAgentDetails(_ ref: SessionRef, agent: String) async throws -> AgentDetailsProjection {
        let client = try await freshInspectorClient(ref)
        let status = try await freshMetadata(client, ref)
        let config = try await client.sessionAgentConfig(ref.sessionID, agent: agent)
        let presentation = try await client.sessionAgentInfo(ref.sessionID)
        let leaf = presentation.team?.availableAgents?.first { $0.name == agent }
        let agentInfo = config.info.objectValue ?? [:]
        let toolsets = agentInfo["Toolsets"]?.arrayValue?.compactMap(Self.agentConfigToolset) ?? []
        let projection = AgentDetailsProjection(
            ref: ref, agentName: agent, description: leaf?.description ?? "", model: leaf?.model ?? "",
            fallbacks: Self.strings(agentInfo["Fallbacks"]), subAgents: Self.strings(agentInfo["SubAgents"]),
            handoffs: Self.strings(agentInfo["Handoffs"]), skills: Self.strings(agentInfo["Skills"]), options: Self.strings(agentInfo["Options"]),
            maxIterations: Int(agentInfo["MaxIterations"]?.numberValue ?? 0),
            historyItems: Int(agentInfo["NumHistoryItems"]?.numberValue ?? 0),
            consecutiveToolCalls: Int(agentInfo["MaxConsecutiveToolCalls"]?.numberValue ?? 0),
            toolsets: toolsets, historical: config.historical == true || !status.canExecute,
            historyReason: config.historyReason, isCurrent: agentInfo["IsCurrent"]?.boolValue ?? false
        )
        agentDetailsBySession[ref, default: [:]][agent] = projection
        return projection
    }

    private func freshInspectorClient(_ ref: SessionRef, capability: String? = nil, execution: Bool = false) async throws -> APIClient {
        guard let client = clients[ref.serverID] else { throw APIError("Server is not connected.") }
        let metadata = try await freshMetadata(client, ref)
        if execution { try metadata.requireExecution(for: ref.sessionID, capability: capability) }
        else if let capability, !metadata.supports(capability) { throw APIError("This session does not support this inspector operation.") }
        guard clients[ref.serverID] === client else { throw APIError("Server changed during the operation.") }
        return client
    }

    private func freshMetadata(_ client: APIClient, _ ref: SessionRef) async throws -> SessionControlMetadata {
        let current = try await client.sessionStatus(ref.sessionID)
        guard current.metadata.sessionID == ref.sessionID,
              clients[ref.serverID] === client else { throw SessionControlsError.identityMismatch }
        return current.metadata
    }

    private static func safetyProjection(_ ref: SessionRef, wire: SessionPermissionsWireResponse, metadata: SessionControlMetadata) -> SafetyProjection {
        let normalized: SafetyPolicyValue?
        switch wire.policy {
        case "strict", "safer": normalized = .strict
        case "balanced", "safe-auto": normalized = .balanced
        case "restricted": normalized = .restricted
        case "autonomous", "unsafe": normalized = .autonomous
        default: normalized = nil
        }
        return SafetyProjection(ref: ref, policy: normalized, toolsApproved: wire.toolsApproved,
                                sourceRules: wire.source ?? PermissionRules(), sessionRules: wire.session ?? PermissionRules(),
                                canEditPolicy: metadata.canExecute && metadata.supports("session_editing"),
                                canEditSessionRules: metadata.canExecute && metadata.supports("session_editing"))
    }

    private static func toolsProjection(_ ref: SessionRef, wire: SessionToolsWireResponse, canRestart: Bool) -> SessionToolsProjection {
        SessionToolsProjection(ref: ref,
                               tools: wire.tools.map { SessionToolDescriptor(name: $0.name, category: $0.category, description: $0.description ?? "") },
                               toolsets: wire.statuses.map { SessionToolsetProjection(name: $0.name, kind: $0.kind ?? "", description: $0.description ?? "", state: Self.lifecycleState($0.state), lastError: $0.lastError, restartCount: $0.restartCount, restartable: $0.restartable) },
                               canRestartToolsets: canRestart)
    }

    private static func lifecycleState(_ value: Int) -> String {
        switch value { case 0: "stopped"; case 1: "starting"; case 2: "ready"; case 3: "degraded"; case 4: "restarting"; case 5: "failed"; default: "unknown (\(value))" }
    }

    private static func strings(_ value: JSONValue?) -> [String] { value?.arrayValue?.compactMap(\.stringValue) ?? [] }
    private static func agentConfigToolset(_ value: JSONValue) -> SessionToolsetProjection? {
        guard let object = value.objectValue,
              let name = object["Name"]?.stringValue,
              let kind = object["Kind"]?.stringValue,
              let state = object["State"]?.stringValue,
              let tools = object["Tools"]?.arrayValue else { return nil }
        let listedTools = tools.compactMap(\.stringValue)
        return SessionToolsetProjection(name: name, kind: kind, state: state, restartable: false, tools: listedTools)
    }

    private static func inspectorPath(_ ref: SessionRef) -> String {
        let id = ref.sessionID.addingPercentEncoding(withAllowedCharacters: CharacterSet(charactersIn: "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-._~")) ?? ref.sessionID
        return "api/v2/sessions/\(id)"
    }
}
