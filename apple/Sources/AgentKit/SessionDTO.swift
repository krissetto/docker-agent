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

private struct TranscriptTail {
    private static let compactionThreshold = 4_096
    private let limit: Int
    private var storage: [TranscriptItem] = []
    private var start = 0
    private(set) var omittedCount = 0

    init(limit: Int) { self.limit = limit }

    mutating func skip(_ count: Int) { omittedCount += count }

    mutating func append(_ item: TranscriptItem) {
        storage.append(item)
        let excess = storage.count - start - limit
        if excess > 0 {
            start += excess
            omittedCount += excess
        }
        if start >= Self.compactionThreshold {
            storage.removeFirst(start)
            start = 0
        }
    }

    var items: [TranscriptItem] { start == storage.count ? [] : Array(storage[start..<storage.count]) }
}

private struct HistoricalToolCallKey: Hashable {
    let owner: String?
    let callID: String
}

private struct HistoricalToolCall {
    let id: String
    let name: String?
    let arguments: String?
}

private struct HistoricalToolCallCandidate {
    var call: HistoricalToolCall
    var result: String?
    var isError: Bool
    var hasResult: Bool
    var isAmbiguous: Bool
}

public struct SessionHistoryPageItemWire: Decodable, Sendable {
    public let position: Int
    public let item: JSONValue
    enum CodingKeys: String, CodingKey { case position, item }

    public func validate(maximumItemBytes: Int) throws {
        guard maximumItemBytes > 0, let object = item.objectValue else {
            throw APIError("Invalid bounded history item.")
        }
        // Bound a conservative estimate before encoding, then check exact item-only JSON bytes.
        guard Self.conservativeSize(.object(object), limit: maximumItemBytes) <= maximumItemBytes else {
            throw APIError("The server returned a history item over the negotiated byte limit.")
        }
        let itemData = try JSONEncoder().encode(JSONValue.object(object))
        guard itemData.count <= maximumItemBytes else { throw APIError("The server returned a history item over the negotiated byte limit.") }
        _ = try decodedItem()
    }

    public func decodedItem() throws -> SessionHistoryItemWire {
        guard var value = item.objectValue else { throw APIError("Invalid bounded history item.") }
        value["position"] = .number(Double(position))
        let data = try JSONEncoder().encode(JSONValue.object(value))
        return try JSONDecoder().decode(SessionHistoryItemWire.self, from: data)
    }

    private static func conservativeSize(_ value: JSONValue, limit: Int) -> Int {
        switch value {
        case .string(let text):
            var size = 2
            for byte in text.utf8 {
                size += (byte == 0x22 || byte == 0x5c || byte < 0x20) ? 6 : 1
                if size > limit { return limit + 1 }
            }
            return size
        case .bool(let bool): return bool ? 4 : 5
        case .number(let number):
            guard number.isFinite, let encoded = try? JSONEncoder().encode(JSONValue.number(number)) else { return limit + 1 }
            return encoded.count
        case .null: return 4
        case .array(let values):
            var size = 2
            for value in values {
                let addition = conservativeSize(value, limit: limit) + 1
                guard addition <= limit - min(size, limit) else { return limit + 1 }
                size += addition
                if size > limit { return limit + 1 }
            }
            return size
        case .object(let values):
            var size = 2
            for (key, value) in values {
                let addition = conservativeSize(.string(key), limit: limit) + 1 + conservativeSize(value, limit: limit) + 1
                guard addition <= limit - min(size, limit) else { return limit + 1 }
                size += addition
                if size > limit { return limit + 1 }
            }
            return size
        }
    }
}

public struct SessionHistoryWindowWire: Decodable, Sendable {
    public let startPosition: Int
    public let endPosition: Int
    public let totalItems: Int
    public let omittedRanges: [SessionHistoryOmittedRangeWire]?
    public let nextCursor: String?
    public let truncated: Bool
    enum CodingKeys: String, CodingKey { case startPosition = "start_position", endPosition = "end_position", totalItems = "total_items", omittedRanges = "omitted_ranges", nextCursor = "next_cursor", truncated }
}

public struct SessionHistoryOmittedRangeWire: Decodable, Sendable {
    public static let knownReasons: Set<String> = ["item_exceeds_item_byte_limit", "item_exceeds_response_byte_limit"]
    public let startPosition: Int
    public let endPosition: Int
    public let reason: String
    enum CodingKeys: String, CodingKey { case startPosition = "start_position", endPosition = "end_position", reason }
    public var hasKnownReason: Bool { Self.knownReasons.contains(reason) }
}


public struct SessionHistoryPageWire: Decodable, Sendable {
    public let projection: String
    public let sessionID: String
    public let epoch: String
    public let cursor: UInt64
    public let transcriptPosition: Int
    public let items: [SessionHistoryPageItemWire]
    public let history: SessionHistoryWindowWire
    enum CodingKeys: String, CodingKey { case projection, sessionID = "session_id", epoch, cursor, transcriptPosition = "transcript_position", items, history }

    public func validateResourceBounds(maxItems: Int, maxItemBytes: Int) throws {
        guard (1...100).contains(maxItems), (1...256 * 1024).contains(maxItemBytes),
              items.count <= maxItems, (history.omittedRanges ?? []).count <= maxItems,
              history.startPosition >= 0, history.endPosition >= history.startPosition,
              history.endPosition - history.startPosition <= maxItems else {
            throw APIError("The server exceeded bounded history scan limits.")
        }
        for item in items { try item.validate(maximumItemBytes: maxItemBytes) }
    }

    public func validate(sessionID expectedSessionID: String, epoch expectedEpoch: String, cursor expectedCursor: UInt64,
                         anchorEndPosition: Int, totalItems: Int, previousStartPosition: Int,
                         maxScannedItems: Int = 50, maxItemBytes: Int = 128 * 1024) throws {
        guard projection == "tail", sessionID == expectedSessionID, epoch == expectedEpoch, cursor == expectedCursor,
              history.endPosition == anchorEndPosition, history.totalItems == totalItems,
              transcriptPosition == anchorEndPosition, history.startPosition >= 0,
              history.startPosition < previousStartPosition,
              previousStartPosition >= history.startPosition,
              previousStartPosition - history.startPosition <= maxScannedItems,
              history.totalItems == anchorEndPosition,
              items.count <= maxScannedItems, (history.omittedRanges ?? []).count <= maxScannedItems,
              (1...100).contains(maxScannedItems), (1...4 * 1024 * 1024).contains(maxItemBytes),
              history.truncated == (history.nextCursor?.isEmpty == false),
              !history.truncated || history.startPosition > 0 else {
            throw APIError("The server returned history for a different transcript window.", statusCode: 409, code: "history_cursor_stale")
        }
        var itemPositions = Set<Int>()
        var last = -1
        for item in items {
            guard item.position >= history.startPosition, item.position < previousStartPosition,
                  item.position < anchorEndPosition, item.position > last else {
                throw APIError("The server returned overlapping or unordered history.")
            }
            try item.validate(maximumItemBytes: maxItemBytes)
            itemPositions.insert(item.position)
            last = item.position
        }
        var ranges: [(Int, Int)] = []
        var previousOmissionEnd = history.startPosition
        for range in history.omittedRanges ?? [] {
            guard range.startPosition >= previousOmissionEnd, range.endPosition > range.startPosition,
                  range.endPosition <= previousStartPosition, range.endPosition <= anchorEndPosition,
                  SessionHistoryOmittedRangeWire.knownReasons.contains(range.reason) else {
                throw APIError("The server returned an invalid omitted history range.")
            }
            if itemPositions.contains(where: { $0 >= range.startPosition && $0 < range.endPosition }) {
                throw APIError("The server returned overlapping item and omitted ranges.")
            }
            ranges.append((range.startPosition, range.endPosition))
            previousOmissionEnd = range.endPosition
        }
        guard Self.coversWindow(start: history.startPosition, end: previousStartPosition,
                                items: itemPositions, omittedRanges: ranges, limit: maxScannedItems) else {
            throw APIError("The server left a gap in bounded history.")
        }
    }

    fileprivate static func coversWindow(start: Int, end: Int, items: Set<Int>,
                                         omittedRanges: [(Int, Int)], limit: Int) -> Bool {
        guard start >= 0, end >= start, end - start <= limit else { return false }
        var position = start
        let sortedItems = items.sorted()
        var itemIndex = 0
        var rangeIndex = 0
        while position < end {
            if itemIndex < sortedItems.count, sortedItems[itemIndex] == position {
                position += 1
                itemIndex += 1
            } else if rangeIndex < omittedRanges.count, omittedRanges[rangeIndex].0 == position {
                position = omittedRanges[rangeIndex].1
                rangeIndex += 1
            } else {
                return false
            }
        }
        return itemIndex == sortedItems.count && rangeIndex == omittedRanges.count
    }

    public func project(ref: SessionRef, current: OpenSessionProjection) throws -> OpenSessionProjection {
        guard sessionID == ref.sessionID, projection == "tail" else { throw SessionAdvancedError.identityMismatch }
        let records = try items.map { try $0.decodedItem() }
        let session = SessionHistoryWire(id: ref.sessionID, title: current.title,
                                         parentID: current.parentSession?.sessionID, source: current.source,
                                         subagentTree: nil, messages: records, inputTokens: current.inputTokens,
                                         outputTokens: current.outputTokens, cost: current.cost, starred: nil)
        let wire = SessionSnapshotWire(parentSessionID: current.parentSession?.sessionID, session: session,
                                       status: StatusWire(interruptedTurns: nil, sessionID: sessionID,
                                                          agentName: current.agentName, state: nil,
                                                          pending: current.pendingInputCount, turnID: current.turnID,
                                                          lastError: current.currentActivity, dormant: nil,
                                                          pauseArmed: nil, paused: nil, pauseGeneration: nil),
                                       interactions: nil, pendingInputs: nil, epoch: epoch, cursor: cursor,
                                       transcriptPosition: transcriptPosition, projection: nil, items: nil, history: nil)
        let projected = wire.project(ref: ref, connection: current.connection)
        let generation = current.transcriptWindow.historyGeneration
        var safeRows = projected.transcript.map { item in
            TranscriptItem(id: item.id, kind: item.kind, owner: item.owner, role: item.role, text: item.text,
                           isStreaming: item.isStreaming, origin: item.origin, toolName: item.toolName,
                           toolArguments: item.toolArguments, toolOutput: item.toolOutput,
                           toolStatus: item.toolStatus, toolCallID: item.toolCallID, turnID: item.turnID,
                           userInputState: item.userInputState, sourcePosition: item.sourcePosition,
                           sourcePartIndex: item.sourcePartIndex, sourceHistoryGeneration: generation,
                           child: item.child, attachments: item.attachments, generatedMedia: item.generatedMedia,
                           userMessageIndex: nil, branchAnchor: nil)
        }
        let ids = Set(safeRows.map(\.id))
        for pending in current.transcript where pending.userInputState == .queued && !ids.contains(pending.id) {
            safeRows.append(pending)
        }
        var result = projected
        result.transcript = safeRows
        result.capabilities = current.capabilities
        result.interactions = current.interactions
        result.state = current.state
        result.turnID = current.turnID
        result.pendingInputCount = current.pendingInputCount
        result.currentActivity = current.currentActivity
        return result
    }
}

public struct SessionSnapshotWire: Decodable, Sendable {
    public let parentSessionID: String?; public let session: SessionHistoryWire?; public let status: StatusWire; public let interactions: [InteractionWire]?; public let pendingInputs: [PendingInputWire]?; public let epoch: String?; public let cursor: UInt64?; public let transcriptPosition: Int?
    public let projection: String?; public let items: [SessionHistoryPageItemWire]?; public let history: SessionHistoryWindowWire?
    enum CodingKeys: String, CodingKey { case parentSessionID = "parent_session_id", session, status, interactions, pendingInputs = "pending_inputs", epoch, cursor, transcriptPosition = "transcript_position", projection, items, history }
    public func validateTail(expectedSessionID: String, maxItems: Int = 50, maxItemBytes: Int = 128 * 1024) throws {
        guard (1...100).contains(maxItems), (1...256 * 1024).contains(maxItemBytes),
              projection == "tail", session?.id == expectedSessionID, status.sessionID == expectedSessionID,
              session?.messages == nil, let epoch, !epoch.isEmpty, cursor != nil, let transcriptPosition,
              transcriptPosition >= 0, let history, history.endPosition == transcriptPosition,
              history.totalItems == transcriptPosition, history.startPosition >= 0,
              history.startPosition <= history.endPosition,
              history.endPosition - history.startPosition <= 100,
              history.truncated == (history.nextCursor?.isEmpty == false) else {
            throw APIError("The server did not provide a valid bounded history snapshot.")
        }
        guard history.endPosition - history.startPosition <= maxItems,
              (items ?? []).count <= maxItems, (history.omittedRanges ?? []).count <= maxItems else {
            throw APIError("The server exceeded bounded history scan limits.")
        }
        var positions = Set<Int>()
        for item in items ?? [] {
            guard item.position >= history.startPosition, item.position < history.endPosition else {
                throw APIError("The server returned an item outside the bounded history window.")
            }
            try item.validate(maximumItemBytes: maxItemBytes)
            positions.insert(item.position)
        }
        var ranges: [(Int, Int)] = []
        var previousOmissionEnd = history.startPosition
        for range in history.omittedRanges ?? [] {
            guard range.startPosition >= previousOmissionEnd, range.endPosition > range.startPosition,
                  range.endPosition <= history.endPosition, SessionHistoryOmittedRangeWire.knownReasons.contains(range.reason),
                  !positions.contains(where: { $0 >= range.startPosition && $0 < range.endPosition }) else {
                throw APIError("The server returned an invalid omitted history range.")
            }
            ranges.append((range.startPosition, range.endPosition))
            previousOmissionEnd = range.endPosition
        }
        guard SessionHistoryPageWire.coversWindow(start: history.startPosition, end: history.endPosition,
                                                  items: positions, omittedRanges: ranges, limit: maxItems) else {
            throw APIError("The server left a gap in bounded history.")
        }
    }

    public func project(ref: SessionRef, connection: ConnectionState, rawProof: RawSnapshotProof? = nil, branchInstanceID: String? = nil) -> OpenSessionProjection {
        if projection == "tail" {
            do { try validateTail(expectedSessionID: ref.sessionID) }
            catch { return OpenSessionProjection(ref: ref, title: "New session", agentName: status.agentName ?? "", connection: connection) }
        }
        let session = self.session; let title = session?.title ?? "New session", agent = status.agentName ?? ""
        let forkOrdinals = session?.topLevelForkOrdinalByItemIndex ?? [:]
        let branchProofMatches = projection != "tail" && rawProof != nil && branchInstanceID?.isEmpty == false && rawProof?.sessionID == session?.id && rawProof?.sessionID == status.sessionID && rawProof?.epoch == (epoch ?? "") && rawProof?.cursor == (cursor ?? 0)
        let branchItems = branchProofMatches ? Dictionary(uniqueKeysWithValues: rawProof!.items.map { ($0.position, $0) }) : [:]
        func branchAnchor(_ index: Int) -> SessionBranchAnchor? {
            guard let proof = rawProof, branchProofMatches, let item = branchItems[index] else { return nil }
            guard let branchInstanceID else { return nil }
            return SessionBranchAnchor(ref: ref, instanceID: branchInstanceID, epoch: proof.epoch, cursor: proof.cursor,
                                       messagesSHA256: proof.messagesSHA256, itemPosition: index, itemSHA256: item.sha256)
        }
        let messages: [SessionHistoryItemWire]
        if projection == "tail" {
            messages = (try? (items ?? []).map { try $0.decodedItem() }) ?? []
        } else {
            messages = session?.messages ?? []
        }
        let toolCallsByID = Self.historicalToolCalls(messages)
        let retainedMessageStart = max(0, messages.count - SessionEventProjector.maximumTranscriptItems)
        var transcript = TranscriptTail(limit: SessionEventProjector.maximumTranscriptItems)
        for (index, item) in messages.enumerated() {
            if index < retainedMessageStart {
                transcript.skip(Self.transcriptRowCount(item))
                continue
            }
            if let entry = item.message {
                if entry.inputOrigin == "runtime" { continue }
                let wire = entry.message
                let userMessageIndex = wire?.role == "user" ? forkOrdinals[index] : nil
                if let parts = wire?.presentation, !parts.isEmpty {
                    for (partIndex, part) in parts.enumerated() {
                        switch part.type {
                        case "content":
                            let rawParts = wire?.multiContent ?? []
                            let turnID = wire?.role == "user" ? entry.turnID : nil
                            let isQueued = wire?.role == "user" && entry.pending == true && entry.accepted == true
                            let rowID = (isQueued ? turnID.map { "pending:\($0)" } : nil) ?? "snapshot:\(item.position ?? index):part:\(partIndex)"
                            transcript.append(TranscriptItem(id: rowID, kind: .message, owner: entry.senderName ?? entry.agentName, role: wire?.role, text: part.text ?? "", origin: entry.inputOrigin, turnID: turnID, userInputState: isQueued ? .queued : nil, sourcePosition: item.position ?? index, sourcePartIndex: partIndex, attachments: SessionMediaProjection.attachments(from: rawParts), userMessageIndex: userMessageIndex, branchAnchor: branchAnchor(index)))
                        case "reasoning": transcript.append(TranscriptItem(id: "snapshot:\(item.position ?? index):part:\(partIndex)", kind: .reasoning, owner: entry.agentName, text: part.text ?? "", sourcePosition: item.position ?? index, sourcePartIndex: partIndex))
                        case "tool_call":
                            let call = part.tool?.call ?? wire?.toolCalls?.first(where: { $0.id == part.toolCallID })
                            let name = call?.function?.name
                            let arguments = call?.function?.arguments.map { String($0.prefix(32_768)) }
                            let result = part.tool?.result.map { String($0.prefix(65_536)) }
                            let uniquelyLinkedError = call.flatMap { call -> Bool? in
                                guard let id = call.id, !id.isEmpty,
                                      let linked = toolCallsByID[HistoricalToolCallKey(owner: entry.agentName, callID: id)],
                                      linked.count == 1, linked[0].isAmbiguous == false else { return nil }
                                return linked[0].isError
                            } == true
                            let failed = part.tool?.isError == true || uniquelyLinkedError
                            transcript.append(TranscriptItem(id: "snapshot:\(item.position ?? index):part:\(partIndex)", kind: .tool, owner: entry.agentName, role: "assistant", text: name ?? "Tool", toolName: name, toolArguments: arguments, toolOutput: result, toolStatus: failed ? "failed" : "complete", toolCallID: call?.id ?? part.toolCallID, sourcePosition: item.position ?? index, sourcePartIndex: partIndex, branchAnchor: branchAnchor(index)))
                        default: break
                        }
                    }
                } else {
                    let content = wire?.content ?? ""
                    if let reasoning = wire?.reasoningContent, !reasoning.isEmpty { transcript.append(TranscriptItem(id: "snapshot:\(item.position ?? index):reason", kind: .reasoning, owner: entry.agentName, text: reasoning, sourcePosition: item.position ?? index, sourcePartIndex: 0)) }
                    let rawParts = wire?.multiContent ?? []
                    let attachments = SessionMediaProjection.attachments(from: rawParts)
                    let media = Self.generatedMediaReferences(rawParts)
                    if wire?.role != "tool", !content.isEmpty || !attachments.isEmpty || !media.isEmpty {
                        let turnID = wire?.role == "user" ? entry.turnID : nil
                        let isQueued = wire?.role == "user" && entry.pending == true && entry.accepted == true
                        let rowID = (isQueued ? turnID.map { "pending:\($0)" } : nil) ?? "snapshot:\(item.position ?? index):message"
                        transcript.append(TranscriptItem(id: rowID, kind: .message, owner: entry.senderName ?? entry.agentName, role: wire?.role, text: content, origin: entry.inputOrigin, turnID: turnID, userInputState: isQueued ? .queued : nil, sourcePosition: item.position ?? index, sourcePartIndex: (wire?.reasoningContent?.isEmpty == false) ? 1 : 0, attachments: attachments, generatedMedia: media, userMessageIndex: userMessageIndex, branchAnchor: branchAnchor(index)))
                    }
                    for (callIndex, call) in (wire?.toolCalls ?? []).enumerated() {
                        transcript.append(TranscriptItem(id: "snapshot:\(item.position ?? index):tool:\(callIndex)", kind: .tool, owner: entry.agentName, role: "assistant", text: call.function?.name ?? "Tool", toolName: call.function?.name, toolArguments: String((call.function?.arguments ?? "").prefix(32_768)), toolOutput: nil, toolStatus: "complete", toolCallID: call.id, sourcePosition: item.position ?? index, sourcePartIndex: callIndex + ((wire?.reasoningContent?.isEmpty == false ? 1 : 0) + ((!content.isEmpty || !attachments.isEmpty || !media.isEmpty) ? 1 : 0)), branchAnchor: branchAnchor(index)))
                    }
                }
                if wire?.role == "tool" {
                    let linkedCall: HistoricalToolCall?
                    if let callID = wire?.toolCallID, !callID.isEmpty,
                       let candidates = toolCallsByID[HistoricalToolCallKey(owner: entry.agentName, callID: callID)],
                       candidates.count == 1, candidates[0].isAmbiguous == false {
                        linkedCall = candidates[0].call
                    } else {
                        linkedCall = nil
                    }
                    let output = String((wire?.content ?? "").prefix(65_536))
                    transcript.append(TranscriptItem(
                        id: "snapshot:\(item.position ?? index):tool-output", kind: .tool, owner: entry.agentName, role: "tool",
                        text: linkedCall == nil ? "Unlinked tool result" : (linkedCall?.name ?? "Tool result"), toolName: linkedCall?.name,
                        toolArguments: linkedCall?.arguments.map { String($0.prefix(32_768)) }, toolOutput: output,
                        toolStatus: wire?.isError == true ? "failed" : "complete", toolCallID: linkedCall?.id ?? wire?.toolCallID, sourcePosition: item.position ?? index, sourcePartIndex: 0, branchAnchor: branchAnchor(index)
                    ))
                }
            } else if let error = item.error { transcript.append(TranscriptItem(id: "snapshot:\(item.position ?? index):error", kind: .error, owner: error.agentName, text: error.message ?? "Session error", sourcePosition: item.position ?? index, sourcePartIndex: 0, branchAnchor: branchAnchor(index))) }
            else if let summary = item.summary, !summary.isEmpty { transcript.append(TranscriptItem(id: "snapshot:\(item.position ?? index):summary", kind: .activity, text: summary, sourcePosition: item.position ?? index, sourcePartIndex: 0, branchAnchor: branchAnchor(index))) }
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
        let projectedPendingTurns = Set(transcript.items.compactMap { $0.userInputState == .queued ? $0.turnID : nil })
        for pending in pendingInputs ?? [] where pending.inputOrigin != "runtime" && !projectedPendingTurns.contains(pending.turnID) {
            transcript.append(TranscriptItem(id: "pending:\(pending.turnID)", kind: .message, owner: pending.senderName, role: "user", text: pending.content, origin: pending.inputOrigin, turnID: pending.turnID, userInputState: .queued, sourcePosition: pending.sessionPosition, sourcePartIndex: pending.sessionPosition == nil ? nil : 0, attachments: SessionMediaProjection.attachments(from: pending.multiContent ?? [])))
        }
        let state: SessionState
        switch status.state?.lowercased() { case "queued": state = .queued; case "running": state = .running; case "cancelling": state = .cancelling; case "waiting": state = .waiting; case "settled", "idle", "stopped": state = .settled; case "failed", "error": state = .failed; case .none: state = .unknown; default: state = .unknown }
        let parentID = parentSessionID ?? session?.parentID
        let parentSession = parentID.flatMap { !$0.isEmpty && $0 != ref.sessionID ? SessionRef(serverID: ref.serverID, sessionID: $0) : nil }
        if projection == "tail", let history {
            let generation = "\(epoch ?? ""):\(cursor ?? 0):\(history.endPosition):\(history.totalItems)"
            let projected = transcript.items.map { item in
                TranscriptItem(id: item.id, kind: item.kind, owner: item.owner, role: item.role, text: item.text, isStreaming: item.isStreaming, origin: item.origin, toolName: item.toolName, toolArguments: item.toolArguments, toolOutput: item.toolOutput, toolStatus: item.toolStatus, toolCallID: item.toolCallID, turnID: item.turnID, userInputState: item.userInputState, sourcePosition: item.sourcePosition, sourcePartIndex: item.sourcePartIndex, sourceHistoryGeneration: generation, child: item.child, attachments: item.attachments, generatedMedia: item.generatedMedia, userMessageIndex: nil, branchAnchor: nil)
            }
            let ranges = (history.omittedRanges ?? []).map { TranscriptOmittedRange(startPosition: $0.startPosition, endPosition: $0.endPosition, reason: $0.reason) }
            return OpenSessionProjection(ref: ref, parentSession: parentSession, title: title, agentName: agent, source: session?.source, state: state, turnID: status.turnID, connection: connection, refreshedAt: .now, currentActivity: status.lastError, transcript: projected, transcriptWindow: TranscriptWindow(isComplete: true, omittedItemCount: max(0, history.startPosition), olderCursor: history.nextCursor, historyGeneration: generation, anchorEndPosition: history.endPosition, anchorCursor: cursor, startPosition: history.startPosition, totalItemCount: history.totalItems, containsTruncatedItems: !ranges.isEmpty, omittedRanges: ranges), omittedTranscriptItemCount: 0, interactions: interactions, capabilities: .init(), agentTree: Self.agentTree(session?.subagentTree, serverID: ref.serverID), inputTokens: session?.inputTokens ?? 0, outputTokens: session?.outputTokens ?? 0, contextLength: (session?.inputTokens ?? 0) + (session?.outputTokens ?? 0), contextLimit: 0, cost: session?.cost ?? 0, cursor: cursor ?? 0, epoch: epoch ?? "", pendingInputCount: status.pending ?? 0)
        }
        return OpenSessionProjection(ref: ref, parentSession: parentSession, title: title, agentName: agent, source: session?.source, state: state, turnID: status.turnID, connection: connection, refreshedAt: .now, currentActivity: status.lastError, transcript: transcript.items, omittedTranscriptItemCount: transcript.omittedCount, interactions: interactions, capabilities: .init(), agentTree: Self.agentTree(session?.subagentTree, serverID: ref.serverID), inputTokens: session?.inputTokens ?? 0, outputTokens: session?.outputTokens ?? 0, contextLength: (session?.inputTokens ?? 0) + (session?.outputTokens ?? 0), contextLimit: 0, cost: session?.cost ?? 0, cursor: cursor ?? 0, epoch: epoch ?? "", pendingInputCount: status.pending ?? 0)
    }
    /// Build exact-ID, exact-agent links without changing source positions or item identities.
    /// Calls and results are pre-indexed separately so an exact-ID result can precede its call
    /// in legacy history. Duplicate calls/results make an ID ambiguous instead of guessing.
    private static func historicalToolCalls(_ messages: [SessionHistoryItemWire]) -> [HistoricalToolCallKey: [HistoricalToolCallCandidate]] {
        var result: [HistoricalToolCallKey: [HistoricalToolCallCandidate]] = [:]

        for item in messages {
            guard let entry = item.message, entry.inputOrigin != "runtime",
                  let owner = entry.agentName?.trimmingCharacters(in: .whitespacesAndNewlines), !owner.isEmpty,
                  let wire = entry.message, wire.role == "assistant" else { continue }
            var callsInMessage: [String: HistoricalToolCall] = [:]
            var ambiguousIDs: Set<String> = []
            for call in wire.toolCalls ?? [] {
                guard let id = call.id, !id.isEmpty else { continue }
                let candidate = HistoricalToolCall(id: id, name: call.function?.name,
                                                   arguments: call.function?.arguments.map { String($0.prefix(32_768)) })
                if callsInMessage[id] != nil {
                    ambiguousIDs.insert(id)
                } else {
                    callsInMessage[id] = candidate
                }
            }
            for part in wire.presentation ?? [] where part.type == "tool_call" {
                if let displayCall = part.tool?.call, let id = displayCall.id, !id.isEmpty {
                    let candidate = HistoricalToolCall(id: id, name: displayCall.function?.name,
                                                       arguments: displayCall.function?.arguments.map { String($0.prefix(32_768)) })
                    if let existing = callsInMessage[id] {
                        // Go's display-only AssistantTool and provider ToolCalls are two views of one
                        // source call when their exact ID/name/arguments agree.
                        if existing.name != candidate.name || existing.arguments != candidate.arguments { ambiguousIDs.insert(id) }
                    } else {
                        callsInMessage[id] = candidate
                    }
                } else if let id = part.toolCallID, !id.isEmpty, callsInMessage[id] == nil {
                    callsInMessage[id] = HistoricalToolCall(id: id, name: nil, arguments: nil)
                }
            }
            for (id, call) in callsInMessage {
                let key = HistoricalToolCallKey(owner: entry.agentName, callID: id)
                result[key, default: []].append(HistoricalToolCallCandidate(call: call, result: nil, isError: false,
                                                                             hasResult: false, isAmbiguous: ambiguousIDs.contains(id)))
            }
        }

        for item in messages {
            guard let entry = item.message, entry.inputOrigin != "runtime",
                  let owner = entry.agentName?.trimmingCharacters(in: .whitespacesAndNewlines), !owner.isEmpty,
                  let wire = entry.message, wire.role == "tool",
                  let callID = wire.toolCallID, !callID.isEmpty else { continue }
            let key = HistoricalToolCallKey(owner: entry.agentName, callID: callID)
            guard var candidates = result[key], candidates.count == 1, candidates[0].isAmbiguous == false else { continue }
            if candidates[0].hasResult {
                candidates[0].isAmbiguous = true
            } else {
                candidates[0].result = wire.content
                candidates[0].isError = wire.isError == true
                candidates[0].hasResult = true
            }
            result[key] = candidates
        }
        return result
    }

    private static func transcriptRowCount(_ item: SessionHistoryItemWire) -> Int {
        if let entry = item.message {
            guard entry.inputOrigin != "runtime", let wire = entry.message else { return 0 }
            if wire.role == "tool" { return 1 }
            if let presentation = wire.presentation, !presentation.isEmpty {
                return presentation.reduce(into: 0) { count, part in
                    if ["content", "reasoning", "tool_call"].contains(part.type) { count += 1 }
                }
            }
            let reasoning = (wire.reasoningContent?.isEmpty == false) ? 1 : 0
            let hasVisibleContent = wire.role != "tool" && (!(wire.content?.isEmpty ?? true) || !(wire.multiContent?.isEmpty ?? true))
            let message = hasVisibleContent ? 1 : 0
            let toolCalls = wire.toolCalls?.count ?? 0
            let toolOutput = wire.role == "tool" ? 1 : 0
            return reasoning + message + toolCalls + toolOutput
        }
        if item.error != nil { return 1 }
        return item.summary?.isEmpty == false ? 1 : 0
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
    public var generateTitle: Bool
    enum CodingKeys: String, CodingKey { case content, mode, requestID = "request_id", multiContent = "multi_content", generateTitle = "generate_title" }
    public init(content: String, mode: String = "", requestID: String, multiContent: [SessionMessagePart]? = nil, generateTitle: Bool = false) { self.content = content; self.mode = mode; self.requestID = requestID; self.multiContent = multiContent; self.generateTitle = generateTitle }
    public init(payload: SessionSendPayload, mode: String = "", requestID: String, generateTitle: Bool = false) { content = payload.content; self.mode = mode; self.requestID = requestID; multiContent = payload.multiContent.isEmpty ? nil : payload.multiContent; self.generateTitle = generateTitle }
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
public struct SessionHistoryMessageWire: Decodable, Sendable { public let agentName: String?; public let message: ChatMessageWire?; public let inputOrigin: String?; public let turnID: String?; public let pending: Bool?; public let accepted: Bool?; public let senderName: String?; enum CodingKeys: String, CodingKey { case agentName = "agent_name", message, inputOrigin = "input_origin", turnID = "actor_turn_id", pending, accepted = "actor_accepted", senderName = "sender_name" } }
public struct ChatMessageWire: Decodable, Sendable {
    public let role: String?; public let content: String?; public let reasoningContent: String?; public let toolCalls: [ToolCallWire]?; public let toolCallID: String?; public let isError: Bool?; public let presentation: [AssistantPartWire]?; public let multiContent: [JSONValue]?
    enum CodingKeys: String, CodingKey { case role, content, reasoningContent = "reasoning_content", toolCalls = "tool_calls", toolCallID = "tool_call_id", isError = "is_error", presentation, multiContent = "multi_content" }
}
public struct AssistantPartWire: Decodable, Sendable { public let type: String; public let text: String?; public let toolCallID: String?; public let tool: AssistantToolWire?; enum CodingKeys: String, CodingKey { case type, text, toolCallID = "tool_call_id", tool } }
public struct AssistantToolWire: Decodable, Sendable { public let call: ToolCallWire; public let result: String?; public let isError: Bool?; enum CodingKeys: String, CodingKey { case call, result, isError = "is_error" } }

public struct EmptyBody: Encodable, Sendable { public init() {} }

public struct PreparedViewResponse: Decodable, Sendable {
    public let version: Int
    public let view: String
    public let projection: String?
    public let info: PreparedViewInfoWire
    public var confirmation: SessionViewConfirmation? { info.confirmation }
}
public struct PreparedViewInfoWire: Decodable, Sendable {
    public let activeAgentName: String?; public let sessionID: String; public let rootSessionID: String; public let rootWorkingDir: String?; public let rootBinding: SessionBindingWire?; public let session: PreparedSessionHeader?; public let binding: SessionBindingWire; public let workingDir: String; public let attach: AttachInfoWire?
    enum CodingKeys: String, CodingKey { case activeAgentName = "active_agent_name", sessionID = "session_id", rootSessionID = "root_session_id", rootWorkingDir = "root_working_dir", rootBinding = "root_binding", session, binding, workingDir = "working_dir", attach }
    var confirmation: SessionViewConfirmation? {
        guard let session, session.id == sessionID, (session.workingDir ?? "") == workingDir else { return nil }
        return SessionViewConfirmation(sessionID: sessionID, rootSessionID: rootSessionID, rootWorkingDir: rootWorkingDir,
                                       rootBinding: rootBinding, source: session.attributes?["docker-agent.actor.source"] ?? "",
                                       workingDir: workingDir, binding: binding, activeAgentName: activeAgentName ?? "", nodeID: attach?.nodeID)
    }
}
public struct PreparedSessionHeader: Decodable, Sendable {
    public let id: String
    public let workingDir: String?
    public let attributes: [String: String]?
    public let title: String?
    public let origin: String?
    enum CodingKeys: String, CodingKey { case id, workingDir = "working_dir", attributes, title, origin }
}
public struct SessionBindingWire: Codable, Sendable, Equatable { public var agentName: String; public var model: String; public var durability: String; public var parentSessionID: String; enum CodingKeys: String, CodingKey { case agentName = "AgentName", model = "Model", durability = "Durability", parentSessionID = "ParentSessionID" }; public init(agentName: String = "", model: String = "", durability: String = "", parentSessionID: String = "") { self.agentName = agentName; self.model = model; self.durability = durability; self.parentSessionID = parentSessionID } }
public struct AttachInfoWire: Decodable, Sendable { public let nodeID: String?; enum CodingKeys: String, CodingKey { case nodeID = "NodeID" } }
public struct SessionViewConfirmation: Codable, Sendable { public var sessionID: String; public var rootSessionID: String; public var rootWorkingDir: String?; public var rootBinding: SessionBindingWire?; public var source: String; public var workingDir: String; public var binding: SessionBindingWire; public var activeAgentName: String; public var nodeID: String?; enum CodingKeys: String, CodingKey { case sessionID = "session_id", rootSessionID = "root_session_id", rootWorkingDir = "root_working_dir", rootBinding = "root_binding", source, workingDir = "working_dir", binding, activeAgentName = "active_agent_name", nodeID = "node_id" } }
public struct OpenViewRequest: Encodable, Sendable { public let kind: String; public let viewConfirmation: SessionViewConfirmation; enum CodingKeys: String, CodingKey { case kind, viewConfirmation = "view_confirmation" }; public init(kind: String, viewConfirmation: SessionViewConfirmation) { self.kind = kind; self.viewConfirmation = viewConfirmation } }
public struct OpenedViewResponse: Decodable, Sendable { public let version: Int; public let view: String; public let projection: String?; public let metadata: OpenedMetadata? }
public struct OpenedMetadata: Decodable, Sendable { public let capabilities: SessionCapabilities }
