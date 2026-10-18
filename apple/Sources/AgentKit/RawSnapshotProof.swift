import CryptoKit
import Foundation

public enum RawSnapshotProofError: Error, Equatable, Sendable {
    case tooLarge
    case invalidJSON
    case unsupportedDepth
    case duplicateKey
    case missingSnapshot
    case missingMessages
    case nullMessages
    case nonCanonicalMessages
    case invalidIdentity
    case invalidCursor
    case invalidEpoch
}

public struct RawSnapshotItemProof: Sendable, Equatable {
    /// Zero-based index in the complete top-level `session.messages` array.
    public let position: Int
    /// Exact bytes of this element, sliced from `messagesJSON` without re-encoding.
    public let rawJSON: Data
    public let sha256: String
}

public struct RawSnapshotProof: Sendable, Equatable {
    public static let maximumSnapshotBytes = 16 * 1024 * 1024
    public let sessionID: String
    public let epoch: String
    public let cursor: UInt64
    /// Exact JSON bytes of the complete `session.messages` array.
    public let messagesJSON: Data
    /// Lowercase SHA-256 of `messagesJSON`; Go's SnapshotProof hashes json.Marshal(MessagesSnapshot()).
    public let messagesSHA256: String
    public let items: [RawSnapshotItemProof]

    /// Extracts a proof from either GET /snapshot JSON or a complete SSE `snapshot` frame.
    /// The complete message array is retained byte-for-byte and never reconstructed from wire DTOs.
    public static func extract(from rawJSON: Data, expectedSessionID: String) throws -> RawSnapshotProof {
        guard rawJSON.count <= maximumSnapshotBytes else { throw RawSnapshotProofError.tooLarge }
        guard !expectedSessionID.isEmpty else { throw RawSnapshotProofError.invalidIdentity }
        var scanner = BoundedJSONScanner(data: rawJSON)
        let root = try scanner.parseDocument()
        let snapshotNode: BoundedJSONScanner.Node
        if let snapshot = root.object?["snapshot"] {
            guard root.object?["type"]?.string == "snapshot" else { throw RawSnapshotProofError.missingSnapshot }
            snapshotNode = snapshot
        } else {
            snapshotNode = root
        }
        guard let snapshotObject = snapshotNode.object,
              let sessionObject = snapshotObject["session"]?.object,
              let statusObject = snapshotObject["status"]?.object else { throw RawSnapshotProofError.missingSnapshot }
        guard sessionObject["id"]?.string == expectedSessionID,
              statusObject["session_id"]?.string == expectedSessionID else { throw RawSnapshotProofError.invalidIdentity }
        guard let epoch = snapshotObject["epoch"]?.string, !epoch.isEmpty else { throw RawSnapshotProofError.invalidEpoch }
        let cursor: UInt64
        if let cursorNode = snapshotObject["cursor"] {
            guard let raw = cursorNode.rawString, let value = UInt64(raw) else { throw RawSnapshotProofError.invalidCursor }
            cursor = value
        } else { cursor = 0 }
        guard let messages = sessionObject["messages"] else { throw RawSnapshotProofError.missingMessages }
        guard messages.array != nil else {
            if messages.isNull { throw RawSnapshotProofError.nullMessages }
            throw RawSnapshotProofError.missingMessages
        }
        let rawArray = Data(rawJSON[messages.range])
        guard isCompactJSON(rawArray) else { throw RawSnapshotProofError.nonCanonicalMessages }
        let items = try messages.array!.enumerated().map { position, node in
            let bytes = Data(rawJSON[node.range])
            guard !bytes.isEmpty else { throw RawSnapshotProofError.invalidJSON }
            return RawSnapshotItemProof(position: position, rawJSON: bytes, sha256: digest(bytes))
        }
        return RawSnapshotProof(sessionID: expectedSessionID, epoch: epoch, cursor: cursor,
                                messagesJSON: rawArray, messagesSHA256: digest(rawArray), items: items)
    }

    private static func digest(_ data: Data) -> String {
        SHA256.hash(data: data).map { String(format: "%02x", $0) }.joined()
    }

    private static func isCompactJSON(_ data: Data) -> Bool {
        var inString = false
        var escaped = false
        for byte in data {
            if inString {
                if escaped { escaped = false }
                else if byte == 0x5C { escaped = true }
                else if byte == 0x22 { inString = false }
            } else if byte == 0x22 { inString = true }
            else if byte == 0x20 || byte == 0x09 || byte == 0x0A || byte == 0x0D { return false }
        }
        return !inString && !escaped
    }
}

/// Complete raw snapshot frame bytes and its typed view, for stream consumers needing both.
public struct RawSnapshotPayload: Sendable {
    public let snapshot: SessionSnapshotWire
    public let rawJSON: Data
    public init(snapshot: SessionSnapshotWire, rawJSON: Data) { self.snapshot = snapshot; self.rawJSON = rawJSON }
}

private struct BoundedJSONScanner {
    struct Node {
        enum Kind { case object, array, string, number, bool, null }
        let kind: Kind
        let range: Range<Int>
        let object: [String: Node]?
        let array: [Node]?
        let string: String?
        let rawString: String?
        var isNull: Bool { kind == .null }
    }

    let data: Data
    var offset = 0
    var depth = 0
    var nodes = 0
    static let maximumDepth = 128
    static let maximumNodes = 500_000

    mutating func parseDocument() throws -> Node {
        skipWhitespace()
        let node = try parseValue()
        skipWhitespace()
        guard offset == data.count else { throw RawSnapshotProofError.invalidJSON }
        return node
    }

    mutating func parseValue() throws -> Node {
        nodes += 1
        guard nodes <= Self.maximumNodes else { throw RawSnapshotProofError.tooLarge }
        skipWhitespace()
        guard offset < data.count else { throw RawSnapshotProofError.invalidJSON }
        let start = offset
        switch data[offset] {
        case 0x7B:
            depth += 1; guard depth <= Self.maximumDepth else { throw RawSnapshotProofError.unsupportedDepth }
            offset += 1; skipWhitespace()
            var fields: [String: Node] = [:]
            if consume(0x7D) { depth -= 1; return Node(kind: .object, range: start..<offset, object: fields, array: nil, string: nil, rawString: nil) }
            while true {
                skipWhitespace()
                guard offset < data.count, data[offset] == 0x22 else { throw RawSnapshotProofError.invalidJSON }
                let key = try parseString().value
                guard fields[key] == nil else { throw RawSnapshotProofError.duplicateKey }
                skipWhitespace(); guard consume(0x3A) else { throw RawSnapshotProofError.invalidJSON }
                fields[key] = try parseValue()
                skipWhitespace()
                if consume(0x7D) { break }
                guard consume(0x2C) else { throw RawSnapshotProofError.invalidJSON }
            }
            depth -= 1
            return Node(kind: .object, range: start..<offset, object: fields, array: nil, string: nil, rawString: nil)
        case 0x5B:
            depth += 1; guard depth <= Self.maximumDepth else { throw RawSnapshotProofError.unsupportedDepth }
            offset += 1; skipWhitespace()
            var values: [Node] = []
            if consume(0x5D) { depth -= 1; return Node(kind: .array, range: start..<offset, object: nil, array: values, string: nil, rawString: nil) }
            while true {
                values.append(try parseValue())
                skipWhitespace()
                if consume(0x5D) { break }
                guard consume(0x2C) else { throw RawSnapshotProofError.invalidJSON }
            }
            depth -= 1
            return Node(kind: .array, range: start..<offset, object: nil, array: values, string: nil, rawString: nil)
        case 0x22:
            let parsed = try parseString()
            return Node(kind: .string, range: start..<offset, object: nil, array: nil, string: parsed.value, rawString: nil)
        case 0x74: try literal("true"); return Node(kind: .bool, range: start..<offset, object: nil, array: nil, string: nil, rawString: nil)
        case 0x66: try literal("false"); return Node(kind: .bool, range: start..<offset, object: nil, array: nil, string: nil, rawString: nil)
        case 0x6E: try literal("null"); return Node(kind: .null, range: start..<offset, object: nil, array: nil, string: nil, rawString: nil)
        case 0x2D, 0x30...0x39:
            try parseNumber()
            let raw = String(decoding: data[start..<offset], as: UTF8.self)
            return Node(kind: .number, range: start..<offset, object: nil, array: nil, string: nil, rawString: raw)
        default: throw RawSnapshotProofError.invalidJSON
        }
    }

    private mutating func parseString() throws -> (value: String, range: Range<Int>) {
        let start = offset
        guard consume(0x22) else { throw RawSnapshotProofError.invalidJSON }
        var escaped = false
        while offset < data.count {
            let byte = data[offset]; offset += 1
            if escaped { escaped = false; continue }
            if byte == 0x5C { escaped = true; continue }
            if byte == 0x22 {
                let range = start..<offset
                guard let value = try? JSONDecoder().decode(String.self, from: Data(data[range])) else { throw RawSnapshotProofError.invalidJSON }
                return (value, range)
            }
            if byte < 0x20 { throw RawSnapshotProofError.invalidJSON }
        }
        throw RawSnapshotProofError.invalidJSON
    }

    private mutating func parseNumber() throws {
        if consume(0x2D), offset >= data.count { throw RawSnapshotProofError.invalidJSON }
        if consume(0x30) { if offset < data.count, (0x30...0x39).contains(data[offset]) { throw RawSnapshotProofError.invalidJSON } }
        else { try digits(required: true) }
        if consume(0x2E) { try digits(required: true) }
        if consume(0x65) || consume(0x45) {
            if !consume(0x2B) { _ = consume(0x2D) }
            try digits(required: true)
        }
    }
    private mutating func digits(required: Bool) throws {
        let start = offset
        while offset < data.count, (0x30...0x39).contains(data[offset]) { offset += 1 }
        if required && start == offset { throw RawSnapshotProofError.invalidJSON }
    }
    private mutating func literal(_ value: String) throws {
        let bytes = Array(value.utf8)
        guard offset + bytes.count <= data.count, Array(data[offset..<(offset + bytes.count)]) == bytes else { throw RawSnapshotProofError.invalidJSON }
        offset += bytes.count
    }
    private mutating func skipWhitespace() { while offset < data.count, [0x20, 0x09, 0x0A, 0x0D].contains(data[offset]) { offset += 1 } }
    private mutating func consume(_ byte: UInt8) -> Bool {
        guard offset < data.count, data[offset] == byte else { return false }
        offset += 1; return true
    }
}
