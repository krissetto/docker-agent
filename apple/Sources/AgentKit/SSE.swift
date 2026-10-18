import Foundation

public struct SSEFrame: Equatable, Sendable {
    public let event: String?
    public let data: String
    public let id: String?
    public let retry: Int?

    public init(event: String? = nil, data: String, id: String? = nil, retry: Int? = nil) {
        self.event = event
        self.data = data
        self.id = id
        self.retry = retry
    }
}

public enum SSEParserError: Error, Equatable, Sendable {
    case lineTooLarge
    case frameTooLarge
    case invalidUTF8
    case unfinishedLine
}

/// Incremental, byte-bounded SSE parser. It retains only the current line and frame.
public struct SSEParser: Sendable {
    public static let defaultMaximumLineBytes = 128 * 1024
    public static let defaultMaximumFrameBytes = 128 * 1024

    private let maximumLineBytes: Int
    private let maximumFrameBytes: Int
    private var line: [UInt8] = []
    private var frameBytes = 0
    private var dataLines: [String] = []
    private var dataFieldCount = 0
    private var eventName: String?
    private var lastEventID: String?
    private var retryMilliseconds: Int?
    private var hasFields = false
    private var previousWasCR = false
    private var finished = false

    public init(maximumLineBytes: Int = Self.defaultMaximumLineBytes,
                maximumFrameBytes: Int = Self.defaultMaximumFrameBytes) {
        self.maximumLineBytes = max(1, maximumLineBytes)
        self.maximumFrameBytes = max(1, maximumFrameBytes)
    }

    public mutating func append(_ bytes: Data) throws -> [SSEFrame] {
        guard !finished else { return [] }
        var frames: [SSEFrame] = []
        for byte in bytes {
            if previousWasCR {
                previousWasCR = false
                if byte == 0x0A { continue }
            }
            if byte == 0x0D {
                try consumeLine(into: &frames)
                previousWasCR = true
            } else if byte == 0x0A {
                try consumeLine(into: &frames)
            } else {
                guard line.count < maximumLineBytes else { throw SSEParserError.lineTooLarge }
                line.append(byte)
            }
        }
        return frames
    }

    public mutating func finish() throws -> [SSEFrame] {
        guard !finished else { return [] }
        finished = true
        var frames: [SSEFrame] = []
        if !line.isEmpty { try consumeLine(into: &frames) }
        try dispatch(into: &frames)
        return frames
    }

    private mutating func consumeLine(into frames: inout [SSEFrame]) throws {
        defer { line.removeAll(keepingCapacity: true) }
        guard let text = String(bytes: line, encoding: .utf8) else { throw SSEParserError.invalidUTF8 }
        guard !text.isEmpty else {
            try dispatch(into: &frames)
            return
        }
        guard !text.hasPrefix(":") else { return }

        let field: Substring
        let value: Substring
        if let separator = text.firstIndex(of: ":") {
            field = text[..<separator]
            var start = text.index(after: separator)
            if start < text.endIndex, text[start] == " " { start = text.index(after: start) }
            value = text[start...]
        } else {
            field = text[...]
            value = ""
        }

        switch field {
        case "data":
            let newSize = frameBytes + value.utf8.count + (dataFieldCount == 0 ? 0 : 1)
            guard newSize <= maximumFrameBytes else { throw SSEParserError.frameTooLarge }
            frameBytes = newSize
            dataFieldCount += 1
            dataLines.append(String(value))
            hasFields = true
        case "event":
            guard frameBytes + value.utf8.count <= maximumFrameBytes else { throw SSEParserError.frameTooLarge }
            frameBytes += value.utf8.count
            eventName = String(value)
            hasFields = true
        case "id":
            if !value.contains("\0") {
                guard frameBytes + value.utf8.count <= maximumFrameBytes else { throw SSEParserError.frameTooLarge }
                frameBytes += value.utf8.count
                lastEventID = String(value)
                hasFields = true
            }
        case "retry":
            if !value.isEmpty, value.utf8.allSatisfy({ (0x30...0x39).contains($0) }), let retry = Int(value) {
                retryMilliseconds = retry
                hasFields = true
            }
        default:
            break
        }
    }

    private mutating func dispatch(into frames: inout [SSEFrame]) throws {
        guard hasFields else { return }
        if dataFieldCount > 0 {
            let data = dataLines.joined(separator: "\n")
            guard data.utf8.count <= maximumFrameBytes else { throw SSEParserError.frameTooLarge }
            frames.append(SSEFrame(event: eventName, data: data, id: lastEventID, retry: retryMilliseconds))
        }
        dataLines.removeAll(keepingCapacity: true)
        dataFieldCount = 0
        eventName = nil
        frameBytes = 0
        hasFields = false
    }
}

public struct StatusWire: Decodable, Sendable {
    public let interruptedTurns: Int?
    public let sessionID: String?
    public let agentName: String?
    public let state: String?
    public let pending: Int?
    public let turnID: String?
    public let lastError: String?
    public let dormant: Bool?
    public let pauseArmed: Bool?
    public let paused: Bool?
    public let pauseGeneration: UInt64?
    enum CodingKeys: String, CodingKey {
        case interruptedTurns = "interrupted_turns", sessionID = "session_id", agentName = "agent_name", state, pending
        case turnID = "turn_id", lastError = "last_error", dormant, pauseArmed = "pause_armed", paused
        case pauseGeneration = "pause_generation"
    }
}

public struct InteractionWire: Decodable, Sendable {
    public let sessionID: String?
    public let interactionID: String
    public let kind: String
    public let elicitationID: String?
    public let event: [String: JSONValue]?
    enum CodingKeys: String, CodingKey {
        case sessionID = "session_id", interactionID = "interaction_id", kind
        case elicitationID = "elicitation_id", event
    }
}

public struct PendingInputWire: Decodable, Sendable {
    public let inputOrigin: String?
    public let senderID: String?
    public let senderName: String?
    public let reportOutcome: String?
    public let inputMode: String?
    public let turnID: String
    public let content: String
    public let multiContent: [JSONValue]?
    public let sessionPosition: Int?
    enum CodingKeys: String, CodingKey {
        case inputOrigin = "input_origin", senderID = "sender_id", senderName = "sender_name"
        case reportOutcome = "report_outcome", inputMode = "input_mode", turnID = "turn_id"
        case content, multiContent = "multi_content", sessionPosition = "session_position"
    }
}

public struct StreamEnvelope: Decodable, Sendable {
    public let version: Int
    public let sessionID: String
    public let turnID: String?
    public let interactionID: String?
    public let epoch: String?
    public let sequence: UInt64
    public let transcriptPosition: Int
    public let event: [String: JSONValue]?
    public let gap: Bool?
    public let firstAvailable: UInt64?
    enum CodingKeys: String, CodingKey {
        case version, sessionID = "session_id", turnID = "turn_id", interactionID = "interaction_id"
        case epoch, sequence, transcriptPosition = "transcript_position", event, gap
        case firstAvailable = "first_available"
    }
    public init(version: Int = 2, sessionID: String, turnID: String? = nil, interactionID: String? = nil,
                epoch: String? = nil, sequence: UInt64, transcriptPosition: Int, event: [String: JSONValue]? = nil,
                gap: Bool? = nil, firstAvailable: UInt64? = nil) {
        self.version = version; self.sessionID = sessionID; self.turnID = turnID; self.interactionID = interactionID
        self.epoch = epoch; self.sequence = sequence; self.transcriptPosition = transcriptPosition
        self.event = event; self.gap = gap; self.firstAvailable = firstAvailable
    }
}

public struct StreamMessage: Decodable, Sendable {
    public let version: Int?
    public let type: String
    public let snapshot: SessionSnapshotWire?
    public let envelope: StreamEnvelope?
    public let cursor: UInt64?
    public let chunk: Data?
    enum CodingKeys: String, CodingKey { case version, type, snapshot, envelope, cursor, chunk }
    public init(version: Int? = nil, type: String, snapshot: SessionSnapshotWire? = nil, envelope: StreamEnvelope? = nil,
                cursor: UInt64? = nil, chunk: Data? = nil) {
        self.version = version; self.type = type; self.snapshot = snapshot; self.envelope = envelope
        self.cursor = cursor; self.chunk = chunk
    }
}

public enum SnapshotAssemblyError: Error, Equatable, Sendable {
    case unexpectedMessage(String)
    case nestedBegin
    case missingBegin
    case cursorMismatch
    case emptyChunk
    case chunkTooLarge
    case snapshotTooLarge
    case invalidSnapshot
    case incompleteSnapshot
}

/// Reassembles the exact contiguous base64 chunks emitted for large session snapshots.
public struct SnapshotAssembler: Sendable {
    public static let maximumChunkBytes = 64 * 1024
    public static let defaultMaximumSnapshotBytes = 16 * 1024 * 1024

    private let maximumSnapshotBytes: Int
    private var assemblingCursor: UInt64?
    private var bytes = Data()
    private var completedRawSnapshot: Data?

    public init(maximumSnapshotBytes: Int = Self.defaultMaximumSnapshotBytes) {
        self.maximumSnapshotBytes = max(1, maximumSnapshotBytes)
    }

    public var isAssembling: Bool { assemblingCursor != nil }

    /// Accepts a raw decoded SSE `data:` payload and preserves the exact JSON bytes
    /// for complete snapshots. The typed overload remains available to projections.
    public mutating func acceptRawJSON(_ frame: Data) throws -> RawSnapshotPayload? {
        guard frame.count <= maximumSnapshotBytes + Self.maximumChunkBytes else { reset(); throw SnapshotAssemblyError.snapshotTooLarge }
        guard let message = try? JSONDecoder().decode(StreamMessage.self, from: frame) else { throw SnapshotAssemblyError.invalidSnapshot }
        if message.type == "snapshot" {
            guard assemblingCursor == nil else { reset(); throw SnapshotAssemblyError.unexpectedMessage(message.type) }
            guard let snapshot = message.snapshot else { throw SnapshotAssemblyError.invalidSnapshot }
            completedRawSnapshot = nil
            _ = try accept(message)
            return RawSnapshotPayload(snapshot: snapshot, rawJSON: frame)
        }
        let snapshot = try accept(message)
        guard message.type == "snapshot_end", let snapshot else { return nil }
        guard let raw = completedRawSnapshot else { throw SnapshotAssemblyError.invalidSnapshot }
        completedRawSnapshot = nil
        return RawSnapshotPayload(snapshot: snapshot, rawJSON: raw)
    }

    public mutating func accept(_ message: StreamMessage) throws -> SessionSnapshotWire? {
        switch message.type {
        case "snapshot":
            guard assemblingCursor == nil else { reset(); throw SnapshotAssemblyError.unexpectedMessage(message.type) }
            guard let snapshot = message.snapshot else { throw SnapshotAssemblyError.invalidSnapshot }
            return snapshot
        case "snapshot_begin":
            guard assemblingCursor == nil else { reset(); throw SnapshotAssemblyError.nestedBegin }
            let cursor = message.cursor ?? 0
            assemblingCursor = cursor
            bytes.removeAll(keepingCapacity: true)
            completedRawSnapshot = nil
            return nil
        case "snapshot_chunk":
            guard let cursor = assemblingCursor else { throw SnapshotAssemblyError.missingBegin }
            guard cursor == (message.cursor ?? 0) else { reset(); throw SnapshotAssemblyError.cursorMismatch }
            guard let chunk = message.chunk, !chunk.isEmpty else { reset(); throw SnapshotAssemblyError.emptyChunk }
            guard chunk.count <= Self.maximumChunkBytes else { reset(); throw SnapshotAssemblyError.chunkTooLarge }
            guard bytes.count <= maximumSnapshotBytes - chunk.count else { reset(); throw SnapshotAssemblyError.snapshotTooLarge }
            bytes.append(chunk)
            return nil
        case "snapshot_end":
            guard let cursor = assemblingCursor else { throw SnapshotAssemblyError.missingBegin }
            guard cursor == (message.cursor ?? 0) else { reset(); throw SnapshotAssemblyError.cursorMismatch }
            let completed = bytes
            reset()
            completedRawSnapshot = completed
            guard let snapshot = try? JSONDecoder().decode(SessionSnapshotWire.self, from: completed) else {
                completedRawSnapshot = nil
                throw SnapshotAssemblyError.invalidSnapshot
            }
            guard (snapshot.cursor ?? 0) == cursor else { completedRawSnapshot = nil; throw SnapshotAssemblyError.cursorMismatch }
            return snapshot
        default:
            if assemblingCursor != nil { reset(); throw SnapshotAssemblyError.unexpectedMessage(message.type) }
            return nil
        }
    }

    public mutating func finish() throws {
        guard assemblingCursor == nil else { reset(); throw SnapshotAssemblyError.incompleteSnapshot }
    }

    private mutating func reset() {
        assemblingCursor = nil
        bytes.removeAll(keepingCapacity: true)
        completedRawSnapshot = nil
    }
}

public enum CursorDecision: Sendable {
    case ready
    case snapshot(SessionSnapshotWire)
    case apply(StreamEnvelope)
    case seed(StreamEnvelope)
    case duplicate
    case resnapshotRequired(String)
    case ignored
}

/// Validates the epoch/sequence stream independently of transport and projection.
public struct SessionEventCursor: Sendable {
    public private(set) var epoch: String?
    public private(set) var sequence: UInt64 = 0
    public private(set) var isReady = false
    public private(set) var requiresSnapshot = false
    private let expectedSessionID: String?

    public init(expectedSessionID: String? = nil, epoch: String? = nil, sequence: UInt64 = 0, isReady: Bool = false) {
        self.expectedSessionID = expectedSessionID
        self.epoch = epoch
        self.sequence = sequence
        self.isReady = isReady
    }

    public mutating func accept(snapshot: SessionSnapshotWire) -> CursorDecision {
        guard let snapshotEpoch = snapshot.epoch, !snapshotEpoch.isEmpty else {
            requiresSnapshot = true
            return .resnapshotRequired("Snapshot is missing its epoch.")
        }
        if let expectedSessionID, let snapshotSessionID = snapshot.status.sessionID,
           snapshotSessionID != expectedSessionID {
            requiresSnapshot = true
            return .resnapshotRequired("Snapshot session identity changed.")
        }
        epoch = snapshotEpoch
        sequence = snapshot.cursor ?? 0
        isReady = false
        requiresSnapshot = false
        return .snapshot(snapshot)
    }

    public mutating func markReady() -> CursorDecision {
        guard !requiresSnapshot, epoch != nil else { return barrier("A fresh snapshot or replay cursor is required before ready.") }
        isReady = true
        return .ready
    }

    public mutating func accept(type: String, envelope: StreamEnvelope? = nil) -> CursorDecision {
        switch type {
        case "ready": return markReady()
        case "snapshot", "snapshot_begin", "snapshot_chunk", "snapshot_end": return .ignored
        case "event": break
        default: return .ignored
        }
        guard !requiresSnapshot else { return .resnapshotRequired("A fresh snapshot is required.") }
        guard let envelope else { return barrier("Event frame has no envelope.") }
        guard envelope.version == 2 else { return barrier("Unsupported event envelope version.") }
        if let expectedSessionID, envelope.sessionID != expectedSessionID {
            return barrier("Event session identity changed.")
        }
        if envelope.sequence == 0 {
            let allowedSeeds: Set<String> = ["stream_started", "agent_choice_reasoning", "agent_choice", "partial_tool_call", "tool_call", "tool_call_output", "tool_call_response"]
            let eventType = envelope.event?["type"]?.stringValue ?? ""
            let eventSessionID = envelope.event?["session_id"]?.stringValue
            let toolCall = envelope.event?["tool_call"]?.objectValue
            let toolCallID = toolCall?["id"]?.stringValue
            let outputCallID = envelope.event?["tool_call_id"]?.stringValue
            let output = envelope.event?["output"]?.stringValue
            let enoughEventIdentity: Bool
            switch eventType {
            case "stream_started": enoughEventIdentity = eventSessionID == envelope.sessionID
            case "agent_choice", "agent_choice_reasoning":
                enoughEventIdentity = eventSessionID == envelope.sessionID && envelope.event?["content"]?.stringValue?.isEmpty == false
            case "partial_tool_call", "tool_call": enoughEventIdentity = toolCallID?.isEmpty == false
            case "tool_call_output": enoughEventIdentity = outputCallID?.isEmpty == false && output?.isEmpty == false
            case "tool_call_response": enoughEventIdentity = outputCallID?.isEmpty == false
            default: enoughEventIdentity = false
            }
            guard !isReady, envelope.turnID == nil, envelope.interactionID == nil,
                  envelope.transcriptPosition == -1, envelope.gap != true,
                  envelope.firstAvailable == nil || envelope.firstAvailable == 0,
                  allowedSeeds.contains(eventType), enoughEventIdentity else {
                return barrier("Invalid sequence-zero live seed.")
            }
            guard let currentEpoch = epoch, currentEpoch == envelope.epoch else {
                return barrier("Live seed epoch changed.")
            }
            return .seed(envelope)
        }
        guard let currentEpoch = epoch, let eventEpoch = envelope.epoch, !eventEpoch.isEmpty else {
            return barrier("Sequenced event arrived before an epoch snapshot.")
        }
        guard eventEpoch == currentEpoch else { return barrier("Event epoch changed.") }
        guard envelope.gap != true else { return barrier("Event stream reported a replay gap.") }
        if envelope.sequence <= sequence { return .duplicate }
        guard sequence < UInt64.max, envelope.sequence == sequence + 1 else {
            return barrier("Event sequence is out of order.")
        }
        sequence = envelope.sequence
        return .apply(envelope)
    }

    private mutating func barrier(_ reason: String) -> CursorDecision {
        requiresSnapshot = true
        return .resnapshotRequired(reason)
    }
}

// Exact lower-snake-case event-stream envelope DTOs needed by SessionDTO and AppModel.
public struct ToolCallFunctionWire: Decodable, Sendable { public let name: String?; public let arguments: String? }
public struct ToolCallWire: Decodable, Sendable { public let id: String?; public let type: String?; public let function: ToolCallFunctionWire? }

public struct SessionErrorWire: Decodable, Sendable {
    public let message: String?
    public let code: String?
    public let agentName: String?
    public let createdAt: String?
    enum CodingKeys: String, CodingKey { case message, code, agentName = "agent_name", createdAt = "created_at" }
}

