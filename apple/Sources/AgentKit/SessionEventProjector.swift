import Foundation

/// Stateful transcript projection for one opened session. It consumes only validated stream envelopes.
public struct SessionEventProjector: Sendable {
    public private(set) var projection: OpenSessionProjection
    private var assistantMessageIndex = 0
    private var openAssistantIDs: Set<String> = []
    private var transcriptIndices: [String: Int]
    private var queuedInputEventSequences: [String: UInt64] = [:]
    private var branchAnchorsInvalidated = false
    private let maximumTextBytes = 64 * 1024
    public static let maximumTranscriptItems = 20_000

    public init(projection: OpenSessionProjection) {
        self.projection = projection
        transcriptIndices = Self.indices(projection.transcript)
    }

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
        if snapshot.projection == "tail" {
            let anchorEnd = next.transcriptWindow.anchorEndPosition ?? 0
            let sameEpoch = snapshot.epoch == projection.epoch
            let baselineCursor = snapshot.cursor ?? 0
            let retainedLive = sameEpoch ? projection.transcript.filter { item in
                if item.userInputState == .queued, let turnID = item.turnID {
                    return (queuedInputEventSequences[turnID] ?? 0) > baselineCursor
                }
                return item.sourcePosition.map { $0 >= anchorEnd } == true || item.sourcePosition == nil
            } : []
            queuedInputEventSequences = queuedInputEventSequences.filter { $0.value > baselineCursor }
            let ids = Set(next.transcript.map(\.id))
            next.transcript.append(contentsOf: retainedLive.filter { !ids.contains($0.id) })
            Self.trimToBounds(&next)
        } else {
            next.transcript = Self.stabilize(old: projection.transcript, new: next.transcript)
        }
        projection = next
        transcriptIndices = Self.indices(next.transcript)
        openAssistantIDs.removeAll(keepingCapacity: true)
        branchAnchorsInvalidated = false
    }

    public mutating func apply(_ envelope: StreamEnvelope) {
        guard let event = envelope.event, let type = event["type"]?.stringValue else { return }
        let owner = event["agent_name"]?.stringValue
        func get(_ key: String) -> JSONValue? { event[key] }
        func text(_ key: String) -> String { get(key)?.stringValue ?? "" }
        func sourcePosition() -> Int? {
            if let position = get("session_position")?.numberValue, position >= 0, position.rounded(.towardZero) == position { return Int(position) }
            return envelope.transcriptPosition >= 0 ? envelope.transcriptPosition : nil
        }
        let eventID = "event:\(envelope.epoch ?? "unknown"):\(envelope.sequence)"
        if !branchAnchorsInvalidated {
            invalidateBranchAnchors()
            branchAnchorsInvalidated = true
        }
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
            finishAssistant()
            let origin = get("input_origin")?.stringValue
            let turnID = envelope.turnID ?? text("turn_id")
            let rawParts = get("multi_content")?.arrayValue ?? []
            let attachments = SessionMediaProjection.attachments(from: rawParts)
            let existingIndex = turnID.isEmpty ? nil : transcriptIndex(turnID: turnID)
            if !turnID.isEmpty { queuedInputEventSequences[turnID] = envelope.sequence }
            guard origin != "runtime" else { return }
            if let existingIndex {
                let old = projection.transcript[existingIndex]
                projection.transcript[existingIndex] = TranscriptItem(id: old.id, kind: .message, owner: get("sender_name")?.stringValue ?? old.owner, role: "user", text: text("message"), origin: origin ?? old.origin, turnID: turnID.isEmpty ? old.turnID : turnID, sourcePosition: sourcePosition() ?? old.sourcePosition, sourcePartIndex: 0, sourceHistoryGeneration: old.sourceHistoryGeneration, attachments: attachments, userMessageIndex: old.userMessageIndex, branchAnchor: old.branchAnchor)
            } else {
                appendTranscript(TranscriptItem(id: eventID, kind: .message, owner: get("sender_name")?.stringValue ?? owner, role: "user", text: text("message"), origin: origin, turnID: turnID.isEmpty ? nil : turnID, sourcePosition: sourcePosition(), sourcePartIndex: 0, attachments: attachments))
            }
        case "pending_user_message_accepted":
            let origin = get("input_origin")?.stringValue
            guard origin == nil || origin == "" || origin == "user" else { return }
            let turnID = text("turn_id")
            guard !turnID.isEmpty else { return }
            queuedInputEventSequences[turnID] = envelope.sequence
            let id = "pending:\(turnID)"
            if let index = transcriptIndex(turnID: turnID) {
                let old = projection.transcript[index]
                projection.transcript[index] = TranscriptItem(id: old.id, kind: .message, owner: get("sender_name")?.stringValue ?? old.owner, role: "user", text: text("message"), origin: origin, turnID: turnID, userInputState: get("immediate")?.boolValue == true ? nil : .queued, sourcePosition: sourcePosition() ?? old.sourcePosition, sourcePartIndex: 0, sourceHistoryGeneration: old.sourceHistoryGeneration, attachments: SessionMediaProjection.attachments(from: get("multi_content")?.arrayValue ?? []), userMessageIndex: old.userMessageIndex, branchAnchor: old.branchAnchor)
            } else {
                appendTranscript(TranscriptItem(id: id, kind: .message, owner: get("sender_name")?.stringValue, role: "user", text: text("message"), origin: origin, turnID: turnID, userInputState: get("immediate")?.boolValue == true ? nil : .queued, sourcePosition: sourcePosition(), sourcePartIndex: 0, attachments: SessionMediaProjection.attachments(from: get("multi_content")?.arrayValue ?? [])))
            }
        case "pending_user_message_edited":
            let turnID = text("turn_id")
            guard !turnID.isEmpty, let index = transcriptIndex(turnID: turnID) else { return }
            let old = projection.transcript[index]
            projection.transcript[index] = TranscriptItem(id: old.id, kind: .message, owner: get("sender_name")?.stringValue ?? old.owner, role: "user", text: text("message"), origin: get("input_origin")?.stringValue ?? old.origin, turnID: turnID, userInputState: old.userInputState, sourcePosition: sourcePosition() ?? old.sourcePosition, sourcePartIndex: 0, sourceHistoryGeneration: old.sourceHistoryGeneration, attachments: SessionMediaProjection.attachments(from: get("multi_content")?.arrayValue ?? []), userMessageIndex: old.userMessageIndex, branchAnchor: old.branchAnchor)
        case "pending_user_message_promoted":
            let turnID = text("turn_id")
            guard !turnID.isEmpty, let index = transcriptIndex(turnID: turnID) else { return }
            let old = projection.transcript[index]
            projection.transcript[index] = TranscriptItem(id: old.id, kind: .message, owner: get("sender_name")?.stringValue ?? old.owner, role: "user", text: text("message").isEmpty ? old.text : text("message"), origin: get("input_origin")?.stringValue ?? old.origin, turnID: turnID, sourcePosition: sourcePosition() ?? old.sourcePosition, sourcePartIndex: 0, sourceHistoryGeneration: old.sourceHistoryGeneration, attachments: SessionMediaProjection.attachments(from: get("multi_content")?.arrayValue ?? []), userMessageIndex: old.userMessageIndex, branchAnchor: old.branchAnchor)
        case "pending_user_message_canceled":
            let turnID = text("turn_id")
            guard !turnID.isEmpty, let index = transcriptIndex(turnID: turnID), projection.transcript[index].userInputState == .queued else { return }
            queuedInputEventSequences[turnID] = nil
            projection.transcript.remove(at: index)
            transcriptIndices = Self.indices(projection.transcript)
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
                if let index = transcriptIndices[messageID] {
                    let old = projection.transcript[index]
                    projection.transcript[index] = TranscriptItem(id: old.id, kind: old.kind, owner: old.owner, role: old.role, text: old.text, isStreaming: old.isStreaming, origin: old.origin, toolName: old.toolName, toolArguments: old.toolArguments, toolOutput: old.toolOutput, toolStatus: old.toolStatus, toolCallID: old.toolCallID, turnID: old.turnID, userInputState: old.userInputState, sourcePosition: old.sourcePosition, sourcePartIndex: old.sourcePartIndex, sourceHistoryGeneration: old.sourceHistoryGeneration, child: old.child, attachments: old.attachments, generatedMedia: Array(Set(old.generatedMedia + media)).sorted { $0.id < $1.id })
                } else {
                    appendTranscript(TranscriptItem(id: messageID, kind: .message, owner: owner, role: "assistant", text: "Generated media", generatedMedia: media))
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
            if let index = transcriptIndices[rowID] {
                let old = projection.transcript[index]
                projection.transcript[index] = TranscriptItem(id: rowID, kind: .tool, owner: owner ?? old.owner, role: "assistant", text: name, toolName: name, toolArguments: arguments, toolOutput: old.toolOutput, toolStatus: type == "tool_call" ? "running" : old.toolStatus, toolCallID: id)
            } else {
                appendTranscript(TranscriptItem(id: rowID, kind: .tool, owner: owner, role: "assistant", text: name, toolName: name, toolArguments: arguments, toolStatus: type == "tool_call" ? "running" : "preparing", toolCallID: id))
            }
        case "tool_call_confirmation":
            let call = get("tool_call")?.objectValue ?? [:]
            let function = call["function"]?.objectValue ?? [:]
            let toolName = function["name"]?.stringValue ?? "Tool"
            appendTranscript(TranscriptItem(id: eventID, kind: .tool, owner: owner, text: "Approval · \(toolName)", toolName: toolName, toolArguments: bounded(function["arguments"]?.stringValue ?? "", to: 32 * 1024), toolStatus: "approval", toolCallID: call["id"]?.stringValue))
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
            if let index = transcriptIndices[rowID] {
                let old = projection.transcript[index]
                projection.transcript[index] = TranscriptItem(id: rowID, kind: .tool, owner: owner ?? old.owner, role: "tool", text: toolName, toolName: toolName, toolArguments: old.toolArguments, toolOutput: bounded(output, to: maximumTextBytes), toolStatus: isError ? "failed" : "complete", toolCallID: old.toolCallID ?? (toolCallID.isEmpty ? nil : toolCallID), turnID: old.turnID, userInputState: old.userInputState)
            } else {
                appendTranscript(TranscriptItem(id: rowID, kind: .tool, owner: owner, role: "tool", text: toolName, toolName: toolName, toolOutput: bounded(output, to: maximumTextBytes), toolStatus: isError ? "failed" : "complete", toolCallID: toolCallID.isEmpty ? nil : toolCallID))
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
            appendTranscript(TranscriptItem(id: eventID, kind: .activity, owner: owner, text: bounded(text("summary"), to: maximumTextBytes)))
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
            appendTranscript(TranscriptItem(id: eventID, kind: .error, owner: owner, text: bounded(text("error"), to: maximumTextBytes)))
        default: break
        }
        projection.refreshedAt = .now
        trimTranscript()
    }

    private mutating func appendAssistant(kind: TranscriptItem.Kind, owner: String?, text: String, turnID: String?) {
        guard !text.isEmpty else { return }
        let key = kind == .reasoning ? "reasoning" : "assistant"
        let id = "\(key):\(projection.epoch):\(turnID ?? "unknown"):\(assistantMessageIndex)"
        if let index = transcriptIndices[id] {
            let old = projection.transcript[index]
            projection.transcript[index] = TranscriptItem(id: id, kind: kind, owner: owner ?? old.owner, role: old.role, text: bounded(old.text + text, to: maximumTextBytes), isStreaming: true)
        } else {
            appendTranscript(TranscriptItem(id: id, kind: kind, owner: owner, text: bounded(text, to: maximumTextBytes), isStreaming: true))
        }
        openAssistantIDs.insert(id)
    }

    private mutating func appendTranscript(_ item: TranscriptItem) {
        transcriptIndices[item.id] = projection.transcript.count
        projection.transcript.append(item)
    }

    private func transcriptIndex(turnID: String) -> Int? {
        projection.transcript.firstIndex { $0.turnID == turnID && $0.role == "user" }
    }

    private static func indices(_ transcript: [TranscriptItem]) -> [String: Int] {
        var result: [String: Int] = [:]
        result.reserveCapacity(transcript.count)
        for (index, item) in transcript.enumerated() { result[item.id] = index }
        return result
    }

    private mutating func finishAssistant() {
        guard !openAssistantIDs.isEmpty else { return }
        for id in openAssistantIDs {
            guard let index = transcriptIndices[id] else { continue }
            let item = projection.transcript[index]
            projection.transcript[index] = TranscriptItem(id: item.id, kind: item.kind, owner: item.owner, role: item.role, text: item.text, isStreaming: false, origin: item.origin, toolName: item.toolName, toolArguments: item.toolArguments, toolOutput: item.toolOutput, toolStatus: item.toolStatus, toolCallID: item.toolCallID, turnID: item.turnID, userInputState: item.userInputState, sourcePosition: item.sourcePosition, sourcePartIndex: item.sourcePartIndex, sourceHistoryGeneration: item.sourceHistoryGeneration, child: item.child, attachments: item.attachments, generatedMedia: item.generatedMedia, userMessageIndex: item.userMessageIndex, branchAnchor: item.branchAnchor)
        }
        openAssistantIDs.removeAll(keepingCapacity: true)
    }


    private static func trimToBounds(_ projection: inout OpenSessionProjection) {
        func estimatedBytes(_ item: TranscriptItem) -> Int {
            item.id.utf8.count + item.text.utf8.count + (item.toolArguments?.utf8.count ?? 0) + (item.toolOutput?.utf8.count ?? 0) + (item.owner?.utf8.count ?? 0) + item.attachments.reduce(0) { $0 + $1.name.utf8.count + $1.mimeType.utf8.count + 96 } + 256
        }
        var bytes = projection.transcript.reduce(0) { $0 + estimatedBytes($1) }
        while projection.transcript.count > Self.maximumTranscriptItems || bytes > 8 * 1024 * 1024 {
            guard !projection.transcript.isEmpty else { break }
            bytes -= estimatedBytes(projection.transcript.removeFirst())
            projection.omittedTranscriptItemCount += 1
        }
    }

    private mutating func trimTranscript() {
        let tail = projection.transcriptWindow.historyGeneration != nil
        func estimatedBytes(_ item: TranscriptItem) -> Int {
            item.id.utf8.count + item.text.utf8.count + (item.owner?.utf8.count ?? 0) + (item.role?.utf8.count ?? 0) +
            (item.origin?.utf8.count ?? 0) + (item.toolName?.utf8.count ?? 0) + (item.toolArguments?.utf8.count ?? 0) +
            (item.toolOutput?.utf8.count ?? 0) + (item.toolStatus?.utf8.count ?? 0) + (item.turnID?.utf8.count ?? 0) +
            item.attachments.reduce(0) { $0 + $1.id.utf8.count + $1.name.utf8.count + $1.mimeType.utf8.count + 64 } +
            item.generatedMedia.reduce(0) { $0 + $1.id.utf8.count + $1.name.utf8.count + $1.path.utf8.count + $1.mimeType.utf8.count + 96 } + 256
        }
        var bytes = tail ? projection.transcript.reduce(0) { $0 + estimatedBytes($1) } : 0
        var removed = 0
        while projection.transcript.count - removed > Self.maximumTranscriptItems || (tail && bytes > 8 * 1024 * 1024) {
            guard removed < projection.transcript.count else { break }
            if tail { bytes -= estimatedBytes(projection.transcript[removed]) }
            removed += 1
        }
        guard removed > 0 else { return }
        projection.transcript.removeFirst(removed)
        projection.omittedTranscriptItemCount += removed
        if tail { projection.transcriptWindow.isComplete = false }
        transcriptIndices = Self.indices(projection.transcript)
        openAssistantIDs = Set(openAssistantIDs.filter { transcriptIndices[$0] != nil })
    }

    private mutating func invalidateBranchAnchors() {
        projection.transcript = projection.transcript.map { item in
            guard item.branchAnchor != nil else { return item }
            return TranscriptItem(id: item.id, kind: item.kind, owner: item.owner, role: item.role, text: item.text,
                                  isStreaming: item.isStreaming, origin: item.origin, toolName: item.toolName,
                                  toolArguments: item.toolArguments, toolOutput: item.toolOutput, toolStatus: item.toolStatus, toolCallID: item.toolCallID,
                                  turnID: item.turnID, userInputState: item.userInputState, sourcePosition: item.sourcePosition, sourcePartIndex: item.sourcePartIndex, sourceHistoryGeneration: item.sourceHistoryGeneration, child: item.child, attachments: item.attachments, generatedMedia: item.generatedMedia,
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
        var oldByTurnID: [String: TranscriptItem] = [:]
        for item in old where item.role == "user" {
            if let turnID = item.turnID, !turnID.isEmpty { oldByTurnID[turnID] = item }
        }
        var oldByKey: [String: [TranscriptItem]] = [:]
        for item in old { oldByKey[fingerprint(item), default: []].append(item) }
        return new.map { item in
            let turnMatch = item.role == "user" ? item.turnID.flatMap { oldByTurnID[$0] } : nil
            let key = fingerprint(item)
            let old: TranscriptItem?
            if let turnMatch {
                old = turnMatch
                oldByKey[key]?.removeAll { $0.id == turnMatch.id }
            } else if var matches = oldByKey[key], !matches.isEmpty {
                old = matches.removeFirst()
                oldByKey[key] = matches
            } else {
                old = nil
            }
            guard let old else { return item }
            return TranscriptItem(id: old.id, kind: item.kind, owner: item.owner ?? old.owner, role: item.role, text: item.text, isStreaming: item.isStreaming, origin: item.origin ?? old.origin, toolName: item.toolName, toolArguments: item.toolArguments, toolOutput: item.toolOutput, toolStatus: item.toolStatus, toolCallID: item.toolCallID, turnID: old.turnID ?? item.turnID, userInputState: turnMatch != nil ? item.userInputState : (item.userInputState ?? old.userInputState), sourcePosition: item.sourcePosition ?? old.sourcePosition, sourcePartIndex: item.sourcePartIndex ?? old.sourcePartIndex, sourceHistoryGeneration: item.sourceHistoryGeneration ?? old.sourceHistoryGeneration, child: item.child, attachments: item.attachments.isEmpty ? old.attachments : item.attachments, generatedMedia: item.generatedMedia, userMessageIndex: item.userMessageIndex ?? old.userMessageIndex, branchAnchor: item.branchAnchor ?? old.branchAnchor)
        }
    }
    private static func fingerprint(_ item: TranscriptItem) -> String { "\(item.kind.rawValue)|\(item.owner ?? "")|\(item.role ?? "")|\(item.text)" }
}
