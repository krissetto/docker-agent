import Foundation
import Testing
import AgentKit
@testable import DockerAgentApple

@MainActor
struct AppFlowTests {
    @Test func connectsOpensReplaysSendsRetriesUnknownRequestAndRespondsToInteraction() async throws {
        let serverID = UUID()
        let host = "flow-\(UUID().uuidString.lowercased()).example"
        let root = FileManager.default.temporaryDirectory.appending(path: "DockerAgentAppFlowTests/\(UUID().uuidString)")
        defer { try? FileManager.default.removeItem(at: root) }
        let state = FlowStubState(host: host)
        let fixtureIdentity = try fixture("server_identity.json")
        let fixtureCatalog = try fixture("catalog.json")
        let discoveredAgents = try fixture("agents.json")
        let preparedView = try fixture("prepare_info.json")
        let openedView = try fixture("open_view.json")
        let snapshot = try fixture("snapshot.json")
        let eventStream = try fixture("event_sequence.sse")
        state.setResponse(for: "GET", path: "/api/v2/server") { _ in .response(200, fixtureIdentity) }
        state.setResponse(for: "GET", path: "/api/agents") { _ in .response(200, discoveredAgents) }
        state.setResponse(for: "GET", path: "/api/v2/sessions") { _ in .response(200, fixtureCatalog) }
        state.setResponse(for: "POST", path: "/api/v2/sessions/start") { request in
            let body = try Self.jsonObject(Self.requestBody(request))
            #expect(body?["source"] as? String == "workspace-source.yaml")
            #expect(body?["agent_name"] as? String == "planner")
            let input = body?["input"] as? [String: Any]
            if body?["title"] as? String == "Keep this title" {
                #expect(input?["content"] as? String == "an explicit session request")
                #expect(input?["generate_title"] as? Bool == false)
            } else {
                #expect(body?["title"] as? String == "")
                #expect(input?["content"] as? String == "new root task")
                #expect(input?["generate_title"] as? Bool == true)
            }
            #expect(input?["request_id"] as? String != nil)
            return .response(202, Data(#"{"session_id":"created-session","turn_id":"created-turn"}"#.utf8))
        }
        state.setResponse(for: "GET", path: "/api/v2/sessions/session-demo-001") { request in
            if URLComponents(url: request.url!, resolvingAgainstBaseURL: false)?.queryItems?.contains(where: { $0.name == "view" && $0.value == "prepare-info" }) == true {
                return .response(200, preparedView)
            }
            return .response(404, Data())
        }
        state.setResponse(for: "PATCH", path: "/api/v2/sessions/session-demo-001") { request in
            #expect(URLComponents(url: request.url!, resolvingAgainstBaseURL: false)?.queryItems?.contains(where: { $0.name == "view" && $0.value == "open-view" }) == true)
            let openBody = try Self.jsonObject(Self.requestBody(request))
            #expect(openBody?["kind"] as? String == "open_view")
            return .response(200, openedView)
        }
        state.setResponse(for: "GET", path: "/api/v2/sessions/session-demo-001/snapshot") { _ in .response(200, snapshot) }
        state.setResponse(for: "GET", path: "/api/v2/sessions/session-demo-001/events") { _ in .response(200, eventStream, contentType: "text/event-stream") }
        state.setResponse(for: "GET", path: "/api/v2/sessions/session-demo-001/status") { _ in .response(200, Data(#"{"metadata":{"session_id":"session-demo-001","agent_name":"root","execution_available":true,"capabilities":{"execution_available":true}},"status":{"session_id":"session-demo-001","agent_name":"root","state":"running","pending":0,"turn_id":"turn-demo-001"}}"#.utf8)) }
        state.setResponse(for: "POST", path: "/api/v2/sessions/session-demo-001/messages") { request in
            let body = try Self.jsonObject(Self.requestBody(request))
            #expect(body?["content"] as? String == "retry this exact input")
            #expect(body?["request_id"] as? String == "flow-request-1")
            #expect(body?["generate_title"] as? Bool == true)
            if state.nextMessageFailure() { return .failure(URLError(.networkConnectionLost)) }
            return .response(202, Data(#"{"session_id":"session-demo-001","turn_id":"turn-accepted"}"#.utf8))
        }
        state.setResponse(for: "POST", path: "/api/v2/sessions/session-demo-001/responses") { request in
            let body = try Self.jsonObject(Self.requestBody(request))
            #expect(body?["interaction_id"] as? String == "interaction-001")
            #expect(body?["kind"] as? String == "confirmation")
            #expect(body?["confirmation"] as? String == "approve")
            return .response(204, Data())
        }

        let configuration = URLSessionConfiguration.ephemeral
        configuration.protocolClasses = [FlowURLProtocol.self]
        FlowURLProtocol.install(state, for: host)
        let tokenStore = FlowTokenStore()
        let configStore = InstanceConfigStore(fileURL: root.appending(path: "instances.json"))
        let outbox = try DurableSendOutbox(fileURL: root.appending(path: "send-outbox.json"))
        let model = AppModel(tokenStore: tokenStore, configStore: configStore, outbox: outbox,
                             reviewDrafts: ReviewDraftStore(fileURL: root.appending(path: "review-drafts.json")),
                             urlSession: URLSession(configuration: configuration), autoConnect: false)
        let draft = ServerDraft(id: serverID, name: "Fixture", baseURL: "https://\(host)", token: "synthetic-token")
        let identity = try await model.testServer(draft)
        #expect(identity.instanceID == "fixture-instance-001")
        try await model.addOrUpdateServer(draft, identity: identity)

        let discovered = try await model.loadAgents(serverID: serverID)
        let configuredRoot = try #require(discovered.first { $0.name == "workspace-source.yaml" })
        #expect(configuredRoot.rootAgentName == "planner")
        _ = try await model.createSession(serverID: serverID, source: configuredRoot, firstMessage: "new root task", operationID: UUID())
        let startRequests = state.requests.filter { $0.httpMethod == "POST" && $0.url?.path == "/api/v2/sessions/start" }
        #expect(startRequests.count == 1)
        let explicitOperationID = UUID()
        _ = try await model.createSession(serverID: serverID, source: configuredRoot, title: "Keep this title",
                                          firstMessage: "an explicit session request", operationID: explicitOperationID)
        let updatedStartRequests = state.requests.filter { $0.httpMethod == "POST" && $0.url?.path == "/api/v2/sessions/start" }
        #expect(updatedStartRequests.count == 2)
        let explicitStart = try Self.jsonObject(Self.requestBody(updatedStartRequests[1]))
        #expect(explicitStart?["title"] as? String == "Keep this title")
        #expect((explicitStart?["input"] as? [String: Any])?["generate_title"] as? Bool == false)
        let missingRootSource = AgentOption(name: "legacy-source.yaml", rootAgentName: nil)
        await #expect(throws: APIError.self) {
            try await model.createSession(serverID: serverID, source: missingRootSource, firstMessage: "must not create")
        }
        #expect(state.requests.filter { $0.httpMethod == "POST" && $0.url?.path == "/api/v2/sessions/start" }.count == 2)

        await model.refreshCatalog(serverID)
        #expect(model.sessions.contains { $0.ref.sessionID == "session-demo-001" })

        let ref = SessionRef(serverID: serverID, sessionID: "session-demo-001")
        async let firstOpen: Void = model.openSession(ref)
        async let duplicateOpen: Void = model.openSession(ref)
        _ = try await (firstOpen, duplicateOpen)
        let openRequests = state.requests.map { "\($0.httpMethod ?? "")|\($0.url?.path ?? "")" }
        #expect(openRequests.contains("GET|/api/v2/sessions/session-demo-001"))
        #expect(openRequests.contains("PATCH|/api/v2/sessions/session-demo-001"))
        #expect(state.requests.filter { $0.url?.path == "/api/v2/sessions/session-demo-001" && $0.httpMethod == "GET" }.count == 1)
        #expect(state.requests.filter { $0.url?.path == "/api/v2/sessions/session-demo-001" && $0.httpMethod == "PATCH" }.count == 1)

        try await waitUntil(timeout: .seconds(5)) {
            model.openSessions[ref]?.cursor == 16 && model.openSessions[ref]?.state == .settled
        }
        let projection = try #require(model.openSessions[ref])
        #expect(projection.transcript.contains { $0.text.contains("README.md documents the demo.") })
        #expect(projection.cursor == 16)
        #expect(projection.interactions.isEmpty)

        state.failNextMessagePost()
        await #expect(throws: Error.self) {
            try await model.send(ref, content: "retry this exact input", requestID: "flow-request-1")
        }
        let unknown = try #require(model.pendingSends[ref]?.first)
        #expect(unknown.requestID == "flow-request-1")
        #expect(unknown.state == .deliveryUnknown)

        try await model.retryPendingSend(ref, requestID: "flow-request-1")
        #expect(model.pendingSends[ref]?.isEmpty == true)
        #expect(state.requests.allSatisfy { $0.value(forHTTPHeaderField: "Authorization") == "Bearer synthetic-token" })
        let postRequests = state.requests.filter { $0.httpMethod == "POST" && $0.url?.path == "/api/v2/sessions/session-demo-001/messages" }
        #expect(postRequests.count == 2)
        let firstPostBody = try Self.jsonObject(Self.requestBody(postRequests[0]))
        let retryPostBody = try Self.jsonObject(Self.requestBody(postRequests[1]))
        let firstRequestID = firstPostBody?["request_id"] as? String
        let retryRequestID = retryPostBody?["request_id"] as? String
        #expect(firstRequestID == retryRequestID)
        #expect(firstPostBody?["generate_title"] as? Bool == true)
        #expect(retryPostBody?["generate_title"] as? Bool == true)

        await #expect(throws: APIError.self) {
            try await model.send(ref, content: "retry this exact input", requestID: "flow-request-1")
        }
        let allPostCount = state.requests.reduce(into: 0) { count, request in
            if request.httpMethod == "POST" && request.url?.path == "/api/v2/sessions/session-demo-001/messages" { count += 1 }
        }
        #expect(allPostCount == 2)

        try await model.respond(ref, interactionID: "interaction-001", kind: "confirmation", confirmation: .approve)
        let hasResponsePost = state.requests.contains { request in
            request.httpMethod == "POST" && request.url?.path == "/api/v2/sessions/session-demo-001/responses"
        }
        #expect(hasResponsePost)
        model.closeSession(ref)
        model.closeSession(ref)
    }

    private func fixture(_ name: String) throws -> Data {
        guard let url = Bundle.module.url(forResource: name, withExtension: nil, subdirectory: "Fixtures") else {
            throw APIError("Missing fixture \(name)")
        }
        return try Data(contentsOf: url)
    }

    nonisolated private static func requestBody(_ request: URLRequest) -> Data {
        if let body = request.httpBody { return body }
        guard let stream = request.httpBodyStream else { return Data() }
        stream.open(); defer { stream.close() }
        var result = Data()
        let buffer = UnsafeMutablePointer<UInt8>.allocate(capacity: 4096)
        defer { buffer.deallocate() }
        while stream.hasBytesAvailable {
            let count = stream.read(buffer, maxLength: 4096)
            if count <= 0 { break }
            result.append(buffer, count: count)
        }
        return result
    }

    nonisolated private static func jsonObject(_ data: Data) throws -> [String: Any]? {
        try JSONSerialization.jsonObject(with: data) as? [String: Any]
    }

    private func waitUntil(timeout: Duration, condition: @escaping @MainActor () -> Bool) async throws {
        let start = ContinuousClock.now
        while !condition() {
            if ContinuousClock.now - start > timeout { throw APIError("Timed out waiting for deterministic event-stream projection.") }
            try await Task.sleep(for: .milliseconds(10))
        }
    }
}

private actor FlowTokenStore: TokenStoring {
    private var values: [UUID: String] = [:]
    func read(for id: UUID) async throws -> String? { values[id] }
    func write(_ token: String, for id: UUID) async throws { values[id] = token }
    func delete(for id: UUID) async throws { values[id] = nil }
}

private enum FlowStubResponse: Sendable {
    case response(Int, Data, contentType: String = "application/json")
    case failure(URLError)
}

private final class FlowStubState: @unchecked Sendable {
    private let lock = NSLock()
    let host: String
    private var handlers: [String: @Sendable (URLRequest) throws -> FlowStubResponse] = [:]
    private var requestsStorage: [URLRequest] = []
    private var failMessageOnce = false
    init(host: String) { self.host = host }
    var requests: [URLRequest] { lock.lock(); defer { lock.unlock() }; return requestsStorage }
    func setResponse(for method: String, path: String, handler: @escaping @Sendable (URLRequest) throws -> FlowStubResponse) {
        lock.lock(); defer { lock.unlock() }; handlers["\(method)|\(path)"] = handler
    }
    func failNextMessagePost() { lock.lock(); defer { lock.unlock() }; failMessageOnce = true }
    func nextMessageFailure() -> Bool { lock.lock(); defer { lock.unlock() }; if failMessageOnce { failMessageOnce = false; return true }; return false }
    func record(_ request: URLRequest) { lock.lock(); defer { lock.unlock() }; requestsStorage.append(request) }
    func handle(_ request: URLRequest) throws -> FlowStubResponse {
        record(request)
        let key = "\(request.httpMethod ?? "GET")|\(request.url?.path ?? "")"
        lock.lock(); let handler = handlers[key]; lock.unlock()
        guard let handler else { return .response(404, Data(#"{"message":"no test handler"}"#.utf8)) }
        return try handler(request)
    }
}

private final class FlowURLProtocol: URLProtocol, @unchecked Sendable {
    private static let lock = NSLock()
    nonisolated(unsafe) private static var states: [String: FlowStubState] = [:]
    static func install(_ state: FlowStubState, for host: String) {
        lock.lock(); defer { lock.unlock() }; states[host] = state
    }
    override class func canInit(with request: URLRequest) -> Bool { true }
    override class func canonicalRequest(for request: URLRequest) -> URLRequest { request }
    override func startLoading() {
        guard let host = request.url?.host else { client?.urlProtocol(self, didFailWithError: URLError(.badURL)); return }
        Self.lock.lock(); let state = Self.states[host]; Self.lock.unlock()
        guard let state else { client?.urlProtocol(self, didFailWithError: URLError(.cannotFindHost)); return }
        do {
            switch try state.handle(request) {
            case .failure(let error): client?.urlProtocol(self, didFailWithError: error)
            case .response(let status, let data, let contentType):
                let response = HTTPURLResponse(url: request.url!, statusCode: status, httpVersion: "HTTP/1.1", headerFields: ["Content-Type": contentType])!
                client?.urlProtocol(self, didReceive: response, cacheStoragePolicy: .notAllowed)
                if !data.isEmpty { client?.urlProtocol(self, didLoad: data) }
                client?.urlProtocolDidFinishLoading(self)
            }
        } catch { client?.urlProtocol(self, didFailWithError: error) }
    }
    override func stopLoading() {}
}
