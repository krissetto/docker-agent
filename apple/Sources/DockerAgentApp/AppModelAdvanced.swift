import AgentKit
import Foundation

public struct SessionForkProjection: Sendable, Equatable {
    public let ref: SessionRef
    public let prefilledText: String
}

extension AppModel {
    /// Branches through the selected verified top-level item. A stale anchor is rejected; it is never retargeted.
    public func branchSessionItem(_ ref: SessionRef, anchor: SessionBranchAnchor, confirmed: Bool) async throws -> SessionRef {
        guard confirmed else { throw SessionWorkflowError.destructiveConfirmationRequired }
        guard anchor.ref == ref, anchor.itemPosition >= 0,
              openSessions[ref]?.transcript.contains(where: { $0.branchAnchor == anchor }) == true,
              servers.first(where: { $0.id == ref.serverID })?.identity?.instanceID == anchor.instanceID else { throw SessionAdvancedError.staleBranchAnchor }
        let client = try await advancedClient(ref, capability: "branching", execution: true)
        try await validateConnectedIdentity(serverID: ref.serverID)
        let (snapshot, proof) = try await verifiedRawSessionSnapshot(client: client, ref: ref)
        guard clients[ref.serverID] === client, let proof,
              proof.epoch == anchor.epoch, proof.cursor == anchor.cursor,
              proof.messagesSHA256 == anchor.messagesSHA256,
              proof.items.indices.contains(anchor.itemPosition),
              proof.items[anchor.itemPosition].sha256 == anchor.itemSHA256 else {
            throw SessionAdvancedError.staleBranchAnchor
        }
        guard let itemCount = snapshot.session?.messages?.count, anchor.itemPosition < itemCount else { throw SessionAdvancedError.invalidForkPosition }
        let status = try await client.sessionStatus(ref.sessionID)
        try status.metadata.requireExecution(for: ref.sessionID, capability: "branching")
        guard clients[ref.serverID] === client, status.metadata.sessionID == ref.sessionID else { throw SessionAdvancedError.identityMismatch }
        try await validateConnectedIdentity(serverID: ref.serverID)
        guard clients[ref.serverID] === client,
              openSessions[ref]?.epoch == anchor.epoch,
              openSessions[ref]?.cursor == anchor.cursor,
              openSessions[ref]?.transcript.contains(where: { $0.branchAnchor == anchor }) == true,
              servers.first(where: { $0.id == ref.serverID })?.identity?.instanceID == anchor.instanceID else {
            throw SessionAdvancedError.staleBranchAnchor
        }
        let response = try await client.branchSession(ref.sessionID,
            request: SessionBranchRequest(position: anchor.position, expectedSnapshot: proof.messagesSHA256), metadata: status.metadata)
        guard clients[ref.serverID] === client, let newID = response.session?.id, newID != ref.sessionID,
              response.metadata.sessionID == newID else { throw SessionAdvancedError.identityMismatch }
        await refreshCatalog(ref.serverID)
        return SessionRef(serverID: ref.serverID, sessionID: newID)
    }

    /// Performs the server's established tools-approval toggle. Enabling it requires explicit autonomous-mode acknowledgement.
    @discardableResult
    public func toggleToolsApproval(_ ref: SessionRef, confirmedAutonomous: Bool) async throws -> SafetyProjection {
        let before = try await loadSafety(ref)
        if !confirmedAutonomous { throw SessionAdvancedError.autonomousConfirmationRequired }
        let client = try await advancedClient(ref, capability: "session_editing", execution: true)
        try await validateConnectedIdentity(serverID: ref.serverID)
        guard clients[ref.serverID] === client else { throw SessionAdvancedError.identityMismatch }
        let metadata = try await freshInspectorMetadata(client, ref: ref)
        let latestWire: SessionPermissionsWireResponse = try await client.request(SessionPermissionsWireResponse.self, path: "api/v2/sessions/" + Self.advancedPath(ref.sessionID) + "/permissions")
        guard latestWire.toolsApproved == before.toolsApproved, clients[ref.serverID] === client else {
            throw APIError("Tool-approval mode changed concurrently. Refresh before toggling again.", statusCode: 409)
        }
        try await client.toggleSessionToolsApproval(ref.sessionID, metadata: metadata)
        guard clients[ref.serverID] === client else { throw SessionAdvancedError.identityMismatch }
        return try await loadSafety(ref)
    }

    private func freshInspectorMetadata(_ client: APIClient, ref: SessionRef) async throws -> SessionControlMetadata {
        let status = try await client.sessionStatus(ref.sessionID)
        guard clients[ref.serverID] === client, status.metadata.sessionID == ref.sessionID else { throw SessionAdvancedError.identityMismatch }
        return status.metadata
    }

    public func parentSession(_ ref: SessionRef) -> SessionRef? {
        openSessions[ref]?.parentSession ?? sessions.first(where: { $0.ref == ref })?.parentID.flatMap { id in
            guard !id.isEmpty, id != ref.sessionID else { return nil }
            return SessionRef(serverID: ref.serverID, sessionID: id)
        }
    }

    public func forkBeforeUserMessage(_ ref: SessionRef, index: Int, confirmed: Bool) async throws -> SessionForkProjection {
        guard confirmed, let openSession = openSessions[ref] else { throw SessionWorkflowError.destructiveConfirmationRequired }
        guard openSession.transcriptWindow.hasCompleteCanonicalCoverage else { throw SessionAdvancedError.transcriptWindowIncomplete }
        let client = try await advancedClient(ref, capability: "branching", execution: true)
        let metadataResponse = try await client.sessionStatus(ref.sessionID)
        guard clients[ref.serverID] === client, metadataResponse.metadata.sessionID == ref.sessionID else { throw SessionAdvancedError.identityMismatch }
        let snapshot: SessionSnapshotWire = try await client.request(SessionSnapshotWire.self, path: "api/v2/sessions/\(Self.advancedPath(ref.sessionID))/snapshot")
        guard clients[ref.serverID] === client, snapshot.session?.id == ref.sessionID else { throw SessionAdvancedError.identityMismatch }
        let candidates = snapshot.session?.forkUserMessages ?? []
        guard index >= 0, index < candidates.count else { throw SessionAdvancedError.invalidForkPosition }
        let target = candidates[index]
        guard target.canPrefillText, let current = openSessions[ref], current.connection != .unauthorized else { throw SessionAdvancedError.forkPositionUnavailable }
        let result = try await client.forkBeforeUserMessage(ref.sessionID, index: index, availableUserMessages: candidates.count, metadata: metadataResponse.metadata)
        let newID = result.id
        guard clients[ref.serverID] === client, newID != ref.sessionID else { throw SessionAdvancedError.identityMismatch }
        await refreshCatalog(ref.serverID)
        return SessionForkProjection(ref: SessionRef(serverID: ref.serverID, sessionID: newID), prefilledText: target.text)
    }

    public func registerSource(serverID: UUID, path: String, workingDir: String? = nil) async throws -> SourceRegistrationResponse {
        try await validateConnectedIdentity(serverID: serverID)
        guard let client = clients[serverID] else { throw APIError("Server is not connected.") }
        let result = try await client.registerSource(path: path, workingDir: workingDir)
        guard clients[serverID] === client else { throw SessionAdvancedError.identityMismatch }
        _ = try await loadAgents(serverID: serverID)
        return result
    }

    /// Edit only an untruncated, user-origin, text-only history message whose fresh snapshot still matches its recent-chat preview. This API has no compare-and-swap, so a concurrent edit can race the final check.
    public func editRecentUserMessage(_ ref: SessionRef, messageID: Int64, replacementText: String) async throws {
        guard messageID > 0, !replacementText.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty,
              replacementText.utf8.count <= 256 * 1024,
              let displayed = recentHistoryBySession[ref]?.messages.first(where: { $0.id == messageID }),
              displayed.role == "user", displayed.truncated != true,
              let client = clients[ref.serverID] else { throw SessionAdvancedError.invalidMessage }
        let status = try await client.sessionStatus(ref.sessionID)
        try status.metadata.requireExecution(for: ref.sessionID, capability: "session_editing")
        guard status.metadata.historical != true, status.status.pending == 0,
              !["running", "starting", "queued", "cancelling"].contains(status.status.state ?? "unknown") else { throw SessionAdvancedError.sessionBusy }
        try await validateConnectedIdentity(serverID: ref.serverID)
        let snapshot = try await client.request(JSONValue.self, path: "api/v2/sessions/\(Self.advancedPath(ref.sessionID))/snapshot")
        guard clients[ref.serverID] === client,
              let snapshotObject = snapshot.objectValue,
              let sessionObject = snapshotObject["session"]?.objectValue,
              sessionObject["id"]?.stringValue == ref.sessionID,
              let messages = sessionObject["messages"]?.arrayValue,
              messages.indices.contains(displayed.position),
              var outer = messages[displayed.position].objectValue,
              let entry = outer["message"]?.objectValue,
              entry["input_origin"]?.stringValue == nil || entry["input_origin"]?.stringValue == "user",
              entry["implicit"]?.boolValue != true,
              var message = entry["message"]?.objectValue,
              message["role"]?.stringValue == "user",
              message["content"]?.stringValue == displayed.text,
              message["multi_content"] == nil || message["multi_content"]?.arrayValue?.isEmpty == true else { throw SessionAdvancedError.invalidMessage }
        message["content"] = .string(replacementText)
        var updatedEntry = entry
        updatedEntry["message"] = .object(message)
        outer["message"] = .object(updatedEntry)
        let response = try await client.updateSessionMessage(ref.sessionID, durableMessageID: messageID, message: .object(updatedEntry))
        guard response.status == "updated", clients[ref.serverID] === client else { throw SessionAdvancedError.identityMismatch }
        recentHistoryBySession[ref] = nil
        _ = try? await refreshSession(ref)
    }

    public func pauseSession(_ ref: SessionRef) async throws -> SessionPauseResponse {
        let client = try await advancedClient(ref, capability: "pause", execution: true)
        let result = try await client.toggleSessionPause(ref.sessionID)
        guard clients[ref.serverID] === client else { throw SessionAdvancedError.identityMismatch }
        return result
    }

    public func stopSessionSubtree(_ node: AgentNodeProjection, confirmed: Bool) async throws -> StopSessionSubtreeResult {
        guard confirmed, let ref = node.session else { throw SessionAdvancedError.subtreeConfirmationRequired }
        let client = try await advancedClient(ref, capability: "stop_subtree", execution: true)
        let result = try await client.stopSessionSubtree(ref.sessionID, confirmedSessionID: ref.sessionID)
        guard clients[ref.serverID] === client else { throw SessionAdvancedError.identityMismatch }
        return result
    }

    public func waitForTurn(_ ref: SessionRef, turnID: String) async throws {
        let client = try await advancedClient(ref)
        try await client.waitForTurn(ref.sessionID, turnID: turnID)
        guard clients[ref.serverID] === client else { throw SessionAdvancedError.identityMismatch }
    }

    public func waitForWorkflow(_ ref: SessionRef) async throws -> SessionWorkflowSettlement {
        let client = try await advancedClient(ref)
        let result = try await client.waitForWorkflow(ref.sessionID)
        guard clients[ref.serverID] === client else { throw SessionAdvancedError.identityMismatch }
        return result
    }

    public func delegationPolicy(_ ref: SessionRef) async throws -> DelegationPolicyResponse {
        let client = try await advancedClient(ref, capability: "delegation_policy")
        let status = try await client.sessionStatus(ref.sessionID)
        let result = try await client.delegationPolicy(ref.sessionID, metadata: status.metadata)
        guard clients[ref.serverID] === client else { throw SessionAdvancedError.identityMismatch }
        return result
    }

    public func setDelegationPolicy(_ ref: SessionRef, enabled: Bool) async throws -> DelegationPolicyResponse {
        let client = try await advancedClient(ref, capability: "delegation_policy", execution: true)
        let status = try await client.sessionStatus(ref.sessionID)
        let result = try await client.setDelegationPolicy(ref.sessionID, enabled: enabled, metadata: status.metadata)
        guard clients[ref.serverID] === client else { throw SessionAdvancedError.identityMismatch }
        return result
    }

    public func removeSessionAttachment(_ ref: SessionRef, path: String) async throws {
        let client = try await advancedClient(ref, capability: "session_editing", execution: true)
        let status = try await client.sessionStatus(ref.sessionID)
        try await client.removeSessionAttachment(ref.sessionID, path: path, metadata: status.metadata)
        guard clients[ref.serverID] === client else { throw SessionAdvancedError.identityMismatch }
    }

    private func advancedClient(_ ref: SessionRef, capability: String? = nil, execution: Bool = false) async throws -> APIClient {
        guard let client = clients[ref.serverID] else { throw APIError("Server is not connected.") }
        try await validateConnectedIdentity(serverID: ref.serverID)
        let status = try await client.sessionStatus(ref.sessionID)
        guard clients[ref.serverID] === client, status.metadata.sessionID == ref.sessionID else { throw SessionAdvancedError.identityMismatch }
        if let capability, execution { try status.metadata.requireExecution(for: ref.sessionID, capability: capability) }
        else if let capability, !status.metadata.supports(capability) { throw SessionAdvancedError.unsupported }
        return client
    }

    private static func advancedPath(_ id: String) -> String {
        id.addingPercentEncoding(withAllowedCharacters: CharacterSet(charactersIn: "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-._~")) ?? id
    }
}
