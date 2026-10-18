import Foundation
import Testing
@testable import AgentKit

struct SessionControlsTests {
    @Test func typedRequestsMatchRegisteredSessionControlWire() async throws {
        let state = MockURLProtocolState()
        let client = try client(state: state)
        let metadata = try metadata()
        state.response = { request in
            if request.url?.path.hasSuffix("/models") == true {
                return (200, Data("[{\"name\":\"Primary\",\"ref\":\"openai/gpt-5\",\"model_id\":\"gpt-5\",\"is_current\":true}]".utf8))
            }
            if request.url?.path.hasSuffix("/thinking-level") == true {
                return (200, Data("{\"levels\":[\"low\",\"high\"],\"current\":\"high\",\"metadata\":\(metadataJSON)}".utf8))
            }
            return (204, Data())
        }

        try await client.setSessionTitle("session-1", title: "A title", metadata: metadata)
        let title = try #require(state.lastRequest)
        #expect(title.httpMethod == "PATCH")
        #expect(title.url?.path == "/api/v2/sessions/session-1/title")
        #expect(try jsonObject(title.httpBody!).asNSDictionary.isEqual(to: ["title": "A title"]))

        try await client.setSessionStarred("session-1", starred: true, metadata: metadata)
        let starred = try #require(state.lastRequest)
        #expect(starred.url?.path == "/api/v2/sessions/session-1/starred")
        #expect(try jsonObject(starred.httpBody!).asNSDictionary.isEqual(to: ["starred": true]))

        let models = try await client.sessionModels("session-1")
        #expect(models == [SessionModelChoice(name: "Primary", ref: "openai/gpt-5", provider: nil, model: nil, modelID: "gpt-5", modelName: nil, isDefault: nil, isCurrent: true, isCustom: nil, isCatalog: nil, family: nil, inputCost: nil, outputCost: nil)])
        let thinking = try await client.setThinkingLevel("session-1", level: "high", metadata: metadata)
        #expect(thinking.current == "high")
        let thinkingRequest = try #require(state.lastRequest)
        #expect(thinkingRequest.httpMethod == "PATCH")
        #expect(thinkingRequest.url?.path == "/api/v2/sessions/session-1/thinking-level")
        #expect(try jsonObject(thinkingRequest.httpBody!).asNSDictionary.isEqual(to: ["level": "high"]))
    }

    @Test func pendingMessageEditUsesSessionEditEnvelopeAndExpectedContentCAS() async throws {
        let state = MockURLProtocolState()
        state.response = { _ in (200, Data("{\"id\":\"session-1\",\"messages\":[]}".utf8)) }
        let client = try client(state: state)
        let updated = try await client.editPendingSessionInput("session-1", turnID: "turn-9", content: "revised", expectedContent: "old", metadata: metadata())
        #expect(updated.id == "session-1")
        let request = try #require(state.lastRequest)
        #expect(request.httpMethod == "PATCH")
        #expect(request.url?.path == "/api/v2/sessions/session-1")
        #expect(try jsonObject(request.httpBody!).asNSDictionary.isEqual(to: [
            "kind": "pending_message",
            "pending_message": ["turn_id": "turn-9", "content": "revised", "expected_content": "old"]
        ]))
    }

    @Test func todosUseStatusAndDescriptionCASWireFields() async throws {
        let state = MockURLProtocolState()
        state.response = { _ in (200, Data("[{\"id\":\"todo 1\",\"description\":\"new\",\"status\":\"in-progress\"}]".utf8)) }
        let client = try client(state: state)
        let todos = try await client.updateTodoDescription("session-1", todoID: "todo 1", expectedDescription: "old", description: "new", metadata: metadata())
        #expect(todos.first?.status == "in-progress")
        let request = try #require(state.lastRequest)
        #expect(request.httpMethod == "PATCH")
        #expect(request.url?.path == "/api/v2/sessions/session-1/todos/todo%201")
        #expect(try jsonObject(request.httpBody!).asNSDictionary.isEqual(to: ["expected_description": "old", "description": "new"]))
    }

    @Test func recentChatUsesOpaqueCursorAndValidatesSessionIdentity() async throws {
        let state = MockURLProtocolState()
        state.response = { _ in
            (200, Data("{\"version\":1,\"session_id\":\"session-1\",\"root_session_id\":\"session-1\",\"runtime_status\":\"resident\",\"execution_available\":true,\"pending\":1,\"attention_known\":true,\"approval_count\":1,\"elicitation_count\":0,\"messages\":[{\"id\":2,\"position\":1,\"role\":\"assistant\",\"agent_name\":\"agent\",\"text\":\"hello\"}],\"truncated_before\":true,\"before_cursor\":\"opaque+/=\"}".utf8))
        }
        let client = try client(state: state)
        let page = try await client.recentChat("session-1", limit: 12, cursor: "old+/=", source: "workspace")
        #expect(page.messages.first?.text == "hello")
        #expect(page.beforeCursor == "opaque+/=")
        let request = try #require(state.lastRequest)
        #expect(request.url?.path == "/api/v2/sessions/session-1/recent-chat")
        let query = URLComponents(url: request.url!, resolvingAgainstBaseURL: false)!.queryItems ?? []
        #expect(query.first(where: { $0.name == "cursor" })?.value == "old+/=")
        #expect(query.first(where: { $0.name == "source" })?.value == "workspace")

        state.response = { _ in (200, Data("{\"version\":1,\"session_id\":\"different\",\"root_session_id\":\"different\",\"runtime_status\":\"historical\",\"execution_available\":false,\"pending\":0,\"attention_known\":false,\"approval_count\":0,\"elicitation_count\":0,\"messages\":[],\"truncated_before\":false}".utf8)) }
        await #expect(throws: SessionControlsError.identityMismatch) {
            try await client.recentChat("session-1")
        }
    }

    @Test func switchAgentAndBranchDecodeNewSessionIdentity() async throws {
        let state = MockURLProtocolState()
        state.response = { request in
            let id = request.url?.path.hasSuffix("/branches") == true ? "branch-1" : "switched-1"
            return (201, Data("{\"metadata\":{\"session_id\":\"\(id)\",\"agent_name\":\"worker\",\"execution_available\":true,\"capabilities\":{\"execution_available\":true}},\"session\":{\"id\":\"\(id)\"}}".utf8))
        }
        let client = try client(state: state)
        let switched = try await client.switchSessionAgent("session-1", agentName: "worker", metadata: metadata())
        #expect(switched.metadata.sessionID == "switched-1")
        #expect(state.lastRequest?.url?.path == "/api/v2/sessions/session-1/switch-agent")
        #expect(try jsonObject(state.lastRequest!.httpBody!).asNSDictionary.isEqual(to: ["agent_name": "worker"]))
        let branch = try await client.branchSession("session-1", request: SessionBranchRequest(position: 3, expectedSnapshot: "proof"), metadata: metadata())
        #expect(branch.metadata.sessionID == "branch-1")
        #expect(try jsonObject(state.lastRequest!.httpBody!).asNSDictionary.isEqual(to: ["position": 3, "expected_snapshot": "proof"]))
    }

    @Test func serverMetadataBlocksHistoricalWritesAndIdentityMismatch() async throws {
        let state = MockURLProtocolState()
        let client = try client(state: state)
        state.metadataResponse = """
        {"metadata":{"session_id":"session-1","agent_name":"agent","historical":true,"execution_available":false,"capabilities":{"execution_available":false}},"status":{"session_id":"session-1","agent_name":"agent","state":"idle","pending":0}}
        """
        await #expect(throws: SessionControlsError.sessionNotExecutable) {
            try await client.setSessionTitle("session-1", title: "must not write")
        }
        #expect(state.lastRequest?.url?.path.hasSuffix("/status") == true)

        state.clearLastRequest()
        state.metadataResponse = """
        {"metadata":{"session_id":"different","agent_name":"agent","execution_available":true,"capabilities":{"execution_available":true,"session_editing":true}},"status":{"session_id":"different","agent_name":"agent","state":"idle","pending":0}}
        """
        await #expect(throws: SessionControlsError.identityMismatch) {
            try await client.setSessionTitle("session-1", title: "wrong target")
        }
        #expect(state.lastRequest?.url?.path.hasSuffix("/status") == true)
    }

    private var metadataJSON: String {
        "{\"session_id\":\"session-1\",\"agent_name\":\"agent\",\"execution_available\":true,\"capabilities\":{\"execution_available\":true,\"session_editing\":true,\"todo_editing\":true,\"branching\":true,\"model_switching\":true,\"thinking_levels\":true,\"compaction\":true,\"pending_message_removal\":true}}"
    }

    private func metadata(json: String? = nil) throws -> SessionControlMetadata {
        try JSONDecoder().decode(SessionControlMetadata.self, from: Data((json ?? metadataJSON).utf8))
    }

    private func client(state: MockURLProtocolState) throws -> APIClient {
        let config = URLSessionConfiguration.ephemeral
        config.protocolClasses = [MockURLProtocol.self]
        state.metadataResponse = "{\"metadata\":\(metadataJSON),\"status\":{\"session_id\":\"session-1\",\"agent_name\":\"agent\",\"state\":\"idle\",\"pending\":0}}"
        let host = UUID().uuidString.lowercased() + ".example"
        MockURLProtocol.register(state, for: host)
        return try APIClient(config: InstanceConfig(id: UUID(), name: "Test", baseURL: "https://\(host)"), token: "secret", session: URLSession(configuration: config))
    }

    private func jsonObject(_ data: Data) throws -> [String: Any] {
        try #require(JSONSerialization.jsonObject(with: data) as? [String: Any])
    }
}

private extension Dictionary where Key == String, Value == Any {
    var asNSDictionary: NSDictionary { self as NSDictionary }
}

private final class MockURLProtocolState: @unchecked Sendable {
    private let lock = NSLock()
    private var requestStorage: URLRequest?
    private var responseStorage: (@Sendable (URLRequest) -> (Int, Data))?
    private var metadataStorage = "{\"metadata\":{\"session_id\":\"session-1\",\"agent_name\":\"agent\",\"execution_available\":true,\"capabilities\":{\"execution_available\":true,\"session_editing\":true,\"todo_editing\":true,\"branching\":true,\"model_switching\":true,\"thinking_levels\":true,\"compaction\":true,\"pending_message_removal\":true,\"model_catalog_refresh\":true}},\"status\":{\"session_id\":\"session-1\",\"agent_name\":\"agent\",\"state\":\"idle\",\"pending\":0}}"
    var lastRequest: URLRequest? { lock.lock(); defer { lock.unlock() }; return requestStorage }
    var metadataResponse: String {
        get { lock.lock(); defer { lock.unlock() }; return metadataStorage }
        set { lock.lock(); defer { lock.unlock() }; metadataStorage = newValue }
    }
    var response: (@Sendable (URLRequest) -> (Int, Data))? {
        get { lock.lock(); defer { lock.unlock() }; return responseStorage }
        set { lock.lock(); defer { lock.unlock() }; responseStorage = newValue }
    }
    func record(_ request: URLRequest) { lock.lock(); defer { lock.unlock() }; requestStorage = request }
    func clearLastRequest() { lock.lock(); defer { lock.unlock() }; requestStorage = nil }
}

private final class MockURLProtocol: URLProtocol, @unchecked Sendable {
    private static let registryLock = NSLock()
    nonisolated(unsafe) private static var states: [String: MockURLProtocolState] = [:]
    static func register(_ state: MockURLProtocolState, for host: String) {
        registryLock.lock(); defer { registryLock.unlock() }; states[host] = state
    }
    private static func state(for request: URLRequest) -> MockURLProtocolState? {
        registryLock.lock(); defer { registryLock.unlock() }; return request.url?.host.flatMap { states[$0] }
    }
    override class func canInit(with request: URLRequest) -> Bool { true }
    override class func canonicalRequest(for request: URLRequest) -> URLRequest { request }
    override func startLoading() {
        guard let state = Self.state(for: request) else { client?.urlProtocol(self, didFailWithError: APIError("Missing test response.")); return }
        var capturedRequest = request
        if capturedRequest.httpBody == nil, let stream = capturedRequest.httpBodyStream {
            stream.open()
            defer { stream.close() }
            var body = Data()
            let buffer = UnsafeMutablePointer<UInt8>.allocate(capacity: 4096)
            defer { buffer.deallocate() }
            while stream.hasBytesAvailable {
                let count = stream.read(buffer, maxLength: 4096)
                if count <= 0 { break }
                body.append(buffer, count: count)
            }
            capturedRequest.httpBody = body
        }
        state.record(capturedRequest)
        let result: (Int, Data)
        if request.url?.path.hasSuffix("/status") == true {
            result = (200, Data(state.metadataResponse.utf8))
        } else {
            result = state.response?(request) ?? (500, Data())
        }
        let (status, data) = result
        let response = HTTPURLResponse(url: request.url!, statusCode: status, httpVersion: "HTTP/1.1", headerFields: ["Content-Type": "application/json"])!
        client?.urlProtocol(self, didReceive: response, cacheStoragePolicy: .notAllowed)
        if !data.isEmpty { client?.urlProtocol(self, didLoad: data) }
        client?.urlProtocolDidFinishLoading(self)
    }
    override func stopLoading() {}
}

extension SessionControlsTests {
    @Test func inspectorPermissionsDecodeSourceAndSessionWireCasingWithoutLeakingDefinition() throws {
        let data = Data(#"{"policy":"safe-auto","tools_approved":false,"source":{"Allow":["read_file"],"Ask":["shell"],"Deny":["delete"]},"session":{"allow":["custom"],"ask":[],"deny":[]}}"#.utf8)
        let wire = try JSONDecoder().decode(SessionPermissionsWireResponse.self, from: data)
        #expect(wire.policy == "safe-auto")
        #expect(wire.source?.allow == ["read_file"])
        #expect(wire.source?.deny == ["delete"])
        #expect(wire.session?.allow == ["custom"])
        let encoded = try JSONEncoder().encode(SessionPermissionsPatch(permissions: wire.session!))
        let body = try #require(JSONSerialization.jsonObject(with: encoded) as? [String: Any])
        let permissions = try #require(body["permissions"] as? [String: Any])
        #expect(permissions["allow"] as? [String] == ["custom"])
        #expect(permissions["Allow"] == nil)
    }

    @Test func inspectorToolsetsRespectRestartabilityAndAgentConfigExposesNoDefinition() throws {
        let tools = try JSONDecoder().decode(SessionToolsWireResponse.self, from: Data(#"{"tools":[{"name":"shell","category":"execution","description":"Run a command","parameters":{},"annotations":{},"outputSchema":{}}],"statuses":[{"name":"local","kind":"shell","description":"Local tools","state":2,"restart_count":2,"restartable":true}]}"#.utf8))
        #expect(tools.tools.first?.name == "shell")
        #expect(tools.statuses.first?.restartable == true)
        let config = try JSONDecoder().decode(SessionAgentConfigResponse.self, from: Data(#"{"session_id":"s","agent_name":"root","info":{"SubAgents":["child"],"Toolsets":[{"Name":"shell","Kind":"local","State":"ready","Tools":["shell"]}],"MaxIterations":8},"definition":{"instructions":"must not be projected"}}"#.utf8))
        #expect(config.info.objectValue?["SubAgents"]?.arrayValue?.first?.stringValue == "child")
        #expect(config.definition != nil)
    }
}
