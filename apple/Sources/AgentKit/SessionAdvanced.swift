import Foundation

public struct SessionPauseResponse: Decodable, Sendable, Equatable { public let paused: Bool }
public struct StopSessionSubtreeResult: Sendable, Equatable { public let sessionID: String }
public struct SessionWorkflowSettlement: Decodable, Sendable, Equatable { public let epoch: String; public let cursor: UInt64 }

public struct SessionForkRequest: Encodable, Sendable, Equatable {
    public let userMessageIndex: Int
    enum CodingKeys: String, CodingKey { case userMessageIndex = "user_message_index" }
    public init(userMessageIndex: Int) { self.userMessageIndex = userMessageIndex }
}
public struct SessionForkResponse: Decodable, Sendable, Equatable {
    public let id: String
    public let title: String?
    public let parentID: String?
    public let source: String?
    public let agentName: String?
    enum CodingKeys: String, CodingKey { case id, title, parentID = "parent_id", source, agentName = "agent_name" }
}
public struct SessionMessageUpdateRequest: Encodable, Sendable, Equatable {
    public let message: JSONValue
    public init(message: JSONValue) { self.message = message }
}
public struct SessionMessageUpdateResponse: Decodable, Sendable, Equatable { public let status: String }
public struct AttachmentRemovalRequest: Encodable, Sendable, Equatable {
    public let path: String
    public init(path: String) { self.path = path }
}
public struct DelegationPolicyResponse: Decodable, Sendable, Equatable { public let enabled: Bool }
public struct DelegationPolicyRequest: Encodable, Sendable, Equatable {
    public let enabled: Bool
    public init(enabled: Bool) { self.enabled = enabled }
}
public struct SourceRegistrationRequest: Encodable, Sendable, Equatable {
    public let path: String
    public let workingDir: String?
    enum CodingKeys: String, CodingKey { case path, workingDir = "working_dir" }
    public init(path: String, workingDir: String? = nil) { self.path = path; self.workingDir = workingDir }
}
public struct SourceRegistrationResponse: Decodable, Sendable, Equatable {
    public let sourceID: String
    public let revision: String
    enum CodingKeys: String, CodingKey { case sourceID = "source_id", revision }
}
public struct SourceRegistrationError: Error, LocalizedError, Sendable, Equatable {
    public let message: String
    public var errorDescription: String? { message }
}

public enum SessionAdvancedError: Error, LocalizedError, Sendable, Equatable {
    case unsupported, sessionNotExecutable, sessionBusy, identityMismatch, invalidTurnID
    case invalidForkPosition, forkPositionUnavailable, invalidMessageID, invalidMessage
    case invalidAttachmentPath, invalidSourcePath, invalidWorkdir, sourceRegistrationUnavailable, subtreeConfirmationRequired, autonomousConfirmationRequired, staleBranchAnchor, invalidResponse
    public var errorDescription: String? {
        switch self {
        case .unsupported: "This operation is not supported by the selected session."
        case .sessionNotExecutable: "This historical or read-only session cannot be changed."
        case .sessionBusy: "This operation requires the session to be settled with no queued inputs."
        case .identityMismatch: "The server response belongs to a different session."
        case .invalidTurnID: "A stable accepted turn ID is required."
        case .invalidForkPosition: "Choose a nonnegative user-message position."
        case .forkPositionUnavailable: "Fork position needs to be within the available user messages."
        case .invalidMessageID: "A positive durable message ID is required."
        case .invalidMessage: "An editable message is required."
        case .invalidAttachmentPath: "Choose an absolute path from this session's attached-file list."
        case .invalidSourcePath: "Enter a server-accessible source path."
        case .sourceRegistrationUnavailable: "This server does not provide source registration."
        case .invalidWorkdir: "The working directory must be a local absolute path or omitted."
        case .subtreeConfirmationRequired: "Confirm the selected session subtree before stopping it."
        case .autonomousConfirmationRequired: "Confirm autonomous mode before enabling tool auto-approval."
        case .staleBranchAnchor: "The transcript changed. Refresh and select a current item to branch."
        case .invalidResponse: "The server returned an invalid operation result."
        }
    }
}

public extension APIClient {
    /// Registers a path on the server host; this never uploads or resolves client-local file URLs.
    func registerSource(path: String, workingDir: String? = nil) async throws -> SourceRegistrationResponse {
        guard !path.isEmpty, path.utf8.count <= 2048,
              path.unicodeScalars.allSatisfy({ !CharacterSet.controlCharacters.contains($0) }),
              !path.lowercased().hasPrefix("file:") else { throw SessionAdvancedError.invalidSourcePath }
        if let workingDir, !workingDir.isEmpty {
            guard workingDir.hasPrefix("/"), workingDir.utf8.count <= 2048,
                  !workingDir.contains("\\"), !workingDir.split(separator: "/").contains("..") else { throw SessionAdvancedError.invalidWorkdir }
        }
        do {
            let response = try await request(SourceRegistrationResponse.self, method: "POST", path: "api/v2/sources", body: SourceRegistrationRequest(path: path, workingDir: workingDir))
            guard !response.sourceID.isEmpty, !response.revision.isEmpty else { throw SessionAdvancedError.identityMismatch }
            return response
        } catch let error as APIError where error.statusCode == 404 {
            throw SessionAdvancedError.sourceRegistrationUnavailable
        }
    }

    /// POST /tools/toggle is a real server-side legacy safety-mode toggle, not a client-inferred policy rewrite.
    func toggleSessionToolsApproval(_ sessionID: String, metadata: SessionControlMetadata) async throws {
        try metadata.requireExecution(for: sessionID, capability: "session_editing")
        try await noContent(method: "POST", path: Self.advancedSessionPath(sessionID) + "/tools/toggle", body: nil, accepted: [200])
    }
    func toggleSessionToolsApproval(_ sessionID: String) async throws {
        let metadata = try await advancedMetadata(sessionID)
        try await toggleSessionToolsApproval(sessionID, metadata: metadata)
    }

    /// POST /pause toggles the arm; the returned value is not a paused-boundary acknowledgement.
    func toggleSessionPause(_ sessionID: String, metadata: SessionControlMetadata) async throws -> SessionPauseResponse {
        try metadata.requireExecution(for: sessionID, capability: "pause")
        return try await request(SessionPauseResponse.self, method: "POST", path: Self.advancedSessionPath(sessionID) + "/pause")
    }
    func toggleSessionPause(_ sessionID: String) async throws -> SessionPauseResponse {
        let metadata = try await advancedMetadata(sessionID)
        return try await toggleSessionPause(sessionID, metadata: metadata)
    }

    /// Stops the selected session subtree. A root stop fences the entire tree; a child stop fences only that child's subtree.
    func stopSessionSubtree(_ sessionID: String, confirmedSessionID: String, metadata: SessionControlMetadata) async throws -> StopSessionSubtreeResult {
        try metadata.requireExecution(for: sessionID, capability: "stop_subtree")
        guard confirmedSessionID == sessionID else { throw SessionAdvancedError.subtreeConfirmationRequired }
        try await noContent(method: "POST", path: Self.advancedSessionPath(sessionID) + "/stop-subtree", body: nil, accepted: [204])
        return StopSessionSubtreeResult(sessionID: sessionID)
    }
    func stopSessionSubtree(_ sessionID: String, confirmedSessionID: String) async throws -> StopSessionSubtreeResult {
        let metadata = try await advancedMetadata(sessionID)
        return try await stopSessionSubtree(sessionID, confirmedSessionID: confirmedSessionID, metadata: metadata)
    }

    /// Waits for an accepted turn's durable settlement; detaching this request does not cancel the turn.
    func waitForTurn(_ sessionID: String, turnID: String) async throws {
        guard !turnID.isEmpty, turnID.utf8.count <= 256 else { throw SessionAdvancedError.invalidTurnID }
        _ = try await advancedMetadata(sessionID)
        try await noContent(method: "POST", path: Self.advancedSessionPath(sessionID) + "/turns/" + Self.advancedPathSegment(turnID) + "/wait", body: nil, accepted: [204])
    }
    func waitForWorkflow(_ sessionID: String) async throws -> SessionWorkflowSettlement {
        _ = try await advancedMetadata(sessionID)
        let result = try await request(SessionWorkflowSettlement.self, method: "POST", path: Self.advancedSessionPath(sessionID) + "/workflow/wait", body: nil)
        guard !result.epoch.isEmpty else { throw SessionAdvancedError.invalidResponse }
        return result
    }

    /// Position is a zero-based ordinal among flat user-role messages, not transcript item position.
    func forkBeforeUserMessage(_ sessionID: String, index: Int, availableUserMessages: Int, metadata: SessionControlMetadata) async throws -> SessionForkResponse {
        try metadata.requireExecution(for: sessionID, capability: "branching")
        guard index >= 0 else { throw SessionAdvancedError.invalidForkPosition }
        guard index < availableUserMessages else { throw SessionAdvancedError.forkPositionUnavailable }
        let result = try await request(SessionForkResponse.self, method: "POST", path: Self.advancedSessionPath(sessionID) + "/fork", body: SessionForkRequest(userMessageIndex: index), accepted: [200, 201])
        guard !result.id.isEmpty, result.id != sessionID else { throw SessionAdvancedError.identityMismatch }
        return result
    }
    func forkBeforeUserMessage(_ sessionID: String, index: Int, availableUserMessages: Int) async throws -> SessionForkResponse {
        let metadata = try await advancedMetadata(sessionID)
        return try await forkBeforeUserMessage(sessionID, index: index, availableUserMessages: availableUserMessages, metadata: metadata)
    }

    /// The caller must obtain durableMessageID from a real server message record; transcript positions are not IDs.
    func updateSessionMessage(_ sessionID: String, durableMessageID: Int64, message: JSONValue, metadata: SessionControlMetadata) async throws -> SessionMessageUpdateResponse {
        try metadata.requireExecution(for: sessionID, capability: "session_editing")
        guard durableMessageID > 0 else { throw SessionAdvancedError.invalidMessageID }
        try Self.validateEditableMessage(message)
        guard let current = try? await sessionStatus(sessionID), current.metadata.sessionID == sessionID else { throw SessionAdvancedError.identityMismatch }
        guard current.status.state == "settled", current.status.pending == 0 else { throw SessionAdvancedError.sessionBusy }
        let response = try await request(SessionMessageUpdateResponse.self, method: "PATCH", path: Self.advancedSessionPath(sessionID) + "/messages/\(durableMessageID)", body: SessionMessageUpdateRequest(message: message))
        guard response.status == "updated" else { throw SessionAdvancedError.invalidResponse }
        return response
    }
    func updateSessionMessage(_ sessionID: String, durableMessageID: Int64, message: JSONValue) async throws -> SessionMessageUpdateResponse {
        let metadata = try await advancedMetadata(sessionID)
        return try await updateSessionMessage(sessionID, durableMessageID: durableMessageID, message: message, metadata: metadata)
    }

    func delegationPolicy(_ sessionID: String, metadata: SessionControlMetadata) async throws -> DelegationPolicyResponse {
        guard metadata.sessionID == sessionID, metadata.supports("delegation_policy") else { throw SessionAdvancedError.unsupported }
        return try await request(DelegationPolicyResponse.self, path: Self.advancedSessionPath(sessionID) + "/delegation-policy")
    }
    func setDelegationPolicy(_ sessionID: String, enabled: Bool, metadata: SessionControlMetadata) async throws -> DelegationPolicyResponse {
        try metadata.requireExecution(for: sessionID, capability: "delegation_policy")
        return try await request(DelegationPolicyResponse.self, method: "PATCH", path: Self.advancedSessionPath(sessionID) + "/delegation-policy", body: DelegationPolicyRequest(enabled: enabled))
    }

    func delegationPolicy(_ sessionID: String) async throws -> DelegationPolicyResponse {
        let metadata = try await advancedMetadata(sessionID)
        return try await delegationPolicy(sessionID, metadata: metadata)
    }

    func setDelegationPolicy(_ sessionID: String, enabled: Bool) async throws -> DelegationPolicyResponse {
        let metadata = try await advancedMetadata(sessionID)
        return try await setDelegationPolicy(sessionID, enabled: enabled, metadata: metadata)
    }

    func removeSessionAttachment(_ sessionID: String, path: String, metadata: SessionControlMetadata) async throws {
        try metadata.requireExecution(for: sessionID, capability: "session_editing")
        guard Self.isAttachedFilePath(path) else { throw SessionAdvancedError.invalidAttachmentPath }
        try await noContent(method: "DELETE", path: Self.advancedSessionPath(sessionID) + "/attachments", body: AttachmentRemovalRequest(path: path), accepted: [204])
    }
    func removeSessionAttachment(_ sessionID: String, path: String) async throws {
        let metadata = try await advancedMetadata(sessionID)
        try await removeSessionAttachment(sessionID, path: path, metadata: metadata)
    }

    private func advancedMetadata(_ sessionID: String) async throws -> SessionControlMetadata {
        let response = try await sessionStatus(sessionID)
        guard response.metadata.sessionID == sessionID else { throw SessionAdvancedError.identityMismatch }
        return response.metadata
    }
    private static func validateEditableMessage(_ message: JSONValue) throws {
        guard case .object(let entry) = message,
              let payload = entry["message"]?.objectValue,
              let role = payload["role"]?.stringValue, role == "user",
              let content = payload["content"]?.stringValue, content.utf8.count <= 256 * 1024 else { throw SessionAdvancedError.invalidMessage }
        if let parts = payload["multi_content"] {
            guard let array = parts.arrayValue, array.count <= 64 else { throw SessionAdvancedError.invalidMessage }
        }
    }
    private static func isAttachedFilePath(_ path: String) -> Bool {
        guard path.hasPrefix("/"), path.utf8.count <= 4096,
              path.unicodeScalars.allSatisfy({ !CharacterSet.controlCharacters.contains($0) }),
              !path.contains("\\") else { return false }
        return true
    }
    private static func advancedSessionPath(_ id: String) -> String { "api/v2/sessions/" + advancedPathSegment(id) }
    private static func advancedPathSegment(_ value: String) -> String { value.addingPercentEncoding(withAllowedCharacters: CharacterSet(charactersIn: "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-._~")) ?? value }
}
