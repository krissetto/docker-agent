import AgentKit
import Foundation
import Observation

@MainActor @Observable public final class AppModel {
    public var servers: [ServerProjection] = []
    public var sessions: [SessionProjection] = []
    public var openSessions: [SessionRef: OpenSessionProjection] = [:]
    public var sessionControls: [SessionRef: SessionControlsProjection] = [:]
    public var recentChats: [SessionRef: RecentChatResponse] = [:]
    public var recentHistoryBySession: [SessionRef: RecentHistoryProjection] = [:]
    public var sessionControlMetadata: [SessionRef: SessionControlMetadata] = [:]
    public var safetyBySession: [SessionRef: SafetyProjection] = [:]
    public var toolsBySession: [SessionRef: SessionToolsProjection] = [:]
    public var teamRosterBySession: [SessionRef: [TeamAgentProjection]] = [:]
    public var agentDetailsBySession: [SessionRef: [String: AgentDetailsProjection]] = [:]
    public var promptsBySession: [SessionRef: [SessionPrompt]] = [:]
    public var forkSkillsBySession: [SessionRef: [SessionForkSkill]] = [:]
    public var contextBySession: [SessionRef: SessionContextBreakdown] = [:]
    public var workspaceSnapshotsBySession: [SessionRef: SessionWorkspaceSnapshots] = [:]
    public var skillOperations: [SessionRef: [SkillOperationProjection]] = [:]
    public var needsYou: [NeedsYouProjection] = []
    public var agentsByServer: [UUID: [AgentOption]] = [:]
    public var nextCatalogCursor: [UUID: String] = [:]
    public var catalogHasMore: Set<UUID> = []
    public var reviewSnapshots: [SessionRef: ReviewSnapshot] = [:]
    public var reviewWorktrees: [String: ReviewWorktree] = [:]
    public var reviewDirectories: [String: ReviewDirectory] = [:]
    public var reviewDocuments: [String: ReviewDocument] = [:]
    public var reviewSendPreviews: [ReviewDraftScope: ReviewSendAttempt] = [:]
    public var isRefreshing = false
    public var errorMessage: String?
    public let reviewDrafts: ReviewDraftStore

    private let tokenStore: any TokenStoring
    private let configStore: InstanceConfigStore
    private let outbox: DurableSendOutbox?
    public private(set) var outboxError: String?
    private let localOnly: Bool
    var clients: [UUID: APIClient] = [:]
    private var tokens: [UUID: String] = [:]
    var streamTasks: [SessionRef: Task<Void, Never>] = [:]
    var sessionLeases: [SessionRef: Int] = [:]
    private var streamGenerations: [SessionRef: UUID] = [:]
    private var activeSceneIDs = Set<UUID>()
    private var sceneLifecycleObserved = false
    private var sceneObserversSuspended = false
    private var unauthorizedServers = Set<UUID>()
    private var testedDrafts: [UUID: ServerDraft] = [:]
    private var testedIdentities: [UUID: ServerIdentity] = [:]
    private let apiSession: URLSession?
    public var pendingSends: [SessionRef: [DurableSendRecord]] = [:]

    public init(tokenStore: any TokenStoring = KeychainTokenStore(), configStore: InstanceConfigStore = InstanceConfigStore(), outbox: DurableSendOutbox? = nil, reviewDrafts: ReviewDraftStore = ReviewDraftStore(), urlSession: URLSession? = nil, autoConnect: Bool = true, localOnly: Bool = false) {
        self.tokenStore = tokenStore; self.configStore = configStore; self.reviewDrafts = reviewDrafts; apiSession = urlSession; self.localOnly = localOnly
        do { self.outbox = try outbox ?? DurableSendOutbox() }
        catch { self.outbox = nil; outboxError = "Secure local send storage is unavailable: \(error.localizedDescription)" }
        servers = localOnly ? [] : configStore.load().map { ServerProjection(id: $0.id, name: $0.name, baseURL: $0.baseURL, state: .unknown, allowsLocalHTTP: $0.allowsLocalHTTP) }
        if !localOnly && autoConnect { Task { await loadSavedCredentials() } }
    }

    public func testServer(_ draft: ServerDraft) async throws -> ServerIdentity {
        guard !localOnly else { throw APIError("Network access is disabled in local smoke mode.") }
        let endpoint = try URLPolicy.validate(draft.baseURL, allowsLocalHTTP: draft.allowsLocalHTTP)
        var token = draft.token
        if token.isEmpty, let saved = servers.first(where: { $0.id == draft.id }), URLPolicy.sameOrigin(saved.baseURL, endpoint.absoluteString) {
            token = try await tokenStore.read(for: draft.id) ?? ""
        }
        let config = InstanceConfig(id: draft.id, name: draft.name, baseURL: draft.baseURL, allowsLocalHTTP: draft.allowsLocalHTTP)
        let client = try APIClient(config: config, token: token, session: apiSession)
        let identity: ServerIdentity = try await client.request(ServerIdentity.self, path: "api/v2/server")
        guard identity.version == 1, identity.sessionAPIVersion == 2 else { throw APIError("Server API v\(identity.version), session API v\(identity.sessionAPIVersion) is unsupported.") }
        testedDrafts[draft.id] = draft
        testedIdentities[draft.id] = identity
        return identity
    }

    public func addOrUpdateServer(_ draft: ServerDraft, identity: ServerIdentity) async throws {
        guard !localOnly else { throw APIError("Server setup is disabled in local smoke mode.") }
        guard testedDrafts[draft.id] == draft, testedIdentities[draft.id] == identity else { throw APIError("Test this exact server configuration before saving it.") }
        let config = InstanceConfig(id: draft.id, name: draft.name, baseURL: draft.baseURL, allowsLocalHTTP: draft.allowsLocalHTTP, instanceID: identity.instanceID, capabilities: identity.capabilities)
        let existing = servers.first(where: { $0.id == draft.id })
        let sameOrigin = existing.map { URLPolicy.sameOrigin($0.baseURL, draft.baseURL) } ?? false
        let savedToken = draft.token.isEmpty && sameOrigin ? (try await tokenStore.read(for: draft.id) ?? "") : draft.token
        try await tokenStore.delete(for: draft.id)
        try configStore.upsert(config)
        if !savedToken.isEmpty { try await tokenStore.write(savedToken, for: draft.id) }
        tokens[draft.id] = savedToken
        if let existing, existing.identity?.instanceID != identity.instanceID || !sameOrigin {
            invalidateServerState(draft.id)
        }
        clients[draft.id] = try APIClient(config: config, token: savedToken, session: apiSession)
        if let index = servers.firstIndex(where: { $0.id == draft.id }) {
            servers[index] = ServerProjection(id: draft.id, name: draft.name, baseURL: draft.baseURL, identity: identity, state: .live, allowsLocalHTTP: config.allowsLocalHTTP, sessionCount: servers[index].sessionCount, refreshedAt: .now)
        } else { servers.append(ServerProjection(id: draft.id, name: draft.name, baseURL: draft.baseURL, identity: identity, state: .live, allowsLocalHTTP: config.allowsLocalHTTP, refreshedAt: .now)) }
        servers.sort { $0.name.localizedCaseInsensitiveCompare($1.name) == .orderedAscending }
        testedDrafts[draft.id] = nil; testedIdentities[draft.id] = nil
    }

    public func removeServer(_ id: UUID) async throws {
        try await tokenStore.delete(for: id); try configStore.remove(id)
        clients[id] = nil; tokens[id] = nil; testedDrafts[id] = nil; testedIdentities[id] = nil
        servers.removeAll { $0.id == id }
        invalidateServerState(id)
        sessionControlMetadata = sessionControlMetadata.filter { $0.key.serverID != id }
        reviewWorktrees = reviewWorktrees.filter { !$0.key.hasPrefix(id.uuidString + "|") }
        reviewDirectories = reviewDirectories.filter { !$0.key.hasPrefix(id.uuidString + "|") }
        reviewDocuments = reviewDocuments.filter { !$0.key.hasPrefix(id.uuidString + "|") }
        pendingSends = pendingSends.filter { $0.key.serverID != id }
    }

    func validateConnectedIdentity(serverID: UUID, expectedInstanceID: String? = nil) async throws {
        guard !localOnly else { throw APIError("Network access is disabled in local smoke mode.") }
        guard let server = servers.first(where: { $0.id == serverID }),
              let client = clients[serverID],
              let configURL = URL(string: server.baseURL) else { throw APIError("Server is not connected.") }
        guard URLPolicy.sameOrigin(server.baseURL, client.config.baseURL) else { throw APIError("The connected server origin changed. Reconnect before retrying.") }
        let pinnedInstance = expectedInstanceID ?? server.identity?.instanceID
        let identity: ServerIdentity = try await client.request(ServerIdentity.self, path: "api/v2/server")
        guard let currentServer = servers.first(where: { $0.id == serverID }),
              clients[serverID] === client,
              currentServer.baseURL == server.baseURL,
              currentServer.identity?.instanceID == pinnedInstance,
              identity.version == 1, identity.sessionAPIVersion == 2,
              pinnedInstance == identity.instanceID,
              URLPolicy.sameOrigin(configURL.absoluteString, client.config.baseURL) else {
            throw APIError("The server identity changed. The saved send or review draft was not retargeted.")
        }
    }

    private func invalidateServerState(_ id: UUID) {
        for ref in streamTasks.keys.filter({ $0.serverID == id }) { streamTasks[ref]?.cancel(); streamTasks[ref] = nil }
        for ref in sessionLeases.keys.filter({ $0.serverID == id }) { sessionLeases[ref] = nil }
        openSessions = openSessions.filter { $0.key.serverID != id }
        sessions.removeAll { $0.ref.serverID == id }
        needsYou.removeAll { $0.id.serverID == id }
        sessionControls = sessionControls.filter { $0.key.serverID != id }
        safetyBySession = safetyBySession.filter { $0.key.serverID != id }
        toolsBySession = toolsBySession.filter { $0.key.serverID != id }
        teamRosterBySession = teamRosterBySession.filter { $0.key.serverID != id }
        agentDetailsBySession = agentDetailsBySession.filter { $0.key.serverID != id }
        skillOperations = skillOperations.filter { $0.key.serverID != id }
        promptsBySession = promptsBySession.filter { $0.key.serverID != id }
        forkSkillsBySession = forkSkillsBySession.filter { $0.key.serverID != id }
        contextBySession = contextBySession.filter { $0.key.serverID != id }
        workspaceSnapshotsBySession = workspaceSnapshotsBySession.filter { $0.key.serverID != id }
        recentChats = recentChats.filter { $0.key.serverID != id }
        recentHistoryBySession = recentHistoryBySession.filter { $0.key.serverID != id }
        reviewSnapshots = reviewSnapshots.filter { $0.key.serverID != id }
        reviewSendPreviews = reviewSendPreviews.filter { $0.key.serverID != id }
        reviewWorktrees = reviewWorktrees.filter { !$0.key.hasPrefix(id.uuidString + "|") }
        reviewDirectories = reviewDirectories.filter { !$0.key.hasPrefix(id.uuidString + "|") }
        reviewDocuments = reviewDocuments.filter { !$0.key.hasPrefix(id.uuidString + "|") }
        pendingSends = pendingSends.filter { $0.key.serverID != id }
        nextCatalogCursor[id] = nil; catalogHasMore.remove(id); agentsByServer[id] = nil
    }

    public func refreshForeground() async {
        isRefreshing = true; defer { isRefreshing = false }
        for server in servers { await refreshCatalog(server.id, reset: true) }
    }

    public func refreshCatalog(_ serverID: UUID, reset: Bool = true) async {
        guard !localOnly else { markServer(serverID, state: .offline); return }
        guard let client = clients[serverID] else { markServer(serverID, state: .offline); return }
        do {
            if reset { nextCatalogCursor[serverID] = nil; catalogHasMore.remove(serverID); sessions.removeAll { $0.ref.serverID == serverID } }
            var query = [URLQueryItem(name: "limit", value: "50")]
            if let cursor = nextCatalogCursor[serverID] { query.append(URLQueryItem(name: "cursor", value: cursor)) }
            let page: CatalogResponse = try await client.request(CatalogResponse.self, path: "api/v2/sessions", query: query)
            let projected = page.sessions.filter { $0.parentID == nil || $0.parentID?.isEmpty == true }.map { $0.projection(serverID: serverID) }
            sessions.append(contentsOf: projected)
            nextCatalogCursor[serverID] = page.nextCursor?.isEmpty == false ? page.nextCursor : nil
            if page.nextCursor?.isEmpty != false { catalogHasMore.remove(serverID) } else { catalogHasMore.insert(serverID) }
            if let index = servers.firstIndex(where: { $0.id == serverID }) {
                servers[index].sessionCount = sessions.filter { $0.ref.serverID == serverID }.count
                servers[index].state = .live; servers[index].refreshedAt = .now
            }
            await refreshNeedsYou(serverID, client: client, rows: projected)
        } catch let error as APIError {
            markServer(serverID, state: error.statusCode == 401 ? .unauthorized : .offline); errorMessage = error.message
        } catch { markServer(serverID, state: .offline); errorMessage = error.localizedDescription }
    }

    public func loadMoreSessions(serverID: UUID) async {
        guard catalogHasMore.contains(serverID) else { return }
        await refreshCatalog(serverID, reset: false)
    }

    public func loadAgents(serverID: UUID) async throws -> [AgentOption] {
        guard let client = clients[serverID] else { throw APIError("Server is not connected.") }
        let values: [AgentOption] = try await client.request([AgentOption].self, path: "api/agents")
        agentsByServer[serverID] = values
        return values
    }

    public func createSession(serverID: UUID, source: AgentOption, title: String = "", model: String = "", workingDir: String? = nil, firstMessage: String? = nil) async throws -> SessionRef {
        guard !source.name.isEmpty, let rootAgentName = source.rootAgentName, !rootAgentName.isEmpty else {
            throw APIError("This server does not identify the configured root agent. Session creation is unavailable until the server is updated.")
        }
        return try await createSession(serverID: serverID, sourceName: source.name, agentName: rootAgentName, title: title, model: model, workingDir: workingDir, firstMessage: firstMessage)
    }

    func createSession(serverID: UUID, sourceName: String, agentName: String, title: String = "", model: String = "", workingDir: String? = nil, firstMessage: String? = nil) async throws -> SessionRef {
        guard let client = clients[serverID] else { throw APIError("Server is not connected.") }
        let sessionID = UUID().uuidString
        if let firstMessage, !firstMessage.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty {
            let body = StartSessionRequest(sessionID: sessionID, source: sourceName, agentName: agentName, title: title, model: model, workingDir: workingDir, input: SessionInput(content: firstMessage, requestID: UUID().uuidString))
            try await client.noContent(method: "POST", path: "api/v2/sessions/start", body: body, accepted: [202])
        } else {
            let body = CreateSessionRequest(sessionID: sessionID, source: sourceName, agentName: agentName, title: title, model: model, workingDir: workingDir)
            try await client.noContent(method: "POST", path: "api/v2/sessions", body: body, accepted: [201])
        }
        let ref = SessionRef(serverID: serverID, sessionID: sessionID)
        await refreshCatalog(serverID)
        return ref
    }

    public func setSceneActive(_ active: Bool, for sceneID: UUID) {
        sceneLifecycleObserved = true
        if active {
            activeSceneIDs.insert(sceneID)
            if sceneObserversSuspended { sceneObserversSuspended = false }
            resumeOpenSessionObservers()
            return
        }
        activeSceneIDs.remove(sceneID)
        guard activeSceneIDs.isEmpty, !sceneObserversSuspended else { return }
        sceneObserversSuspended = true
        for ref in Array(streamTasks.keys) { streamGenerations[ref] = nil; streamTasks[ref]?.cancel(); streamTasks[ref] = nil }
    }

    public func reconnectServer(_ serverID: UUID) async throws {
        guard !localOnly else { throw APIError("Network access is disabled in local smoke mode.") }
        guard let config = configStore.load().first(where: { $0.id == serverID }) else { throw APIError("Server configuration is unavailable.") }
        let previousIdentity = servers.first(where: { $0.id == serverID })?.identity?.instanceID
        let pinnedInstance = config.instanceID ?? previousIdentity
        guard let configured = servers.first(where: { $0.id == serverID }), URLPolicy.sameOrigin(configured.baseURL, config.baseURL) else {
            throw APIError("Server origin changed. Edit and test this server before reconnecting.")
        }
        let token = try await tokenStore.read(for: serverID) ?? ""
        let client = try APIClient(config: config, token: token, session: apiSession)
        let identity: ServerIdentity = try await client.request(ServerIdentity.self, path: "api/v2/server")
        guard identity.version == 1, identity.sessionAPIVersion == 2,
              pinnedInstance == nil || pinnedInstance == identity.instanceID else {
            throw APIError("Server identity changed. Edit and test this server before reconnecting.")
        }
        for ref in Array(streamTasks.keys) where ref.serverID == serverID {
            streamGenerations[ref] = nil
            streamTasks[ref]?.cancel()
            streamTasks[ref] = nil
        }
        if config.instanceID == nil {
            var pinned = config; pinned.instanceID = identity.instanceID; pinned.capabilities = identity.capabilities
            try configStore.upsert(pinned)
        }
        clients[serverID] = client
        tokens[serverID] = token
        unauthorizedServers.remove(serverID)
        if let index = servers.firstIndex(where: { $0.id == serverID }) {
            servers[index].identity = identity
            servers[index].state = .live
            servers[index].refreshedAt = .now
        }
        for ref in openSessions.keys where ref.serverID == serverID {
            if var projection = openSessions[ref] { projection.connection = .reconnecting; openSessions[ref] = projection }
        }
        resumeOpenSessionObservers(serverID: serverID)
    }

    private func resumeOpenSessionObservers(serverID: UUID? = nil) {
        guard !sceneObserversSuspended else { return }
        for ref in openSessions.keys where serverID == nil || ref.serverID == serverID {
            guard sessionLeases[ref, default: 0] > 0, streamTasks[ref] == nil,
                  let client = clients[ref.serverID], !unauthorizedServers.contains(ref.serverID) else { continue }
            startObserving(ref, client: client)
        }
    }

    private func startObserving(_ ref: SessionRef, client: APIClient) {
        guard streamTasks[ref] == nil else { return }
        let generation = UUID()
        streamGenerations[ref] = generation
        streamTasks[ref] = Task { [weak self] in
            guard let self else { return }
            await self.follow(ref, client: client, generation: generation)
        }
    }

    private func observationIsCurrent(_ ref: SessionRef, client: APIClient, generation: UUID) -> Bool {
        !Task.isCancelled && streamGenerations[ref] == generation && clients[ref.serverID] === client && openSessions[ref] != nil
    }

    private var openingSessions = Set<SessionRef>()

    public func openSession(_ ref: SessionRef) async throws {
        guard !localOnly else { throw APIError("Network access is disabled in local smoke mode.") }
        if openSessions[ref] != nil {
            sessionLeases[ref, default: 0] += 1
            if unauthorizedServers.contains(ref.serverID) { try await reconnectServer(ref.serverID) }
            return
        }
        guard !openingSessions.contains(ref) else { throw APIError("This session is already opening in another window.") }
        openingSessions.insert(ref)
        defer { openingSessions.remove(ref) }
        sessionLeases[ref] = 1
        do {
            guard let client = clients[ref.serverID] else { throw APIError("Server is not connected.") }
            let encodedID = Self.pathComponent(ref.sessionID)
            let prepared: PreparedViewResponse = try await client.request(PreparedViewResponse.self, path: "api/v2/sessions/\(encodedID)", query: [URLQueryItem(name: "view", value: "prepare-info")])
            guard clients[ref.serverID] === client else { throw APIError("Server was removed or changed while opening the session.") }
            guard prepared.view == "prepare-info", let confirmation = prepared.confirmation else { throw APIError("Server did not provide a confirmed session view.") }
            let patch = OpenViewRequest(kind: "open_view", viewConfirmation: confirmation)
            let opened: OpenedViewResponse = try await client.request(OpenedViewResponse.self, method: "PATCH", path: "api/v2/sessions/\(encodedID)", query: [URLQueryItem(name: "view", value: "open-view")], body: patch)
            guard clients[ref.serverID] === client else { throw APIError("Server was removed or changed while opening the session.") }
            guard opened.view == "open-view", opened.version == 2 else { throw APIError("Server did not confirm the session view.") }
            let (initial, rawProof) = try await verifiedRawSessionSnapshot(client: client, ref: ref)
            guard clients[ref.serverID] === client else { throw APIError("Server was removed or changed while opening the session.") }
            var projection = initial.project(ref: ref, connection: .live, rawProof: rawProof, branchInstanceID: servers.first(where: { $0.id == ref.serverID })?.identity?.instanceID)
            projection.capabilities = opened.metadata?.capabilities ?? .init()
            openSessions[ref] = projection
            refreshPendingSends(ref)
            startObserving(ref, client: client)
        } catch {
            sessionLeases[ref] = nil
            throw error
        }
    }

    public func closeSession(_ ref: SessionRef) {
        let remaining = max(0, (sessionLeases[ref] ?? 1) - 1)
        if remaining > 0 { sessionLeases[ref] = remaining; return }
        sessionLeases[ref] = nil
        streamGenerations[ref] = nil
        streamTasks[ref]?.cancel(); streamTasks[ref] = nil; openSessions[ref] = nil
    }

    public func send(_ ref: SessionRef, content: String, attachments: [SessionDocumentAttachment] = [], mode: String = "submit", requestID: String = UUID().uuidString) async throws {
        let payload = try SessionSendPayload(content: content, attachments: attachments)
        let identity = try await durableIdentity(for: ref)
        guard let outbox else { throw APIError(outboxError ?? "Durable send storage is unavailable.") }
        let record = try outbox.prepare(identity: identity, sendPayload: payload, mode: mode, requestID: requestID)
        try await validateConnectedIdentity(serverID: ref.serverID)
        if record.state == .accepted { refreshPendingSends(ref); throw APIError("This request was already accepted and cannot be resent.") }
        guard record.state == .prepared else { refreshPendingSends(ref); throw APIError("This send may already have been delivered. Use Retry exact send to safely repeat it, or discard it with an explicit warning.") }
        try await deliver(record, identity: identity)
    }

    public func retryPendingSend(_ ref: SessionRef, requestID: String) async throws {
        try await validateConnectedIdentity(serverID: ref.serverID)
        let identity = try await durableIdentity(for: ref)
        guard let outbox else { throw APIError(outboxError ?? "Durable send storage is unavailable.") }
        guard let record = outbox.pending(for: identity).first(where: { $0.requestID == requestID }) else { throw APIError("No pending send matches this session.") }
        guard record.state == .prepared || record.state == .deliveryUnknown else { throw APIError("This send is not retryable.") }
        try await deliver(record, identity: identity)
    }

    public func discardPendingSend(_ ref: SessionRef, requestID: String, confirmUnknownDelivery: Bool = false) throws {
        let identity = try durableIdentityFromCurrent(ref)
        guard let outbox else { throw APIError(outboxError ?? "Durable send storage is unavailable.") }
        try outbox.discard(requestID: requestID, identity: identity, confirmUnknownDelivery: confirmUnknownDelivery)
        refreshPendingSends(ref)
    }

    private func deliver(_ record: DurableSendRecord, identity: DurableSendIdentity) async throws {
        guard let outbox else { throw APIError(outboxError ?? "Durable send storage is unavailable.") }
        guard let client = clients[record.identity.serverID] else { throw APIError("Server is not connected.") }
        let latest = try await client.sessionStatus(record.identity.sessionID)
        guard latest.metadata.sessionID == record.identity.sessionID, latest.metadata.agentName == openSessions[record.identity.session]?.agentName || openSessions[record.identity.session] == nil else { throw APIError("Session identity changed before send.") }
        _ = try outbox.markAttempt(requestID: record.requestID, identity: identity)
        refreshPendingSends(record.identity.session)
        do {
            let body = try record.sendPayload.requestBody(requestID: record.requestID, mode: record.mode)
            try await client.noContent(method: "POST", path: "api/v2/sessions/\(Self.pathComponent(record.identity.sessionID))/messages", body: body, accepted: [202])
            _ = try outbox.markAccepted(requestID: record.requestID, identity: identity)
            refreshPendingSends(record.identity.session)
        } catch {
            let outcome: DurableDeliveryOutcome = (error as? APIError)?.statusCode.map { (400..<500).contains($0) && $0 != 408 && $0 != 429 ? .definitelyNotDelivered : .unknown } ?? .unknown
            _ = try? outbox.markFailure(requestID: record.requestID, identity: identity, outcome: outcome)
            refreshPendingSends(record.identity.session)
            throw error
        }
    }

    private func durableIdentity(for ref: SessionRef) async throws -> DurableSendIdentity {
        guard let server = servers.first(where: { $0.id == ref.serverID }), let instanceID = server.identity?.instanceID,
              let url = URL(string: server.baseURL) else { throw APIError("Connect to this server again before sending.") }
        return try DurableSendIdentity(serverID: ref.serverID, serverInstanceID: instanceID, session: ref, serverURL: url)
    }

    private func durableIdentityFromCurrent(_ ref: SessionRef) throws -> DurableSendIdentity {
        guard let server = servers.first(where: { $0.id == ref.serverID }), let instanceID = server.identity?.instanceID,
              let url = URL(string: server.baseURL) else { throw APIError("Connect to this server again before resolving the pending send.") }
        return try DurableSendIdentity(serverID: ref.serverID, serverInstanceID: instanceID, session: ref, serverURL: url)
    }

    private func refreshPendingSends(_ ref: SessionRef) {
        guard let outbox, let identity = try? durableIdentityFromCurrent(ref) else { pendingSends[ref] = []; return }
        pendingSends[ref] = outbox.pending(for: identity)
    }

    public func retry(_ ref: SessionRef) async throws {
        guard let client = clients[ref.serverID] else { throw APIError("Server is not connected.") }
        try await client.noContent(method: "POST", path: "api/v2/sessions/\(Self.pathComponent(ref.sessionID))/retry", body: EmptyBody(), accepted: [202])
    }

    public func cancel(_ ref: SessionRef) async throws {
        guard let client = clients[ref.serverID] else { throw APIError("Server is not connected.") }
        try await client.noContent(method: "POST", path: "api/v2/sessions/\(Self.pathComponent(ref.sessionID))/cancel", body: EmptyBody(), accepted: [200])
    }

    public func respond(_ ref: SessionRef, interactionID: String, kind: String, confirmation: ConfirmationDecision? = nil, toolName: String? = nil, reason: String? = nil, elicitationID: String? = nil, action: String? = nil, content: [String: JSONValue]? = nil) async throws {
        guard let client = clients[ref.serverID] else { throw APIError("Server is not connected.") }
        let request = SessionResponseRequest(interactionID: interactionID, kind: kind, confirmation: confirmation?.rawValue, toolName: toolName, reason: reason, elicitationID: elicitationID, action: action, content: content)
        try await client.noContent(method: "POST", path: "api/v2/sessions/\(Self.pathComponent(ref.sessionID))/responses", body: request, accepted: [204])
    }

    public func reviewSnapshot(_ ref: SessionRef, refresh: Bool = false) async throws -> ReviewSnapshot {
        if !refresh, let cached = reviewSnapshots[ref] { return cached }
        guard let client = clients[ref.serverID] else { throw APIError("Server is not connected.") }
        let response: GitReviewSnapshotResponse = try await client.request(GitReviewSnapshotResponse.self, path: "api/v2/sessions/\(Self.pathComponent(ref.sessionID))/git-review")
        reviewSnapshots[ref] = response.snapshot
        return response.snapshot
    }

    public func reviewWorktree(_ ref: SessionRef, generation: UInt64, worktreeID: String) async throws -> ReviewWorktree {
        guard let client = clients[ref.serverID] else { throw APIError("Server is not connected.") }
        let response: GitReviewWorktreeResponse = try await client.request(GitReviewWorktreeResponse.self, method: "POST", path: "api/v2/sessions/\(Self.pathComponent(ref.sessionID))/git-review/worktree", body: GitReviewWorktreeRequest(generation: generation, worktreeID: worktreeID))
        guard response.generation == generation else { throw APIError("Review snapshot changed. Refresh Git review.", statusCode: 409, code: "review_generation_expired") }
        reviewWorktrees[Self.reviewKey(ref, worktreeID)] = response.worktree
        return response.worktree
    }

    public func reviewDirectory(_ ref: SessionRef, generation: UInt64, worktreeID: String, path: String) async throws -> ReviewDirectory {
        guard let client = clients[ref.serverID] else { throw APIError("Server is not connected.") }
        let response: GitReviewDirectoryResponse = try await client.request(GitReviewDirectoryResponse.self, method: "POST", path: "api/v2/sessions/\(Self.pathComponent(ref.sessionID))/git-review/directory", body: GitReviewDirectoryRequest(generation: generation, worktreeID: worktreeID, path: path))
        guard response.generation == generation else { throw APIError("Review snapshot changed. Refresh Git review.", statusCode: 409, code: "review_generation_expired") }
        reviewDirectories[Self.reviewKey(ref, worktreeID) + "|" + path] = response.directory
        return response.directory
    }

    public func reviewDocument(_ ref: SessionRef, generation: UInt64, key: ReviewDocumentKey) async throws -> ReviewDocument {
        guard let client = clients[ref.serverID] else { throw APIError("Server is not connected.") }
        let response: GitReviewDocumentResponse = try await client.request(GitReviewDocumentResponse.self, method: "POST", path: "api/v2/sessions/\(Self.pathComponent(ref.sessionID))/git-review/document", body: GitReviewDocumentRequest(generation: generation, key: key))
        guard response.generation == generation else { throw APIError("Review snapshot changed. Refresh Git review.", statusCode: 409, code: "review_generation_expired") }
        reviewDocuments[Self.documentKey(ref, key)] = response.document
        return response.document
    }

    public func resolveGeneratedMedia(_ ref: SessionRef, reference: GeneratedMediaReference) async throws -> Data {
        guard openSessions[ref] != nil, let client = clients[ref.serverID] else { throw APIError("Open this session again before loading generated media.") }
        guard (try? reference.request) != nil else { throw SessionMediaError.invalidGeneratedMediaReference }
        try await validateConnectedIdentity(serverID: ref.serverID)
        guard clients[ref.serverID] === client, openSessions[ref] != nil else { throw APIError("The session or server changed while loading generated media.") }
        let bytes = try await client.resolveGeneratedMedia(sessionID: ref.sessionID, reference: reference)
        guard clients[ref.serverID] === client, openSessions[ref] != nil else { throw APIError("The session or server changed while loading generated media.") }
        return bytes
    }

    public func reviewDraft(scope: ReviewDraftScope) -> ReviewDraftProjection { reviewDrafts.draft(for: scope) }
    @discardableResult public func addReviewComment(scope: ReviewDraftScope, anchor: ReviewAnchor, body: String) throws -> ReviewComment { try reviewDrafts.add(scope: scope, anchor: anchor, body: body) }
    public func editReviewComment(scope: ReviewDraftScope, commentID: UUID, body: String) throws { try reviewDrafts.edit(scope: scope, commentID: commentID, body: body) }
    public func deleteReviewComment(scope: ReviewDraftScope, commentID: UUID) { reviewDrafts.remove(scope: scope, commentID: commentID) }
    public func reanchorReviewComment(scope: ReviewDraftScope, commentID: UUID, anchor: ReviewAnchor) { reviewDrafts.explicitlyReanchor(scope: scope, commentID: commentID, anchor: anchor) }

    public func prepareReviewSend(scope: ReviewDraftScope, focusedSession: SessionRef) async throws -> ReviewSendAttempt {
        guard scope.serverID == focusedSession.serverID, scope.sessionID == focusedSession.sessionID else { throw ReviewDraftError.changed }
        guard let server = servers.first(where: { $0.id == scope.serverID }), server.identity?.instanceID == scope.instanceID else { throw ReviewDraftError.changed }
        try await validateConnectedIdentity(serverID: scope.serverID, expectedInstanceID: scope.instanceID)
        let draft = reviewDrafts.draft(for: scope)
        guard !draft.comments.isEmpty else { throw ReviewDraftError.empty }
        // Keep exact pending payload and target for ambiguous retries; never retarget it.
        if let attempt = reviewDrafts.attempts[scope] { return attempt }
        let snapshot = try await reviewSnapshot(focusedSession, refresh: true)
        guard snapshot.repositories.contains(where: { $0.id == scope.repositoryID && $0.worktrees.contains(where: { $0.id == scope.worktreeID }) }) else { throw ReviewDraftError.changed }
        var checked: [UUID: ReviewAnchor] = [:]
        var checkedDocuments: [ReviewDocumentKey: ReviewDocument] = [:]
        for comment in draft.comments {
            let anchor = comment.anchor
            let key = ReviewDocumentKey(mode: anchor.mode, worktreeID: scope.worktreeID, path: anchor.path, group: anchor.group)
            let document: ReviewDocument
            if let cached = checkedDocuments[key] { document = cached }
            else { document = try await reviewDocument(focusedSession, generation: snapshot.generation, key: key); checkedDocuments[key] = document }
            let fingerprint = reviewDocumentFingerprint(document)
            guard anchor.documentFingerprint == fingerprint, anchorContextExists(anchor, in: document) else { reviewDrafts.markCommentStale(scope: scope, commentID: comment.id); throw ReviewDraftError.changed }
            var confirmed = anchor; confirmed.generation = snapshot.generation; checked[comment.id] = confirmed
        }
        guard let client = clients[scope.serverID] else { throw APIError("Server is not connected.") }
        let info: AgentInfoResponse = try await client.request(AgentInfoResponse.self, path: "api/v2/sessions/\(Self.pathComponent(focusedSession.sessionID))/agent-info")
        guard let currentAgent = info.focusedAgentName, !currentAgent.isEmpty else { throw APIError("Could not confirm the current session agent.") }
        // Equal full-document fingerprint and anchor context is explicit fresh validation; no line-number rebasing occurs.
        let attempt: ReviewSendAttempt
        if let pending = reviewDrafts.attempts[scope], pending.state == .previewed {
            let captured = reviewDrafts.draft(for: scope).comments
            let payload = reviewDrafts.serializeForSend(captured, workspaceLabel: scope.workspaceLabel)
            attempt = try reviewDrafts.updatePreview(scope: scope, requestID: pending.requestID, expectedAgent: currentAgent, payload: payload, comments: captured)
        } else {
            attempt = try reviewDrafts.beginSend(scope: scope, focusedSession: focusedSession, expectedAgent: currentAgent, currentGeneration: snapshot.generation, freshlyChecked: checked)
        }
        reviewSendPreviews[scope] = attempt
        return attempt
    }

    public func confirmReviewSend(scope: ReviewDraftScope, focusedSession: SessionRef) async throws {
        guard scope.serverID == focusedSession.serverID, scope.sessionID == focusedSession.sessionID else { throw ReviewDraftError.changed }
        guard let attempt = reviewSendPreviews[scope] ?? reviewDrafts.attempts[scope], attempt.focusedSession == focusedSession else { throw APIError("Preview this review draft before sending.") }
        guard let server = servers.first(where: { $0.id == scope.serverID }), server.identity?.instanceID == scope.instanceID else { throw ReviewDraftError.changed }
        try await validateConnectedIdentity(serverID: scope.serverID, expectedInstanceID: scope.instanceID)
        guard let client = clients[scope.serverID] else { throw APIError("Server is not connected.") }
        let info: AgentInfoResponse = try await client.request(AgentInfoResponse.self, path: "api/v2/sessions/\(Self.pathComponent(focusedSession.sessionID))/agent-info")
        guard info.focusedAgentName == attempt.expectedAgent else { throw APIError("The session's current agent changed. Review the send target and confirm again.") }
        if let stored = reviewDrafts.attempts[scope], stored.state == .deliveryUnknown {
            guard stored.requestID == attempt.requestID else { throw ReviewDraftError.changed }
        } else {
            try reviewDrafts.markSending(scope: scope, requestID: attempt.requestID)
        }
        let body = SessionInput(content: attempt.payload, mode: "submit", requestID: attempt.requestID)
        do {
            try await client.noContent(method: "POST", path: "api/v2/sessions/\(Self.pathComponent(focusedSession.sessionID))/messages", body: body, accepted: [202])
            try reviewDrafts.acceptSend(scope: scope, requestID: attempt.requestID, session: focusedSession)
            reviewSendPreviews[scope] = nil
        } catch {
            reviewDrafts.markDeliveryUnknown(scope: scope, requestID: attempt.requestID)
            reviewDrafts.failSend(scope: scope, message: error.localizedDescription)
            throw error
        }
    }

    private func anchorContextExists(_ anchor: ReviewAnchor, in document: ReviewDocument) -> Bool {
        if anchor.context.isEmpty { return anchor.oldStart == 0 && anchor.newStart == 0 }
        let lines = document.lines.map(\.text)
        guard anchor.context.count <= lines.count else { return false }
        guard anchor.newStart > 0 || anchor.oldStart > 0 else { return false }
        let hunkRanges = diffLineRanges(document.lines)
        let start = hunkRanges.first { range in
            range.contains { index in
                let line = document.lines[index]
                let matchesNew = anchor.newStart == 0 || (line.newLine >= anchor.newStart && line.newLine <= anchor.newEnd)
                let matchesOld = anchor.oldStart == 0 || (line.oldLine >= anchor.oldStart && line.oldLine <= anchor.oldEnd)
                return matchesNew && matchesOld
            }
        }
        guard let start, start.count >= anchor.context.count else { return false }
        return (0...(start.count - anchor.context.count)).contains { offset in Array(lines[start.lowerBound + offset..<start.lowerBound + offset + anchor.context.count]) == anchor.context }
    }
    private func diffLineRanges(_ lines: [ReviewDocumentLine]) -> [Range<Int>] {
        var starts: [Int] = []
        for index in lines.indices where lines[index].kind == .hunk { starts.append(index) }
        if starts.isEmpty { return lines.isEmpty ? [] : [lines.indices] }
        return starts.enumerated().map { position, start in start..<(position + 1 < starts.count ? starts[position + 1] : lines.count) }
    }
    private static func reviewKey(_ ref: SessionRef, _ worktreeID: String) -> String { "\(ref.serverID.uuidString)|\(ref.sessionID)|\(worktreeID)" }
    private static func documentKey(_ ref: SessionRef, _ key: ReviewDocumentKey) -> String { "\(reviewKey(ref, key.worktreeID))|\(key.mode.rawValue)|\(key.group?.rawValue ?? "")|\(key.path)" }

    func verifiedRawSessionSnapshot(client: APIClient, ref: SessionRef) async throws -> (SessionSnapshotWire, RawSnapshotProof?) {
        let raw = try await client.requestBytes(method: "GET", path: "api/v2/sessions/\(Self.pathComponent(ref.sessionID))/snapshot",
                                                maximumBytes: RawSnapshotProof.maximumSnapshotBytes, expectedContentType: "application/json")
        let snapshot = try JSONDecoder().decode(SessionSnapshotWire.self, from: raw)
        guard snapshot.session?.id == ref.sessionID, snapshot.status.sessionID == ref.sessionID else { throw SessionAdvancedError.identityMismatch }
        let proof = try? RawSnapshotProof.extract(from: raw, expectedSessionID: ref.sessionID)
        return (snapshot, proof)
    }

    @discardableResult public func refreshSession(_ ref: SessionRef, generation: UUID? = nil) async throws -> SessionSnapshotWire {
        guard let client = clients[ref.serverID] else { throw APIError("Server is not connected.") }
        let (snapshot, rawProof) = try await verifiedRawSessionSnapshot(client: client, ref: ref)
        guard clients[ref.serverID] === client, openSessions[ref] != nil,
              generation == nil || streamGenerations[ref] == generation,
              !Task.isCancelled else { throw APIError("Session observation changed while refreshing.") }
        var value = snapshot.project(ref: ref, connection: openSessions[ref]?.connection ?? .live, rawProof: rawProof, branchInstanceID: servers.first(where: { $0.id == ref.serverID })?.identity?.instanceID)
        value.capabilities = openSessions[ref]?.capabilities ?? .init()
        if let old = openSessions[ref] { value.transcript = stableTranscript(old.transcript, value.transcript) }
        openSessions[ref] = value
        return snapshot
    }

    func consumeStreamSnapshot(_ snapshot: SessionSnapshotWire, rawProof: RawSnapshotProof? = nil, ref: SessionRef, cursor: inout SessionEventCursor?, projector: inout SessionEventProjector?) throws {
        var currentCursor = cursor ?? SessionEventCursor(expectedSessionID: ref.sessionID)
        guard case .snapshot = currentCursor.accept(snapshot: snapshot) else { throw StreamResnapshotRequired() }
        if let oldEpoch = openSessions[ref]?.epoch, let newEpoch = snapshot.epoch, !oldEpoch.isEmpty, oldEpoch != newEpoch {
            markAcceptedSkillOperationsUnknown(ref, reason: "The server restarted before this operation reached a terminal event; completion is unknown.")
        }
        var current = projector ?? SessionEventProjector(projection: snapshot.project(ref: ref, connection: openSessions[ref]?.connection ?? .live))
        current.replace(with: snapshot, rawProof: rawProof, branchInstanceID: servers.first(where: { $0.id == ref.serverID })?.identity?.instanceID)
        cursor = currentCursor
        projector = current
        openSessions[ref] = current.projection
    }

    func consumeStreamEvent(_ type: String, envelope: StreamEnvelope?, ref: SessionRef, cursor: inout SessionEventCursor?, projector: inout SessionEventProjector?) throws {
        guard var currentCursor = cursor else { throw APIError("Session event cursor is unavailable.") }
        switch currentCursor.accept(type: type, envelope: envelope) {
        case .ready:
            cursor = currentCursor
            return
        case .snapshot, .duplicate, .ignored: return
        case .resnapshotRequired: throw StreamResnapshotRequired()
        case .seed(let envelope):
            applySkillOperation(envelope, to: ref)
            guard var current = projector else { throw APIError("Session event projector is unavailable.") }
            current.apply(envelope)
            cursor = currentCursor
            projector = current
            openSessions[ref] = current.projection
        case .apply(let envelope):
            applySkillOperation(envelope, to: ref)
            guard var current = projector else { throw APIError("Session event projector is unavailable.") }
            current.apply(envelope)
            current.updateCursor(sequence: currentCursor.sequence, epoch: currentCursor.epoch ?? current.projection.epoch)
            cursor = currentCursor
            projector = current
            openSessions[ref] = current.projection
        }
    }

    private func follow(_ ref: SessionRef, client: APIClient, generation: UUID) async {
        var failures = 0
        var cursor: SessionEventCursor?
        var projector: SessionEventProjector?
        while observationIsCurrent(ref, client: client, generation: generation) {
            guard var projection = openSessions[ref] else { finishObservation(ref, generation: generation); return }
            if cursor == nil {
                cursor = SessionEventCursor(expectedSessionID: ref.sessionID, epoch: projection.epoch.isEmpty ? nil : projection.epoch, sequence: projection.cursor, isReady: true)
                projector = SessionEventProjector(projection: projection)
            }
            projection.connection = failures == 0 ? .live : .reconnecting; projection.refreshedAt = .now; openSessions[ref] = projection
            do {
                let (bytes, _) = try await client.eventStream(sessionID: ref.sessionID, since: cursor?.epoch == nil ? nil : cursor?.sequence, epoch: cursor?.epoch)
                guard observationIsCurrent(ref, client: client, generation: generation) else { finishObservation(ref, generation: generation); return }
                var parser = SSEParser()
                var assembler = SnapshotAssembler()
                for try await byte in bytes {
                    guard observationIsCurrent(ref, client: client, generation: generation) else { finishObservation(ref, generation: generation); return }
                    let frames = try parser.append(Data([byte]))
                    for frame in frames {
                        guard let data = frame.data.data(using: .utf8) else { throw APIError("Invalid session event frame.") }
                        if let payload = try assembler.acceptRawJSON(data) {
                            let proof = try? RawSnapshotProof.extract(from: payload.rawJSON, expectedSessionID: ref.sessionID)
                            try consumeStreamSnapshot(payload.snapshot, rawProof: proof, ref: ref, cursor: &cursor, projector: &projector)
                            projection = projector!.projection
                            continue
                        }
                        guard let message = try? JSONDecoder().decode(StreamMessage.self, from: data) else { throw APIError("Invalid session event frame.") }
                        try consumeStreamEvent(message.type, envelope: message.envelope, ref: ref, cursor: &cursor, projector: &projector)
                        if let current = projector?.projection { projection = current }
                    }
                }
                try assembler.finish()
                let endFrames = try parser.finish()
                if !endFrames.isEmpty { throw APIError("Incomplete event stream frame.") }
                throw APIError("Session event stream ended.")
            } catch is CancellationError { finishObservation(ref, generation: generation); return }
            catch let error as APIError where error.statusCode == 401 || error.statusCode == 403 {
                if observationIsCurrent(ref, client: client, generation: generation) {
                    unauthorizedServers.insert(ref.serverID)
                    markServer(ref.serverID, state: .unauthorized)
                    for other in openSessions.keys where other.serverID == ref.serverID {
                        if var current = openSessions[other] { current.connection = .unauthorized; openSessions[other] = current }
                        if other != ref { streamGenerations[other] = nil; streamTasks[other]?.cancel(); streamTasks[other] = nil }
                    }
                    errorMessage = "Server authorization expired. Reconnect this server to resume session updates."
                }
                finishObservation(ref, generation: generation)
                return
            }
            catch is StreamResnapshotRequired {
                guard observationIsCurrent(ref, client: client, generation: generation) else { finishObservation(ref, generation: generation); return }
                markAcceptedSkillOperationsUnknown(ref, reason: "Event replay lost its previous epoch or cursor; completion is unknown.")
                failures = 0
                cursor = SessionEventCursor(expectedSessionID: ref.sessionID)
                projector = nil
                if var current = openSessions[ref] { current.cursor = 0; current.epoch = ""; current.connection = .reconnecting; openSessions[ref] = current }
                continue
            }
            catch {
                guard observationIsCurrent(ref, client: client, generation: generation) else { finishObservation(ref, generation: generation); return }
                failures += 1
                if failures >= 3 {
                    if var current = openSessions[ref] { current.connection = .offline; openSessions[ref] = current }
                    try? await Task.sleep(for: .seconds(3))
                    failures = 0
                } else { try? await Task.sleep(for: .milliseconds(250 * failures)) }
                if let snapshot = try? await refreshSession(ref, generation: generation) {
                    if projector == nil, let current = openSessions[ref] { projector = SessionEventProjector(projection: current) }
                    projector?.replace(with: snapshot)
                    if var current = projector {
                        current.updateCursor(sequence: snapshot.cursor ?? 0, epoch: snapshot.epoch ?? "")
                        projector = current
                        openSessions[ref] = current.projection
                        cursor = SessionEventCursor(expectedSessionID: ref.sessionID, epoch: current.projection.epoch.isEmpty ? nil : current.projection.epoch, sequence: current.projection.cursor, isReady: true)
                    }
                }
            }
        }
    }

    private func finishObservation(_ ref: SessionRef, generation: UUID) {
        guard streamGenerations[ref] == generation else { return }
        streamGenerations[ref] = nil
        streamTasks[ref] = nil
    }

    private func applySkillOperation(_ envelope: StreamEnvelope, to ref: SessionRef) {
        guard let event = envelope.event,
              event["type"]?.stringValue == "skill_operation",
              event["session_id"]?.stringValue == ref.sessionID,
              let operationID = event["operation_id"]?.stringValue, !operationID.isEmpty,
              let skillName = event["skill"]?.stringValue, !skillName.isEmpty else { return }
        let state: ForkSkillOperationState
        switch event["status"]?.stringValue {
        case "accepted": state = .accepted
        case "completed": state = .completed
        case "failed": state = .failed
        default: state = .unknown
        }
        let existing = skillOperations[ref]?.first(where: { $0.operationID == operationID })
        if existing?.state == .completed || existing?.state == .failed { return }
        upsertSkillOperation(SkillOperationProjection(operationID: operationID, skillName: skillName, state: state,
                                                       error: event["error"]?.stringValue.map { String($0.prefix(1024)) }), ref: ref)
    }

    private func markAcceptedSkillOperationsUnknown(_ ref: SessionRef, reason: String) {
        guard var operations = skillOperations[ref] else { return }
        for index in operations.indices where operations[index].state == .accepted {
            operations[index].state = .unknown
            operations[index].error = reason
        }
        skillOperations[ref] = operations
    }

    private func refreshNeedsYou(_ serverID: UUID, client: APIClient, rows: [SessionProjection]) async {
        var current = needsYou.filter { $0.id.serverID != serverID }
        for row in rows where row.state == .waiting || row.needsYouReason != nil {
            let reason = row.needsYouReason ?? (row.state == .waiting ? "Waiting for input" : "Needs attention")
            current.append(NeedsYouProjection(id: row.ref, reason: reason, refreshedAt: .now))
        }
        needsYou = current
    }

    private func stableTranscript(_ old: [TranscriptItem], _ new: [TranscriptItem]) -> [TranscriptItem] {
        let oldByFingerprint = Dictionary(grouping: old, by: { "\($0.kind.rawValue)|\($0.owner ?? "")|\($0.role ?? "")|\($0.text)" })
        return new.map { item in
            let key = "\(item.kind.rawValue)|\(item.owner ?? "")|\(item.role ?? "")|\(item.text)"
            if let candidate = oldByFingerprint[key]?.first(where: { $0.id.hasPrefix("snapshot:") }) {
                return TranscriptItem(id: candidate.id, kind: item.kind, owner: item.owner, role: item.role, text: item.text, isStreaming: item.isStreaming, origin: item.origin, toolName: item.toolName, toolArguments: item.toolArguments, toolOutput: item.toolOutput, toolStatus: item.toolStatus, child: item.child, attachments: item.attachments, generatedMedia: item.generatedMedia, userMessageIndex: item.userMessageIndex, branchAnchor: item.branchAnchor)
            }
            return item
        }
    }

    func loadSavedCredentials() async {
        for config in configStore.load() {
            do {
                let token = try await tokenStore.read(for: config.id) ?? ""
                tokens[config.id] = token
                let client = try APIClient(config: config, token: token, session: apiSession)
                let identity: ServerIdentity = try await client.request(ServerIdentity.self, path: "api/v2/server")
                guard identity.version == 1, identity.sessionAPIVersion == 2, config.instanceID == nil || config.instanceID == identity.instanceID else { throw APIError("Server identity changed or is unsupported. Edit and test this server before reconnecting.") }
                clients[config.id] = client
                if let index = servers.firstIndex(where: { $0.id == config.id }) {
                    servers[index].identity = identity; servers[index].state = .live; servers[index].refreshedAt = .now
                }

            } catch { markServer(config.id, state: .offline) }
        }
        await refreshForeground()
    }

    private func markServer(_ id: UUID, state: ConnectionState) {
        guard let i = servers.firstIndex(where: { $0.id == id }) else { return }; servers[i].state = state; servers[i].refreshedAt = .now
    }
    private static func pathComponent(_ value: String) -> String { value.addingPercentEncoding(withAllowedCharacters: CharacterSet(charactersIn: "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-._~")) ?? value }
}

private struct StreamResnapshotRequired: Error {}

private actor LocalSmokeTokenStore: TokenStoring {
    func read(for id: UUID) async throws -> String? { nil }
    func write(_ token: String, for id: UUID) async throws { throw APIError("Local smoke mode does not store credentials.") }
    func delete(for id: UUID) async throws {}
}

@MainActor
extension AppModel {
    public static func localSmokeModel(fileManager: FileManager = .default) throws -> AppModel {
        let root = fileManager.temporaryDirectory.appending(path: "DockerAgentLocalSmoke/\(UUID().uuidString)", directoryHint: .isDirectory)
        try fileManager.createDirectory(at: root, withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])
        let config = InstanceConfigStore(fileURL: root.appending(path: "instances.json"))
        let outbox = try DurableSendOutbox(fileURL: root.appending(path: "outbox.json"))
        let reviews = ReviewDraftStore(fileURL: root.appending(path: "review-drafts.json"))
        let configuration = URLSessionConfiguration.ephemeral
        configuration.protocolClasses = [LocalSmokeNetworkBlocker.self]
        let network = URLSession(configuration: configuration)
        return AppModel(tokenStore: LocalSmokeTokenStore(), configStore: config, outbox: outbox, reviewDrafts: reviews, urlSession: network, autoConnect: false, localOnly: true)
    }
}

private final class LocalSmokeNetworkBlocker: URLProtocol, @unchecked Sendable {
    override class func canInit(with request: URLRequest) -> Bool { true }
    override class func canonicalRequest(for request: URLRequest) -> URLRequest { request }
    override func startLoading() {
        client?.urlProtocol(self, didFailWithError: APIError("Network access is disabled in local smoke mode."))
    }
    override func stopLoading() {}
}
