import Foundation

public struct CatalogResponse: Decodable, Sendable { public let nextCursor: String?; public let sources: [CatalogSource]; public let sessions: [CatalogSession]; public let version: Int; enum CodingKeys: String, CodingKey { case nextCursor = "next_cursor", sources, sessions, version } }
public struct CatalogSource: Decodable, Sendable { public let name: String; public let canCreate: Bool; enum CodingKeys: String, CodingKey { case name, canCreate = "can_create" } }
public struct CatalogSession: Decodable, Sendable {
    public let sessionID: String; public let parentID: String?; public let source: String?; public let agentName: String?; public let title: String?; public let createdAt: String?; public let updatedAt: String?; public let cost: Double?; public let state: String?; public let stateKnown: Bool?; public let activity: String?; public let interactions: Int?; public let pending: Int?; public let lastError: String?; public let loaded: Bool?; public let loadable: Bool?; public let starred: Bool?; public let workingDir: String?
    enum CodingKeys: String, CodingKey { case sessionID = "session_id", parentID = "parent_id", source, agentName = "agent_name", title, createdAt = "created_at", updatedAt = "updated_at", cost, state, stateKnown = "state_known", activity, interactions, pending, lastError = "last_error", loaded, loadable, starred, workingDir = "working_dir" }
    public func projection(serverID: UUID) -> SessionProjection {
        let status: SessionState
        switch state?.lowercased() { case "running", "active": status = .running; case "waiting", "paused", "needs_input": status = .waiting; case "settled", "idle", "stopped": status = .settled; case "failed", "error": status = .failed; case .none: status = .unknown; default: status = .unknown }
        let formatter = ISO8601DateFormatter()
        let date = formatter.date(from: updatedAt ?? "") ?? formatter.date(from: createdAt ?? "")
        let reason = (interactions ?? 0) > 0 ? "Waiting for approval or response" : ((pending ?? 0) > 0 ? "Waiting for input" : (lastError?.isEmpty == false ? lastError : nil))
        return SessionProjection(ref: SessionRef(serverID: serverID, sessionID: sessionID), title: title ?? "New session", agentName: agentName ?? "", source: source, state: status, activity: activity, needsYouReason: reason, updatedAt: date, cost: cost ?? 0, parentID: parentID, starred: starred ?? false, loaded: loaded ?? false, loadable: loadable ?? false)
    }
}

public struct SessionSnapshotWire: Decodable, Sendable {
    public let parentSessionID: String?; public let session: SessionHistoryWire?; public let status: StatusWire; public let interactions: [InteractionWire]?; public let pendingInputs: [PendingInputWire]?; public let epoch: String?; public let cursor: UInt64?; public let transcriptPosition: Int?
    enum CodingKeys: String, CodingKey { case parentSessionID = "parent_session_id", session, status, interactions, pendingInputs = "pending_inputs", epoch, cursor, transcriptPosition = "transcript_position" }
    public func project(ref: SessionRef, connection: ConnectionState, rawProof: RawSnapshotProof? = nil, branchInstanceID: String? = nil) -> OpenSessionProjection {
        let session = self.session; let title = session?.title ?? "New session", agent = status.agentName ?? ""
        let forkOrdinals = session?.topLevelForkOrdinalByItemIndex ?? [:]
        let branchProofMatches = rawProof != nil && branchInstanceID?.isEmpty == false && rawProof?.sessionID == session?.id && rawProof?.sessionID == status.sessionID && rawProof?.epoch == (epoch ?? "") && rawProof?.cursor == (cursor ?? 0)
        let branchItems = branchProofMatches ? Dictionary(uniqueKeysWithValues: rawProof!.items.map { ($0.position, $0) }) : [:]
        func branchAnchor(_ index: Int) -> SessionBranchAnchor? {
            guard let proof = rawProof, branchProofMatches, let item = branchItems[index] else { return nil }
            guard let branchInstanceID else { return nil }
            return SessionBranchAnchor(ref: ref, instanceID: branchInstanceID, epoch: proof.epoch, cursor: proof.cursor,
                                       messagesSHA256: proof.messagesSHA256, itemPosition: index, itemSHA256: item.sha256)
        }
        var transcript: [TranscriptItem] = []
        for (index, item) in (session?.messages ?? []).enumerated() {
            if let entry = item.message {
                if entry.inputOrigin == "runtime" { continue }
                let wire = entry.message
                let userMessageIndex = wire?.role == "user" ? forkOrdinals[index] : nil
                if let parts = wire?.presentation, !parts.isEmpty {
                    for (partIndex, part) in parts.enumerated() {
                        switch part.type {
                        case "content":
                            let rawParts = wire?.multiContent ?? []
                            transcript.append(TranscriptItem(id: "snapshot:\(item.position ?? index):part:\(partIndex)", kind: .message, owner: entry.agentName, role: wire?.role, text: part.text ?? "", origin: entry.inputOrigin, attachments: SessionMediaProjection.attachments(from: rawParts), userMessageIndex: userMessageIndex, branchAnchor: branchAnchor(index)))
                        case "reasoning": transcript.append(TranscriptItem(id: "snapshot:\(item.position ?? index):part:\(partIndex)", kind: .reasoning, owner: entry.agentName, text: part.text ?? ""))
                        case "tool_call":
                            let call = wire?.toolCalls?.first(where: { $0.id == part.toolCallID })
                            transcript.append(TranscriptItem(id: "snapshot:\(item.position ?? index):part:\(partIndex)", kind: .tool, owner: entry.agentName, text: call?.function?.name ?? "Tool", toolName: call?.function?.name, toolArguments: String((call?.function?.arguments ?? "").prefix(32_768)), toolStatus: "complete", branchAnchor: branchAnchor(index)))
                        default: break
                        }
                    }
                } else {
                    let content = wire?.content ?? ""
                    if let reasoning = wire?.reasoningContent, !reasoning.isEmpty { transcript.append(TranscriptItem(id: "snapshot:\(item.position ?? index):reason", kind: .reasoning, owner: entry.agentName, text: reasoning)) }
                    let rawParts = wire?.multiContent ?? []
                    let attachments = SessionMediaProjection.attachments(from: rawParts)
                    let media = Self.generatedMediaReferences(rawParts)
                    if !content.isEmpty || !attachments.isEmpty || !media.isEmpty { transcript.append(TranscriptItem(id: "snapshot:\(item.position ?? index):message", kind: .message, owner: entry.agentName, role: wire?.role, text: content, origin: entry.inputOrigin, attachments: attachments, generatedMedia: media, userMessageIndex: userMessageIndex, branchAnchor: branchAnchor(index))) }
                    for (callIndex, call) in (wire?.toolCalls ?? []).enumerated() { transcript.append(TranscriptItem(id: "snapshot:\(item.position ?? index):tool:\(callIndex)", kind: .tool, owner: entry.agentName, text: call.function?.name ?? "Tool", toolName: call.function?.name, toolArguments: String((call.function?.arguments ?? "").prefix(32_768)), toolStatus: "complete", branchAnchor: branchAnchor(index))) }
                }
                if wire?.role == "tool" { transcript.append(TranscriptItem(id: "snapshot:\(index):tool-output", kind: .tool, owner: entry.agentName, role: "tool", text: "Tool result", toolOutput: String((wire?.content ?? "").prefix(65_536)), toolStatus: wire?.isError == true ? "failed" : "complete", branchAnchor: branchAnchor(index))) }
            } else if let error = item.error { transcript.append(TranscriptItem(id: "snapshot:\(item.position ?? index):error", kind: .error, owner: error.agentName, text: error.message ?? "Session error", branchAnchor: branchAnchor(index))) }
            else if let summary = item.summary, !summary.isEmpty { transcript.append(TranscriptItem(id: "snapshot:\(item.position ?? index):summary", kind: .activity, text: summary, branchAnchor: branchAnchor(index))) }
        }
        let interactions = (self.interactions ?? []).compactMap { wire -> InteractionProjection? in
            let owner = wire.event?["agent_name"]?.stringValue ?? agent
            switch wire.kind {
            case "confirmation": let call = wire.event?["tool_call"]?.objectValue ?? [:], fn = call["function"]?.objectValue ?? [:]; return .confirmation(id: wire.interactionID, owner: owner, toolName: fn["name"]?.stringValue ?? "Tool", arguments: String((fn["arguments"]?.stringValue ?? "").prefix(32_768)), decisions: [.approve, .approveBalanced, .approveAutonomous, .approveTool, .reject])
            case "elicitation": return .elicitation(id: wire.interactionID, owner: owner, title: wire.event?["message"]?.stringValue ?? "Question", fields: Self.elicitationFields(wire.event?["schema"]), elicitationID: wire.elicitationID ?? "")
            case "max_iterations": return .maxIterations(id: wire.interactionID, owner: owner, limit: Int(wire.event?["max_iterations"]?.numberValue ?? 0))
            default: return nil
            }
        }
        for pending in pendingInputs ?? [] where pending.inputOrigin != "runtime" { transcript.append(TranscriptItem(id: "snapshot:pending:\(pending.turnID)", kind: .message, owner: pending.senderName, role: "user", text: pending.content, origin: pending.inputOrigin)) }
        let state: SessionState
        switch status.state?.lowercased() { case "queued": state = .queued; case "running": state = .running; case "cancelling": state = .cancelling; case "waiting": state = .waiting; case "settled", "idle", "stopped": state = .settled; case "failed", "error": state = .failed; case .none: state = .unknown; default: state = .unknown }
        let parentID = parentSessionID ?? session?.parentID
        let parentSession = parentID.flatMap { !$0.isEmpty && $0 != ref.sessionID ? SessionRef(serverID: ref.serverID, sessionID: $0) : nil }
        return OpenSessionProjection(ref: ref, parentSession: parentSession, title: title, agentName: agent, source: session?.source, state: state, turnID: status.turnID, connection: connection, refreshedAt: .now, currentActivity: status.lastError, transcript: transcript, interactions: interactions, capabilities: .init(), agentTree: Self.agentTree(session?.subagentTree, serverID: ref.serverID), inputTokens: session?.inputTokens ?? 0, outputTokens: session?.outputTokens ?? 0, contextLength: (session?.inputTokens ?? 0) + (session?.outputTokens ?? 0), contextLimit: 0, cost: session?.cost ?? 0, cursor: cursor ?? 0, epoch: epoch ?? "", pendingInputCount: status.pending ?? 0)
    }
    public static func generatedMediaReferences(_ parts: [JSONValue]) -> [GeneratedMediaReference] {
        parts.compactMap { part in
            guard case .object = part,
                  let data = try? JSONEncoder().encode(part),
                  let wire = try? JSONDecoder().decode(GeneratedMediaPartWire.self, from: data),
                  let reference = try? GeneratedMediaReference(part: wire) else { return nil }
            return reference
        }
    }

    public static func agentTree(_ value: JSONValue?, serverID: UUID) -> [AgentNodeProjection] {
        guard let tree = value?.objectValue, let nodes = tree["nodes"]?.arrayValue else { return [] }
        func node(_ value: JSONValue) -> AgentNodeProjection? {
            guard let obj = value.objectValue else { return nil }
            let info = obj["node"]?.objectValue ?? obj
            guard let id = info["id"]?.stringValue, !id.isEmpty else { return nil }
            let agent = info["agent"]?.stringValue ?? "Agent"
            let sessionID = info["session_id"]?.stringValue
            let children = (obj["children"]?.arrayValue ?? []).compactMap(node)
            let state: SessionState
            switch info["state"]?.stringValue {
            case "starting": state = .queued
            case "running": state = .running
            case "idle", "completed", "stopped": state = .settled
            case "failed": state = .failed
            default: state = .unknown
            }
            let name = info["name"]?.stringValue ?? agent
            return AgentNodeProjection(
                id: id,
                session: sessionID.map { SessionRef(serverID: serverID, sessionID: $0) },
                agentName: name,
                state: state,
                needsAttention: info["needs_attention"]?.boolValue ?? false,
                waitingOn: info["waiting_on"]?.stringValue,
                cost: info["cost"]?.numberValue ?? 0,
                inputTokens: Int64(info["input_tokens"]?.numberValue ?? 0),
                outputTokens: Int64(info["output_tokens"]?.numberValue ?? 0),
                children: children
            )
        }
        return nodes.compactMap(node)
    }
    public static func elicitationFields(_ schema: JSONValue?) -> [ElicitationField] { guard let object = schema?.objectValue, let properties = object["properties"]?.objectValue else { return [] }; let required = Set((object["required"]?.arrayValue ?? []).compactMap(\.stringValue)); return properties.keys.sorted().map { key in let value = properties[key]?.objectValue ?? [:]; return ElicitationField(id: key, label: value["title"]?.stringValue ?? key, type: value["type"]?.stringValue ?? "string", required: required.contains(key), options: (value["enum"]?.arrayValue ?? []).compactMap(\.stringValue)) } }
}

public struct CreateSessionRequest: Encodable, Sendable { public var sessionID: String; public var source: String; public var agentName: String; public var title: String; public var model: String; public var workingDir: String?; enum CodingKeys: String, CodingKey { case sessionID = "session_id", source, agentName = "agent_name", title, model, workingDir = "working_dir" }; public init(sessionID: String, source: String = "", agentName: String, title: String = "", model: String = "", workingDir: String? = nil) { self.sessionID = sessionID; self.source = source; self.agentName = agentName; self.title = title; self.model = model; self.workingDir = workingDir } }
public struct SessionInput: Encodable, Sendable {
    public var content: String
    public var mode: String
    public var requestID: String
    public var multiContent: [SessionMessagePart]?
    enum CodingKeys: String, CodingKey { case content, mode, requestID = "request_id", multiContent = "multi_content" }
    public init(content: String, mode: String = "", requestID: String, multiContent: [SessionMessagePart]? = nil) { self.content = content; self.mode = mode; self.requestID = requestID; self.multiContent = multiContent }
    public init(payload: SessionSendPayload, mode: String = "", requestID: String) { content = payload.content; self.mode = mode; self.requestID = requestID; multiContent = payload.multiContent.isEmpty ? nil : payload.multiContent }
}
public struct StartSessionRequest: Encodable, Sendable { public var sessionID: String; public var source: String; public var agentName: String; public var title: String; public var model: String; public var workingDir: String?; public var input: SessionInput; enum CodingKeys: String, CodingKey { case sessionID = "session_id", source, agentName = "agent_name", title, model, workingDir = "working_dir", input }; public init(sessionID: String, source: String = "", agentName: String, title: String = "", model: String = "", workingDir: String? = nil, input: SessionInput) { self.sessionID = sessionID; self.source = source; self.agentName = agentName; self.title = title; self.model = model; self.workingDir = workingDir; self.input = input } }
public struct SessionResponseRequest: Encodable, Sendable { public var interactionID: String; public var kind: String; public var confirmation: String?; public var toolName: String?; public var reason: String?; public var elicitationID: String?; public var action: String?; public var content: [String: JSONValue]?; enum CodingKeys: String, CodingKey { case interactionID = "interaction_id", kind, confirmation, toolName = "tool_name", reason, elicitationID = "elicitation_id", action, content }; public init(interactionID: String, kind: String, confirmation: String? = nil, toolName: String? = nil, reason: String? = nil, elicitationID: String? = nil, action: String? = nil, content: [String: JSONValue]? = nil) { self.interactionID = interactionID; self.kind = kind; self.confirmation = confirmation; self.toolName = toolName; self.reason = reason; self.elicitationID = elicitationID; self.action = action; self.content = content } }

public struct ForkUserMessageCandidate: Sendable, Equatable, Identifiable {
    public let index: Int
    public let text: String
    public let isTopLevel: Bool
    public let isTruncated: Bool
    public let containsAttachments: Bool
    public var id: Int { index }
    public var canForkBefore: Bool { isTopLevel && !isTruncated && !containsAttachments }
    public var canPrefillText: Bool { canForkBefore }
    public init(index: Int, text: String, isTopLevel: Bool, isTruncated: Bool, containsAttachments: Bool = false) { self.index = index; self.text = text; self.isTopLevel = isTopLevel; self.isTruncated = isTruncated; self.containsAttachments = containsAttachments }
}

public struct SessionHistoryWire: Decodable, Sendable {
    public let id: String; public let title: String?; public let parentID: String?; public let source: String?; public let subagentTree: JSONValue?; public let messages: [SessionHistoryItemWire]?; public let inputTokens: Int64?; public let outputTokens: Int64?; public let cost: Double?; public let starred: Bool?
    enum CodingKeys: String, CodingKey { case id, title, parentID = "parent_id", source, subagentTree = "subagent_tree", messages, inputTokens = "input_tokens", outputTokens = "output_tokens", cost, starred }
    public var forkUserMessages: [ForkUserMessageCandidate] {
        var ordinal = 0
        func collect(_ session: SessionHistoryWire, topLevel: Bool, into result: inout [ForkUserMessageCandidate]) {
            for item in session.messages ?? [] {
                if let entry = item.message, let message = entry.message {
                    if message.role == "user" {
                        result.append(ForkUserMessageCandidate(index: ordinal, text: message.content ?? "", isTopLevel: topLevel, isTruncated: false, containsAttachments: !(message.multiContent?.isEmpty ?? true)))
                        ordinal += 1
                    }
                } else if let child = item.subSession {
                    collect(child, topLevel: false, into: &result)
                }
            }
        }
        var result: [ForkUserMessageCandidate] = []
        collect(self, topLevel: true, into: &result)
        return result
    }

    /// Top-level `session.messages` array index mapped to GetAllMessages' flattened user ordinal.
    public var topLevelForkOrdinalByItemIndex: [Int: Int] {
        var ordinal = 0
        var mapping: [Int: Int] = [:]
        func userMessageCount(_ session: SessionHistoryWire) -> Int {
            var count = 0
            for item in session.messages ?? [] {
                if let entry = item.message, entry.message?.role != "system", let message = entry.message {
                    if message.role == "user" { count += 1 }
                } else if let child = item.subSession { count += userMessageCount(child) }
            }
            return count
        }
        for (index, item) in (messages ?? []).enumerated() {
            if let entry = item.message, entry.message?.role != "system", let message = entry.message {
                if message.role == "user" { mapping[index] = ordinal; ordinal += 1 }
                else { /* Non-user messages occupy a flat position but do not change user ordinal. */ }
            } else if let child = item.subSession { ordinal += userMessageCount(child) }
        }
        return mapping
    }
}
public struct SessionHistoryItemWire: Decodable, Sendable { public let message: SessionHistoryMessageWire?; public let subSession: SessionHistoryWire?; public let error: SessionErrorWire?; public let summary: String?; public let position: Int?; enum CodingKeys: String, CodingKey { case message, subSession = "sub_session", error, summary, position } }
public struct SessionHistoryMessageWire: Decodable, Sendable { public let agentName: String?; public let message: ChatMessageWire?; public let inputOrigin: String?; enum CodingKeys: String, CodingKey { case agentName = "agent_name", message, inputOrigin = "input_origin" } }
public struct ChatMessageWire: Decodable, Sendable {
    public let role: String?; public let content: String?; public let reasoningContent: String?; public let toolCalls: [ToolCallWire]?; public let toolCallID: String?; public let isError: Bool?; public let presentation: [AssistantPartWire]?; public let multiContent: [JSONValue]?
    enum CodingKeys: String, CodingKey { case role, content, reasoningContent = "reasoning_content", toolCalls = "tool_calls", toolCallID = "tool_call_id", isError = "is_error", presentation, multiContent = "multi_content" }
}
public struct AssistantPartWire: Decodable, Sendable { public let type: String; public let text: String?; public let toolCallID: String?; public let tool: AssistantToolWire?; enum CodingKeys: String, CodingKey { case type, text, toolCallID = "tool_call_id", tool } }
public struct AssistantToolWire: Decodable, Sendable { public let call: ToolCallWire; public let result: String?; public let isError: Bool?; enum CodingKeys: String, CodingKey { case call, result, isError = "is_error" } }

public struct EmptyBody: Encodable, Sendable { public init() {} }

public struct PreparedViewResponse: Decodable, Sendable { public let version: Int; public let view: String; public let info: PreparedViewInfoWire; public var confirmation: SessionViewConfirmation? { info.confirmation } }
public struct PreparedViewInfoWire: Decodable, Sendable {
    public let activeAgentName: String?; public let sessionID: String; public let rootSessionID: String; public let rootWorkingDir: String?; public let rootBinding: SessionBindingWire?; public let session: PreparedSessionHeader?; public let binding: SessionBindingWire; public let workingDir: String; public let attach: AttachInfoWire?
    enum CodingKeys: String, CodingKey { case activeAgentName = "active_agent_name", sessionID = "session_id", rootSessionID = "root_session_id", rootWorkingDir = "root_working_dir", rootBinding = "root_binding", session, binding, workingDir = "working_dir", attach }
    var confirmation: SessionViewConfirmation? { guard let session else { return nil }; return SessionViewConfirmation(sessionID: sessionID, rootSessionID: rootSessionID, rootWorkingDir: rootWorkingDir, rootBinding: rootBinding, source: session.attributes?["docker-agent.actor.source"] ?? "", workingDir: workingDir, binding: binding, activeAgentName: activeAgentName ?? "", nodeID: attach?.nodeID) }
}
public struct PreparedSessionHeader: Decodable, Sendable { public let attributes: [String: String]? }
public struct SessionBindingWire: Codable, Sendable, Equatable { public var agentName: String; public var model: String; public var durability: String; public var parentSessionID: String; enum CodingKeys: String, CodingKey { case agentName = "AgentName", model = "Model", durability = "Durability", parentSessionID = "ParentSessionID" }; public init(agentName: String = "", model: String = "", durability: String = "", parentSessionID: String = "") { self.agentName = agentName; self.model = model; self.durability = durability; self.parentSessionID = parentSessionID } }
public struct AttachInfoWire: Decodable, Sendable { public let nodeID: String?; enum CodingKeys: String, CodingKey { case nodeID = "NodeID" } }
public struct SessionViewConfirmation: Codable, Sendable { public var sessionID: String; public var rootSessionID: String; public var rootWorkingDir: String?; public var rootBinding: SessionBindingWire?; public var source: String; public var workingDir: String; public var binding: SessionBindingWire; public var activeAgentName: String; public var nodeID: String?; enum CodingKeys: String, CodingKey { case sessionID = "session_id", rootSessionID = "root_session_id", rootWorkingDir = "root_working_dir", rootBinding = "root_binding", source, workingDir = "working_dir", binding, activeAgentName = "active_agent_name", nodeID = "node_id" } }
public struct OpenViewRequest: Encodable, Sendable { public let kind: String; public let viewConfirmation: SessionViewConfirmation; enum CodingKeys: String, CodingKey { case kind, viewConfirmation = "view_confirmation" }; public init(kind: String, viewConfirmation: SessionViewConfirmation) { self.kind = kind; self.viewConfirmation = viewConfirmation } }
public struct OpenedViewResponse: Decodable, Sendable { public let version: Int; public let view: String; public let metadata: OpenedMetadata? }
public struct OpenedMetadata: Decodable, Sendable { public let capabilities: SessionCapabilities }
