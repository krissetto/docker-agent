import Foundation

public enum DurableSendState: String, Codable, Sendable {
    case prepared
    case sending
    case deliveryUnknown
    case accepted
}

public enum DurableDeliveryOutcome: Sendable, Equatable {
    case definitelyNotDelivered
    case unknown
}

public enum DurableSendOutboxError: Error, LocalizedError, Sendable, Equatable {
    case invalidIdentity
    case invalidPayload
    case conflictingRequestID
    case identityMismatch
    case invalidTransition
    case unsafeToDiscard
    case outboxLimitReached
    case storageLimitReached
    case corruptStore

    public var errorDescription: String? {
        switch self {
        case .invalidIdentity: "The send target identity is invalid."
        case .invalidPayload: "The message is invalid or exceeds the durable outbox limit."
        case .conflictingRequestID: "This request ID is already bound to a different send."
        case .identityMismatch: "The pending send belongs to a different server or session."
        case .invalidTransition: "The pending send cannot make that lifecycle transition."
        case .unsafeToDiscard: "Delivery may already have occurred; explicitly confirm discarding this unknown send."
        case .outboxLimitReached: "The durable outbox is full. Clear accepted history or resolve pending sends before starting another send."
        case .storageLimitReached: "The durable outbox storage limit was reached."
        case .corruptStore: "The durable outbox could not be read safely."
        }
    }
}

public struct DurableSendIdentity: Codable, Equatable, Sendable {
    public let serverID: UUID
    public let serverInstanceID: String
    public let session: SessionRef
    public let origin: String

    public init(serverID: UUID, serverInstanceID: String, session: SessionRef, serverURL: URL) throws {
        guard serverID == session.serverID,
              !serverInstanceID.isEmpty, serverInstanceID.utf8.count <= 256,
              let origin = Self.canonicalOrigin(serverURL) else { throw DurableSendOutboxError.invalidIdentity }
        self.serverID = serverID
        self.serverInstanceID = serverInstanceID
        self.session = session
        self.origin = origin
    }

    public var sessionID: String { session.sessionID }

    private static func canonicalOrigin(_ url: URL) -> String? {
        guard var components = URLComponents(url: url, resolvingAgainstBaseURL: false),
              let schemeValue = components.scheme?.lowercased(),
              ["http", "https"].contains(schemeValue),
              let hostValue = components.host?.lowercased(), !hostValue.isEmpty,
              components.user == nil, components.password == nil else { return nil }
        components.scheme = schemeValue
        components.host = hostValue
        components.path = ""
        components.query = nil
        components.fragment = nil
        if (schemeValue == "https" && components.port == 443) || (schemeValue == "http" && components.port == 80) {
            components.port = nil
        }
        return components.string
    }
}

public struct DurableSendRecord: Codable, Equatable, Sendable, Identifiable {
    public let identity: DurableSendIdentity
    public let requestID: String
    public let payload: String
    public let mode: String
    public let createdAt: Date
    public let attemptCount: Int
    public let lastAttemptAt: Date?
    public let multiContent: [SessionMessagePart]?
    public let acceptedAt: Date?
    public let state: DurableSendState

    public var id: String { requestID }
    public var sendPayload: SessionSendPayload { get throws { try SessionSendPayload(content: payload, multiContent: multiContent ?? []) } }
    public var attachments: [OutgoingAttachment] { (try? sendPayload.attachments) ?? [] }

    fileprivate init(storage: StoredDurableSend) {
        identity = storage.identity
        requestID = storage.requestID
        payload = storage.payload
        mode = storage.mode
        createdAt = storage.createdAt
        attemptCount = storage.attemptCount
        lastAttemptAt = storage.lastAttemptAt
        multiContent = storage.multiContent
        acceptedAt = storage.acceptedAt
        state = storage.state
    }
}

private struct StoredDurableSend: Codable, Equatable, Sendable {
    let identity: DurableSendIdentity
    let requestID: String
    let payload: String
    let mode: String
    let createdAt: Date
    var attemptCount: Int
    var lastAttemptAt: Date?
    var multiContent: [SessionMessagePart]?
    var acceptedAt: Date?
    var state: DurableSendState
}

private struct DurableOutboxDocument: Codable, Sendable {
    let version: Int
    var records: [StoredDurableSend]
}

/// Persists exact ordinary-message requests before transport. It never sends or retries on its own.
public final class DurableSendOutbox: @unchecked Sendable {
    public static let maximumRecords = 64
    public static let maximumPendingRecords = 32
    public static let maximumPayloadBytes = 48 * 1024
    public static let maximumRequestIDBytes = 128
    public static let maximumStoreBytes = 2 * 1024 * 1024

    private let fileURL: URL
    private let fileManager: FileManager
    private let lock = NSLock()
    private var records: [StoredDurableSend]

    public convenience init() throws {
        let root = FileManager.default.urls(for: .applicationSupportDirectory, in: .userDomainMask).first
            ?? FileManager.default.temporaryDirectory
        try self.init(fileURL: root.appending(path: "DockerAgentApple/send-outbox.json"))
    }

    public init(fileURL: URL, fileManager: FileManager = .default) throws {
        self.fileURL = fileURL
        self.fileManager = fileManager
        var loaded = try Self.readStore(at: fileURL, fileManager: fileManager)
        guard loaded.count <= Self.maximumRecords,
              loaded.allSatisfy(Self.isValid),
              Set(loaded.map(\.requestID)).count == loaded.count,
              loaded.filter({ $0.state != .accepted }).count <= Self.maximumPendingRecords else {
            throw DurableSendOutboxError.corruptStore
        }
        var recovered = false
        for index in loaded.indices where loaded[index].state == .sending {
            loaded[index].state = .deliveryUnknown
            recovered = true
        }
        records = loaded
        if recovered { try persist(loaded) }
    }

    /// Creates a new explicit send. A matching ID returns its original immutable request.
    @discardableResult
    public func prepare(identity: DurableSendIdentity, payload: String, mode: String = "submit",
                        requestID: String = UUID().uuidString) throws -> DurableSendRecord {
        let sendPayload: SessionSendPayload
        do { sendPayload = try SessionSendPayload.text(payload) }
        catch { throw DurableSendOutboxError.invalidPayload }
        return try prepare(identity: identity, sendPayload: sendPayload, mode: mode, requestID: requestID)
    }

    /// Persists the complete immutable multi-content body before the caller attempts transport.
    @discardableResult
    public func prepare(identity: DurableSendIdentity, sendPayload: SessionSendPayload, mode: String = "submit",
                        requestID: String = UUID().uuidString) throws -> DurableSendRecord {
        lock.lock(); defer { lock.unlock() }
        guard !requestID.isEmpty, requestID.utf8.count <= Self.maximumRequestIDBytes,
              sendPayload.content.utf8.count <= Self.maximumPayloadBytes,
              ["", "submit", "steer"].contains(mode) else { throw DurableSendOutboxError.invalidPayload }
        guard (try? sendPayload.encodedRequest(requestID: requestID, mode: mode)) != nil else {
            throw DurableSendOutboxError.invalidPayload
        }
        if let existing = records.first(where: { $0.requestID == requestID }) {
            guard existing.identity == identity, existing.payload == sendPayload.content, existing.multiContent == (sendPayload.multiContent.isEmpty ? nil : sendPayload.multiContent), existing.mode == mode else {
                throw DurableSendOutboxError.conflictingRequestID
            }
            return DurableSendRecord(storage: existing)
        }
        guard records.count < Self.maximumRecords,
              records.filter({ $0.state != .accepted }).count < Self.maximumPendingRecords else {
            throw DurableSendOutboxError.outboxLimitReached
        }
        var next = records
        next.append(StoredDurableSend(identity: identity, requestID: requestID, payload: sendPayload.content, mode: mode,
                                      createdAt: .now, attemptCount: 0, lastAttemptAt: nil,
                                      multiContent: sendPayload.multiContent.isEmpty ? nil : sendPayload.multiContent,
                                      acceptedAt: nil, state: .prepared))
        try commit(next)
        return DurableSendRecord(storage: next[next.count - 1])
    }

    public func pending(for identity: DurableSendIdentity) -> [DurableSendRecord] {
        lock.lock(); defer { lock.unlock() }
        return records.filter { $0.identity == identity && $0.state != .accepted }
            .sorted { $0.createdAt < $1.createdAt }.map(DurableSendRecord.init(storage:))
    }

    public func history(for identity: DurableSendIdentity) -> [DurableSendRecord] {
        lock.lock(); defer { lock.unlock() }
        return records.filter { $0.identity == identity }
            .sorted { $0.createdAt < $1.createdAt }.map(DurableSendRecord.init(storage:))
    }

    /// Must be called immediately before an explicit POST attempt.
    @discardableResult
    public func markAttempt(requestID: String, identity: DurableSendIdentity) throws -> DurableSendRecord {
        try update(requestID: requestID, identity: identity) { record in
            guard record.state == .prepared || record.state == .deliveryUnknown else {
                throw DurableSendOutboxError.invalidTransition
            }
            guard record.attemptCount < Int.max else { throw DurableSendOutboxError.invalidTransition }
            record.state = .sending
            record.attemptCount += 1
            record.lastAttemptAt = .now
        }
    }

    /// Store only delivery classification; transport errors may contain credentials and are never persisted.
    @discardableResult
    public func markFailure(requestID: String, identity: DurableSendIdentity,
                            outcome: DurableDeliveryOutcome) throws -> DurableSendRecord {
        try update(requestID: requestID, identity: identity) { record in
            guard record.state == .sending || record.state == .deliveryUnknown else {
                throw DurableSendOutboxError.invalidTransition
            }
            record.state = outcome == .definitelyNotDelivered ? .prepared : .deliveryUnknown
        }
    }

    @discardableResult
    public func markAccepted(requestID: String, identity: DurableSendIdentity) throws -> DurableSendRecord {
        try update(requestID: requestID, identity: identity) { record in
            guard record.state == .sending || record.state == .deliveryUnknown else {
                throw DurableSendOutboxError.invalidTransition
            }
            record.state = .accepted
            record.acceptedAt = .now
        }
    }

    /// Unknown delivery can be discarded only with an explicit user acknowledgement of possible loss.
    public func discard(requestID: String, identity: DurableSendIdentity, confirmUnknownDelivery: Bool = false) throws {
        lock.lock(); defer { lock.unlock() }
        guard let existing = records.first(where: { $0.requestID == requestID }) else { return }
        guard existing.identity == identity else { throw DurableSendOutboxError.identityMismatch }
        guard existing.state != .accepted, existing.state != .sending else { throw DurableSendOutboxError.unsafeToDiscard }
        if existing.state == .deliveryUnknown && !confirmUnknownDelivery { throw DurableSendOutboxError.unsafeToDiscard }
        var next = records
        next.removeAll { $0.requestID == requestID }
        try commit(next)
    }

    /// Explicitly clears only accepted request-ID tombstones; pending sends are never evicted.
    public func clearAcceptedHistory() throws {
        lock.lock(); defer { lock.unlock() }
        let next = records.filter { $0.state != .accepted }
        try commit(next)
    }

    private func update(requestID: String, identity: DurableSendIdentity,
                        mutation: (inout StoredDurableSend) throws -> Void) throws -> DurableSendRecord {
        lock.lock(); defer { lock.unlock() }
        guard let index = records.firstIndex(where: { $0.requestID == requestID }) else {
            throw DurableSendOutboxError.invalidTransition
        }
        guard records[index].identity == identity else { throw DurableSendOutboxError.identityMismatch }
        var next = records
        try mutation(&next[index])
        try commit(next)
        return DurableSendRecord(storage: next[index])
    }

    private func commit(_ next: [StoredDurableSend]) throws {
        try persist(next)
        records = next
    }

    private func persist(_ values: [StoredDurableSend]) throws {
        guard values.count <= Self.maximumRecords else { throw DurableSendOutboxError.outboxLimitReached }
        let data: Data
        do { data = try JSONEncoder().encode(DurableOutboxDocument(version: 1, records: values)) }
        catch { throw DurableSendOutboxError.storageLimitReached }
        guard data.count <= Self.maximumStoreBytes else { throw DurableSendOutboxError.storageLimitReached }
        let directory = fileURL.deletingLastPathComponent()
        do {
            try fileManager.createDirectory(at: directory, withIntermediateDirectories: true,
                                            attributes: [.posixPermissions: 0o700])
            try fileManager.setAttributes([.posixPermissions: 0o700], ofItemAtPath: directory.path)
            #if os(iOS)
            try data.write(to: fileURL, options: [.atomic, .completeFileProtectionUnlessOpen])
            #else
            try data.write(to: fileURL, options: [.atomic])
            #endif
            try fileManager.setAttributes([.posixPermissions: 0o600], ofItemAtPath: fileURL.path)
        } catch {
            throw DurableSendOutboxError.storageLimitReached
        }
    }

    private static func readStore(at url: URL, fileManager: FileManager) throws -> [StoredDurableSend] {
        guard fileManager.fileExists(atPath: url.path) else { return [] }
        do {
            let attributes = try fileManager.attributesOfItem(atPath: url.path)
            guard let size = attributes[.size] as? NSNumber, size.intValue <= maximumStoreBytes else {
                throw DurableSendOutboxError.corruptStore
            }
            let data = try Data(contentsOf: url)
            let document = try JSONDecoder().decode(DurableOutboxDocument.self, from: data)
            guard document.version == 1 else { throw DurableSendOutboxError.corruptStore }
            return document.records
        } catch let error as DurableSendOutboxError { throw error }
        catch { throw DurableSendOutboxError.corruptStore }
    }

    private static func isValid(_ record: StoredDurableSend) -> Bool {
        !record.requestID.isEmpty && record.requestID.utf8.count <= maximumRequestIDBytes
            && record.identity.session.serverID == record.identity.serverID
            && !record.identity.serverInstanceID.isEmpty && !record.identity.origin.isEmpty
            && record.payload.utf8.count <= maximumPayloadBytes
            && (try? SessionSendPayload(content: record.payload, multiContent: record.multiContent ?? []).encodedRequest(requestID: record.requestID, mode: record.mode)) != nil
            && (!record.payload.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty || !(record.multiContent?.isEmpty ?? true))
            && ["", "submit", "steer"].contains(record.mode)
            && record.attemptCount >= 0
            && (record.state == .accepted ? record.acceptedAt != nil : record.acceptedAt == nil)
    }
}
