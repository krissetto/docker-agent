import CryptoKit
import Foundation
import UniformTypeIdentifiers

public enum SessionMediaError: Error, LocalizedError, Sendable, Equatable {
    case invalidName
    case invalidMIMEType
    case emptyDocument
    case documentTooLarge
    case invalidDocumentSource
    case emptyPayload
    case requestTooLarge
    case invalidGeneratedMediaReference

    public var errorDescription: String? {
        switch self {
        case .invalidName: "Choose a file with a safe display name."
        case .invalidMIMEType: "The attachment MIME type is invalid."
        case .emptyDocument: "The selected attachment is empty."
        case .documentTooLarge: "The selected attachment is too large for an inline message."
        case .invalidDocumentSource: "A document must contain exactly one inline text or binary source."
        case .emptyPayload: "Enter a message or attach a document before sending."
        case .requestTooLarge: "The encoded message exceeds the 768 KiB client safety limit. Remove attachments or choose smaller files."
        case .invalidGeneratedMediaReference: "The generated media reference is incomplete or unsafe."
        }
    }
}

public struct SessionDocumentSource: Codable, Sendable, Equatable {
    public let inlineText: String?
    public let inlineData: String?
    enum CodingKeys: String, CodingKey { case inlineText = "inline_text", inlineData = "inline_data" }

    public init(inlineText: String) throws {
        guard !inlineText.isEmpty else { throw SessionMediaError.emptyDocument }
        guard inlineText.utf8.count <= SessionSendPayload.maximumDocumentBytes else { throw SessionMediaError.documentTooLarge }
        self.inlineText = inlineText
        inlineData = nil
    }

    public init(inlineData: Data) throws {
        guard !inlineData.isEmpty else { throw SessionMediaError.emptyDocument }
        guard inlineData.count <= SessionSendPayload.maximumDocumentBytes else { throw SessionMediaError.documentTooLarge }
        self.inlineText = nil
        self.inlineData = inlineData.base64EncodedString()
    }

    fileprivate var byteCount: Int {
        inlineText?.utf8.count ?? inlineData.flatMap(SessionMediaProjection.decodedBase64Length) ?? 0
    }
    fileprivate var isValid: Bool {
        let exactlyOneSource = (inlineData == nil) != (inlineText == nil)
        let validBinary = inlineData.map { SessionMediaProjection.decodedBase64Length($0) != nil } ?? true
        return exactlyOneSource && validBinary && byteCount > 0 && byteCount <= SessionSendPayload.maximumDocumentBytes
    }
}

public struct SessionDocument: Codable, Sendable, Equatable {
    public let name: String
    public let mimeType: String
    public let size: Int64
    public let source: SessionDocumentSource
    enum CodingKeys: String, CodingKey { case name, mimeType = "mime_type", size, source }

    fileprivate init(name: String, mimeType: String, source: SessionDocumentSource) {
        self.name = name
        self.mimeType = mimeType
        size = Int64(source.byteCount)
        self.source = source
    }
}

public struct SessionMessagePart: Codable, Sendable, Equatable {
    public let type: String
    public let document: SessionDocument
    public init(document: SessionDocument) { type = "document"; self.document = document }
    fileprivate var isValid: Bool {
        type == "document" && SessionDocumentAttachment.isSafeName(document.name)
            && SessionDocumentAttachment.isSafeMIMEType(document.mimeType)
            && document.size == document.source.byteCount && document.source.isValid
    }
}

/// A locally imported attachment, ready for exact JSON `multi_content` submission.
public struct SessionDocumentAttachment: Sendable, Equatable, Identifiable {
    public let id: UUID
    public let name: String
    public let mimeType: String
    public let byteCount: Int
    public let encodedByteCount: Int
    public let part: SessionMessagePart

    public static let maximumDocumentBytes = SessionSendPayload.maximumDocumentBytes
    public static let maximumNameBytes = 255

    private init(id: UUID = UUID(), name: String, mimeType: String, source: SessionDocumentSource) throws {
        guard Self.isSafeName(name) else { throw SessionMediaError.invalidName }
        guard Self.isSafeMIMEType(mimeType) else { throw SessionMediaError.invalidMIMEType }
        let document = SessionDocument(name: name, mimeType: mimeType, source: source)
        let part = SessionMessagePart(document: document)
        guard part.isValid else { throw SessionMediaError.invalidDocumentSource }
        let encodedCount = try JSONEncoder().encode(part).count
        self.id = id
        self.name = name
        self.mimeType = mimeType
        byteCount = source.byteCount
        encodedByteCount = encodedCount
        self.part = part
    }

    public static func fromData(_ data: Data, name: String, mimeType: String) throws -> Self {
        try Self(name: name, mimeType: mimeType, source: SessionDocumentSource(inlineData: data))
    }

    public static func fromText(_ text: String, name: String, mimeType: String = "text/plain") throws -> Self {
        try Self(name: name, mimeType: mimeType, source: SessionDocumentSource(inlineText: text))
    }

    /// Reads a user-selected local file while its security-scoped URL is accessible.
    public static func fromFile(_ url: URL, mimeType overrideMIMEType: String? = nil) throws -> Self {
        let accessed = url.startAccessingSecurityScopedResource()
        defer { if accessed { url.stopAccessingSecurityScopedResource() } }
        let values = try url.resourceValues(forKeys: [.isRegularFileKey, .nameKey])
        guard values.isRegularFile == true else { throw SessionMediaError.invalidName }
        let fileName = values.name ?? url.lastPathComponent
        let data: Data
        do {
            let handle = try FileHandle(forReadingFrom: url)
            defer { try? handle.close() }
            var bounded = Data()
            while let chunk = try handle.read(upToCount: min(64 * 1024, SessionSendPayload.maximumDocumentBytes + 1 - bounded.count)), !chunk.isEmpty {
                bounded.append(chunk)
                guard bounded.count <= SessionSendPayload.maximumDocumentBytes else { throw SessionMediaError.documentTooLarge }
            }
            guard !bounded.isEmpty else { throw SessionMediaError.emptyDocument }
            data = bounded
        } catch let error as SessionMediaError { throw error }
        catch { throw error }
        let inferred = UTType(filenameExtension: url.pathExtension)?.preferredMIMEType ?? "application/octet-stream"
        return try fromData(data, name: fileName, mimeType: overrideMIMEType ?? inferred)
    }

    public static func isSafeName(_ value: String) -> Bool {
        !value.isEmpty && value.utf8.count <= maximumNameBytes && value != "." && value != ".."
            && !value.contains("/") && !value.contains("\\")
            && value.unicodeScalars.allSatisfy { !CharacterSet.controlCharacters.contains($0) }
    }

    public static func isSafeMIMEType(_ value: String) -> Bool {
        let components = value.split(separator: "/", omittingEmptySubsequences: false)
        guard components.count == 2 else { return false }
        let token = CharacterSet(charactersIn: "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789!#$&^_.+-")
        return components.allSatisfy { !$0.isEmpty && $0.unicodeScalars.allSatisfy(token.contains) }
    }
}

public struct OutgoingAttachment: Codable, Sendable, Equatable, Identifiable {
    public let id: String
    public let name: String
    public let mimeType: String
    public let byteCount: Int
    public let encodedByteCount: Int
    public let part: SessionMessagePart

    public init(_ attachment: SessionDocumentAttachment) {
        id = attachment.id.uuidString
        name = attachment.name
        mimeType = attachment.mimeType
        byteCount = attachment.byteCount
        encodedByteCount = attachment.encodedByteCount
        part = attachment.part
    }

    fileprivate init(part: SessionMessagePart, index: Int) throws {
        guard part.isValid else { throw SessionMediaError.invalidDocumentSource }
        let encoded = try JSONEncoder().encode(part)
        let digest = SHA256.hash(data: Data(String(index).utf8) + encoded)
        id = digest.map { String(format: "%02x", $0) }.joined()
        name = part.document.name
        mimeType = part.document.mimeType
        byteCount = part.document.source.byteCount
        encodedByteCount = encoded.count
        self.part = part
    }
}

/// Safe display-only metadata for an attachment in transcript history.
public struct MessageAttachmentSummary: Codable, Sendable, Equatable, Identifiable {
    public let id: String
    public let name: String
    public let mimeType: String
    public let byteCount: Int?
    enum CodingKeys: String, CodingKey { case id, name, mimeType = "mime_type", byteCount = "byte_count" }
    public init(id: String, name: String, mimeType: String, byteCount: Int? = nil) {
        self.id = id; self.name = name; self.mimeType = mimeType; self.byteCount = byteCount
    }
}

public enum SessionMediaProjection {
    /// Projects bounded metadata from arbitrary JSON wire parts without decoding or retaining binary bodies.
    public static func attachments(from parts: [JSONValue]) -> [MessageAttachmentSummary] {
        parts.enumerated().compactMap { index, part in
            guard let object = part.objectValue else { return nil }
            if let document = object["document"]?.objectValue {
                let name = document["name"]?.stringValue ?? ""
                let mimeType = document["mime_type"]?.stringValue ?? "application/octet-stream"
                guard SessionDocumentAttachment.isSafeName(name), SessionDocumentAttachment.isSafeMIMEType(mimeType) else { return nil }
                let source = document["source"]?.objectValue ?? [:]
                let count: Int?
                if let inlineText = source["inline_text"]?.stringValue { count = inlineText.utf8.count }
                else if let inlineData = source["inline_data"]?.stringValue {
                    guard let decodedCount = decodedBase64Length(inlineData) else { return nil }
                    count = decodedCount
                } else if let rawSize = document["size"]?.numberValue {
                    guard rawSize.isFinite, rawSize >= 0, rawSize <= Double(SessionSendPayload.maximumDocumentBytes) else { return nil }
                    count = Int(rawSize)
                } else { count = nil }
                if let count, count > SessionSendPayload.maximumDocumentBytes { return nil }
                let stableID = SHA256.hash(data: Data("\(index)|\(name)|\(mimeType)|\(count ?? 0)".utf8)).map { String(format: "%02x", $0) }.joined()
                return MessageAttachmentSummary(id: stableID, name: name, mimeType: mimeType, byteCount: count)
            }
            if let file = object["file"]?.objectValue {
                let rawName = file["name"]?.stringValue ?? file["path"]?.stringValue ?? ""
                let name = URL(fileURLWithPath: rawName).lastPathComponent
                let mimeType = file["mime_type"]?.stringValue ?? "application/octet-stream"
                guard SessionDocumentAttachment.isSafeName(name), SessionDocumentAttachment.isSafeMIMEType(mimeType) else { return nil }
                let stableID = SHA256.hash(data: Data("\(index)|\(name)|\(mimeType)".utf8)).map { String(format: "%02x", $0) }.joined()
                return MessageAttachmentSummary(id: stableID, name: name, mimeType: mimeType, byteCount: nil)
            }
            if let image = object["image_url"]?.objectValue, let value = image["url"]?.stringValue,
               value.lowercased().hasPrefix("data:image/") {
                let tail = value.split(separator: ",", maxSplits: 1, omittingEmptySubsequences: false)
                guard tail.count == 2, tail[0].contains(";base64") else { return nil }
                let mime = String(tail[0].dropFirst(5).split(separator: ";", maxSplits: 1).first ?? "")
                guard SessionDocumentAttachment.isSafeMIMEType(mime), let count = decodedBase64Length(String(tail[1])), count <= SessionSendPayload.maximumDocumentBytes else { return nil }
                let name = "image-\(index).\(mime.split(separator: "/").last ?? "bin")"
                let stableID = SHA256.hash(data: Data("\(index)|\(name)|\(mime)|\(count)".utf8)).map { String(format: "%02x", $0) }.joined()
                return MessageAttachmentSummary(id: stableID, name: name, mimeType: mime, byteCount: count)
            }
            return nil
        }
    }

    fileprivate static func decodedBase64Length(_ value: String) -> Int? {
        let bytes = value.utf8
        guard !bytes.isEmpty, bytes.count.isMultiple(of: 4), bytes.allSatisfy({
            (0x41...0x5A).contains($0) || (0x61...0x7A).contains($0) || (0x30...0x39).contains($0) || $0 == 0x2B || $0 == 0x2F || $0 == 0x3D
        }) else { return nil }
        let padding = bytes.reversed().prefix(while: { $0 == 0x3D }).count
        guard padding <= 2, !bytes.dropLast(padding).contains(0x3D) else { return nil }
        return bytes.count / 4 * 3 - padding
    }
}

/// Immutable user text and document parts shared by validation, persistence, and retry.
public struct SessionSendPayload: Codable, Sendable, Equatable {
    public static let maximumDocumentBytes = 512 * 1024
    public static let maximumEncodedRequestBytes = 768 * 1024

    public let content: String
    public let multiContent: [SessionMessagePart]
    enum CodingKeys: String, CodingKey { case content, multiContent = "multi_content" }

    public init(content: String, attachments: [SessionDocumentAttachment] = []) throws {
        try self.init(content: content, multiContent: attachments.map(\.part))
    }

    /// Decoding and outbox recovery revalidate decoded parts before exposing them for resend.
    public init(content: String, multiContent: [SessionMessagePart]) throws {
        guard multiContent.count <= 16, multiContent.allSatisfy(\.isValid) else { throw SessionMediaError.invalidDocumentSource }
        guard !content.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty || !multiContent.isEmpty else { throw SessionMediaError.emptyPayload }
        self.content = content
        self.multiContent = multiContent
    }

    public var attachments: [OutgoingAttachment] {
        multiContent.enumerated().compactMap { index, part in try? OutgoingAttachment(part: part, index: index) }
    }

    public func encodedRequest(requestID: String, mode: String = "submit", generateTitle: Bool = false) throws -> Data {
        guard !requestID.isEmpty, requestID.utf8.count <= 128, ["", "submit", "steer"].contains(mode) else { throw SessionMediaError.emptyPayload }
        let request = SessionSendRequest(content: content, mode: mode, requestID: requestID, multiContent: multiContent.isEmpty ? nil : multiContent, generateTitle: generateTitle)
        let data = try JSONEncoder().encode(request)
        guard data.count <= Self.maximumEncodedRequestBytes else { throw SessionMediaError.requestTooLarge }
        return data
    }

    public init(from decoder: Decoder) throws {
        let container = try decoder.container(keyedBy: CodingKeys.self)
        let content = try container.decode(String.self, forKey: .content)
        let parts = try container.decodeIfPresent([SessionMessagePart].self, forKey: .multiContent) ?? []
        do { try self.init(content: content, multiContent: parts) }
        catch { throw DecodingError.dataCorruptedError(forKey: .multiContent, in: container, debugDescription: "Invalid or oversized inline document payload.") }
    }

    public static func text(_ value: String) throws -> Self { try Self(content: value) }

    public func requestBody(requestID: String, mode: String = "submit", generateTitle: Bool = false) throws -> SessionSendRequest {
        _ = try encodedRequest(requestID: requestID, mode: mode, generateTitle: generateTitle)
        return SessionSendRequest(content: content, mode: mode, requestID: requestID,
                                  multiContent: multiContent.isEmpty ? nil : multiContent, generateTitle: generateTitle)
    }

    public static func encodedRequestSize(content: String, parts: [SessionMessagePart], requestID: String, mode: String = "submit", generateTitle: Bool = false) throws -> Int {
        try SessionSendPayload(content: content, multiContent: parts).encodedRequest(requestID: requestID, mode: mode, generateTitle: generateTitle).count
    }
}

public struct SessionSendRequest: Encodable, Sendable, Equatable {
    public let content: String
    public let mode: String
    public let requestID: String
    public let multiContent: [SessionMessagePart]?
    public let generateTitle: Bool
    enum CodingKeys: String, CodingKey {
        case content, mode, requestID = "request_id", multiContent = "multi_content", generateTitle = "generate_title"
    }
    public init(content: String, mode: String = "submit", requestID: String, multiContent: [SessionMessagePart]? = nil, generateTitle: Bool = false) {
        self.content = content; self.mode = mode; self.requestID = requestID; self.multiContent = multiContent; self.generateTitle = generateTitle
    }
}

public struct GeneratedMediaReferenceRequest: Codable, Sendable, Equatable {
    public let ownerSessionID: String
    public let root: String
    public let path: String
    enum CodingKeys: String, CodingKey { case ownerSessionID = "owner_session_id", root, path }
    public init(ownerSessionID: String, root: String, path: String) throws {
        guard !ownerSessionID.isEmpty, ownerSessionID.utf8.count <= 256, root == "workspace",
              GeneratedMediaReferenceRequest.isSafeRelativePath(path) else { throw SessionMediaError.invalidGeneratedMediaReference }
        self.ownerSessionID = ownerSessionID; self.root = root; self.path = path
    }
    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        try self.init(ownerSessionID: c.decode(String.self, forKey: .ownerSessionID), root: c.decode(String.self, forKey: .root), path: c.decode(String.self, forKey: .path))
    }
    private static func isSafeRelativePath(_ value: String) -> Bool {
        guard !value.isEmpty, value.utf8.count <= 1024, !value.hasPrefix("/"), !value.contains("\\") else { return false }
        return value.split(separator: "/", omittingEmptySubsequences: false).allSatisfy { !$0.isEmpty && $0 != "." && $0 != ".." }
    }
}

public struct GeneratedMediaReference: Decodable, Sendable, Equatable, Hashable, Identifiable {
    public let ownerSessionID: String
    public let root: String
    public let path: String
    public let name: String
    public let mimeType: String
    enum CodingKeys: String, CodingKey { case ownerSessionID = "owner_session_id", root, path, name, mimeType = "mime_type" }
    public var id: String { "\(ownerSessionID)|\(root)|\(path)" }
    public var request: GeneratedMediaReferenceRequest { get throws { try GeneratedMediaReferenceRequest(ownerSessionID: ownerSessionID, root: root, path: path) } }

    public init(ownerSessionID: String, root: String, path: String, name: String, mimeType: String) throws {
        guard Self.isValid(ownerSessionID: ownerSessionID, root: root, path: path, name: name, mimeType: mimeType) else { throw SessionMediaError.invalidGeneratedMediaReference }
        self.ownerSessionID = ownerSessionID
        self.root = root
        self.path = path
        self.name = name
        self.mimeType = mimeType
    }

    public init(from decoder: Decoder) throws {
        let container = try decoder.container(keyedBy: CodingKeys.self)
        try self.init(ownerSessionID: container.decode(String.self, forKey: .ownerSessionID),
                      root: container.decode(String.self, forKey: .root),
                      path: container.decode(String.self, forKey: .path),
                      name: container.decode(String.self, forKey: .name),
                      mimeType: container.decode(String.self, forKey: .mimeType))
    }

    fileprivate static func isValid(ownerSessionID: String, root: String, path: String, name: String, mimeType: String) -> Bool {
        guard !ownerSessionID.isEmpty, ownerSessionID.utf8.count <= 256, root == "workspace",
              SessionDocumentAttachment.isSafeName(name), SessionDocumentAttachment.isSafeMIMEType(mimeType),
              !path.isEmpty, path.utf8.count <= 1024, !path.hasPrefix("/"), !path.contains("\\") else { return false }
        let components = path.split(separator: "/", omittingEmptySubsequences: false)
        return !components.isEmpty && components.allSatisfy { !$0.isEmpty && $0 != "." && $0 != ".." }
    }
}

public struct GeneratedMediaSourceWire: Decodable, Sendable {
    public let inlineText: String?
    public let inlineData: String?
    public let artifactPath: String?
    public let artifactRoot: String?
    public let artifactOwnerSessionID: String?
    enum CodingKeys: String, CodingKey {
        case inlineText = "inline_text", inlineData = "inline_data", artifactPath = "artifact_path"
        case artifactRoot = "artifact_root", artifactOwnerSessionID = "artifact_owner_session_id"
    }
}

public struct GeneratedMediaDocumentWire: Decodable, Sendable {
    public let name: String
    public let mimeType: String
    public let source: GeneratedMediaSourceWire
    enum CodingKeys: String, CodingKey { case name, mimeType = "mime_type", source }
}

public struct GeneratedMediaPartWire: Decodable, Sendable {
    public let type: String
    public let document: GeneratedMediaDocumentWire?
}

public extension GeneratedMediaReference {
    init(part: GeneratedMediaPartWire) throws {
        guard part.type == "document", let document = part.document,
              let owner = document.source.artifactOwnerSessionID,
              let root = document.source.artifactRoot,
              let path = document.source.artifactPath else { throw SessionMediaError.invalidGeneratedMediaReference }
        try self.init(ownerSessionID: owner, root: root, path: path, name: document.name, mimeType: document.mimeType)
    }
}

public extension APIClient {
    func resolveGeneratedMedia(sessionID: String, reference: GeneratedMediaReference) async throws -> Data {
        guard GeneratedMediaReference.isValid(ownerSessionID: reference.ownerSessionID, root: reference.root, path: reference.path, name: reference.name, mimeType: reference.mimeType) else { throw SessionMediaError.invalidGeneratedMediaReference }
        let encodedID = sessionID.addingPercentEncoding(withAllowedCharacters: CharacterSet(charactersIn: "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-._~")) ?? sessionID
        let body = try reference.request
        return try await requestBytes(method: "POST", path: "api/v2/sessions/\(encodedID)/generated-media",
                                      body: body, maximumBytes: 32 * 1024 * 1024)
    }
}

/// Wire decoder for user-submitted attachment fixtures and stored send bodies.
public struct SessionInputRequest: Decodable, Sendable {
    public let mode: String?
    public let content: String
    public let multiContent: [JSONValue]?
    public let requestID: String?
    enum CodingKeys: String, CodingKey { case mode, content, multiContent = "multi_content", requestID = "request_id" }
}
