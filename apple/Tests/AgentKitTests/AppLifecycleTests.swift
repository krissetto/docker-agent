import Foundation
import Testing
import AgentKit
@testable import DockerAgentApple

@MainActor
struct AppLifecycleTests {
    @Test func twoForegroundScenesKeepObservationUntilLastSceneBackgroundsAndResumeAtCursor() async throws {
        let setup = try makeModel()
        let ref = SessionRef(serverID: setup.serverID, sessionID: "session-demo-001")
        setup.model.clients[setup.serverID] = setup.client
        setup.model.servers = [ServerProjection(id: setup.serverID, name: "lifecycle", baseURL: "https://\(setup.host)", identity: setup.identity, state: .live)]
        let snapshot = try fixtureSnapshot(epoch: "epoch-a", cursor: 7)
        setup.model.openSessions[ref] = snapshot.project(ref: ref, connection: .live)
        setup.model.sessionLeases[ref] = 1
        setup.state.sseMode = .stalled

        let firstScene = UUID()
        let secondScene = UUID()
        setup.model.setSceneActive(true, for: firstScene)
        try await waitUntil { setup.state.requestsFor("/api/v2/sessions/session-demo-001/events").count == 1 }
        setup.model.setSceneActive(true, for: secondScene)
        setup.model.setSceneActive(false, for: firstScene)
        #expect(setup.model.streamTasks[ref] != nil)
        #expect(setup.state.stopLoadingCount == 0)

        setup.model.setSceneActive(false, for: secondScene)
        try await waitUntil { setup.state.stopLoadingCount > 0 }
        #expect(setup.model.openSessions[ref]?.cursor == 7)
        #expect(setup.model.sessionLeases[ref] == 1)
        #expect(setup.state.requests.filter { ($0.url?.path ?? "").hasSuffix("/cancel") || ($0.url?.path ?? "").hasSuffix("/messages") }.count == 0)

        setup.state.sseMode = .stalled
        setup.model.setSceneActive(true, for: firstScene)
        try await waitUntil { setup.state.requestsFor("/api/v2/sessions/session-demo-001/events").count == 2 }
        let eventRequests = setup.state.requests.filter { ($0.url?.path ?? "").hasSuffix("/events") }
        #expect(URLComponents(url: eventRequests[1].url!, resolvingAgainstBaseURL: false)?.queryItems?.first(where: { $0.name == "since" })?.value == "7")
        #expect(URLComponents(url: eventRequests[1].url!, resolvingAgainstBaseURL: false)?.queryItems?.first(where: { $0.name == "since_epoch" })?.value == "epoch-a")
        #expect(setup.state.requests.filter { ($0.url?.path ?? "").hasSuffix("/open-view") || ($0.httpMethod == "PATCH" && $0.url?.path == "/api/v2/sessions/session-demo-001") }.count == 0)
        #expect(setup.state.requests.filter { ($0.url?.path ?? "").hasSuffix("/messages") || ($0.url?.path ?? "").hasSuffix("/cancel") }.count == 0)
        setup.model.setSceneActive(false, for: firstScene)
        try await waitUntil { setup.state.stopLoadingCount > 1 }
    }

    @Test func unauthorizedStreamStopsRetriesAndOnlyExplicitReconnectResumes() async throws {
        let setup = try makeModel()
        let ref = SessionRef(serverID: setup.serverID, sessionID: "session-demo-001")
        setup.model.clients[setup.serverID] = setup.client
        setup.model.servers = [ServerProjection(id: setup.serverID, name: "lifecycle", baseURL: "https://\(setup.host)", identity: setup.identity, state: .live)]
        setup.model.openSessions[ref] = try fixtureSnapshot(epoch: "epoch-a", cursor: 0).project(ref: ref, connection: .live)
        setup.model.sessionLeases[ref] = 1
        setup.state.sseMode = .unauthorized
        setup.model.setSceneActive(true, for: UUID())
        try await waitUntil { setup.model.servers.first?.state == .unauthorized }
        #expect(setup.model.openSessions[ref]?.connection == .unauthorized)
        try await Task.sleep(for: .milliseconds(50))
        #expect(setup.state.requestsFor("/api/v2/sessions/session-demo-001/events").count == 1)

        setup.state.sseMode = .stalled
        setup.state.identityInstanceID = setup.identity.instanceID
        let sceneID = UUID()
        setup.model.setSceneActive(true, for: sceneID)
        try await setup.model.reconnectServer(setup.serverID)
        try await waitUntil { setup.state.requestsFor("/api/v2/sessions/session-demo-001/events").count == 2 }
        #expect(setup.model.servers.first?.identity?.instanceID == setup.identity.instanceID)
        #expect(setup.state.requests.filter { ($0.url?.path ?? "").hasSuffix("/cancel") || ($0.url?.path ?? "").hasSuffix("/messages") }.count == 0)
        setup.model.setSceneActive(false, for: sceneID)
        try await waitUntil { setup.state.stopLoadingCount > 0 }
    }

    private func makeModel() throws -> Setup {
        let serverID = UUID()
        let host = "lifecycle-\(UUID().uuidString.lowercased()).example"
        let root = FileManager.default.temporaryDirectory.appending(path: "DockerAgentLifecycleTests/\(UUID().uuidString)")
        let config = InstanceConfig(id: serverID, name: "lifecycle", baseURL: "https://\(host)", instanceID: "lifecycle-instance")
        let state = LifecycleURLState(host: host)
        let configStore = InstanceConfigStore(fileURL: root.appending(path: "instances.json"))
        try configStore.upsert(config)
        let sessionConfiguration = URLSessionConfiguration.ephemeral
        sessionConfiguration.protocolClasses = [LifecycleURLProtocol.self]
        let urlSession = URLSession(configuration: sessionConfiguration)
        let client = try APIClient(config: config, token: "", session: urlSession)
        let identity = ServerIdentity(version: 1, sessionAPIVersion: 2, instanceID: "lifecycle-instance", ready: true)
        let model = AppModel(tokenStore: LifecycleTokenStore(), configStore: configStore, outbox: try DurableSendOutbox(fileURL: root.appending(path: "outbox.json")), reviewDrafts: ReviewDraftStore(fileURL: root.appending(path: "review-drafts.json")), urlSession: urlSession, autoConnect: false)
        LifecycleURLProtocol.install(state)
        state.identityInstanceID = identity.instanceID
        state.serverIdentityData = try JSONEncoder().encode(identity)
        return Setup(model: model, client: client, state: state, serverID: serverID, host: host, identity: identity)
    }

    private func fixtureSnapshot(epoch: String, cursor: UInt64) throws -> SessionSnapshotWire {
        let object: [String: Any] = ["session": ["id": "session-demo-001"], "status": ["session_id": "session-demo-001", "agent_name": "root", "state": "running"], "epoch": epoch, "cursor": cursor]
        return try JSONDecoder().decode(SessionSnapshotWire.self, from: JSONSerialization.data(withJSONObject: object))
    }

    private func waitUntil(timeout: Duration = .seconds(2), _ predicate: @escaping @MainActor () -> Bool) async throws {
        let start = ContinuousClock.now
        while !predicate() {
            if ContinuousClock.now - start > timeout { throw APIError("Timed out waiting for lifecycle transition.") }
            try await Task.sleep(for: .milliseconds(10))
        }
    }
}

@MainActor
private struct Setup {
    let model: AppModel
    let client: APIClient
    let state: LifecycleURLState
    let serverID: UUID
    let host: String
    let identity: ServerIdentity
}

private actor LifecycleTokenStore: TokenStoring {
    func read(for id: UUID) async throws -> String? { nil }
    func write(_ token: String, for id: UUID) async throws {}
    func delete(for id: UUID) async throws {}
}

private final class LifecycleURLState: @unchecked Sendable {
    enum SSEMode { case stalled, finite, unauthorized }
    let host: String
    private let lock = NSLock()
    private var requestStorage: [URLRequest] = []
    private var stoppedStorage = 0
    var sseMode: SSEMode = .stalled { didSet { lock.lock(); defer { lock.unlock() }; modeStorage = sseMode } }
    private var modeStorage: SSEMode = .stalled
    var identityInstanceID = "lifecycle-instance"
    var serverIdentityData = Data()
    init(host: String) { self.host = host }
    var requests: [URLRequest] { lock.lock(); defer { lock.unlock() }; return requestStorage }
    var stopLoadingCount: Int { lock.lock(); defer { lock.unlock() }; return stoppedStorage }
    func requestsFor(_ path: String) -> [URLRequest] { requests.filter { $0.url?.path == path } }
    func stop() { lock.lock(); defer { lock.unlock() }; stoppedStorage += 1 }
    func handle(_ request: URLRequest) -> (Int, Data, String, Bool) {
        lock.lock(); requestStorage.append(request); let mode = modeStorage; lock.unlock()
        let path = request.url?.path ?? ""
        if path == "/api/v2/server" { return (200, serverIdentityData, "application/json", true) }
        if path.hasSuffix("/events") {
            switch mode {
            case .stalled: return (200, Data(), "text/event-stream", false)
            case .finite: return (200, Data("event: ready\ndata: {\"version\":2,\"type\":\"ready\",\"cursor\":7}\n\n".utf8), "text/event-stream", true)
            case .unauthorized: return (401, Data(#"{"message":"unauthorized"}"#.utf8), "application/json", true)
            }
        }
        return (404, Data(#"{"message":"not found"}"#.utf8), "application/json", true)
    }
}

private final class LifecycleURLProtocol: URLProtocol, @unchecked Sendable {
    private static let lock = NSLock()
    nonisolated(unsafe) private static var states: [String: LifecycleURLState] = [:]
    private var state: LifecycleURLState?
    static func install(_ state: LifecycleURLState) { lock.lock(); defer { lock.unlock() }; states[state.host] = state }
    override class func canInit(with request: URLRequest) -> Bool { true }
    override class func canonicalRequest(for request: URLRequest) -> URLRequest { request }
    override func startLoading() {
        guard let host = request.url?.host else { client?.urlProtocol(self, didFailWithError: URLError(.badURL)); return }
        Self.lock.lock(); let state = Self.states[host]; Self.lock.unlock(); self.state = state
        guard let state else { client?.urlProtocol(self, didFailWithError: URLError(.cannotFindHost)); return }
        let (status, data, type, finish) = state.handle(request)
        let response = HTTPURLResponse(url: request.url!, statusCode: status, httpVersion: "HTTP/1.1", headerFields: ["Content-Type": type])!
        client?.urlProtocol(self, didReceive: response, cacheStoragePolicy: .notAllowed)
        if !data.isEmpty { client?.urlProtocol(self, didLoad: data) }
        if finish { client?.urlProtocolDidFinishLoading(self) }
    }
    override func stopLoading() { state?.stop() }
}
