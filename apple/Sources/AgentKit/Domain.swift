import Foundation

public struct ServerRef: Codable, Hashable, Identifiable, Sendable {
    public let id: UUID
    public init(id: UUID) { self.id = id }
    public var idValue: UUID { id }
}

public struct SessionRef: Codable, Hashable, Identifiable, Sendable {
    public let serverID: UUID
    public let sessionID: String
    public init(serverID: UUID, sessionID: String) { self.serverID = serverID; self.sessionID = sessionID }
    public var id: Self { self }
}

public enum ConnectionState: String, Codable, Sendable { case unknown, testing, live, reconnecting, offline, unauthorized }
public enum SessionState: String, Codable, Sendable { case idle, queued, running, cancelling, waiting, settled, failed, unknown }

public struct ServerIdentity: Codable, Equatable, Sendable {
    public let version: Int
    public let sessionAPIVersion: Int
    public let instanceID: String
    public let ready: Bool
    public let workspaceRoot: String?
    public let source: String?
    public let capabilities: [String]
    enum CodingKeys: String, CodingKey { case version, sessionAPIVersion = "session_api_version", instanceID = "instance_id", ready, workspaceRoot = "workspace_root", source, capabilities }
    public init(version: Int, sessionAPIVersion: Int, instanceID: String, ready: Bool, workspaceRoot: String? = nil, source: String? = nil, capabilities: [String] = []) {
        self.version = version; self.sessionAPIVersion = sessionAPIVersion; self.instanceID = instanceID; self.ready = ready; self.workspaceRoot = workspaceRoot; self.source = source; self.capabilities = capabilities
    }
}

public struct ServerDraft: Sendable, Equatable {
    public var id: UUID
    public var name: String
    public var baseURL: String
    public var token: String
    public var allowsLocalHTTP: Bool
    public init(id: UUID = UUID(), name: String, baseURL: String, token: String = "", allowsLocalHTTP: Bool = false) {
        self.id = id; self.name = name; self.baseURL = baseURL; self.token = token; self.allowsLocalHTTP = allowsLocalHTTP
    }
}

public struct ServerProjection: Identifiable, Sendable, Equatable {
    public let id: UUID
    public let name: String
    public let baseURL: String
    public var identity: ServerIdentity?
    public var state: ConnectionState
    public var allowsLocalHTTP: Bool
    public var sessionCount: Int
    public var refreshedAt: Date?
    public init(id: UUID, name: String, baseURL: String, identity: ServerIdentity? = nil, state: ConnectionState = .unknown, allowsLocalHTTP: Bool = false, sessionCount: Int = 0, refreshedAt: Date? = nil) {
        self.id = id; self.name = name; self.baseURL = baseURL; self.identity = identity; self.state = state; self.allowsLocalHTTP = allowsLocalHTTP; self.sessionCount = sessionCount; self.refreshedAt = refreshedAt
    }
}

public struct SessionProjection: Identifiable, Sendable, Equatable {
    public let ref: SessionRef
    public var id: SessionRef { ref }
    public var parentSession: SessionRef?
    public var title: String
    public var source: String?
    public var agentName: String
    public var state: SessionState
    public var activity: String?
    public var needsYouReason: String?
    public var updatedAt: Date?
    public var cost: Double
    public var parentID: String?
    public var starred: Bool
    public var loaded: Bool
    public var loadable: Bool
    public init(ref: SessionRef, parentSession: SessionRef? = nil, title: String, agentName: String, source: String? = nil, state: SessionState = .unknown, activity: String? = nil, needsYouReason: String? = nil, updatedAt: Date? = nil, cost: Double = 0, parentID: String? = nil, starred: Bool = false, loaded: Bool = false, loadable: Bool = false) {
        self.ref = ref; self.parentSession = parentSession; self.title = title; self.source = source; self.agentName = agentName; self.state = state; self.activity = activity; self.needsYouReason = needsYouReason; self.updatedAt = updatedAt; self.cost = cost; self.parentID = parentID; self.starred = starred; self.loaded = loaded; self.loadable = loadable
    }
}

public struct AgentOption: Codable, Sendable, Equatable, Identifiable {
    /// Registered API source identifier; it is not the configured root agent name.
    public var name: String
    public var rootAgentName: String?
    public var description: String
    public var multi: Bool
    public var commands: [String]
    public var id: String { name }
    enum CodingKeys: String, CodingKey { case name, rootAgentName = "root_agent_name", description, multi, commands }
    public init(name: String, rootAgentName: String? = nil, description: String = "", multi: Bool = false, commands: [String] = []) { self.name = name; self.rootAgentName = rootAgentName; self.description = description; self.multi = multi; self.commands = commands }
}

public struct SessionCapabilities: Codable, Sendable, Equatable {
    public var raw: [String: JSONValue]
    public var availableModels: [String]
    public var thinkingLevels: [String]
    enum CodingKeys: String, CodingKey { case availableModels = "available_models", thinkingLevels = "thinking_levels" }
    public init(raw: [String: JSONValue] = [:], availableModels: [String] = [], thinkingLevels: [String] = []) { self.raw = raw; self.availableModels = availableModels; self.thinkingLevels = thinkingLevels }
    public init(from decoder: Decoder) throws {
        let container = try decoder.container(keyedBy: DynamicCodingKey.self)
        var values: [String: JSONValue] = [:]
        for key in container.allKeys { if key.stringValue != "available_models" && key.stringValue != "thinking_levels", let value = try? container.decode(JSONValue.self, forKey: key) { values[key.stringValue] = value } }
        raw = values
        availableModels = (try? container.decode([String].self, forKey: DynamicCodingKey(stringValue: "available_models")!)) ?? []
        thinkingLevels = (try? container.decode([String].self, forKey: DynamicCodingKey(stringValue: "thinking_levels")!)) ?? []
    }
    public func encode(to encoder: Encoder) throws { var container = encoder.container(keyedBy: DynamicCodingKey.self); for (key, value) in raw { try container.encode(value, forKey: DynamicCodingKey(stringValue: key)!) }; try container.encode(availableModels, forKey: DynamicCodingKey(stringValue: "available_models")!); try container.encode(thinkingLevels, forKey: DynamicCodingKey(stringValue: "thinking_levels")!) }
    public func supports(_ name: String) -> Bool { (raw[name]?.boolValue ?? false) }
}

public indirect enum JSONValue: Codable, Sendable, Equatable {
    case string(String), number(Double), bool(Bool), object([String: JSONValue]), array([JSONValue]), null
    public init(from decoder: Decoder) throws {
        let c = try decoder.singleValueContainer()
        if c.decodeNil() { self = .null }
        else if let v = try? c.decode(Bool.self) { self = .bool(v) }
        else if let v = try? c.decode(Double.self) { self = .number(v) }
        else if let v = try? c.decode(String.self) { self = .string(v) }
        else if let v = try? c.decode([String: JSONValue].self) { self = .object(v) }
        else { self = .array(try c.decode([JSONValue].self)) }
    }
    public func encode(to encoder: Encoder) throws {
        var c = encoder.singleValueContainer()
        switch self { case .string(let v): try c.encode(v); case .number(let v): try c.encode(v); case .bool(let v): try c.encode(v); case .object(let v): try c.encode(v); case .array(let v): try c.encode(v); case .null: try c.encodeNil() }
    }
    public var boolValue: Bool? { if case .bool(let v) = self { v } else { nil } }
    public var stringValue: String? { if case .string(let v) = self { v } else { nil } }
    public var numberValue: Double? { if case .number(let v) = self { v } else { nil } }
    public var objectValue: [String: JSONValue]? { if case .object(let v) = self { v } else { nil } }
    public var arrayValue: [JSONValue]? { if case .array(let v) = self { v } else { nil } }
}

public struct SessionBranchAnchor: Sendable, Equatable, Hashable {
    public let ref: SessionRef
    public let instanceID: String
    public var sessionID: String { ref.sessionID }
    public let epoch: String
    public let cursor: UInt64
    public let messagesSHA256: String
    public let itemPosition: Int
    public let itemSHA256: String
    /// Exclusive branch cut including this item: server copies items in `[0, position)`.
    public var position: Int { itemPosition + 1 }

    public init(ref: SessionRef, instanceID: String, epoch: String, cursor: UInt64, messagesSHA256: String, itemPosition: Int, itemSHA256: String) {
        self.ref = ref; self.instanceID = instanceID; self.epoch = epoch; self.cursor = cursor
        self.messagesSHA256 = messagesSHA256; self.itemPosition = itemPosition; self.itemSHA256 = itemSHA256
    }
}

public struct TranscriptItem: Identifiable, Sendable, Equatable {
    public enum Kind: String, Sendable { case message, tool, reasoning, activity, error, delegation }
    public let id: String
    public let kind: Kind
    public let owner: String?
    public let role: String?
    public let text: String
    public let isStreaming: Bool
    public let origin: String?
    public let toolName: String?
    public let toolArguments: String?
    public let toolOutput: String?
    public let toolStatus: String?
    public let child: SessionRef?
    public let attachments: [MessageAttachmentSummary]
    public let generatedMedia: [GeneratedMediaReference]
    public let userMessageIndex: Int?
    public let branchAnchor: SessionBranchAnchor?
    public init(id: String, kind: Kind, owner: String? = nil, role: String? = nil, text: String, isStreaming: Bool = false, origin: String? = nil, toolName: String? = nil, toolArguments: String? = nil, toolOutput: String? = nil, toolStatus: String? = nil, child: SessionRef? = nil, attachments: [MessageAttachmentSummary] = [], generatedMedia: [GeneratedMediaReference] = [], userMessageIndex: Int? = nil, branchAnchor: SessionBranchAnchor? = nil) {
        self.id = id; self.kind = kind; self.owner = owner; self.role = role; self.text = text; self.isStreaming = isStreaming; self.origin = origin; self.toolName = toolName; self.toolArguments = toolArguments; self.toolOutput = toolOutput; self.toolStatus = toolStatus; self.child = child; self.attachments = attachments; self.generatedMedia = generatedMedia; self.userMessageIndex = userMessageIndex; self.branchAnchor = branchAnchor
    }
}

public enum ConfirmationDecision: String, Codable, Sendable, CaseIterable { case approve, approveBalanced = "approve-balanced", approveAutonomous = "approve-autonomous", approveTool = "approve-tool", reject }
public struct ElicitationField: Identifiable, Sendable, Equatable {
    public let id: String
    public let label: String
    public let type: String
    public let required: Bool
    public let options: [String]
    public init(id: String, label: String, type: String = "string", required: Bool = false, options: [String] = []) { self.id = id; self.label = label; self.type = type; self.required = required; self.options = options }
}
public enum InteractionProjection: Identifiable, Sendable, Equatable {
    case confirmation(id: String, owner: String, toolName: String, arguments: String, decisions: [ConfirmationDecision])
    case elicitation(id: String, owner: String, title: String, fields: [ElicitationField], elicitationID: String)
    case maxIterations(id: String, owner: String, limit: Int)
    public var id: String { switch self { case .confirmation(let id, _, _, _, _), .elicitation(let id, _, _, _, _), .maxIterations(let id, _, _): id } }
}

public struct AgentNodeProjection: Identifiable, Sendable, Equatable {
    public let id: String
    public var nodeID: String { id }
    public let session: SessionRef?
    public let agentName: String
    public let state: SessionState
    public let needsAttention: Bool
    public let waitingOn: String?
    public let cost: Double
    public let inputTokens: Int64
    public let outputTokens: Int64
    public let children: [AgentNodeProjection]
    public init(id: String, session: SessionRef? = nil, agentName: String, state: SessionState = .unknown, needsAttention: Bool = false, waitingOn: String? = nil, cost: Double = 0, inputTokens: Int64 = 0, outputTokens: Int64 = 0, children: [AgentNodeProjection] = []) { self.id = id; self.session = session; self.agentName = agentName; self.state = state; self.needsAttention = needsAttention; self.waitingOn = waitingOn; self.cost = cost; self.inputTokens = inputTokens; self.outputTokens = outputTokens; self.children = children }
}

public struct OpenSessionProjection: Identifiable, Sendable, Equatable {
    public let ref: SessionRef
    public var id: SessionRef { ref }
    public var parentSession: SessionRef?
    public var title: String
    public var source: String?
    public var agentName: String
    public var state: SessionState
    public var turnID: String?
    public var connection: ConnectionState
    public var refreshedAt: Date?
    public var currentActivity: String?
    public var transcript: [TranscriptItem]
    public var interactions: [InteractionProjection]
    public var capabilities: SessionCapabilities
    public var agentTree: [AgentNodeProjection]
    public var inputTokens: Int64
    public var outputTokens: Int64
    public var contextLength: Int64
    public var contextLimit: Int64
    public var compactionThreshold: Double?
    public var cost: Double
    public var cursor: UInt64
    public var epoch: String
    public var pendingInputCount: Int
    public init(ref: SessionRef, parentSession: SessionRef? = nil, title: String, agentName: String, source: String? = nil, state: SessionState = .unknown, turnID: String? = nil, connection: ConnectionState = .unknown, refreshedAt: Date? = nil, currentActivity: String? = nil, transcript: [TranscriptItem] = [], interactions: [InteractionProjection] = [], capabilities: SessionCapabilities = .init(), agentTree: [AgentNodeProjection] = [], inputTokens: Int64 = 0, outputTokens: Int64 = 0, contextLength: Int64 = 0, contextLimit: Int64 = 0, compactionThreshold: Double? = nil, cost: Double = 0, cursor: UInt64 = 0, epoch: String = "", pendingInputCount: Int = 0) {
        self.ref = ref; self.parentSession = parentSession; self.title = title; self.source = source; self.agentName = agentName; self.state = state; self.turnID = turnID; self.connection = connection; self.refreshedAt = refreshedAt; self.currentActivity = currentActivity; self.transcript = transcript; self.interactions = interactions; self.capabilities = capabilities; self.agentTree = agentTree; self.inputTokens = inputTokens; self.outputTokens = outputTokens; self.contextLength = contextLength; self.contextLimit = contextLimit; self.compactionThreshold = compactionThreshold; self.cost = cost; self.cursor = cursor; self.epoch = epoch; self.pendingInputCount = pendingInputCount
    }
}

public struct PendingInputProjection: Identifiable, Sendable, Equatable {
    public let turnID: String
    public let content: String
    public let origin: String?
    public let senderName: String?
    public var id: String { turnID }
    public var isUserEditable: Bool { origin == nil || origin == "user" }
    public init(turnID: String, content: String, origin: String? = nil, senderName: String? = nil) { self.turnID = turnID; self.content = content; self.origin = origin; self.senderName = senderName }
}

public struct RecentHistoryProjection: Sendable, Equatable {
    public let ref: SessionRef
    public var messages: [RecentChatMessageWire]
    public var beforeCursor: String?
    public var truncatedBefore: Bool
    public var hasMore: Bool { beforeCursor?.isEmpty == false }
    public init(ref: SessionRef, messages: [RecentChatMessageWire], beforeCursor: String?, truncatedBefore: Bool) {
        self.ref = ref; self.messages = messages; self.beforeCursor = beforeCursor; self.truncatedBefore = truncatedBefore
    }
}

public struct SessionControlsProjection: Identifiable, Sendable, Equatable {
    public let ref: SessionRef
    public var id: SessionRef { ref }
    public var starred: Bool
    public var model: String?
    public var thinkingLevel: String?
    public var thinkingLevels: [String]
    public var historical: Bool
    public var executionAvailable: Bool
    public var paused: Bool
    public var pauseRequested: Bool
    public var capabilities: SessionCapabilities
    public var todos: [SessionTodoWire]
    public var pendingInputs: [PendingInputProjection]
    public var agentTree: [AgentNodeProjection]
    public init(ref: SessionRef, starred: Bool = false, model: String? = nil, thinkingLevel: String? = nil, thinkingLevels: [String] = [], historical: Bool = false, executionAvailable: Bool = true, paused: Bool = false, pauseRequested: Bool = false, capabilities: SessionCapabilities = .init(), todos: [SessionTodoWire] = [], pendingInputs: [PendingInputProjection] = [], agentTree: [AgentNodeProjection] = []) {
        self.ref = ref; self.starred = starred; self.model = model; self.thinkingLevel = thinkingLevel; self.thinkingLevels = thinkingLevels; self.historical = historical; self.executionAvailable = executionAvailable; self.paused = paused; self.pauseRequested = pauseRequested; self.capabilities = capabilities; self.todos = todos; self.pendingInputs = pendingInputs; self.agentTree = agentTree
    }
}

public struct NeedsYouProjection: Identifiable, Sendable, Equatable {
    public let id: SessionRef
    public let reason: String
    public let refreshedAt: Date?
    public init(id: SessionRef, reason: String, refreshedAt: Date? = nil) { self.id = id; self.reason = reason; self.refreshedAt = refreshedAt }
}

public struct APIError: Error, LocalizedError, Sendable, Equatable {
    public let message: String
    public let statusCode: Int?
    public let code: String?
    public init(_ message: String, statusCode: Int? = nil, code: String? = nil) { self.message = message; self.statusCode = statusCode; self.code = code }
    public var errorDescription: String? { message }
}

public enum ReviewFileGroup: String, Codable, Sendable, CaseIterable { case conflicts, staged, unstaged, untracked, none }
public enum ReviewLoadState: String, Codable, Sendable { case unscanned, loading, ready, error }
public enum ReviewDocumentMode: String, Codable, Sendable { case changes, files }
public enum ReviewLineKind: String, Codable, Sendable { case context, add, remove, hunk, note }

public struct ReviewScopeInfo: Codable, Sendable, Equatable {
    enum CodingKeys: String, CodingKey { case label = "Label", note = "Note", remote = "Remote", remoteLabel = "RemoteLabel", unavailable = "Unavailable" }
    public var label: String
    public var note: String
    public var remote: Bool
    public var remoteLabel: String
    public var unavailable: String
    public init(label: String = "", note: String = "", remote: Bool = false, remoteLabel: String = "", unavailable: String = "") { self.label = label; self.note = note; self.remote = remote; self.remoteLabel = remoteLabel; self.unavailable = unavailable }
}
public struct ReviewSnapshot: Codable, Sendable, Equatable {
    enum CodingKeys: String, CodingKey { case generation = "Generation", capturedAt = "CapturedAt", maybeOutdated = "MaybeOutdated", scope = "Scope", repositories = "Repositories" }
    public var generation: UInt64
    public var capturedAt: Date?
    public var maybeOutdated: Bool
    public var scope: ReviewScopeInfo
    public var repositories: [ReviewRepository]
    public init(generation: UInt64, capturedAt: Date? = nil, maybeOutdated: Bool = false, scope: ReviewScopeInfo = .init(), repositories: [ReviewRepository] = []) { self.generation = generation; self.capturedAt = capturedAt; self.maybeOutdated = maybeOutdated; self.scope = scope; self.repositories = repositories }
}
public struct ReviewRepository: Codable, Identifiable, Sendable, Equatable {
    enum CodingKeys: String, CodingKey { case id = "ID", name = "Name", kind = "Kind", worktrees = "Worktrees", hiddenOutside = "HiddenOutside", error = "Err" }
    public var id: String
    public var name: String
    public var kind: String
    public var worktrees: [ReviewWorktree]
    public var hiddenOutside: Int
    public var error: String
    public init(id: String, name: String, kind: String = "", worktrees: [ReviewWorktree] = [], hiddenOutside: Int = 0, error: String = "") { self.id = id; self.name = name; self.kind = kind; self.worktrees = worktrees; self.hiddenOutside = hiddenOutside; self.error = error }
}
public struct ReviewFileChange: Codable, Identifiable, Sendable, Equatable {
    enum CodingKeys: String, CodingKey { case path = "Path", oldPath = "OldPath", group = "Group", status = "Status", added = "Added", removed = "Removed", binary = "Binary" }
    public var path: String
    public var oldPath: String
    public var group: ReviewFileGroup
    public var status: String
    public var added: Int
    public var removed: Int
    public var binary: Bool
    public var id: String { path + "\u{0}" + oldPath + "\u{0}" + group.rawValue }
    public init(path: String, oldPath: String = "", group: ReviewFileGroup, status: String = "", added: Int = 0, removed: Int = 0, binary: Bool = false) { self.path = path; self.oldPath = oldPath; self.group = group; self.status = status; self.added = added; self.removed = removed; self.binary = binary }
}
public struct ReviewWorktree: Codable, Identifiable, Sendable, Equatable {
    enum CodingKeys: String, CodingKey { case id = "ID", path = "Path", branch = "Branch", detached = "Detached", detachedHead = "DetachedHead", current = "Current", linked = "Linked", state = "State", error = "Err", files = "Files" }
    public var id: String
    public var path: String
    public var branch: String
    public var detached: Bool
    public var detachedHead: String
    public var current: Bool
    public var linked: Bool
    public var state: String
    public var error: String
    public var files: [ReviewFileChange]
    public init(id: String, path: String = "", branch: String = "", detached: Bool = false, detachedHead: String = "", current: Bool = false, linked: Bool = false, state: String = "", error: String = "", files: [ReviewFileChange] = []) { self.id = id; self.path = path; self.branch = branch; self.detached = detached; self.detachedHead = detachedHead; self.current = current; self.linked = linked; self.state = state; self.error = error; self.files = files }
}
public struct ReviewDirectoryEntry: Codable, Identifiable, Sendable, Equatable {
    enum CodingKeys: String, CodingKey { case name = "Name", isDirectory = "Dir" }
    public var name: String
    public var isDirectory: Bool
    public var id: String { name }
    public init(name: String, isDirectory: Bool) { self.name = name; self.isDirectory = isDirectory }
}
public struct ReviewDirectory: Codable, Sendable, Equatable {
    enum CodingKeys: String, CodingKey { case worktreeID = "WorktreeID", path = "Path", state = "State", entries = "Entries", truncated = "Truncated", error = "Err" }
    public var worktreeID: String
    public var path: String
    public var state: ReviewLoadState
    public var entries: [ReviewDirectoryEntry]
    public var truncated: Bool
    public var error: String
    public init(worktreeID: String, path: String, state: ReviewLoadState = .ready, entries: [ReviewDirectoryEntry] = [], truncated: Bool = false, error: String = "") { self.worktreeID = worktreeID; self.path = path; self.state = state; self.entries = entries; self.truncated = truncated; self.error = error }
}
public struct ReviewDocumentKey: Codable, Hashable, Sendable {
    enum CodingKeys: String, CodingKey { case mode = "Mode", worktreeID = "WorktreeID", path = "Path", group = "Group" }
    public var mode: ReviewDocumentMode
    public var worktreeID: String
    public var path: String
    public var group: ReviewFileGroup?
    public init(mode: ReviewDocumentMode, worktreeID: String, path: String, group: ReviewFileGroup? = nil) { self.mode = mode; self.worktreeID = worktreeID; self.path = path; self.group = group }
}
public struct ReviewDocumentLine: Codable, Identifiable, Sendable, Equatable {
    enum CodingKeys: String, CodingKey { case kind = "Kind", oldLine = "Old", newLine = "New", text = "Text" }
    public var kind: ReviewLineKind
    public var oldLine: Int
    public var newLine: Int
    public var text: String
    public var id: String { "\(kind.rawValue):\(oldLine):\(newLine):\(text.hashValue)" }
    public init(kind: ReviewLineKind, oldLine: Int = 0, newLine: Int = 0, text: String = "") { self.kind = kind; self.oldLine = oldLine; self.newLine = newLine; self.text = text }
}
public struct ReviewDocument: Codable, Sendable, Equatable {
    enum CodingKeys: String, CodingKey { case key = "Key", state = "State", lines = "Lines", binary = "Binary", truncated = "Truncated", note = "Note", error = "Err", patchHeader = "PatchHeader" }
    public var key: ReviewDocumentKey
    public var state: ReviewLoadState
    public var lines: [ReviewDocumentLine]
    public var binary: Bool
    public var truncated: Bool
    public var note: String
    public var error: String
    public var patchHeader: String
    public init(key: ReviewDocumentKey, state: ReviewLoadState = .ready, lines: [ReviewDocumentLine] = [], binary: Bool = false, truncated: Bool = false, note: String = "", error: String = "", patchHeader: String = "") { self.key = key; self.state = state; self.lines = lines; self.binary = binary; self.truncated = truncated; self.note = note; self.error = error; self.patchHeader = patchHeader }
}

public struct ReviewDraftScope: Codable, Hashable, Sendable {
    public let serverID: UUID
    public let instanceID: String
    public let sessionID: String
    public let repositoryID: String
    public let worktreeID: String
    public let workspaceLabel: String
    public init(serverID: UUID, instanceID: String, sessionID: String, repositoryID: String, worktreeID: String, workspaceLabel: String) { self.serverID = serverID; self.instanceID = instanceID; self.sessionID = sessionID; self.repositoryID = repositoryID; self.worktreeID = worktreeID; self.workspaceLabel = workspaceLabel }
}
public struct ReviewAnchorLine: Codable, Sendable, Equatable {
    public let kind: ReviewLineKind
    public let oldLine: Int
    public let newLine: Int
    public let text: String
    public init(kind: ReviewLineKind, oldLine: Int = 0, newLine: Int = 0, text: String) { self.kind = kind; self.oldLine = oldLine; self.newLine = newLine; self.text = text }
}
public struct ReviewAnchor: Codable, Sendable, Equatable {
    public var path: String; public var oldPath: String; public var mode: ReviewDocumentMode; public var group: ReviewFileGroup?
    public var generation: UInt64; public var documentFingerprint: String; public var oldStart: Int; public var oldEnd: Int; public var newStart: Int; public var newEnd: Int; public var context: [String]; public var contextLines: [ReviewAnchorLine]
    public init(path: String, oldPath: String = "", mode: ReviewDocumentMode, group: ReviewFileGroup? = nil, generation: UInt64, documentFingerprint: String, oldStart: Int = 0, oldEnd: Int = 0, newStart: Int = 0, newEnd: Int = 0, context: [String] = [], contextLines: [ReviewAnchorLine] = []) {
        self.path = path; self.oldPath = oldPath; self.mode = mode; self.group = group; self.generation = generation; self.documentFingerprint = documentFingerprint; self.oldStart = oldStart; self.oldEnd = oldEnd; self.newStart = newStart; self.newEnd = newEnd; self.context = contextLines.isEmpty ? context : contextLines.map(\.text); self.contextLines = contextLines.isEmpty ? context.map { ReviewAnchorLine(kind: .context, text: $0) } : contextLines
    }
}
public struct ReviewComment: Codable, Identifiable, Sendable, Equatable {
    public let id: UUID; public var anchor: ReviewAnchor; public var body: String; public var stale: Bool
    public init(id: UUID = UUID(), anchor: ReviewAnchor, body: String, stale: Bool = false) { self.id = id; self.anchor = anchor; self.body = body; self.stale = stale }
}
public enum ReviewDraftStatus: String, Codable, Sendable { case idle, sending, failed }
public struct ReviewDraftProjection: Codable, Sendable, Equatable {
    public var scope: ReviewDraftScope; public var comments: [ReviewComment]; public var status: ReviewDraftStatus; public var error: String?
    public init(scope: ReviewDraftScope, comments: [ReviewComment] = [], status: ReviewDraftStatus = .idle, error: String? = nil) { self.scope = scope; self.comments = comments; self.status = status; self.error = error }
}

private struct DynamicCodingKey: CodingKey { var stringValue: String; var intValue: Int?; init?(stringValue: String) { self.stringValue = stringValue; self.intValue = nil }; init?(intValue: Int) { self.stringValue = String(intValue); self.intValue = intValue } }
