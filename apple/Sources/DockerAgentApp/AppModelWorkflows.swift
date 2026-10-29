import AgentKit
import Foundation

public struct SessionBatchDeleteProjection: Sendable, Equatable {
    public let deletedIDs: [String]
    public let failedIDs: [String]
    public var deletedCount: Int { deletedIDs.count }
    public var failedCount: Int { failedIDs.count }
}

extension AppModel {
    public func loadPrompts(_ ref: SessionRef) async throws -> [SessionPrompt] {
        let (client, metadata) = try await workflowSession(ref, capability: "mcp_prompts")
        let result = try await client.sessionPrompts(ref.sessionID, metadata: metadata)
        try await ensureSameWorkflowClient(client, ref: ref)
        promptsBySession[ref] = result
        return result
    }

    /// Resolves prompt text into a draft result only; it never sends text to the agent.
    public func expandPrompt(_ ref: SessionRef, key: String, args: [String: String] = [:]) async throws -> String {
        let (client, metadata) = try await workflowSession(ref, capability: "mcp_prompts")
        let prompts = try await client.sessionPrompts(ref.sessionID, metadata: metadata)
        promptsBySession[ref] = prompts
        let text = try await client.executeSessionPrompt(ref.sessionID, key: key, args: args, metadata: metadata, discoveredPrompts: prompts)
        try await ensureSameWorkflowClient(client, ref: ref)
        return text
    }

    public func loadForkSkills(_ ref: SessionRef) async throws -> [SessionForkSkill] {
        let (client, metadata) = try await workflowSession(ref, capability: "fork_skills")
        let result = try await client.forkSkills(ref.sessionID, metadata: metadata)
        try await ensureSameWorkflowClient(client, ref: ref)
        forkSkillsBySession[ref] = result
        return result
    }

    /// `operation_id` is correlation only; a transport failure is surfaced as unknown and never retried automatically.
    public func runForkSkill(_ ref: SessionRef, name: String, task: String) async throws -> SkillOperationProjection {
        let (client, metadata) = try await workflowSession(ref, capability: "fork_skills", execution: true)
        let skills = try await client.forkSkills(ref.sessionID, metadata: metadata)
        forkSkillsBySession[ref] = skills
        guard skills.contains(where: { $0.name == name && $0.context == "fork" }),
              !task.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty else { throw SessionWorkflowError.invalidSkill }
        try await ensureSameWorkflowClient(client, ref: ref)
        let request = SessionForkSkillRunRequest(name: name, task: task)
        let row = SkillOperationProjection(operationID: request.operationID, skillName: name, state: .unknown,
                                           error: "The request is in progress. Its operation ID is correlation only and is not safe to retry.")
        upsertSkillOperation(row, ref: ref)
        do {
            let accepted = try await client.startForkSkill(ref.sessionID, request: request)
            try await ensureSameWorkflowClient(client, ref: ref)
            guard accepted.sessionID == ref.sessionID, accepted.operationID == request.operationID else { throw SessionWorkflowError.identityMismatch }
            var current = skillOperations[ref]?.first(where: { $0.operationID == request.operationID }) ?? row
            if current.state == .unknown { current.state = .accepted; current.error = nil; upsertSkillOperation(current, ref: ref) }
            return current
        } catch {
            if let apiError = error as? APIError, let statusCode = apiError.statusCode, (400..<500).contains(statusCode) {
                skillOperations[ref]?.removeAll { $0.operationID == request.operationID }
            } else {
                var current = skillOperations[ref]?.first(where: { $0.operationID == request.operationID }) ?? row
                if current.state != .completed && current.state != .failed {
                    current.state = .unknown
                    current.error = "The fork may have started, but its acceptance could not be confirmed. Do not repeat unless you accept possible duplication."
                    upsertSkillOperation(current, ref: ref)
                }
            }
            throw error
        }
    }

    public func loadContextBreakdown(_ ref: SessionRef) async throws -> SessionContextBreakdown {
        let (client, metadata) = try await workflowSession(ref, capability: "context_inspection")
        let result = try await client.sessionContext(ref.sessionID, metadata: metadata)
        try await ensureSameWorkflowClient(client, ref: ref)
        contextBySession[ref] = result
        return result
    }

    public func loadWorkspaceSnapshots(_ ref: SessionRef) async throws -> SessionWorkspaceSnapshots {
        let (client, metadata) = try await workflowSession(ref, capability: "snapshots")
        let result = try await client.workspaceSnapshots(ref.sessionID, metadata: metadata)
        try await ensureSameWorkflowClient(client, ref: ref)
        workspaceSnapshotsBySession[ref] = result
        return result
    }

    @discardableResult
    public func undoWorkspace(_ ref: SessionRef, proof: String, confirmed: Bool) async throws -> SessionWorkspaceSnapshotResult {
        let (client, metadata) = try await workflowSession(ref, capability: "snapshots", execution: true)
        let result = try await client.undoWorkspaceSnapshot(ref.sessionID, proof: proof, confirmed: confirmed)
        try await ensureSameWorkflowClient(client, ref: ref)
        workspaceSnapshotsBySession[ref] = try await client.workspaceSnapshots(ref.sessionID, metadata: metadata)
        return result
    }

    @discardableResult
    public func resetWorkspace(_ ref: SessionRef, keep: Int, proof: String, confirmed: Bool) async throws -> SessionWorkspaceSnapshotResult {
        let (client, metadata) = try await workflowSession(ref, capability: "snapshots", execution: true)
        let result = try await client.resetWorkspaceSnapshots(ref.sessionID, proof: proof, keep: keep, confirmed: confirmed)
        try await ensureSameWorkflowClient(client, ref: ref)
        workspaceSnapshotsBySession[ref] = try await client.workspaceSnapshots(ref.sessionID, metadata: metadata)
        return result
    }

    public func recoveryExport(_ ref: SessionRef) async throws -> SessionRecoveryExport {
        guard let server = servers.first(where: { $0.id == ref.serverID }), let client = clients[ref.serverID] else { throw APIError("Server is not connected.") }
        try await validateConnectedIdentity(serverID: ref.serverID, expectedInstanceID: server.identity?.instanceID)
        let result = try await client.recoveryExport(ref.sessionID, expectedInstanceID: server.identity?.instanceID ?? "")
        try await ensureSameWorkflowClient(client, ref: ref)
        return result
    }

    public func exportSessionsJSON(serverID: UUID, sessionIDs: [String], confirmedSessionIDs: Set<String>) async throws -> SessionJSONExport {
        guard !sessionIDs.isEmpty, Set(sessionIDs) == confirmedSessionIDs,
              sessionIDs.allSatisfy({ id in sessions.contains(where: { $0.ref.serverID == serverID && $0.ref.sessionID == id }) }),
              let server = servers.first(where: { $0.id == serverID }), let client = clients[serverID] else { throw SessionWorkflowError.destructiveConfirmationRequired }
        try await validateConnectedIdentity(serverID: serverID, expectedInstanceID: server.identity?.instanceID)
        let result = try await client.exportSessions(sessionIDs, expectedInstanceID: server.identity?.instanceID ?? "")
        guard clients[serverID] === client else { throw SessionWorkflowError.identityMismatch }
        return result
    }

    public func deleteSessions(serverID: UUID, sessionIDs: [String], confirmedSessionIDs: Set<String>) async throws -> SessionBatchDeleteProjection {
        guard !sessionIDs.isEmpty, Set(sessionIDs) == confirmedSessionIDs,
              sessionIDs.allSatisfy({ id in sessions.contains(where: { $0.ref.serverID == serverID && $0.ref.sessionID == id }) }),
              let server = servers.first(where: { $0.id == serverID }), let client = clients[serverID] else { throw SessionWorkflowError.destructiveConfirmationRequired }
        try await validateConnectedIdentity(serverID: serverID, expectedInstanceID: server.identity?.instanceID)
        let response = try await client.deleteSessions(sessionIDs, expectedInstanceID: server.identity?.instanceID ?? "", confirmedSessionIDs: confirmedSessionIDs)
        let deleted = try response.confirmedDeletedIDs(from: sessionIDs)
        guard clients[serverID] === client else { throw SessionWorkflowError.identityMismatch }
        for id in deleted { removeLocalSessionState(SessionRef(serverID: serverID, sessionID: id)) }
        sessions.removeAll { $0.ref.serverID == serverID && deleted.contains($0.ref.sessionID) }
        return SessionBatchDeleteProjection(deletedIDs: deleted, failedIDs: response.failedIDs ?? [])
    }

    private func workflowSession(_ ref: SessionRef, capability: String, execution: Bool = false) async throws -> (APIClient, SessionControlMetadata) {
        guard let client = clients[ref.serverID] else { throw APIError("Server is not connected.") }
        try await validateConnectedIdentity(serverID: ref.serverID)
        let status = try await freshWorkflowMetadata(client, ref: ref)
        if execution { try status.requireExecution(for: ref.sessionID, capability: capability) }
        else if !status.supports(capability) { throw SessionWorkflowError.unsupported }
        try await ensureSameWorkflowClient(client, ref: ref)
        return (client, status)
    }

    private func freshWorkflowMetadata(_ client: APIClient, ref: SessionRef) async throws -> SessionControlMetadata {
        let response = try await client.sessionStatus(ref.sessionID)
        guard response.metadata.sessionID == ref.sessionID, clients[ref.serverID] === client else { throw SessionWorkflowError.identityMismatch }
        return response.metadata
    }

    private func ensureSameWorkflowClient(_ client: APIClient, ref: SessionRef) async throws {
        try await validateConnectedIdentity(serverID: ref.serverID)
        guard clients[ref.serverID] === client else { throw SessionWorkflowError.identityMismatch }
    }

    func upsertSkillOperation(_ operation: SkillOperationProjection, ref: SessionRef) {
        var values = skillOperations[ref] ?? []
        if let index = values.firstIndex(where: { $0.operationID == operation.operationID }) { values[index] = operation }
        else { values.append(operation) }
        skillOperations[ref] = Array(values.suffix(32))
    }

    private func removeLocalSessionState(_ ref: SessionRef) {
        streamTasks[ref]?.cancel(); streamTasks[ref] = nil; sessionLeases[ref] = nil
        cancelOpeningSession(ref)
        openSessions[ref] = nil; sessionControls[ref] = nil; sessionControlMetadata[ref] = nil
        safetyBySession[ref] = nil; toolsBySession[ref] = nil; teamRosterBySession[ref] = nil; agentDetailsBySession[ref] = nil
        promptsBySession[ref] = nil; forkSkillsBySession[ref] = nil; contextBySession[ref] = nil; workspaceSnapshotsBySession[ref] = nil
        skillOperations[ref] = nil; recentChats[ref] = nil; reviewSnapshots[ref] = nil; pendingSends[ref] = nil
        needsYou.removeAll { $0.id == ref }
        promptsBySession[ref] = nil; forkSkillsBySession[ref] = nil; contextBySession[ref] = nil; workspaceSnapshotsBySession[ref] = nil
        let previewScopes = reviewSendPreviews.keys.filter { $0.serverID == ref.serverID && $0.sessionID == ref.sessionID }
        for scope in previewScopes { reviewSendPreviews[scope] = nil }
        let prefix = "\(ref.serverID.uuidString)|\(ref.sessionID)|"
        reviewWorktrees = reviewWorktrees.filter { !$0.key.hasPrefix(prefix) }
        reviewDirectories = reviewDirectories.filter { !$0.key.hasPrefix(prefix) }
        reviewDocuments = reviewDocuments.filter { !$0.key.hasPrefix(prefix) }
    }
}
