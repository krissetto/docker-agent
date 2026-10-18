import Foundation
import Testing
@testable import AgentKit

struct SessionMediaTests {
    @Test func encodesDocumentPartsWithBase64InlineBytesAndPlainText() throws {
        let bytes = Data([0, 1, 2, 253, 254, 255])
        let binary = try SessionDocumentAttachment.fromData(bytes, name: "photo.png", mimeType: "image/png")
        let text = try SessionDocumentAttachment.fromText("hello\n", name: "note.txt")
        #expect(binary.byteCount == bytes.count)
        #expect(binary.name == "photo.png")
        #expect(text.mimeType == "text/plain")
        #expect(text.byteCount == 6)

        let payload = try SessionSendPayload(content: "Look at these.", attachments: [binary, text])
        let request = try payload.encodedRequest(requestID: "request-1", mode: "submit", generateTitle: true)
        let root = try #require(JSONSerialization.jsonObject(with: request) as? [String: Any])
        #expect(root["content"] as? String == "Look at these.")
        #expect(root["request_id"] as? String == "request-1")
        #expect(root["mode"] as? String == "submit")
        #expect(root["generate_title"] as? Bool == true)
        let parts = try #require(root["multi_content"] as? [[String: Any]])
        #expect(parts.count == 2)
        #expect(parts[0]["type"] as? String == "document")
        let binaryDocument = try #require(parts[0]["document"] as? [String: Any])
        #expect(binaryDocument["name"] as? String == "photo.png")
        #expect(binaryDocument["mime_type"] as? String == "image/png")
        #expect(binaryDocument["size"] as? Int == bytes.count)
        let binarySource = try #require(binaryDocument["source"] as? [String: Any])
        #expect(binarySource["inline_data"] as? String == bytes.base64EncodedString())
        let textDocument = try #require(parts[1]["document"] as? [String: Any])
        let textSource = try #require(textDocument["source"] as? [String: Any])
        #expect(textSource["inline_text"] as? String == "hello\n")
    }

    @Test func validatesLocalAttachmentNamesTypesAndSizesWithoutTruncation() throws {
        #expect(throws: SessionMediaError.emptyDocument) { try SessionDocumentAttachment.fromData(Data(), name: "empty.bin", mimeType: "application/octet-stream") }
        #expect(throws: SessionMediaError.invalidName) { try SessionDocumentAttachment.fromData(Data([1]), name: "../secret", mimeType: "application/octet-stream") }
        #expect(throws: SessionMediaError.invalidName) { try SessionDocumentAttachment.fromData(Data([1]), name: "bad\nname.txt", mimeType: "text/plain") }
        #expect(throws: SessionMediaError.invalidMIMEType) { try SessionDocumentAttachment.fromData(Data([1]), name: "file.bin", mimeType: "text/plain;token=secret") }
        #expect(throws: SessionMediaError.documentTooLarge) {
            try SessionDocumentAttachment.fromData(Data(repeating: 0, count: SessionDocumentAttachment.maximumDocumentBytes + 1), name: "large.bin", mimeType: "application/octet-stream")
        }
        #expect(throws: SessionMediaError.emptyPayload) { try SessionSendPayload(content: "  ", attachments: []) }
        #expect(throws: SessionMediaError.emptyPayload) { try SessionSendPayload.text("\n") }
    }

    @Test func requestBudgetMeasuresCompleteJSONIncludingBase64AndControlFields() throws {
        let attachment = try SessionDocumentAttachment.fromText(String(repeating: "a", count: 200 * 1024), name: "large.txt")
        let payload = try SessionSendPayload(content: String(repeating: "x", count: 600 * 1024), attachments: [attachment])
        #expect(throws: SessionMediaError.requestTooLarge) {
            try payload.encodedRequest(requestID: String(repeating: "r", count: 128), mode: "submit", generateTitle: true)
        }

        let nearLimit = try SessionSendPayload(content: "ok")
        let wire = try nearLimit.encodedRequest(requestID: "id", mode: "submit", generateTitle: true)
        #expect(wire.count < SessionSendPayload.maximumEncodedRequestBytes)
        #expect(try SessionSendPayload.encodedRequestSize(content: "ok", parts: [], requestID: "id", generateTitle: true) == wire.count)
    }

    @Test func durableOutboxReloadPreservesExactMultiContentAndRejectsChangedBytesForSameID() throws {
        let url = temporaryURL()
        defer { try? FileManager.default.removeItem(at: url.deletingLastPathComponent()) }
        let identity = try identity()
        let attachment = try SessionDocumentAttachment.fromData(Data([1, 2, 3, 4]), name: "scan.png", mimeType: "image/png")
        let payload = try SessionSendPayload(content: "Describe this.", attachments: [attachment])
        let outbox = try DurableSendOutbox(fileURL: url)
        let first = try outbox.prepare(identity: identity, sendPayload: payload, requestID: "media-request")
        #expect(first.attachments.count == 1)
        #expect(first.attachments[0].name == "scan.png")
        let firstSendPayload = try first.sendPayload
        #expect(firstSendPayload == payload)

        let reloaded = try DurableSendOutbox(fileURL: url)
        let loaded = try #require(reloaded.pending(for: identity).first)
        let reloadedPayload = try loaded.sendPayload
        #expect(reloadedPayload == payload)
        #expect(loaded.attachments.first?.part.document.source.inlineData == "AQIDBA==")
        let changedAttachment = try SessionDocumentAttachment.fromData(Data([1, 2, 3, 5]), name: "scan.png", mimeType: "image/png")
        let changedPayload = try SessionSendPayload(content: "Describe this.", attachments: [changedAttachment])
        #expect(throws: DurableSendOutboxError.conflictingRequestID) {
            try reloaded.prepare(identity: identity, sendPayload: changedPayload, requestID: "media-request")
        }

        _ = try reloaded.markAttempt(requestID: loaded.requestID, identity: identity)
        let recovered = try DurableSendOutbox(fileURL: url)
        let retry = try #require(recovered.pending(for: identity).first)
        #expect(retry.state == .deliveryUnknown)
        let retryPayload = try retry.sendPayload
        #expect(retryPayload == payload)
        let retryRequest = try retryPayload.encodedRequest(requestID: retry.requestID)
        let originalRequest = try payload.encodedRequest(requestID: "media-request")
        let retryWire = try JSONDecoder().decode(JSONValue.self, from: retryRequest)
        let originalWire = try JSONDecoder().decode(JSONValue.self, from: originalRequest)
        #expect(retryWire == originalWire)
        let body = try retryPayload.requestBody(requestID: retry.requestID)
        #expect(body.multiContent == payload.multiContent)
    }

    @Test func legacyTextOnlyOutboxRecordsDecodeWithoutMultiContent() throws {
        let url = temporaryURL()
        defer { try? FileManager.default.removeItem(at: url.deletingLastPathComponent()) }
        let id = UUID(uuidString: "E4349681-2198-4CAC-8A5E-A6D7256407B8")!
        let document: [String: Any] = ["version": 1, "records": [[
            "identity": ["serverID": id.uuidString, "serverInstanceID": "instance-old", "session": ["serverID": id.uuidString, "sessionID": "session-old"], "origin": "https://legacy.example"],
            "requestID": "old-request", "payload": "legacy text", "mode": "submit", "createdAt": 1_700_000_000.0,
            "attemptCount": 0, "state": "prepared"
        ]]]
        try FileManager.default.createDirectory(at: url.deletingLastPathComponent(), withIntermediateDirectories: true)
        try JSONSerialization.data(withJSONObject: document).write(to: url)
        let outbox = try DurableSendOutbox(fileURL: url)
        let session = SessionRef(serverID: id, sessionID: "session-old")
        let identity = try DurableSendIdentity(serverID: id, serverInstanceID: "instance-old", session: session, serverURL: URL(string: "https://legacy.example")!)
        let record = try #require(outbox.pending(for: identity).first)
        #expect(record.payload == "legacy text")
        #expect(record.multiContent == nil)
        #expect(try record.sendPayload == SessionSendPayload.text("legacy text"))
    }

    @Test func generatedMediaReferencesPreserveOwnerAndRejectPathsOrInlinePayload() throws {
        let json = Data(#"{"type":"document","document":{"name":"cat.png","mime_type":"image/png","source":{"artifact_owner_session_id":"owner-1","artifact_root":"workspace","artifact_path":"images/cat.png"}}}"#.utf8)
        let part = try JSONDecoder().decode(GeneratedMediaPartWire.self, from: json)
        let reference = try GeneratedMediaReference(part: part)
        #expect(reference.ownerSessionID == "owner-1")
        #expect(reference.root == "workspace")
        #expect(reference.path == "images/cat.png")
        #expect(throws: SessionMediaError.invalidGeneratedMediaReference) {
            try GeneratedMediaReference(ownerSessionID: "owner-1", root: "workspace", path: "../secret", name: "cat.png", mimeType: "image/png")
        }
        let inlineOnly = try JSONDecoder().decode(GeneratedMediaPartWire.self, from: Data(#"{"type":"document","document":{"name":"cat.png","mime_type":"image/png","source":{"inline_data":"AQ=="}}}"#.utf8))
        #expect(throws: SessionMediaError.invalidGeneratedMediaReference) { try GeneratedMediaReference(part: inlineOnly) }
    }

    @Test func goGeneratedMediaAndAttachmentFixturesProjectMetadataWithoutRetainingInlineBytes() throws {
        let request = try JSONDecoder().decode(SessionInputRequest.self, from: fixture("session_media_parts.json"))
        #expect(request.mode == "submit")
        #expect(request.content == "Please inspect the attached notes and chart.")
        #expect(request.multiContent?.count == 2)
        let summaries = SessionMediaProjection.attachments(from: request.multiContent ?? [])
        #expect(summaries.map(\.name) == ["notes.txt", "chart.png"])
        #expect(summaries.map(\.mimeType) == ["text/plain", "image/png"])
        #expect(summaries.map(\.byteCount) == [18, 3])
        #expect(summaries.allSatisfy { !$0.name.isEmpty })

        let referenceRequest = try JSONDecoder().decode(GeneratedMediaReferenceRequest.self, from: fixture("generated_media_ref.json"))
        #expect(referenceRequest.ownerSessionID == "session-demo-001")
        #expect(referenceRequest.path == "generated/chart.png")

        let event = try JSONDecoder().decode(StreamEnvelope.self, from: fixture("generated_media_event.json"))
        let rawParts = event.event?["generated_media"]?.arrayValue ?? []
        #expect(SessionMediaProjection.attachments(from: rawParts).map(\.name) == ["chart.png"])
        let artifact = try JSONDecoder().decode(GeneratedMediaPartWire.self, from: JSONEncoder().encode(rawParts[0]))
        let portableReference = try GeneratedMediaReference(part: artifact)
        #expect(try portableReference.request == referenceRequest)
        #expect(artifact.document?.source.inlineData == nil)
        #expect(artifact.document?.source.artifactOwnerSessionID == "session-demo-001")
    }

    @Test func generatedMediaResolverPostsOnlyOwnerRootPathAndBoundsRawResponse() async throws {
        let state = MediaStubState()
        state.response = { request in
            #expect(request.httpMethod == "POST")
            #expect(request.url?.path == "/api/v2/sessions/viewer%20session/generated-media")
            #expect(request.value(forHTTPHeaderField: "Authorization") == "Bearer synthetic")
            let body = try Self.jsonObject(Self.requestBody(request))
            #expect(body?.count == 3)
            #expect(body?["owner_session_id"] as? String == "owner-session")
            #expect(body?["root"] as? String == "workspace")
            #expect(body?["path"] as? String == "rendered/cat.png")
            return .response(200, Data([137, 80, 78, 71]), contentType: "application/octet-stream")
        }
        let client = try client(host: state.host, state: state)
        let ref = try GeneratedMediaReference(ownerSessionID: "owner-session", root: "workspace", path: "rendered/cat.png", name: "cat.png", mimeType: "image/png")
        #expect(try await client.resolveGeneratedMedia(sessionID: "viewer session", reference: ref) == Data([137, 80, 78, 71]))

        state.response = { _ in .response(200, Data(), contentType: "image/png") }
        await #expect(throws: APIError.self) { try await client.resolveGeneratedMedia(sessionID: "viewer session", reference: ref) }

        state.response = { _ in .oversized(32 * 1024 * 1024 + 1) }
        await #expect(throws: APIError.self) { try await client.resolveGeneratedMedia(sessionID: "viewer session", reference: ref) }
    }

    private func fixture(_ name: String) throws -> Data {
        guard let url = Bundle.module.url(forResource: name, withExtension: nil, subdirectory: "Fixtures") else { throw APIError("Missing fixture \(name)") }
        return try Data(contentsOf: url)
    }

    private func identity(serverID: UUID = UUID()) throws -> DurableSendIdentity {
        try DurableSendIdentity(serverID: serverID, serverInstanceID: "instance-media",
                                session: SessionRef(serverID: serverID, sessionID: "session-media"),
                                serverURL: URL(string: "https://media.example/api")!)
    }

    private func temporaryURL() -> URL {
        FileManager.default.temporaryDirectory.appending(path: "SessionMediaTests/\(UUID().uuidString)/outbox.json")
    }

    private func client(host: String, state: MediaStubState) throws -> APIClient {
        let configuration = URLSessionConfiguration.ephemeral
        configuration.protocolClasses = [MediaURLProtocol.self]
        MediaURLProtocol.install(state, host: host)
        return try APIClient(config: InstanceConfig(id: UUID(), name: "Media", baseURL: "https://\(host)"), token: "synthetic", session: URLSession(configuration: configuration))
    }

    nonisolated private static func requestBody(_ request: URLRequest) -> Data {
        if let body = request.httpBody { return body }
        guard let stream = request.httpBodyStream else { return Data() }
        stream.open(); defer { stream.close() }
        var data = Data()
        let buffer = UnsafeMutablePointer<UInt8>.allocate(capacity: 1024)
        defer { buffer.deallocate() }
        while stream.hasBytesAvailable {
            let count = stream.read(buffer, maxLength: 1024)
            if count <= 0 { break }
            data.append(buffer, count: count)
        }
        return data
    }

    nonisolated private static func jsonObject(_ data: Data) throws -> [String: Any]? {
        try JSONSerialization.jsonObject(with: data) as? [String: Any]
    }
}

private enum MediaStubResponse: Sendable {
    case response(Int, Data, contentType: String)
    case oversized(Int)
}

private final class MediaStubState: @unchecked Sendable {
    let host = "media-\(UUID().uuidString.lowercased()).example"
    private let lock = NSLock()
    private var responseStorage: (@Sendable (URLRequest) throws -> MediaStubResponse)?
    var response: (@Sendable (URLRequest) throws -> MediaStubResponse)? {
        get { lock.lock(); defer { lock.unlock() }; return responseStorage }
        set { lock.lock(); defer { lock.unlock() }; responseStorage = newValue }
    }
}

private final class MediaURLProtocol: URLProtocol, @unchecked Sendable {
    private static let lock = NSLock()
    nonisolated(unsafe) private static var states: [String: MediaStubState] = [:]
    static func install(_ state: MediaStubState, host: String) { lock.lock(); defer { lock.unlock() }; states[host] = state }
    override class func canInit(with request: URLRequest) -> Bool { true }
    override class func canonicalRequest(for request: URLRequest) -> URLRequest { request }
    override func startLoading() {
        guard let host = request.url?.host else { client?.urlProtocol(self, didFailWithError: URLError(.badURL)); return }
        Self.lock.lock(); let state = Self.states[host]; Self.lock.unlock()
        guard let state else { client?.urlProtocol(self, didFailWithError: URLError(.cannotFindHost)); return }
        do {
            switch try state.response?(request) ?? .response(404, Data(), contentType: "application/json") {
            case .response(let status, let data, let contentType):
                let response = HTTPURLResponse(url: request.url!, statusCode: status, httpVersion: "HTTP/1.1", headerFields: ["Content-Type": contentType, "Content-Length": String(data.count)])!
                client?.urlProtocol(self, didReceive: response, cacheStoragePolicy: .notAllowed)
                if !data.isEmpty { client?.urlProtocol(self, didLoad: data) }
                client?.urlProtocolDidFinishLoading(self)
            case .oversized(let count):
                let response = HTTPURLResponse(url: request.url!, statusCode: 200, httpVersion: "HTTP/1.1", headerFields: ["Content-Type": "application/octet-stream", "Content-Length": String(count)])!
                client?.urlProtocol(self, didReceive: response, cacheStoragePolicy: .notAllowed)
                client?.urlProtocolDidFinishLoading(self)
            }
        } catch { client?.urlProtocol(self, didFailWithError: error) }
    }
    override func stopLoading() {}
}
