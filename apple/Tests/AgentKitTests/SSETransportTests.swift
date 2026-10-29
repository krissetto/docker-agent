import Foundation
import Testing
@testable import AgentKit

struct SSETransportTests {
    @Test func tailEventStreamNegotiatesTailProjectionWithoutReplayCursor() async throws {
        let state = TransportStubState()
        state.plan = .stalled(prefix: Data())
        let client = try makeClient(state)
        let (_, _) = try await client.eventStream(sessionID: "session-id", since: nil, epoch: nil, tailProjection: true)
        let query = URLComponents(url: try #require(state.request?.url), resolvingAgainstBaseURL: false)?.queryItems ?? []
        #expect(query.map(\.name) == ["projection", "max_items", "max_bytes", "max_item_bytes"])
        #expect(query.map(\.value) == ["tail", "50", "2097152", "131072"])
        #expect(!query.contains(where: { $0.name == "since" || $0.name == "since_epoch" }))
    }

    @Test func tailEventStreamRejectsReplayCursorCombination() async throws {
        let state = TransportStubState()
        let client = try makeClient(state)
        await #expect(throws: APIError.self) {
            try await client.eventStream(sessionID: "session-id", since: 1, epoch: "e", tailProjection: true)
        }
        #expect(!state.started)
    }

    @Test func eventRequestTimeoutBoundsStalledAsyncBytesAfterHeaders() async throws {
        let state = TransportStubState()
        state.plan = .stalled(prefix: Data(": ping\n\n".utf8))
        let client = try makeClient(state)
        var headersAt: ContinuousClock.Instant?
        var prefixBytesSeen = false
        let started = ContinuousClock.now
        var timeoutCode: URLError.Code?
        do {
            let (bytes, _) = try await client.eventStream(sessionID: "session-id", since: nil, epoch: nil, idleTimeout: 0.5)
            #expect(state.started)
            #expect(state.headersDelivered)
            headersAt = state.headersTime
            var parser = SSEParser()
            for try await byte in bytes {
                prefixBytesSeen = true
                _ = try parser.append(Data([byte]))
            }
        } catch let error as URLError where error.code == .timedOut {
            timeoutCode = error.code
        }
        #expect(state.headersDelivered, "the request must reach response headers before timeout")
        #expect(prefixBytesSeen, "the response body must deliver an SSE heartbeat before idling")
        #expect(timeoutCode == .timedOut)
        #expect(ContinuousClock.now - (headersAt ?? started) >= .milliseconds(450))
        try await waitUntil { state.stopLoadingCount > 0 }
    }

    @Test func heartbeatCommentBytesResetRequestInactivityAndDoNotCreateFrames() async throws {
        let state = TransportStubState()
        state.plan = .heartbeats((0..<12).map { Data(": ping-\($0)\n\n".utf8) }, spacing: .milliseconds(100))
        let client = try makeClient(state)
        let (bytes, _) = try await client.eventStream(sessionID: "session-id", since: nil, epoch: nil, idleTimeout: 0.35)
        #expect(state.headersDelivered)
        let counter = StreamFrameCounter()
        let task = Task {
            var parser = SSEParser()
            for try await byte in bytes { counter.add(try parser.append(Data([byte])).count) }
        }
        try await waitUntil(timeout: .seconds(2)) { state.heartbeatCount >= 8 }
        #expect(state.stopLoadingCount == 0, "heartbeat bytes should extend the stream past several idle-timeout periods")
        task.cancel()
        _ = await task.result
        try await waitUntil { state.stopLoadingCount > 0 }
        #expect(counter.value == 0, "SSE comments must not surface as data frames")
    }

    @Test func cancelingTaskWhileAsyncBytesWaitsForBodyStopsUnderlyingRequest() async throws {
        let state = TransportStubState()
        state.plan = .stalled(prefix: Data())
        let client = try makeClient(state, token: "synthetic")
        let task = Task {
            let (bytes, _) = try await client.eventStream(sessionID: "session-id", since: 4, epoch: "epoch-a", idleTimeout: 45)
            for try await _ in bytes {}
        }
        try await waitUntil { state.started }
        task.cancel()
        _ = try? await task.value
        try await waitUntil { state.stopLoadingCount > 0 }
        let request = try #require(state.request)
        #expect(request.url?.query == "since=4&since_epoch=epoch-a")
        #expect(request.value(forHTTPHeaderField: "Accept") == "text/event-stream")
        #expect(request.value(forHTTPHeaderField: "Authorization") == "Bearer synthetic")
    }

    @Test func unauthorizedEventStreamFailsWithoutAutomaticTransportRetry() async throws {
        let state = TransportStubState()
        state.plan = .response(401, Data(#"{"message":"unauthorized"}"#.utf8))
        let client = try makeClient(state, token: "synthetic-token")
        await #expect(throws: APIError.self) {
            try await client.eventStream(sessionID: "session-id", since: 1, epoch: "epoch")
        }
        #expect(state.request?.value(forHTTPHeaderField: "Authorization") == "Bearer synthetic-token")
        #expect(state.requestsStarted == 1)
        #expect(state.stopLoadingCount > 0)
    }

    private func makeClient(_ state: TransportStubState, token: String = "") throws -> APIClient {
        let configuration = URLSessionConfiguration.ephemeral
        configuration.protocolClasses = [TransportURLProtocol.self]
        TransportURLProtocol.install(state, for: state.host)
        return try APIClient(config: InstanceConfig(id: UUID(), name: "SSE", baseURL: "https://\(state.host)"), token: token, session: URLSession(configuration: configuration))
    }

    private func waitUntil(timeout: Duration = .seconds(2), _ predicate: @escaping () -> Bool) async throws {
        let started = ContinuousClock.now
        while !predicate() {
            if ContinuousClock.now - started > timeout { throw APIError("Timed out waiting for URLProtocol request teardown.") }
            try await Task.sleep(for: .milliseconds(10))
        }
    }
}

private enum TransportPlan: Sendable {
    case stalled(prefix: Data)
    case heartbeats([Data], spacing: Duration)
    case response(Int, Data)
}

private final class StreamFrameCounter: @unchecked Sendable {
    private let lock = NSLock()
    private var storage = 0
    var value: Int { lock.lock(); defer { lock.unlock() }; return storage }
    func add(_ count: Int) { lock.lock(); defer { lock.unlock() }; storage += count }
}

private final class TransportStubState: @unchecked Sendable {
    let host = "sse-\(UUID().uuidString.lowercased()).example"
    private let lock = NSLock()
    private var planStorage: TransportPlan = .stalled(prefix: Data())
    private var requestStorage: URLRequest?
    private var startedStorage = 0
    private var stoppedStorage = 0
    private var headersStorage = false
    private var heartbeatStorage = 0
    private var headersTimeStorage: ContinuousClock.Instant?
    var plan: TransportPlan { get { lock.lock(); defer { lock.unlock() }; return planStorage } set { lock.lock(); defer { lock.unlock() }; planStorage = newValue } }
    var request: URLRequest? { lock.lock(); defer { lock.unlock() }; return requestStorage }
    var requestsStarted: Int { lock.lock(); defer { lock.unlock() }; return startedStorage }
    var started: Bool { requestsStarted > 0 }
    var headersDelivered: Bool { lock.lock(); defer { lock.unlock() }; return headersStorage }
    var heartbeatCount: Int { lock.lock(); defer { lock.unlock() }; return heartbeatStorage }
    var headersTime: ContinuousClock.Instant? { lock.lock(); defer { lock.unlock() }; return headersTimeStorage }
    func markHeaders() { lock.lock(); defer { lock.unlock() }; headersStorage = true; headersTimeStorage = ContinuousClock.now }
    var isStopped: Bool { stopLoadingCount > 0 }
    func markHeartbeat() { lock.lock(); defer { lock.unlock() }; heartbeatStorage += 1 }
    var stopLoadingCount: Int { lock.lock(); defer { lock.unlock() }; return stoppedStorage }
    func begin(_ request: URLRequest) -> TransportPlan { lock.lock(); defer { lock.unlock() }; requestStorage = request; startedStorage += 1; return planStorage }
    func stop() { lock.lock(); defer { lock.unlock() }; stoppedStorage += 1 }
}

private final class TransportURLProtocol: URLProtocol, @unchecked Sendable {
    private static let lock = NSLock()
    nonisolated(unsafe) private static var states: [String: TransportStubState] = [:]
    static func install(_ state: TransportStubState, for host: String) { lock.lock(); defer { lock.unlock() }; states[host] = state }
    override class func canInit(with request: URLRequest) -> Bool { true }
    override class func canonicalRequest(for request: URLRequest) -> URLRequest { request }
    override func startLoading() {
        guard let host = request.url?.host else { client?.urlProtocol(self, didFailWithError: URLError(.badURL)); return }
        Self.lock.lock(); let state = Self.states[host]; Self.lock.unlock()
        guard let state else { client?.urlProtocol(self, didFailWithError: URLError(.cannotFindHost)); return }
        switch state.begin(request) {
        case .stalled(let prefix):
            sendHeaders(status: 200, contentType: "text/event-stream", state: state)
            if !prefix.isEmpty { client?.urlProtocol(self, didLoad: prefix) }
        case .heartbeats(let chunks, let spacing):
            sendHeaders(status: 200, contentType: "text/event-stream", state: state)
            let delay = Double(spacing.components.seconds) + Double(spacing.components.attoseconds) / 1e18
            for (index, chunk) in chunks.enumerated() {
                DispatchQueue.global().asyncAfter(deadline: .now() + delay * Double(index + 1)) { [self, state] in
                    guard !state.isStopped else { return }
                    state.markHeartbeat()
                    client?.urlProtocol(self, didLoad: chunk)
                }
            }
        case .response(let status, let data):
            sendHeaders(status: status, contentType: "application/json", state: state)
            if !data.isEmpty { client?.urlProtocol(self, didLoad: data) }
            client?.urlProtocolDidFinishLoading(self)
        }
    }
    private func sendHeaders(status: Int, contentType: String, state: TransportStubState) {
        state.markHeaders()
        let headers = contentType == "text/event-stream" ? ["Content-Type": contentType, "Content-Length": "1048576"] : ["Content-Type": contentType]
        let response = HTTPURLResponse(url: request.url!, statusCode: status, httpVersion: "HTTP/1.1", headerFields: headers)!
        client?.urlProtocol(self, didReceive: response, cacheStoragePolicy: .notAllowed)
    }
    override func stopLoading() {
        guard let host = request.url?.host else { return }
        Self.lock.lock(); let state = Self.states[host]; Self.lock.unlock(); state?.stop()
    }
}
