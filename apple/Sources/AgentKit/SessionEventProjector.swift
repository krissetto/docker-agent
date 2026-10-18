import Foundation

/// Stateful transcript projection for one opened session. It consumes only validated stream envelopes.
public struct SessionEventProjector: Sendable {
    public private(set) var projection: OpenSessionProjection
    private var assistantMessageIndex = 0
    private var assistantIsOpen = false
    private let maximumTextBytes = 64 * 1024
    private let maximumTranscriptItems = 20_000

    public init(projection: OpenSessionProjection) { self.projection = projection }

    public mutating func updateCursor(sequence: UInt64, epoch: String) {
        projection.cursor = sequence
        projection.epoch = epoch
    }

    public mutating func replace(with snapshot: SessionSnapshotWire, rawProof: RawSnapshotProof? = nil, branchInstanceID: String? = nil) {
        if let epoch = snapshot.epoch, epoch != projection.epoch {
            assistantMessageIndex = 0
        }
        var next = snapshot.project(ref: projection.ref, connection: projection.connection, rawProof: rawProof, branchInstanceID: branchInstanceID)
        next.capabilities = projection.capabilities
        next.transcript = Self.stabilize(old: projection.transcript, new: next.transcript)
        projection = next
        assistantIsOpen = false
    }

    public mutating func apply(_ envelope: StreamEnvelope) {
        guard let event = envelope.event, let type = event["type"]?.stringValue else { return }
        let owner = event["agent_name"]?.stringValue
        func get(_ key: String) -> JSONValue? { event[key] }
        func text(_ key: String) -> String { get(key)?.stringValue ?? "" }
        let eventID = "event:\(envelope.epoch ?? "unknown"):\(envelope.sequence)"
        invalidateBranchAnchors()
        switch type {
        case "stream_started":
            projection.state = .running
            projection.turnID = envelope.turnID
        case "stream_stopped":
            if projection.state == .running { projection.state = .idle }
            projection.turnID = nil
            finishAssistant()
        case "turn_settled":
            projection.state = text("outcome") == "failed" ? .failed : .settled
            projection.turnID = nil
            finishAssistant()
        case "user_message":
            guard text("input_origin") != "runtime" else { return }
            finishAssistant()
            let rawParts = get("multi_content")?.arrayValue ?? []
            projection.transcript.append(TranscriptItem(id: eventID, kind: .message, owner: owner, role: "user", text: text("message"), origin: get("input_origin")?.stringValue, attachments: SessionMediaProjection.attachments(from: rawParts)))
        case "agent_choice":
            appendAssistant(kind: .message, owner: owner, text: text("content"), turnID: envelope.turnID)
        case "agent_choice_reasoning":
            appendAssistant(kind: .reasoning, owner: owner, text: text("content"), turnID: envelope.turnID)
        case "message_added":
            let mediaParts = get("generated_media")?.arrayValue ?? []
            let media = SessionSnapshotWire.generatedMediaReferences(mediaParts)
            if !media.isEmpty {
                let position = Int(get("session_position")?.numberValue ?? -1)
                let messageID = "generated-media:\(projection.epoch):\(envelope.turnID ?? "unknown"):\(position)"
                if let index = projection.transcript.firstIndex(where: { $0.id == messageID }) {
                    let old = projection.transcript[index]
                    projection.transcript[index] = TranscriptItem(id: old.id, kind: old.kind, owner: old.owner, role: old.role, text: old.text, isStreaming: old.isStreaming, origin: old.origin, toolName: old.toolName, toolArguments: old.toolArguments, toolOutput: old.toolOutput, toolStatus: old.toolStatus, child: old.child, attachments: old.attachments, generatedMedia: Array(Set(old.generatedMedia + media)).sorted { $0.id < $1.id })
                } else {
                    projection.transcript.append(TranscriptItem(id: messageID, kind: .message, owner: owner, role: "assistant", text: "Generated media", generatedMedia: media))
                }
            }
            let position = Int(get("session_position")?.numberValue ?? -1)
            let role = text("message_role")
            if role == "assistant" || role.isEmpty {
                finishAssistant()
                assistantMessageIndex = max(assistantMessageIndex + 1, position)
            }
        case "partial_tool_call", "tool_call":
            let call = get("tool_call")?.objectValue ?? [:]
            let id = call["id"]?.stringValue
            guard let id, !id.isEmpty else { return }
            let function = call["function"]?.objectValue ?? [:]
            let name = function["name"]?.stringValue ?? "Tool"
            let arguments = bounded(function["arguments"]?.stringValue ?? "", to: 32 * 1024)
            let rowID = toolRowID(turnID: envelope.turnID, toolCallID: id)
            if let index = projection.transcript.firstIndex(where: { $0.id == rowID }) {
                let old = projection.transcript[index]
                projection.transcript[index] = TranscriptItem(id: rowID, kind: .tool, owner: owner ?? old.owner, text: name, toolName: name, toolArguments: arguments, toolOutput: old.toolOutput, toolStatus: type == "tool_call" ? "running" : old.toolStatus)
            } else {
                projection.transcript.append(TranscriptItem(id: rowID, kind: .tool, owner: owner, text: name, toolName: name, toolArguments: arguments, toolStatus: type == "tool_call" ? "running" : "preparing"))
            }
        case "tool_call_confirmation":
            let call = get("tool_call")?.objectValue ?? [:]
            let function = call["function"]?.objectValue ?? [:]
            let toolName = function["name"]?.stringValue ?? "Tool"
            projection.transcript.append(TranscriptItem(id: eventID, kind: .tool, owner: owner, text: "Approval · \(toolName)", toolName: toolName, toolArguments: bounded(function["arguments"]?.stringValue ?? "", to: 32 * 1024), toolStatus: "approval"))
            if let interactionID = envelope.interactionID {
                projection.interactions.removeAll { $0.id == interactionID }
                projection.interactions.append(.confirmation(id: interactionID, owner: owner ?? projection.agentName, toolName: toolName, arguments: bounded(function["arguments"]?.stringValue ?? "", to: 32 * 1024), decisions: [.approve, .approveBalanced, .approveAutonomous, .approveTool, .reject]))
            }
        case "tool_call_output", "tool_call_response", "shell":
            let toolCallID = text("tool_call_id")
            let toolName = get("tool_definition")?.objectValue?["name"]?.stringValue ?? (type == "shell" ? "Command" : "Tool")
            let result = get("result")?.objectValue
            let output = type == "shell" ? text("output") : (result?["output"]?.stringValue ?? (type == "tool_call_response" ? text("response") : text("output")))
            let isError = result?["isError"]?.boolValue == true
            let rowID = toolCallID.isEmpty ? eventID : toolRowID(turnID: envelope.turnID, toolCallID: toolCallID)
            if let index = projection.transcript.firstIndex(where: { $0.id == rowID }) {
                let old = projection.transcript[index]
                projection.transcript[index] = TranscriptItem(id: rowID, kind: .tool, owner: owner ?? old.owner, text: toolName, toolName: toolName, toolArguments: old.toolArguments, toolOutput: bounded(output, to: maximumTextBytes), toolStatus: isError ? "failed" : "complete")
            } else {
                projection.transcript.append(TranscriptItem(id: rowID, kind: .tool, owner: owner, text: toolName, toolName: toolName, toolOutput: bounded(output, to: maximumTextBytes), toolStatus: isError ? "failed" : "complete"))
            }
        case "token_usage":
            if let usage = get("usage")?.objectValue {
                projection.inputTokens = integer(usage["input_tokens"], fallback: projection.inputTokens)
                projection.outputTokens = integer(usage["output_tokens"], fallback: projection.outputTokens)
                projection.contextLength = integer(usage["context_length"], fallback: projection.contextLength)
                projection.contextLimit = integer(usage["context_limit"], fallback: projection.contextLimit)
                projection.cost = usage["cost"]?.numberValue ?? projection.cost
                projection.compactionThreshold = usage["compaction_threshold"]?.numberValue ?? projection.compactionThreshold
            }
        case "subagent_tree":
            projection.agentTree = SessionSnapshotWire.agentTree(get("snapshot"), serverID: projection.ref.serverID)
        case "session_title": projection.title = text("title")
        case "session_summary":
            finishAssistant()
            projection.transcript.append(TranscriptItem(id: eventID, kind: .activity, owner: owner, text: bounded(text("summary"), to: maximumTextBytes)))
        case "elicitation_request":
            if let interactionID = envelope.interactionID {
                projection.interactions.removeAll { $0.id == interactionID }
                projection.interactions.append(.elicitation(id: interactionID, owner: owner ?? projection.agentName, title: text("message"), fields: SessionSnapshotWire.elicitationFields(get("schema")), elicitationID: text("elicitation_id")))
            }
        case "max_iterations_reached":
            if let interactionID = envelope.interactionID {
                projection.interactions.removeAll { $0.id == interactionID }
                projection.interactions.append(.maxIterations(id: interactionID, owner: owner ?? projection.agentName, limit: Int(get("max_iterations")?.numberValue ?? 0)))
            }
        case "interaction_resolved":
            let interactionID = get("interaction_id")?.stringValue ?? envelope.interactionID
            if let interactionID { projection.interactions.removeAll { $0.id == interactionID } }
        case "error":
            projection.state = .failed
            projection.transcript.append(TranscriptItem(id: eventID, kind: .error, owner: owner, text: bounded(text("error"), to: maximumTextBytes)))
        default: break
        }
        projection.refreshedAt = .now
        if projection.transcript.count > maximumTranscriptItems { projection.transcript.removeFirst(projection.transcript.count - maximumTranscriptItems) }
    }

    private mutating func appendAssistant(kind: TranscriptItem.Kind, owner: String?, text: String, turnID: String?) {
        guard !text.isEmpty else { return }
        let key = kind == .reasoning ? "reasoning" : "assistant"
        let id = "\(key):\(projection.epoch):\(turnID ?? "unknown"):\(assistantMessageIndex)"
        if let index = projection.transcript.lastIndex(where: { $0.id == id }) {
            let old = projection.transcript[index]
            projection.transcript[index] = TranscriptItem(id: id, kind: kind, owner: owner ?? old.owner, role: old.role, text: bounded(old.text + text, to: maximumTextBytes), isStreaming: true)
        } else {
            projection.transcript.append(TranscriptItem(id: id, kind: kind, owner: owner, text: bounded(text, to: maximumTextBytes), isStreaming: true))
        }
        assistantIsOpen = true
    }

    private mutating func finishAssistant() {
        guard assistantIsOpen else { return }
        projection.transcript = projection.transcript.map { item in
            guard item.id.hasPrefix("assistant:") || item.id.hasPrefix("reasoning:") else { return item }
            return TranscriptItem(id: item.id, kind: item.kind, owner: item.owner, role: item.role, text: item.text, isStreaming: false, origin: item.origin, toolName: item.toolName, toolArguments: item.toolArguments, toolOutput: item.toolOutput, toolStatus: item.toolStatus, child: item.child, attachments: item.attachments, generatedMedia: item.generatedMedia, userMessageIndex: item.userMessageIndex, branchAnchor: item.branchAnchor)
        }
        assistantIsOpen = false
    }

    private mutating func invalidateBranchAnchors() {
        projection.transcript = projection.transcript.map { item in
            guard item.branchAnchor != nil else { return item }
            return TranscriptItem(id: item.id, kind: item.kind, owner: item.owner, role: item.role, text: item.text,
                                  isStreaming: item.isStreaming, origin: item.origin, toolName: item.toolName,
                                  toolArguments: item.toolArguments, toolOutput: item.toolOutput, toolStatus: item.toolStatus,
                                  child: item.child, attachments: item.attachments, generatedMedia: item.generatedMedia,
                                  userMessageIndex: item.userMessageIndex)
        }
    }
    private func toolRowID(turnID: String?, toolCallID: String) -> String { "tool:\(turnID ?? "unknown"):\(toolCallID)" }
    private func bounded(_ value: String, to limit: Int) -> String {
        guard value.utf8.count > limit else { return value }
        let prefix = value.utf8.prefix(limit)
        return String(decoding: prefix, as: UTF8.self) + "\n… truncated …"
    }
    private func integer(_ value: JSONValue?, fallback: Int64) -> Int64 { value?.numberValue.map(Int64.init) ?? fallback }

    private static func stabilize(old: [TranscriptItem], new: [TranscriptItem]) -> [TranscriptItem] {
        var oldByKey: [String: [TranscriptItem]] = [:]
        for item in old { oldByKey[fingerprint(item), default: []].append(item) }
        return new.map { item in
            let key = fingerprint(item)
            guard var matches = oldByKey[key], !matches.isEmpty else { return item }
            let old = matches.removeFirst()
            oldByKey[key] = matches
            return TranscriptItem(id: old.id, kind: item.kind, owner: item.owner, role: item.role, text: item.text, isStreaming: item.isStreaming, origin: item.origin, toolName: item.toolName, toolArguments: item.toolArguments, toolOutput: item.toolOutput, toolStatus: item.toolStatus, child: item.child, attachments: item.attachments, generatedMedia: item.generatedMedia, userMessageIndex: item.userMessageIndex, branchAnchor: item.branchAnchor)
        }
    }
    private static func fingerprint(_ item: TranscriptItem) -> String { "\(item.kind.rawValue)|\(item.owner ?? "")|\(item.role ?? "")|\(item.text)" }
}
