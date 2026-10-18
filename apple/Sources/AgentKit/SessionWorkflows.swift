import Foundation

public struct SessionPromptArgument: Decodable, Sendable, Equatable, Identifiable {
    public let name: String
    public let description: String
    public let required: Bool
    public var id: String { name }
}

public struct SessionPrompt: Sendable, Equatable, Identifiable {
    /// Session-local opaque discovery key; pass this exact key to execute.
    public let key: String
    public let name: String
    public let description: String
    public let arguments: [SessionPromptArgument]
    public var id: String { key }
}

private struct PromptArgumentWire: Decodable { let name: String; let description: String?; let required: Bool? }
private struct PromptInfoWire: Decodable { let name: String; let description: String?; let arguments: [PromptArgumentWire]? }
public struct SessionPromptRequest: Encodable, Sendable {
    public let name: String
    public let args: [String: String]?
    public init(name: String, args: [String: String]? = nil) { self.name = name; self.args = args }
}
public struct SessionPromptResult: Decodable, Sendable, Equatable { public let text: String }

public struct SessionContextCategory: Decodable, Sendable, Equatable {
    public let tokens: Int64
    public let items: Int
}
public struct SessionContextFile: Decodable, Sendable, Equatable, Identifiable {
    public let path: String
    public let tokens: Int64
    public let missing: Bool?
    public var id: String { path }
}
public struct SessionContextBreakdown: Decodable, Sendable, Equatable {
    public let systemPrompt: SessionContextCategory
    public let toolDefinitions: SessionContextCategory
    public let promptFiles: SessionContextCategory
    public let messages: SessionContextCategory
    public let toolResults: SessionContextCategory
    public let compactionSummary: SessionContextCategory
    public let promptFileItems: [SessionContextFile]?
    public let attachedFiles: [SessionContextFile]?
    public let latestCompactionSummary: String?
    public let contextLimit: Int64
    public let model: String
    public let compactionModel: String?
    public let compactionContextLimit: Int64?
    public let primaryContextLimit: Int64?
    public var estimatedTotalTokens: Int64 {
        systemPrompt.tokens + toolDefinitions.tokens + promptFiles.tokens + messages.tokens + toolResults.tokens + compactionSummary.tokens
    }
    enum CodingKeys: String, CodingKey {
        case systemPrompt = "system_prompt", toolDefinitions = "tool_definitions", promptFiles = "prompt_files"
        case messages, toolResults = "tool_results", compactionSummary = "compaction_summary"
        case promptFileItems = "prompt_file_items", attachedFiles = "attached_files"
        case latestCompactionSummary = "latest_compaction_summary", contextLimit = "context_limit", model
        case compactionModel = "compaction_model", compactionContextLimit = "compaction_context_limit", primaryContextLimit = "primary_context_limit"
    }
}

public struct SessionForkSkill: Decodable, Sendable, Equatable, Identifiable {
    public let name: String
    public let description: String?
    public let context: String?
    public let model: String?
    public let local: Bool?
    public var id: String { name }
}

public enum ForkSkillOperationState: String, Sendable, Equatable {
    case accepted, completed, failed, unknown
}

public struct SkillOperationProjection: Sendable, Equatable, Identifiable {
    public let operationID: String
    public let skillName: String
    public var state: ForkSkillOperationState
    public var error: String?
    public var id: String { operationID }
    public init(operationID: String, skillName: String, state: ForkSkillOperationState, error: String? = nil) {
        self.operationID = operationID; self.skillName = skillName; self.state = state; self.error = error
    }
}

public struct SessionForkSkillRunRequest: Encodable, Sendable, Equatable {
    public let name: String
    public let task: String
    public let operationID: String
    enum CodingKeys: String, CodingKey { case name, task, operationID = "operation_id" }
    public init(name: String, task: String, operationID: String = UUID().uuidString) { self.name = name; self.task = task; self.operationID = operationID }
}
public struct SessionForkSkillOperation: Decodable, Sendable, Equatable {
    public let sessionID: String
    public let operationID: String
    enum CodingKeys: String, CodingKey { case sessionID = "session_id", operationID = "operation_id" }
}

public struct SessionRecoveryExport: Codable, Sendable {
    public let id: String
    public let title: String?
    public let createdAt: String?
    public let messages: [JSONValue]?
    public let inputTokens: Int64?
    public let outputTokens: Int64?
    public let workingDir: String?
    public let toolsApproved: Bool?
    enum CodingKeys: String, CodingKey {
        case id, title, createdAt = "created_at", messages, inputTokens = "input_tokens"
        case outputTokens = "output_tokens", workingDir = "working_dir", toolsApproved = "tools_approved"
    }
}

public struct SessionBatchExportRequest: Encodable, Sendable, Equatable {
    public let sessionIDs: [String]
    public let format: String?
    enum CodingKeys: String, CodingKey { case sessionIDs = "session_ids", format }
    public init(sessionIDs: [String], format: String? = nil) { self.sessionIDs = sessionIDs; self.format = format }
}
public struct SessionJSONExport: Sendable {
    public let exportFormat: String
    public let timestamp: String?
    public let sessionCount: Int
    public let exportedSessionIDs: [String]
    private let response: JSONValue

    fileprivate init(response: JSONValue, requestedIDs: [String]) throws {
        guard let object = response.objectValue,
              let format = object["export_format"]?.stringValue, format == "json",
              let sessions = object["sessions"]?.arrayValue,
              let count = object["session_count"]?.numberValue, count >= 0, count.rounded(.towardZero) == count,
              Int(count) == sessions.count else { throw SessionWorkflowError.identityMismatch }
        let ids = sessions.compactMap { $0.objectValue?["id"]?.stringValue }
        guard ids.count == sessions.count, ids.allSatisfy(requestedIDs.contains), Set(ids).count == ids.count else { throw SessionWorkflowError.identityMismatch }
        exportFormat = format
        timestamp = object["timestamp"]?.stringValue
        sessionCount = sessions.count
        exportedSessionIDs = ids
        self.response = response
    }

    /// Lossless-to-schema JSON re-encoding retains message/item fields not modeled by UI DTOs.
    public func encodedJSON(prettyPrinted: Bool = true) throws -> Data {
        let encoder = JSONEncoder()
        if prettyPrinted { encoder.outputFormatting = [.prettyPrinted, .sortedKeys] }
        return try encoder.encode(response)
    }
}

public struct SessionBatchDeleteRequest: Encodable, Sendable, Equatable {
    public let sessionIDs: [String]
    enum CodingKeys: String, CodingKey { case sessionIDs = "session_ids" }
    public init(sessionIDs: [String]) { self.sessionIDs = sessionIDs }
}
public struct SessionBatchDeleteResult: Decodable, Sendable, Equatable {
    public let deletedCount: Int
    public let failedCount: Int
    public let failedIDs: [String]?
    enum CodingKeys: String, CodingKey { case deletedCount = "deleted_count", failedCount = "failed_count", failedIDs = "failed_ids" }

    public func confirmedDeletedIDs(from requestedIDs: [String]) throws -> [String] {
        guard let failedIDs else { throw SessionWorkflowError.identityMismatch }
        let failed = failedIDs
        guard failed.count == failedCount, Set(failed).count == failed.count, failed.allSatisfy(requestedIDs.contains),
              deletedCount >= 0, failedCount >= 0, deletedCount + failedCount == requestedIDs.count else {
            throw SessionWorkflowError.identityMismatch
        }
        let failedSet = Set(failed)
        return requestedIDs.filter { !failedSet.contains($0) }
    }
}

public struct SessionWorkspaceSnapshots: Decodable, Sendable, Equatable {
    public let sessionID: String
    public let enabled: Bool
    public let files: [Int]
    public let proof: String
    enum CodingKeys: String, CodingKey { case sessionID = "session_id", enabled, files, proof }
}
public struct SessionWorkspaceSnapshotRequest: Encodable, Sendable, Equatable {
    public let expectedProof: String
    public let keep: Int?
    enum CodingKeys: String, CodingKey { case expectedProof = "expected_proof", keep }
    public init(expectedProof: String, keep: Int? = nil) { self.expectedProof = expectedProof; self.keep = keep }
}
public struct SessionWorkspaceSnapshotResult: Decodable, Sendable, Equatable {
    public let sessionID: String
    public let restoredFiles: Int
    public let restored: Bool
    enum CodingKeys: String, CodingKey { case sessionID = "session_id", restoredFiles = "restored_files", restored }
}

public enum SessionWorkflowError: Error, LocalizedError, Sendable, Equatable {
    case identityMismatch
    case unsupported
    case invalidPromptKey
    case invalidSkill
    case invalidSessionSelection
    case destructiveConfirmationRequired
    case invalidSnapshotProof
    case snapshotsUnavailable
    case sessionNotIdle
    public var errorDescription: String? {
        switch self {
        case .identityMismatch: "The response belongs to a different session."
        case .unsupported: "This operation is unavailable for the selected session."
        case .invalidPromptKey: "Select a prompt from this session's current prompt list."
        case .invalidSkill: "Select an available fork skill and provide a task."
        case .invalidSessionSelection: "Select at least one session."
        case .destructiveConfirmationRequired: "Confirm the exact sessions or workspace restore operation before continuing."
        case .invalidSnapshotProof: "Refresh workspace snapshots before restoring."
        case .snapshotsUnavailable: "This session has no restorable workspace snapshots."
        case .sessionNotIdle: "Workspace snapshots can only be restored while the session is idle."
        }
    }
}

public extension APIClient {
    func sessionPrompts(_ sessionID: String, metadata: SessionControlMetadata) async throws -> [SessionPrompt] {
        try metadata.requireExecution(for: sessionID, capability: "mcp_prompts")
        let raw = try await request([String: PromptInfoWire].self, path: Self.workflowPath(sessionID) + "/mcp/prompts")
        return raw.map { key, value in
            SessionPrompt(key: key, name: value.name, description: value.description ?? "",
                          arguments: (value.arguments ?? []).map { .init(name: $0.name, description: $0.description ?? "", required: $0.required ?? false) })
        }.sorted { $0.key < $1.key }
    }

    func executeSessionPrompt(_ sessionID: String, key: String, args: [String: String] = [:], metadata: SessionControlMetadata, discoveredPrompts: [SessionPrompt]) async throws -> String {
        try metadata.requireExecution(for: sessionID, capability: "mcp_prompts")
        guard discoveredPrompts.contains(where: { $0.key == key }) else { throw SessionWorkflowError.invalidPromptKey }
        let result = try await request(SessionPromptResult.self, method: "POST", path: Self.workflowPath(sessionID) + "/mcp/prompts/execute", body: SessionPromptRequest(name: key, args: args))
        return result.text
    }

    func sessionContext(_ sessionID: String, metadata: SessionControlMetadata) async throws -> SessionContextBreakdown {
        try metadata.requireExecution(for: sessionID, capability: "context_inspection")
        return try await request(SessionContextBreakdown.self, path: Self.workflowPath(sessionID) + "/context")
    }

    func forkSkills(_ sessionID: String, metadata: SessionControlMetadata) async throws -> [SessionForkSkill] {
        try metadata.requireExecution(for: sessionID, capability: "fork_skills")
        let raw = try await request([[String: JSONValue]].self, path: Self.workflowPath(sessionID) + "/skills")
        return raw.compactMap { value in
            guard let name = value["Name"]?.stringValue, !name.isEmpty,
                  value["Context"]?.stringValue == "fork" else { return nil }
            return SessionForkSkill(name: name, description: value["Description"]?.stringValue,
                                    context: value["Context"]?.stringValue, model: value["Model"]?.stringValue,
                                    local: value["Local"]?.boolValue)
        }.sorted { $0.name < $1.name }
    }

    /// Starting a fork skill is an asynchronous side effect; operation_id is correlation, not deduplication.
    func startForkSkill(_ sessionID: String, runRequest: SessionForkSkillRunRequest,
                        metadata: SessionControlMetadata, availableSkills: [SessionForkSkill]) async throws -> SessionForkSkillOperation {
        try metadata.requireExecution(for: sessionID, capability: "fork_skills")
        guard availableSkills.contains(where: { $0.name == runRequest.name && $0.context == "fork" }),
              !runRequest.task.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty,
              !runRequest.operationID.isEmpty else { throw SessionWorkflowError.invalidSkill }
        let result = try await request(SessionForkSkillOperation.self, method: "POST", path: Self.workflowPath(sessionID) + "/skills/run", body: runRequest, accepted: [202])
        guard result.sessionID == sessionID, result.operationID == runRequest.operationID else { throw SessionWorkflowError.identityMismatch }
        return result
    }

    func recoveryExport(_ sessionID: String, expectedInstanceID: String) async throws -> SessionRecoveryExport {
        try await ensureWorkflowInstance(expectedInstanceID)
        let result = try await request(SessionRecoveryExport.self, path: "api/v2/sessions/\(Self.workflowPathSegment(sessionID))/recovery")
        guard result.id == sessionID else { throw SessionWorkflowError.identityMismatch }
        return result
    }

    func exportSessionsJSON(_ sessionIDs: [String], expectedInstanceID: String) async throws -> SessionJSONExport {
        try await exportSessions(sessionIDs, expectedInstanceID: expectedInstanceID)
    }

    func exportSessions(_ sessionIDs: [String], expectedInstanceID: String) async throws -> SessionJSONExport {
        try await ensureWorkflowInstance(expectedInstanceID)
        let ids = try Self.validatedSessionIDs(sessionIDs)
        let response = try await request(JSONValue.self, method: "POST", path: "api/v2/sessions/batch/export", body: SessionBatchExportRequest(sessionIDs: ids, format: "json"))
        return try SessionJSONExport(response: response, requestedIDs: ids)
    }

    func deleteSessions(_ sessionIDs: [String], expectedInstanceID: String, confirmedSessionIDs: Set<String>) async throws -> SessionBatchDeleteResult {
        try await ensureWorkflowInstance(expectedInstanceID)
        let ids = try Self.validatedSessionIDs(sessionIDs)
        guard Set(ids) == confirmedSessionIDs else { throw SessionWorkflowError.destructiveConfirmationRequired }
        let result = try await request(SessionBatchDeleteResult.self, method: "POST", path: "api/v2/sessions/batch/delete", body: SessionBatchDeleteRequest(sessionIDs: ids))
        _ = try result.confirmedDeletedIDs(from: ids)
        return result
    }

    func workspaceSnapshots(_ sessionID: String, metadata: SessionControlMetadata) async throws -> SessionWorkspaceSnapshots {
        try metadata.requireExecution(for: sessionID, capability: "snapshots")
        let result = try await request(SessionWorkspaceSnapshots.self, path: Self.workflowPath(sessionID) + "/workspace-snapshots")
        guard result.sessionID == sessionID else { throw SessionWorkflowError.identityMismatch }
        return result
    }

    func undoWorkspaceSnapshot(_ sessionID: String, proof: String, confirmed: Bool,
                               metadata: SessionControlMetadata, currentStatus: StatusWire) async throws -> SessionWorkspaceSnapshotResult {
        try await restoreWorkspace(sessionID, proof: proof, keep: nil, confirmed: confirmed, metadata: metadata, status: currentStatus, operation: "undo")
    }

    func resetWorkspaceSnapshots(_ sessionID: String, proof: String, keep: Int, confirmed: Bool,
                                 metadata: SessionControlMetadata, currentStatus: StatusWire) async throws -> SessionWorkspaceSnapshotResult {
        try await restoreWorkspace(sessionID, proof: proof, keep: keep, confirmed: confirmed, metadata: metadata, status: currentStatus, operation: "reset")
    }

    func validateWorkspaceSnapshotRestore(sessionID: String, snapshots: SessionWorkspaceSnapshots, proof: String, keep: Int, confirmed: Bool, metadata: SessionControlMetadata, currentStatus: StatusWire) throws {
        try metadata.requireExecution(for: sessionID, capability: "snapshots")
        guard confirmed else { throw SessionWorkflowError.destructiveConfirmationRequired }
        guard !proof.isEmpty, proof.utf8.count <= 256, keep >= 0 else { throw SessionWorkflowError.invalidSnapshotProof }
        guard currentStatus.sessionID == sessionID, currentStatus.state == "settled", currentStatus.pending == 0 else { throw SessionWorkflowError.sessionNotIdle }
        guard snapshots.sessionID == sessionID, snapshots.enabled, !snapshots.proof.isEmpty, snapshots.proof == proof, !snapshots.files.isEmpty else { throw SessionWorkflowError.snapshotsUnavailable }
        guard keep <= snapshots.files.count else { throw SessionWorkflowError.invalidSnapshotProof }
    }

    private func restoreWorkspace(_ sessionID: String, proof: String, keep: Int?, confirmed: Bool,
                                 metadata: SessionControlMetadata, status: StatusWire, operation: String) async throws -> SessionWorkspaceSnapshotResult {
        let snapshot = try await workspaceSnapshots(sessionID, metadata: metadata)
        try validateWorkspaceSnapshotRestore(sessionID: sessionID, snapshots: snapshot, proof: proof, keep: keep ?? 0, confirmed: confirmed, metadata: metadata, currentStatus: status)
        let body = SessionWorkspaceSnapshotRequest(expectedProof: proof, keep: keep)
        let result = try await request(SessionWorkspaceSnapshotResult.self, method: "POST", path: Self.workflowPath(sessionID) + "/workspace-snapshots/\(operation)", body: body)
        guard result.sessionID == sessionID else { throw SessionWorkflowError.identityMismatch }
        return result
    }

    func sessionPrompts(_ sessionID: String) async throws -> [SessionPrompt] {
        let metadata = try await freshWorkflowMetadata(sessionID)
        return try await sessionPrompts(sessionID, metadata: metadata)
    }

    func executeSessionPrompt(_ sessionID: String, key: String, args: [String: String] = [:], discoveredPrompts: [SessionPrompt]) async throws -> String {
        let metadata = try await freshWorkflowMetadata(sessionID)
        return try await executeSessionPrompt(sessionID, key: key, args: args, metadata: metadata, discoveredPrompts: discoveredPrompts)
    }

    func sessionContext(_ sessionID: String) async throws -> SessionContextBreakdown {
        let metadata = try await freshWorkflowMetadata(sessionID)
        return try await sessionContext(sessionID, metadata: metadata)
    }

    func forkSkills(_ sessionID: String) async throws -> [SessionForkSkill] {
        let metadata = try await freshWorkflowMetadata(sessionID)
        return try await forkSkills(sessionID, metadata: metadata)
    }

    func startForkSkill(_ sessionID: String, request: SessionForkSkillRunRequest) async throws -> SessionForkSkillOperation {
        let metadata = try await freshWorkflowMetadata(sessionID)
        let availableSkills = try await forkSkills(sessionID, metadata: metadata)
        return try await startForkSkill(sessionID, runRequest: request, metadata: metadata, availableSkills: availableSkills)
    }

    func workspaceSnapshots(_ sessionID: String) async throws -> SessionWorkspaceSnapshots {
        let metadata = try await freshWorkflowMetadata(sessionID)
        return try await workspaceSnapshots(sessionID, metadata: metadata)
    }

    func undoWorkspaceSnapshot(_ sessionID: String, proof: String, confirmed: Bool) async throws -> SessionWorkspaceSnapshotResult {
        let response = try await sessionStatus(sessionID)
        guard response.metadata.sessionID == sessionID else { throw SessionWorkflowError.identityMismatch }
        return try await undoWorkspaceSnapshot(sessionID, proof: proof, confirmed: confirmed, metadata: response.metadata, currentStatus: response.status)
    }

    func resetWorkspaceSnapshot(_ sessionID: String, proof: String, keep: Int, confirmed: Bool) async throws -> SessionWorkspaceSnapshotResult {
        try await resetWorkspaceSnapshots(sessionID, proof: proof, keep: keep, confirmed: confirmed)
    }

    func resetWorkspaceSnapshots(_ sessionID: String, proof: String, keep: Int, confirmed: Bool) async throws -> SessionWorkspaceSnapshotResult {
        let response = try await sessionStatus(sessionID)
        guard response.metadata.sessionID == sessionID else { throw SessionWorkflowError.identityMismatch }
        return try await resetWorkspaceSnapshots(sessionID, proof: proof, keep: keep, confirmed: confirmed, metadata: response.metadata, currentStatus: response.status)
    }

    private func freshWorkflowMetadata(_ sessionID: String) async throws -> SessionControlMetadata {
        let status = try await request(SessionStatusResponse.self, path: "api/v2/sessions/\(Self.workflowPathSegment(sessionID))/status")
        guard status.metadata.sessionID == sessionID else { throw SessionWorkflowError.identityMismatch }
        return status.metadata
    }

    private func ensureWorkflowInstance(_ instanceID: String) async throws {
        guard !instanceID.isEmpty else { throw SessionWorkflowError.identityMismatch }
        let identity = try await request(ServerIdentity.self, path: "api/v2/server")
        guard identity.version == 1, identity.sessionAPIVersion == 2, identity.instanceID == instanceID else { throw SessionWorkflowError.identityMismatch }
    }

    private static func validatedSessionIDs(_ values: [String]) throws -> [String] {
        guard !values.isEmpty else { throw SessionWorkflowError.invalidSessionSelection }
        var seen = Set<String>()
        for value in values where value.isEmpty || value.utf8.count > 256 || !seen.insert(value).inserted {
            throw SessionWorkflowError.invalidSessionSelection
        }
        return values
    }

    private static func workflowPath(_ sessionID: String) -> String { "api/v2/sessions/" + workflowPathSegment(sessionID) }
    private static func workflowPathSegment(_ value: String) -> String {
        value.addingPercentEncoding(withAllowedCharacters: CharacterSet(charactersIn: "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-._~")) ?? value
    }


}
