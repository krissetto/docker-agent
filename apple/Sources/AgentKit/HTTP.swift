import Foundation
import OSLog
import Security

public enum URLPolicy {
    public static func sameOrigin(_ lhs: String, _ rhs: String) -> Bool {
        guard let a = URLComponents(string: lhs), let b = URLComponents(string: rhs),
              a.scheme?.lowercased() == b.scheme?.lowercased(), a.host?.lowercased() == b.host?.lowercased() else { return false }
        func port(_ c: URLComponents) -> Int? {
            c.port ?? ((c.scheme?.lowercased() == "https") ? 443 : (c.scheme?.lowercased() == "http" ? 80 : nil))
        }
        return port(a) == port(b)
    }
    public static func validate(_ raw: String, allowsLocalHTTP: Bool) throws -> URL {
        guard let components = URLComponents(string: raw), let scheme = components.scheme?.lowercased(), let host = components.host, !host.isEmpty else { throw APIError("Enter a valid server URL.") }
        guard components.user == nil, components.password == nil, components.fragment == nil, components.query == nil else { throw APIError("Server URL must not contain credentials or a fragment.") }
        switch scheme {
        case "https": break
        case "http":
            guard allowsLocalHTTP, isLoopback(host) else { throw APIError("HTTP is allowed only for an explicitly enabled loopback development server.") }
        default: throw APIError("Use an HTTPS server URL.")
        }
        guard let url = components.url else { throw APIError("Enter a valid server URL.") }
        return url
    }
    private static func isLoopback(_ host: String) -> Bool {
        let h = host.lowercased().trimmingCharacters(in: CharacterSet(charactersIn: "[]"))
        return h == "localhost" || h == "127.0.0.1" || h == "::1"
    }
}

public struct InstanceConfig: Codable, Sendable, Equatable, Identifiable {
    public var id: UUID
    public var name: String
    public var baseURL: String
    public var allowsLocalHTTP: Bool
    public var instanceID: String?
    public var capabilities: [String]
    public var preferredSource: String?
    public init(id: UUID, name: String, baseURL: String, allowsLocalHTTP: Bool = false, instanceID: String? = nil, capabilities: [String] = [], preferredSource: String? = nil) {
        self.id = id; self.name = name; self.baseURL = baseURL; self.allowsLocalHTTP = allowsLocalHTTP; self.instanceID = instanceID; self.capabilities = capabilities; self.preferredSource = preferredSource
    }
}

public protocol TokenStoring: Sendable {
    func read(for id: UUID) async throws -> String?
    func write(_ token: String, for id: UUID) async throws
    func delete(for id: UUID) async throws
}

public actor KeychainTokenStore: TokenStoring {
    private let service: String
    public init(service: String = "com.docker.docker-agent.apple") { self.service = service }
    public func read(for id: UUID) throws -> String? {
        let query: [String: Any] = [kSecClass as String: kSecClassGenericPassword, kSecAttrService as String: service, kSecAttrAccount as String: id.uuidString, kSecReturnData as String: true, kSecMatchLimit as String: kSecMatchLimitOne]
        
        var item: CFTypeRef?
        let status = SecItemCopyMatching(query as CFDictionary, &item)
        if status == errSecItemNotFound { return nil }
        guard status == errSecSuccess, let data = item as? Data, let token = String(data: data, encoding: .utf8) else { throw APIError("Could not read the saved server credential.") }
        return token
    }
    public func write(_ token: String, for id: UUID) throws {
        let key: [String: Any] = [kSecClass as String: kSecClassGenericPassword, kSecAttrService as String: service, kSecAttrAccount as String: id.uuidString]
        let attributes: [String: Any] = [kSecValueData as String: Data(token.utf8), kSecAttrAccessible as String: kSecAttrAccessibleWhenUnlockedThisDeviceOnly, kSecAttrSynchronizable as String: false]
        let update = SecItemUpdate(key as CFDictionary, attributes as CFDictionary)
        if update == errSecItemNotFound {
            var add = key; add.merge(attributes) { _, new in new }
            let status = SecItemAdd(add as CFDictionary, nil)
            guard status == errSecSuccess else { throw APIError("Could not save the server credential.") }
        } else if update != errSecSuccess { throw APIError("Could not update the server credential.") }
    }
    public func delete(for id: UUID) throws {
        let key: [String: Any] = [kSecClass as String: kSecClassGenericPassword, kSecAttrService as String: service, kSecAttrAccount as String: id.uuidString]
        let status = SecItemDelete(key as CFDictionary)
        guard status == errSecSuccess || status == errSecItemNotFound else { throw APIError("Could not remove the saved server credential.") }
    }
}

public final class APIClient: @unchecked Sendable {
    private static let responseLogger = Logger(subsystem: "dev.dockeragent.apple", category: "network")
    private static let maximumJSONResponseBytes = 16 * 1024 * 1024
    public static let maximumSessionViewResponseBytes = 48 * 1024 * 1024
    public static let maximumMetadataSessionViewResponseBytes = 4 * 1024 * 1024
    public let config: InstanceConfig
    private let token: String
    private let baseURL: URL
    private let session: URLSession
    private let decoder = JSONDecoder()
    private let encoder = JSONEncoder()
    public init(config: InstanceConfig, token: String, session: URLSession? = nil) throws {
        self.config = config; self.token = token
        self.baseURL = try URLPolicy.validate(config.baseURL, allowsLocalHTTP: config.allowsLocalHTTP)
        let configuration = URLSessionConfiguration.ephemeral
        configuration.httpCookieStorage = nil; configuration.urlCache = nil; configuration.requestCachePolicy = .reloadIgnoringLocalCacheData
        self.session = session ?? URLSession(configuration: configuration, delegate: RedirectGuard(), delegateQueue: nil)
    }
    public func request<T: Decodable>(_ type: T.Type, method: String = "GET", path: String, query: [URLQueryItem] = [], body: Encodable? = nil, accepted: Set<Int> = [200]) async throws -> T {
        try await request(type, method: method, path: path, query: query, body: body, accepted: accepted,
                          maximumResponseBytes: Self.maximumJSONResponseBytes)
    }
    private func request<T: Decodable>(_ type: T.Type, method: String, path: String, query: [URLQueryItem], body: Encodable?, accepted: Set<Int>, maximumResponseBytes: Int) async throws -> T {
        let (responseData, response) = try await send(method: method, path: path, query: query, body: body, maximumResponseBytes: maximumResponseBytes)
        guard accepted.contains(response.statusCode) else { throw decodeError(responseData, status: response.statusCode) }
        do { return try decoder.decode(T.self, from: responseData) }
        catch { throw APIError("The server returned an invalid response.", statusCode: response.statusCode) }
    }
    public func prepareSessionView(_ sessionID: String, metadataOnly: Bool = false) async throws -> PreparedViewResponse {
        try await requestSessionView(PreparedViewResponse.self, sessionID: sessionID,
                                     view: "prepare-info", metadataOnly: metadataOnly, body: nil)
    }
    public func sessionHistoryTail(_ sessionID: String, maxItems: Int = 50, maxBytes: Int = 2 * 1024 * 1024, maxItemBytes: Int = 128 * 1024) async throws -> SessionSnapshotWire {
        try validateHistoryLimits(maxItems: maxItems, maxBytes: maxBytes, maxItemBytes: maxItemBytes)
        let snapshot = try await request(SessionSnapshotWire.self, method: "GET", path: Self.sessionPath(sessionID) + "/snapshot", query: historyQuery(maxItems: maxItems, maxBytes: maxBytes, maxItemBytes: maxItemBytes), body: nil, accepted: [200], maximumResponseBytes: maxBytes)
        try snapshot.validateTail(expectedSessionID: sessionID, maxItems: maxItems, maxItemBytes: maxItemBytes)
        return snapshot
    }

    public func sessionHistoryPage(_ sessionID: String, cursor: String, maxItems: Int = 50, maxBytes: Int = 2 * 1024 * 1024, maxItemBytes: Int = 128 * 1024) async throws -> SessionHistoryPageWire {
        try validateHistoryLimits(maxItems: maxItems, maxBytes: maxBytes, maxItemBytes: maxItemBytes)
        guard !cursor.isEmpty, cursor.utf8.count <= 8 * 1024 else { throw APIError("Invalid history cursor.") }
        let query = [URLQueryItem(name: "cursor", value: cursor)] + historyQuery(maxItems: maxItems, maxBytes: maxBytes, maxItemBytes: maxItemBytes)
        let page = try await request(SessionHistoryPageWire.self, method: "GET", path: Self.sessionPath(sessionID) + "/history", query: query, body: nil, accepted: [200], maximumResponseBytes: maxBytes)
        try page.validateResourceBounds(maxItems: maxItems, maxItemBytes: maxItemBytes)
        return page
    }

    private func historyQuery(maxItems: Int, maxBytes: Int, maxItemBytes: Int) -> [URLQueryItem] {
        [URLQueryItem(name: "projection", value: "tail"), URLQueryItem(name: "max_items", value: String(maxItems)), URLQueryItem(name: "max_bytes", value: String(maxBytes)), URLQueryItem(name: "max_item_bytes", value: String(maxItemBytes))]
    }

    private func validateHistoryLimits(maxItems: Int, maxBytes: Int, maxItemBytes: Int) throws {
        guard (1...100).contains(maxItems), (16 * 1024...4 * 1024 * 1024).contains(maxBytes), (1...min(256 * 1024, maxBytes)).contains(maxItemBytes) else {
            throw APIError("Invalid bounded history limits.")
        }
    }

    public func openSessionView(_ sessionID: String, confirmation: SessionViewConfirmation, metadataOnly: Bool = false) async throws -> OpenedViewResponse {
        try await requestSessionView(OpenedViewResponse.self, method: "PATCH", sessionID: sessionID,
                                     view: "open-view", metadataOnly: metadataOnly,
                                     body: OpenViewRequest(kind: "open_view", viewConfirmation: confirmation))
    }
    private func requestSessionView<T: Decodable>(_ type: T.Type, method: String = "GET", sessionID: String, view: String,
                                                  metadataOnly: Bool, body: Encodable?) async throws -> T {
        let query = [URLQueryItem(name: "view", value: view)] + (metadataOnly ? [URLQueryItem(name: "projection", value: "metadata")] : [])
        let maximum = metadataOnly ? Self.maximumMetadataSessionViewResponseBytes : Self.maximumSessionViewResponseBytes
        let response = try await request(type, method: method, path: Self.sessionViewPath(sessionID), query: query,
                                         body: body, accepted: [200], maximumResponseBytes: maximum)
        let marker: String?
        if let prepared = response as? PreparedViewResponse { marker = prepared.projection }
        else if let opened = response as? OpenedViewResponse { marker = opened.projection }
        else { marker = nil }
        guard !metadataOnly || marker == "metadata" else {
            let phase = view == "prepare-info" ? "prepare" : "open"
            throw APIError("The server did not confirm the requested metadata-only session view \(phase) projection.")
        }
        return response
    }
    private static func sessionViewPath(_ sessionID: String) -> String { sessionPath(sessionID) }
    private static func sessionPath(_ sessionID: String) -> String {
        let escaped = sessionID.addingPercentEncoding(withAllowedCharacters: CharacterSet(charactersIn: "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-._~")) ?? sessionID
        return "api/v2/sessions/\(escaped)"
    }
    public func noContent(method: String, path: String, query: [URLQueryItem] = [], body: Encodable? = nil, accepted: Set<Int> = [200, 202, 204]) async throws {
        let (data, response) = try await send(method: method, path: path, query: query, body: body)
        guard accepted.contains(response.statusCode) else { throw decodeError(data, status: response.statusCode) }
    }
    public func requestBytes(method: String = "GET", path: String, query: [URLQueryItem] = [], body: Encodable? = nil, maximumBytes: Int = 32 * 1024 * 1024, expectedContentType: String = "application/octet-stream") async throws -> Data {
        guard maximumBytes > 0 else { throw APIError("Invalid binary response size limit.") }
        guard !path.split(separator: "/").contains(where: { $0 == ".." }) else { throw APIError("Invalid API path.") }
        var url = path.split(separator: "/", omittingEmptySubsequences: true).reduce(baseURL) { current, segment in current.appending(path: String(segment), directoryHint: .notDirectory) }
        if !query.isEmpty {
            var components = URLComponents(url: url, resolvingAgainstBaseURL: false)!
            components.queryItems = query
            guard let result = components.url else { throw APIError("Invalid API request URL.") }
            url = result
        }
        var request = URLRequest(url: url)
        request.httpMethod = method
        request.timeoutInterval = 30
        request.setValue(expectedContentType, forHTTPHeaderField: "Accept")
        if !token.isEmpty { request.setValue("Bearer \(token)", forHTTPHeaderField: "Authorization") }
        if let body {
            let encoded = try JSONEncoder().encode(AnyEncodable(body))
            guard encoded.count <= 768 * 1024 else { throw APIError("Encoded attachment request exceeds the client's 768 KiB safety limit.") }
            request.httpBody = encoded
            request.setValue("application/json", forHTTPHeaderField: "Content-Type")
        }
        let bytes: URLSession.AsyncBytes
        let response: URLResponse
        (bytes, response) = try await session.bytes(for: request)
        guard let http = response as? HTTPURLResponse else { throw APIError("The server returned an invalid HTTP response.") }
        guard (200..<300).contains(http.statusCode) else {
            var errorData = Data()
            for try await byte in bytes {
                guard errorData.count < 16 * 1024 else { break }
                errorData.append(byte)
            }
            throw decodeError(errorData, status: http.statusCode)
        }
        let actualType = http.value(forHTTPHeaderField: "Content-Type")?.split(separator: ";", maxSplits: 1).first?.trimmingCharacters(in: .whitespacesAndNewlines).lowercased()
        guard actualType == expectedContentType.lowercased() else { throw APIError("The server returned an unexpected binary content type.") }
        if http.expectedContentLength >= 0, http.expectedContentLength > Int64(maximumBytes) {
            Self.logResponseLimit(endpoint: Self.endpointClass(method: method, path: path, query: query), status: http.statusCode,
                                  advertisedBytes: http.expectedContentLength, observedBytes: 0, limitBytes: maximumBytes)
            throw APIError("The server's binary response exceeds the client size limit.")
        }
        var data = Data()
        data.reserveCapacity(http.expectedContentLength >= 0 ? Int(http.expectedContentLength) : min(maximumBytes, 64 * 1024))
        for try await byte in bytes {
            guard data.count < maximumBytes else {
                Self.logResponseLimit(endpoint: Self.endpointClass(method: method, path: path, query: query), status: http.statusCode,
                                      advertisedBytes: http.expectedContentLength, observedBytes: data.count + 1, limitBytes: maximumBytes)
                throw APIError("The server's binary response exceeds the client size limit.")
            }
            data.append(byte)
        }
        return data
    }
    private func send(method: String, path: String, query: [URLQueryItem], body: Encodable?, maximumResponseBytes: Int = APIClient.maximumJSONResponseBytes) async throws -> (Data, HTTPURLResponse) {
        guard !path.split(separator: "/").contains(where: { $0 == ".." }) else { throw APIError("Invalid API path.") }
        var url = path.split(separator: "/", omittingEmptySubsequences: true).reduce(baseURL) { current, segment in current.appending(path: String(segment), directoryHint: .notDirectory) }
        if !query.isEmpty { var c = URLComponents(url: url, resolvingAgainstBaseURL: false)!; c.queryItems = query; guard let u = c.url else { throw APIError("Invalid API request URL.") }; url = u }
        var request = URLRequest(url: url); request.httpMethod = method; request.timeoutInterval = 30
        if !token.isEmpty { request.setValue("Bearer \(token)", forHTTPHeaderField: "Authorization") }
        if let body {
            let data = try JSONEncoder().encode(AnyEncodable(body)); guard data.count <= 4 * 1024 * 1024 else { throw APIError("Request body exceeds the 4 MiB client limit.") }; request.httpBody = data; request.setValue("application/json", forHTTPHeaderField: "Content-Type")
        }
        let bytes: URLSession.AsyncBytes
        let response: URLResponse
        (bytes, response) = try await session.bytes(for: request)
        guard let http = response as? HTTPURLResponse else { throw APIError("The server returned an invalid HTTP response.") }
        if http.expectedContentLength >= 0, http.expectedContentLength > Int64(maximumResponseBytes) {
            Self.logResponseLimit(endpoint: Self.endpointClass(method: method, path: path, query: query), status: http.statusCode,
                                  advertisedBytes: http.expectedContentLength, observedBytes: 0, limitBytes: maximumResponseBytes)
            throw Self.responseLimitError(maximumResponseBytes)
        }
        var data = Data()
        for try await byte in bytes {
            guard data.count < maximumResponseBytes else {
                Self.logResponseLimit(endpoint: Self.endpointClass(method: method, path: path, query: query), status: http.statusCode,
                                      advertisedBytes: http.expectedContentLength, observedBytes: data.count + 1,
                                      limitBytes: maximumResponseBytes)
                throw Self.responseLimitError(maximumResponseBytes)
            }
            data.append(byte)
        }
        return (data, http)
    }
    private static func endpointClass(method: String, path: String, query: [URLQueryItem]) -> String {
        let components = path.split(separator: "/")
        if components.suffix(1).first == "snapshot" { return "session_snapshot" }
        if components.count >= 4, components[0] == "api", components[1] == "v2", components[2] == "sessions" {
            if query.contains(where: { $0.name == "view" && $0.value == "prepare-info" }) { return "session_view_prepare_info" }
            if query.contains(where: { $0.name == "view" && $0.value == "open-view" }) { return "session_view_open_view" }
            return "session_api"
        }
        return "api"
    }
    private static func responseLimitError(_ maximumResponseBytes: Int) -> APIError {
        if maximumResponseBytes == maximumJSONResponseBytes {
            return APIError("Server response exceeded the 16 MiB client limit.")
        }
        if maximumResponseBytes == maximumMetadataSessionViewResponseBytes {
            return APIError("Session metadata projection exceeded the 4 MiB client limit.")
        }
        if maximumResponseBytes == maximumSessionViewResponseBytes {
            return APIError("The full session view exceeds the 48 MiB client limit. Update the server to use metadata-only session views.")
        }
        return APIError("Server response exceeded the configured client size limit.")
    }
    private static func logResponseLimit(endpoint: String, status: Int, advertisedBytes: Int64, observedBytes: Int, limitBytes: Int) {
        responseLogger.error("response limit exceeded endpoint=\(endpoint, privacy: .public) status=\(status, privacy: .public) advertised_bytes=\(advertisedBytes, privacy: .public) observed_bytes=\(observedBytes, privacy: .public) limit_bytes=\(limitBytes, privacy: .public)")
    }
    private func decodeError(_ data: Data, status: Int) -> APIError {
        struct Wire: Decodable { let code: String?; let error: String?; let message: String? }
        let wire = try? decoder.decode(Wire.self, from: data)
        let raw = wire?.message ?? "Server request failed (HTTP \(status))."
        let redacted = token.isEmpty ? raw : raw.replacingOccurrences(of: token, with: "[redacted]")
        return APIError(String(redacted.prefix(1000)), statusCode: status, code: wire?.code ?? wire?.error)
    }
    public static let eventStreamIdleTimeout: TimeInterval = 45

    public func eventStream(sessionID: String, since: UInt64?, epoch: String?, tailProjection: Bool = false) async throws -> (URLSession.AsyncBytes, HTTPURLResponse) {
        try await eventStream(sessionID: sessionID, since: since, epoch: epoch, tailProjection: tailProjection, idleTimeout: Self.eventStreamIdleTimeout)
    }

    func eventStream(sessionID: String, since: UInt64?, epoch: String?, idleTimeout: TimeInterval) async throws -> (URLSession.AsyncBytes, HTTPURLResponse) {
        try await eventStream(sessionID: sessionID, since: since, epoch: epoch, tailProjection: false, idleTimeout: idleTimeout)
    }

    private func eventStream(sessionID: String, since: UInt64?, epoch: String?, tailProjection: Bool, idleTimeout: TimeInterval) async throws -> (URLSession.AsyncBytes, HTTPURLResponse) {
        guard idleTimeout.isFinite, idleTimeout > 0, !tailProjection || since == nil else { throw APIError("Invalid event stream projection options.") }
        let escapedID = sessionID.addingPercentEncoding(withAllowedCharacters: CharacterSet(charactersIn: "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-._~")) ?? sessionID
        var components = URLComponents(url: baseURL.appending(path: "api/v2/sessions/\(escapedID)/events"), resolvingAgainstBaseURL: false)!
        var query: [URLQueryItem] = []
        if tailProjection { query += historyQuery(maxItems: 50, maxBytes: 2 * 1024 * 1024, maxItemBytes: 128 * 1024) }
        else if let since { query = [URLQueryItem(name: "since", value: String(since)), URLQueryItem(name: "since_epoch", value: epoch ?? "")] }
        components.queryItems = query.isEmpty ? nil : query
        guard let url = components.url else { throw APIError("Invalid event stream URL.") }
        var request = URLRequest(url: url)
        request.setValue("text/event-stream", forHTTPHeaderField: "Accept")
        request.timeoutInterval = idleTimeout
        if !token.isEmpty { request.setValue("Bearer \(token)", forHTTPHeaderField: "Authorization") }
        let (bytes, response) = try await session.bytes(for: request)
        guard let http = response as? HTTPURLResponse, http.statusCode == 200 else { throw APIError("Could not open the session event stream.", statusCode: (response as? HTTPURLResponse)?.statusCode) }
        return (bytes, http)
    }
}

private struct AnyEncodable: Encodable {
    let value: any Encodable
    init(_ value: any Encodable) { self.value = value }
    func encode(to encoder: Encoder) throws { try value.encode(to: encoder) }
}

private final class RedirectGuard: NSObject, URLSessionTaskDelegate, @unchecked Sendable {
    func urlSession(_ session: URLSession, task: URLSessionTask, willPerformHTTPRedirection response: HTTPURLResponse, newRequest request: URLRequest, completionHandler: @escaping (URLRequest?) -> Void) { completionHandler(nil) }
}
