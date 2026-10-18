import Foundation
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
    public init(id: UUID, name: String, baseURL: String, allowsLocalHTTP: Bool = false, instanceID: String? = nil, capabilities: [String] = []) {
        self.id = id; self.name = name; self.baseURL = baseURL; self.allowsLocalHTTP = allowsLocalHTTP; self.instanceID = instanceID; self.capabilities = capabilities
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
        let (responseData, response) = try await send(method: method, path: path, query: query, body: body)
        guard accepted.contains(response.statusCode) else { throw decodeError(responseData, status: response.statusCode) }
        do { return try decoder.decode(T.self, from: responseData) }
        catch { throw APIError("The server returned an invalid response.", statusCode: response.statusCode) }
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
        let (bytes, response) = try await session.bytes(for: request)
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
            throw APIError("The server's binary response exceeds the client size limit.")
        }
        var data = Data()
        data.reserveCapacity(http.expectedContentLength >= 0 ? Int(http.expectedContentLength) : min(maximumBytes, 64 * 1024))
        for try await byte in bytes {
            guard data.count < maximumBytes else { throw APIError("The server's binary response exceeds the client size limit.") }
            data.append(byte)
        }
        return data
    }
    private func send(method: String, path: String, query: [URLQueryItem], body: Encodable?) async throws -> (Data, HTTPURLResponse) {
        guard !path.split(separator: "/").contains(where: { $0 == ".." }) else { throw APIError("Invalid API path.") }
        var url = path.split(separator: "/", omittingEmptySubsequences: true).reduce(baseURL) { current, segment in current.appending(path: String(segment), directoryHint: .notDirectory) }
        if !query.isEmpty { var c = URLComponents(url: url, resolvingAgainstBaseURL: false)!; c.queryItems = query; guard let u = c.url else { throw APIError("Invalid API request URL.") }; url = u }
        var request = URLRequest(url: url); request.httpMethod = method; request.timeoutInterval = 30
        if !token.isEmpty { request.setValue("Bearer \(token)", forHTTPHeaderField: "Authorization") }
        if let body {
            let data = try JSONEncoder().encode(AnyEncodable(body)); guard data.count <= 4 * 1024 * 1024 else { throw APIError("Request body exceeds the 4 MiB client limit.") }; request.httpBody = data; request.setValue("application/json", forHTTPHeaderField: "Content-Type")
        }
        let (bytes, response) = try await session.bytes(for: request)
        guard let http = response as? HTTPURLResponse else { throw APIError("The server returned an invalid HTTP response.") }
        var data = Data()
        for try await byte in bytes {
            guard data.count < 16 * 1024 * 1024 else { throw APIError("Server response exceeded the 16 MiB client limit.") }
            data.append(byte)
        }
        return (data, http)
    }
    private func decodeError(_ data: Data, status: Int) -> APIError {
        struct Wire: Decodable { let code: String?; let message: String? }
        let wire = try? decoder.decode(Wire.self, from: data)
        let raw = wire?.message ?? "Server request failed (HTTP \(status))."
        let redacted = token.isEmpty ? raw : raw.replacingOccurrences(of: token, with: "[redacted]")
        return APIError(String(redacted.prefix(1000)), statusCode: status, code: wire?.code)
    }
    public static let eventStreamIdleTimeout: TimeInterval = 45

    public func eventStream(sessionID: String, since: UInt64?, epoch: String?) async throws -> (URLSession.AsyncBytes, HTTPURLResponse) {
        try await eventStream(sessionID: sessionID, since: since, epoch: epoch, idleTimeout: Self.eventStreamIdleTimeout)
    }

    func eventStream(sessionID: String, since: UInt64?, epoch: String?, idleTimeout: TimeInterval) async throws -> (URLSession.AsyncBytes, HTTPURLResponse) {
        guard idleTimeout.isFinite, idleTimeout > 0 else { throw APIError("Invalid event stream idle timeout.") }
        let escapedID = sessionID.addingPercentEncoding(withAllowedCharacters: CharacterSet(charactersIn: "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-._~")) ?? sessionID
        var components = URLComponents(url: baseURL.appending(path: "api/v2/sessions/\(escapedID)/events"), resolvingAgainstBaseURL: false)!
        if let since { components.queryItems = [URLQueryItem(name: "since", value: String(since)), URLQueryItem(name: "since_epoch", value: epoch ?? "")] }
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
