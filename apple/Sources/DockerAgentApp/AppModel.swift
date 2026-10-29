import AgentKit
import Foundation
import Observation

public struct SessionViewLease: Hashable, Sendable {
    public let ref: SessionRef
    public let id: UUID

    init(ref: SessionRef, id: UUID = UUID()) {
        self.ref = ref
        self.id = id
    }
}

private struct SessionOpenAttempt {
    let id: UUID
    let task: Task<Void, Error>
}

private struct SessionCreateAttempt {
    let sessionID: String
    let target: String
    let payload: String
}

@MainActor
final class ProjectionPublicationThrottle {
    private let interval: Duration
    private let publishProjection: @MainActor (OpenSessionProjection) -> Void
    private var latest: OpenSessionProjection?
    private var task: Task<Void, Never>?

    init(interval: Duration = .milliseconds(50), publish: @escaping @MainActor (OpenSessionProjection) -> Void) {
        self.interval = interval
        publishProjection = publish
    }

    func schedule(_ projection: OpenSessionProjection) {
        latest = projection
        guard task == nil else { return }
        task = Task { @MainActor [weak self] in
            guard let self else { return }
            try? await Task.sleep(for: interval)
            guard !Task.isCancelled else { return }
            flush()
        }
    }

    func publishImmediately(_ projection: OpenSessionProjection) {
        task?.cancel()
        task = nil
        latest = nil
        publishProjection(projection)
    }

    func flush() {
        task?.cancel()
        task = nil
        guard let latest else { return }
        self.latest = nil
        publishProjection(latest)
    }

    func cancel() {
        task?.cancel()
        task = nil
        latest = nil
    }
}

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
    public var catalogLoadedServers: Set<UUID> = []
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
    private var sessionLeaseGenerations: [SessionRef: [UUID: UUID]] = [:]
    private var legacySessionLeases: [SessionRef: [SessionViewLease]] = [:]
    private var sessionOpeningTasks: [SessionRef: SessionOpenAttempt] = [:]
    private var sessionOpenIDs: [SessionRef: UUID] = [:]
    private var historyPagingSupported: [SessionRef: Bool] = [:]
    public var historyPageLoading: Set<SessionRef> = []
    public var historyPageErrors: [SessionRef: String] = [:]
    private var historyPageRequestIDs: [SessionRef: UUID] = [:]
    private var historyRebaselinePending = Set<SessionRef>()
    private var activeSceneIDs = Set<UUID>()
    private var sceneLifecycleObserved = false
    private var sceneObserversSuspended = false
    private var unauthorizedServers = Set<UUID>()
    private var testedDrafts: [UUID: ServerDraft] = [:]
    private var testedIdentities: [UUID: ServerIdentity] = [:]
    private let apiSession: URLSession?
    private var sessionCreationTasks: [String: Task<SessionRef, Error>] = [:]
    private var completedSessionCreates: [String: SessionRef] = [:]
    private var completedSessionCreateKeys: [String] = []
    private var createAttemptsByOperation: [String: SessionCreateAttempt] = [:]
    private var createAttemptKeys: [String] = []
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
        let existing = servers.first(where: { $0.id == draft.id })
        let sameOrigin = existing.map { URLPolicy.sameOrigin($0.baseURL, draft.baseURL) } ?? false
        let preferredSource = sameOrigin ? existingPreferredSource(draft.id) : nil
        let config = InstanceConfig(id: draft.id, name: draft.name, baseURL: draft.baseURL, allowsLocalHTTP: draft.allowsLocalHTTP, instanceID: identity.instanceID, capabilities: identity.capabilities, preferredSource: preferredSource)
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
        for ref in Array(historyPageRequestIDs.keys).filter({ $0.serverID == id }) { historyPageRequestIDs[ref] = nil; historyPageLoading.remove(ref); historyPageErrors[ref] = nil; historyRebaselinePending.remove(ref) }
        historyPagingSupported = historyPagingSupported.filter { $0.key.serverID != id }
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
        guard identity.version == 1, identity.sessionAPIVersion == 2 else {
            throw APIError("The server API version is unsupported.")
        }
        guard let currentServer = servers.first(where: { $0.id == serverID }),
              clients[serverID] === client,
              currentServer.baseURL == server.baseURL,
              currentServer.identity?.instanceID == pinnedInstance,
              URLPolicy.sameOrigin(configURL.absoluteString, client.config.baseURL) else {
            throw APIError("The server changed while validating its identity.")
        }
        guard pinnedInstance == nil || pinnedInstance == identity.instanceID else {
            markServerIdentityChanged(serverID)
            throw APIError(ServerConnectionIssue.identityChanged.message)
        }
        clearServerConnectionIssue(serverID)
    }

    private func invalidateServerState(_ id: UUID) {
        for ref in streamTasks.keys.filter({ $0.serverID == id }) { streamTasks[ref]?.cancel(); streamTasks[ref] = nil }
        for ref in sessionLeases.keys.filter({ $0.serverID == id }) {
            sessionLeases[ref] = nil
            sessionLeaseGenerations[ref] = nil
            legacySessionLeases[ref] = nil
            sessionOpenIDs[ref] = nil
            historyPagingSupported[ref] = nil
            historyPageRequestIDs[ref] = nil
            historyPageLoading.remove(ref)
            historyPageErrors[ref] = nil
            historyRebaselinePending.remove(ref)
        }
        for ref in Array(sessionOpeningTasks.keys).filter({ $0.serverID == id }) {
            sessionOpeningTasks[ref]?.task.cancel()
            sessionOpeningTasks[ref] = nil
        }
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
        nextCatalogCursor[id] = nil; catalogHasMore.remove(id); catalogLoadedServers.remove(id); agentsByServer[id] = nil
    }

    public func refreshForeground() async {
        isRefreshing = true; defer { isRefreshing = false }
        for server in servers { await refreshCatalog(server.id, reset: true) }
    }

    public func refreshCatalog(_ serverID: UUID, reset: Bool = true) async {
        guard !localOnly else { markServer(serverID, state: .offline); return }
        guard let client = clients[serverID] else { markServer(serverID, state: .offline); return }
        do {
            if reset { nextCatalogCursor[serverID] = nil; catalogHasMore.remove(serverID); catalogLoadedServers.remove(serverID); sessions.removeAll { $0.ref.serverID == serverID } }
            var query = [URLQueryItem(name: "limit", value: "50")]
            if let cursor = nextCatalogCursor[serverID] { query.append(URLQueryItem(name: "cursor", value: cursor)) }
            let page: CatalogResponse = try await client.request(CatalogResponse.self, path: "api/v2/sessions", query: query)
            guard clients[serverID] === client else { return }
            let projected = page.sessions.filter { $0.parentID == nil || $0.parentID?.isEmpty == true }.map { $0.projection(serverID: serverID) }
            sessions.append(contentsOf: projected)
            catalogLoadedServers.insert(serverID)
            nextCatalogCursor[serverID] = page.nextCursor?.isEmpty == false ? page.nextCursor : nil
            if page.nextCursor?.isEmpty != false { catalogHasMore.remove(serverID) } else { catalogHasMore.insert(serverID) }
            if let index = servers.firstIndex(where: { $0.id == serverID }) {
                servers[index].sessionCount = sessions.filter { $0.ref.serverID == serverID }.count
                servers[index].state = .live; servers[index].refreshedAt = .now
            }
            await refreshNeedsYou(serverID, client: client, rows: projected)
            guard clients[serverID] === client else { return }
        } catch let error as APIError {
            guard clients[serverID] === client else { return }
            markServer(serverID, state: error.statusCode == 401 ? .unauthorized : .offline); errorMessage = error.message
        } catch {
            guard clients[serverID] === client else { return }
            markServer(serverID, state: .offline); errorMessage = error.localizedDescription
        }
    }

    public func loadMoreSessions(serverID: UUID) async {
        guard catalogHasMore.contains(serverID) else { return }
        await refreshCatalog(serverID, reset: false)
    }

    public func loadAgents(serverID: UUID) async throws -> [AgentOption] {
        guard let client = clients[serverID] else { throw APIError("Server is not connected.") }
        let values: [AgentOption] = try await client.request([AgentOption].self, path: "api/agents")
        guard clients[serverID] === client else { throw APIError("The server changed while loading its agent teams.") }
        agentsByServer[serverID] = values
        return values
    }

    public func defaultSessionSource(serverID: UUID) async throws -> AgentOption {
        let sources = try await loadAgents(serverID: serverID)
        return try resolveSessionSource(sources, serverID: serverID)
    }

    public func resolveSessionSource(_ sources: [AgentOption], serverID: UUID) throws -> AgentOption {
        try resolveDefaultSource(sources, serverID: serverID)
    }

    public func rememberSessionSource(_ source: AgentOption, serverID: UUID) throws {
        guard let config = configStore.load().first(where: { $0.id == serverID }),
              agentsByServer[serverID]?.contains(where: { $0 == source && $0.isStartable }) == true else {
            throw APIError("Refresh the server's agent list before saving this choice.")
        }
        var updated = config
        updated.preferredSource = source.name
        try configStore.upsert(updated)
    }

    public func createDefaultSession(serverID: UUID, operationID: UUID = UUID()) async throws -> SessionRef {
        let (client, instanceID) = try sessionCreationTarget(serverID)
        let workspace = servers.first(where: { $0.id == serverID })?.identity?.workspaceRoot
        let key = ["default", serverID.uuidString, instanceID ?? "", String(describing: ObjectIdentifier(client)), operationID.uuidString].joined(separator: "\u{1f}")
        return try await coalescedSessionCreate(key: key) {
            let sources: [AgentOption] = try await client.request([AgentOption].self, path: "api/agents")
            try self.requireCurrentCreationTarget(serverID, client: client, instanceID: instanceID)
            self.agentsByServer[serverID] = sources
            let source = try self.resolveDefaultSource(sources, serverID: serverID)
            return try await self.performCreateSession(serverID: serverID, client: client, instanceID: instanceID,
                                                       operationID: operationID, sourceName: source.name,
                                                       agentName: source.rootAgentName!, title: "", model: "",
                                                       workingDir: workspace, firstMessage: nil)
        }
    }

    public func createSession(serverID: UUID, source: AgentOption, title: String = "", model: String = "", workingDir: String? = nil, firstMessage: String? = nil, operationID: UUID = UUID()) async throws -> SessionRef {
        guard !source.name.isEmpty else { throw APIError("Select a valid agent source.") }
        guard let rootAgentName = source.rootAgentName?.trimmingCharacters(in: .whitespacesAndNewlines), !rootAgentName.isEmpty else {
            throw APIError("This server doesn't report the configured root agent. Update the server before creating a session.")
        }
        let (client, instanceID) = try sessionCreationTarget(serverID)
        let directory = workingDir ?? servers.first(where: { $0.id == serverID })?.identity?.workspaceRoot
        let sessionTitle = title.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty ? "" : title
        let key = ["explicit", serverID.uuidString, instanceID ?? "", String(describing: ObjectIdentifier(client)), operationID.uuidString,
                   source.name, rootAgentName, sessionTitle, model, directory ?? "", firstMessage ?? ""].joined(separator: "\u{1f}")
        return try await coalescedSessionCreate(key: key) {
            try await self.performCreateSession(serverID: serverID, client: client, instanceID: instanceID,
                                                operationID: operationID, sourceName: source.name,
                                                agentName: rootAgentName, title: sessionTitle, model: model,
                                                workingDir: directory, firstMessage: firstMessage)
        }
    }

    func createSession(serverID: UUID, sourceName: String, agentName: String, title: String = "", model: String = "", workingDir: String? = nil, firstMessage: String? = nil) async throws -> SessionRef {
        let (client, instanceID) = try sessionCreationTarget(serverID)
        return try await performCreateSession(serverID: serverID, client: client, instanceID: instanceID,
                                              operationID: UUID(), sourceName: sourceName, agentName: agentName,
                                              title: title, model: model, workingDir: workingDir, firstMessage: firstMessage)
    }

    private func coalescedSessionCreate(key: String, operation: @escaping @MainActor () async throws -> SessionRef) async throws -> SessionRef {
        if let completed = completedSessionCreates[key] { return completed }
        if let active = sessionCreationTasks[key] { return try await active.value }
        let task = Task { try await operation() }
        sessionCreationTasks[key] = task
        do {
            let ref = try await task.value
            sessionCreationTasks[key] = nil
            completedSessionCreates[key] = ref
            completedSessionCreateKeys.append(key)
            if completedSessionCreateKeys.count > 32 {
                completedSessionCreates[completedSessionCreateKeys.removeFirst()] = nil
            }
            return ref
        } catch {
            sessionCreationTasks[key] = nil
            throw error
        }
    }

    private func sessionCreationTarget(_ serverID: UUID) throws -> (APIClient, String?) {
        guard let client = clients[serverID], let server = servers.first(where: { $0.id == serverID }) else {
            throw APIError("Server is not connected.")
        }
        return (client, server.identity?.instanceID)
    }

    private func requireCurrentCreationTarget(_ serverID: UUID, client: APIClient, instanceID: String?) throws {
        guard clients[serverID] === client,
              servers.first(where: { $0.id == serverID })?.identity?.instanceID == instanceID else {
            throw APIError("The server changed while creating the session; it was not opened on another profile.")
        }
    }

    private func resolveDefaultSource(_ sources: [AgentOption], serverID: UUID) throws -> AgentOption {
        let preferred = configStore.load().first(where: { $0.id == serverID })?.preferredSource
        if let preferred {
            guard let source = sources.first(where: { $0.name == preferred && $0.isStartable }) else {
                throw APIError("Your saved agent team is no longer available. Choose another in Options….")
            }
            return source
        }
        if let configured = servers.first(where: { $0.id == serverID })?.identity?.source, !configured.isEmpty {
            guard let source = sources.first(where: { $0.name == configured && $0.isStartable }) else {
                throw APIError("The server's configured default agent team is unavailable. Refresh the server or choose another in Options….")
            }
            return source
        }
        guard !sources.isEmpty else { throw APIError("This server has no available agent teams.") }
        if sources.count == 1, let only = sources.first, only.isStartable { return only }
        if !sources.contains(where: \.isStartable) { throw APIError("This server doesn't report configured root agents. Update the server before creating a session.") }
        throw APIError("Choose an agent team in Options… before creating a session on this multi-source server.")
    }

    private func performCreateSession(serverID: UUID, client: APIClient, instanceID: String?, operationID: UUID,
                                     sourceName: String, agentName: String, title: String, model: String,
                                     workingDir: String?, firstMessage: String?) async throws -> SessionRef {
        try requireCurrentCreationTarget(serverID, client: client, instanceID: instanceID)
        let operationKey = [serverID.uuidString, operationID.uuidString].joined(separator: "\u{1f}")
        let targetKey = "\(instanceID ?? "")|\(ObjectIdentifier(client))"
        let payloadKey = [sourceName, agentName, title, model, workingDir ?? "", firstMessage ?? ""].joined(separator: "\u{1f}")
        let attempt: SessionCreateAttempt
        if let existing = createAttemptsByOperation[operationKey] {
            guard existing.target == targetKey, existing.payload == payloadKey else {
                throw APIError("This create action is already bound to a different server or input. Start a fresh action rather than replaying it.")
            }
            attempt = existing
        } else {
            attempt = SessionCreateAttempt(sessionID: UUID().uuidString, target: targetKey, payload: payloadKey)
            createAttemptsByOperation[operationKey] = attempt
            createAttemptKeys.append(operationKey)
            if createAttemptKeys.count > 64 {
                createAttemptsByOperation[createAttemptKeys.removeFirst()] = nil
            }
        }
        let sessionID = attempt.sessionID
        var alreadyCreated = false
        do {
            if let firstMessage, !firstMessage.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty {
                let body = StartSessionRequest(sessionID: sessionID, source: sourceName, agentName: agentName, title: title, model: model,
                                               workingDir: workingDir, input: SessionInput(content: firstMessage, requestID: operationID.uuidString,
                                                                                         generateTitle: title.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty))
                try await client.noContent(method: "POST", path: "api/v2/sessions/start", body: body, accepted: [202])
            } else {
                let body = CreateSessionRequest(sessionID: sessionID, source: sourceName, agentName: agentName, title: title, model: model, workingDir: workingDir)
                try await client.noContent(method: "POST", path: "api/v2/sessions", body: body, accepted: [201])
            }
        } catch let error as APIError where error.statusCode == 409 {
            alreadyCreated = true
        } catch let error as APIError where error.statusCode.map({ (400..<500).contains($0) && $0 != 408 && $0 != 409 && $0 != 429 }) == true {
            createAttemptsByOperation[operationKey] = nil
            createAttemptKeys.removeAll { $0 == operationKey }
            throw error
        }
        try requireCurrentCreationTarget(serverID, client: client, instanceID: instanceID)
        let ref = SessionRef(serverID: serverID, sessionID: sessionID)
        await refreshCatalog(serverID)
        try requireCurrentCreationTarget(serverID, client: client, instanceID: instanceID)
        if alreadyCreated, !sessions.contains(where: { $0.ref == ref }) {
            throw APIError("The previous create attempt may have committed, but this server could not confirm it. Retry using the same action.")
        }
        createAttemptsByOperation[operationKey] = nil
        createAttemptKeys.removeAll { $0 == operationKey }
        return ref
    }

    private func existingPreferredSource(_ id: UUID) -> String? {
        configStore.load().first(where: { $0.id == id })?.preferredSource
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
        guard identity.version == 1, identity.sessionAPIVersion == 2 else {
            throw APIError("Server API version is unsupported.")
        }
        guard pinnedInstance == nil || pinnedInstance == identity.instanceID else {
            markServerIdentityChanged(serverID)
            throw APIError(ServerConnectionIssue.identityChanged.message)
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
            servers[index].connectionIssue = nil
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

    /// Acquires an independently releasable owner for one view of a session.
    /// Concurrent views share the same confirmed open; cancelling one waiter only
    /// releases that view's lease and never cancels another owner's open.
    public func acquireSessionView(_ ref: SessionRef) async throws -> SessionViewLease {
        try await acquireSessionLease(ref, legacyOwner: false)
    }

    private func acquireSessionLease(_ ref: SessionRef, legacyOwner: Bool) async throws -> SessionViewLease {
        guard !localOnly else { throw APIError("Network access is disabled in local smoke mode.") }
        try Task.checkCancellation()
        let generation: UUID
        if openSessions[ref] != nil {
            generation = sessionOpenIDs[ref] ?? UUID()
            sessionOpenIDs[ref] = generation
        } else if let opening = sessionOpeningTasks[ref] {
            generation = opening.id
        } else {
            guard let client = clients[ref.serverID] else { throw APIError("Server is not connected.") }
            let instanceID = servers.first(where: { $0.id == ref.serverID })?.identity?.instanceID
            generation = UUID()
            sessionOpenIDs[ref] = generation
            sessionLeases[ref] = 0
            let task = Task { try await self.openSessionNetwork(ref, client: client, instanceID: instanceID, openingID: generation) }
            sessionOpeningTasks[ref] = SessionOpenAttempt(id: generation, task: task)
            let lease = SessionViewLease(ref: ref)
            addSessionLease(lease, generation: generation)
            if legacyOwner { legacySessionLeases[ref, default: []].append(lease) }
            return try await awaitSessionOpen(lease, task: task)
        }

        let lease = SessionViewLease(ref: ref)
        addSessionLease(lease, generation: generation)
        if legacyOwner { legacySessionLeases[ref, default: []].append(lease) }
        if let opening = sessionOpeningTasks[ref], opening.id == generation {
            return try await awaitSessionOpen(lease, task: opening.task)
        }
        if unauthorizedServers.contains(ref.serverID) {
            do { try await reconnectServer(ref.serverID) }
            catch { releaseSessionView(lease); throw error }
        }
        return lease
    }

    private func awaitSessionOpen(_ lease: SessionViewLease, task: Task<Void, Error>) async throws -> SessionViewLease {
        do {
            try await withTaskCancellationHandler {
                try await task.value
            } onCancel: { [weak self] in
                Task { @MainActor [weak self] in
                    guard let self else { return }
                    self.releaseSessionView(lease)
                }
            }
            try Task.checkCancellation()
            guard let generation = sessionLeaseGenerations[lease.ref]?[lease.id],
                  sessionOpenIDs[lease.ref] == generation,
                  openSessions[lease.ref] != nil else {
                throw CancellationError()
            }
            if sessionOpeningTasks[lease.ref]?.id == generation { sessionOpeningTasks[lease.ref] = nil }
            return lease
        } catch {
            releaseSessionView(lease)
            throw error
        }
    }

    private func addSessionLease(_ lease: SessionViewLease, generation: UUID) {
        sessionLeaseGenerations[lease.ref, default: [:]][lease.id] = generation
        sessionLeases[lease.ref] = sessionLeaseGenerations[lease.ref]?.count ?? 0
    }

    /// Idempotently releases only the owner returned by `acquireSessionView`.
    public func releaseSessionView(_ lease: SessionViewLease) {
        guard var owners = sessionLeaseGenerations[lease.ref],
              let generation = owners.removeValue(forKey: lease.id) else { return }
        if owners.isEmpty {
            sessionLeaseGenerations[lease.ref] = nil
            sessionLeases[lease.ref] = nil
        } else {
            sessionLeaseGenerations[lease.ref] = owners
            sessionLeases[lease.ref] = owners.count
        }
        legacySessionLeases[lease.ref]?.removeAll { $0.id == lease.id }
        if !owners.values.contains(generation), sessionOpenIDs[lease.ref] == generation {
            closeSessionGeneration(lease.ref, generation: generation)
        }
    }

    private func closeSessionGeneration(_ ref: SessionRef, generation: UUID?) {
        if let generation, sessionOpenIDs[ref] != generation { return }
        sessionOpenIDs[ref] = nil
        historyPagingSupported[ref] = nil
        historyPageRequestIDs[ref] = nil
        historyPageLoading.remove(ref)
        historyPageErrors[ref] = nil
        historyRebaselinePending.remove(ref)
        streamGenerations[ref] = nil
        if sessionOpeningTasks[ref]?.id == generation {
            sessionOpeningTasks[ref]?.task.cancel()
            sessionOpeningTasks[ref] = nil
        }
        streamTasks[ref]?.cancel()
        streamTasks[ref] = nil
        openSessions[ref] = nil
    }

    private func failSessionOpenGeneration(_ ref: SessionRef, generation: UUID) {
        guard sessionOpenIDs[ref] == generation else { return }
        let failedLeaseIDs = sessionLeaseGenerations[ref, default: [:]].compactMap { $0.value == generation ? $0.key : nil }
        for leaseID in failedLeaseIDs { sessionLeaseGenerations[ref]?[leaseID] = nil }
        if sessionLeaseGenerations[ref]?.isEmpty == true { sessionLeaseGenerations[ref] = nil }
        legacySessionLeases[ref]?.removeAll { failedLeaseIDs.contains($0.id) }
        let count = sessionLeaseGenerations[ref]?.count ?? 0
        sessionLeases[ref] = count == 0 ? nil : count
        sessionOpenIDs[ref] = nil
        historyPagingSupported[ref] = nil
        historyPageRequestIDs[ref] = nil
        historyPageLoading.remove(ref)
        historyPageErrors[ref] = nil
        historyRebaselinePending.remove(ref)
        if sessionOpeningTasks[ref]?.id == generation { sessionOpeningTasks[ref] = nil }
        streamGenerations[ref] = nil
        streamTasks[ref]?.cancel()
        streamTasks[ref] = nil
        openSessions[ref] = nil
    }

    /// Compatibility owner for non-view callers. Pair each successful call with
    /// one `closeSession`; view lifecycle uses the explicit lease API instead.
    public func openSession(_ ref: SessionRef) async throws {
        _ = try await acquireSessionLease(ref, legacyOwner: true)
    }

    func cancelOpeningSession(_ ref: SessionRef) {
        guard let generation = sessionOpenIDs[ref], openSessions[ref] == nil else { return }
        sessionOpeningTasks[ref]?.task.cancel()
        failSessionOpenGeneration(ref, generation: generation)
    }

    private func openSessionNetwork(_ ref: SessionRef, client: APIClient, instanceID: String?, openingID: UUID) async throws {
        do {
            try Task.checkCancellation()
            guard sessionOpenIDs[ref] == openingID,
                  sessionLeaseGenerations[ref]?.values.contains(openingID) == true else { throw CancellationError() }
            guard clients[ref.serverID] === client,
                  servers.first(where: { $0.id == ref.serverID })?.identity?.instanceID == instanceID else {
                throw APIError("The server changed while opening the session.")
            }
            // Capabilities can be added by rebuilding the server without changing its stable
            // instance ID. Refresh once for this shared open generation; never replay session writes.
            let identity: ServerIdentity = try await client.request(ServerIdentity.self, path: "api/v2/server")
            try Task.checkCancellation()
            guard sessionOpenIDs[ref] == openingID,
                  sessionLeaseGenerations[ref]?.values.contains(openingID) == true,
                  clients[ref.serverID] === client,
                  servers.first(where: { $0.id == ref.serverID })?.identity?.instanceID == instanceID else {
                throw APIError("The server changed while opening the session.")
            }
            guard identity.version == 1, identity.sessionAPIVersion == 2 else {
                throw APIError("Server API version is unsupported.")
            }
            guard instanceID == nil || identity.instanceID == instanceID else {
                markServerIdentityChanged(ref.serverID)
                throw APIError(ServerConnectionIssue.identityChanged.message)
            }
            historyPagingSupported[ref] = identity.supportsSessionHistoryPages
            if let serverIndex = servers.firstIndex(where: { $0.id == ref.serverID }) {
                servers[serverIndex].identity = identity
                servers[serverIndex].connectionIssue = nil
                servers[serverIndex].refreshedAt = .now
            }
            let useMetadataProjection = identity.supportsMetadataSessionView
            let prepared = try await client.prepareSessionView(ref.sessionID, metadataOnly: useMetadataProjection)
            try Task.checkCancellation()
            guard sessionOpenIDs[ref] == openingID,
                  sessionLeaseGenerations[ref]?.values.contains(openingID) == true else { throw CancellationError() }
            guard clients[ref.serverID] === client, servers.first(where: { $0.id == ref.serverID })?.identity?.instanceID == instanceID else { throw APIError("Server was removed or changed while opening the session.") }
            guard prepared.version == 2, prepared.view == "prepare-info",
                  prepared.info.sessionID == ref.sessionID,
                  let confirmation = prepared.confirmation, confirmation.sessionID == ref.sessionID else {
                throw APIError("Server did not provide a confirmed view for the requested session.")
            }
            guard !useMetadataProjection || prepared.projection == "metadata" else {
                throw APIError("The server advertised metadata-only session views but did not confirm the metadata projection.")
            }
            let opened = try await client.openSessionView(ref.sessionID, confirmation: confirmation, metadataOnly: useMetadataProjection)
            try Task.checkCancellation()
            guard sessionOpenIDs[ref] == openingID,
                  sessionLeaseGenerations[ref]?.values.contains(openingID) == true else { throw CancellationError() }
            guard clients[ref.serverID] === client, servers.first(where: { $0.id == ref.serverID })?.identity?.instanceID == instanceID else { throw APIError("Server was removed or changed while opening the session.") }
            guard opened.view == "open-view", opened.version == 2 else { throw APIError("Server did not confirm the session view.") }
            guard !useMetadataProjection || opened.projection == "metadata" else {
                throw APIError("The server advertised metadata-only session views but did not confirm the opened metadata projection.")
            }
            let (initial, rawProof): (SessionSnapshotWire, RawSnapshotProof?)
            if identity.supportsSessionHistoryPages {
                let tail = try await client.sessionHistoryTail(ref.sessionID)
                try tail.validateTail(expectedSessionID: ref.sessionID)
                initial = tail
                rawProof = nil
            } else {
                (initial, rawProof) = try await verifiedRawSessionSnapshot(client: client, ref: ref)
            }
            try Task.checkCancellation()
            guard sessionOpenIDs[ref] == openingID,
                  sessionLeaseGenerations[ref]?.values.contains(openingID) == true else { throw CancellationError() }
            guard clients[ref.serverID] === client, servers.first(where: { $0.id == ref.serverID })?.identity?.instanceID == instanceID else { throw APIError("Server was removed or changed while opening the session.") }
            var projection = initial.project(ref: ref, connection: .live, rawProof: rawProof, branchInstanceID: servers.first(where: { $0.id == ref.serverID })?.identity?.instanceID)
            projection.capabilities = opened.metadata?.capabilities ?? .init()
            openSessions[ref] = projection
            refreshPendingSends(ref)
            startObserving(ref, client: client)
            if sessionOpeningTasks[ref]?.id == openingID { sessionOpeningTasks[ref] = nil }
        } catch {
            failSessionOpenGeneration(ref, generation: openingID)
            throw error
        }
    }

    public func closeSession(_ ref: SessionRef) {
        if var leases = legacySessionLeases[ref], let lease = leases.popLast() {
            legacySessionLeases[ref] = leases.isEmpty ? nil : leases
            releaseSessionView(lease)
            return
        }
        // Never let an unscoped compatibility close consume a view-owned lease.
        guard sessionLeaseGenerations[ref]?.isEmpty != false else { return }
        // Preserve old test/manual observation setups that seed lease counts directly.
        let remaining = max(0, (sessionLeases[ref] ?? 1) - 1)
        sessionLeases[ref] = remaining == 0 ? nil : remaining
        if remaining == 0 {
            sessionLeaseGenerations[ref] = nil
            closeSessionGeneration(ref, generation: sessionOpenIDs[ref])
        }
    }

    public func send(_ ref: SessionRef, content: String, attachments: [SessionDocumentAttachment] = [], mode: String = "submit", requestID: String = UUID().uuidString) async throws {
        let payload = try SessionSendPayload(content: content, attachments: attachments)
        let identity = try await durableIdentity(for: ref)
        guard let outbox else { throw APIError(outboxError ?? "Durable send storage is unavailable.") }
        let record = try outbox.prepare(identity: identity, sendPayload: payload, mode: mode, requestID: requestID, generateTitle: true)
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
            let body = try record.sendPayload.requestBody(requestID: record.requestID, mode: record.mode, generateTitle: record.generateTitle)
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

    public func loadOlderSessionHistory(_ ref: SessionRef) async throws {
        guard !historyPageLoading.contains(ref) else { return }
        guard historyPagingSupported[ref] == true else { throw APIError("This server does not support bounded session history.") }
        guard let client = clients[ref.serverID], let initial = openSessions[ref],
              let cursor = initial.transcriptWindow.olderCursor,
              let start = initial.transcriptWindow.startPosition,
              let anchorEnd = initial.transcriptWindow.anchorEndPosition,
              let totalItems = initial.transcriptWindow.totalItemCount,
              let anchorCursor = initial.transcriptWindow.anchorCursor,
              !initial.epoch.isEmpty else { throw APIError("No older server history is available.") }
        guard let generation = sessionOpenIDs[ref] else { throw CancellationError() }
        let requestID = UUID()
        historyPageRequestIDs[ref] = requestID
        historyPageLoading.insert(ref)
        historyPageErrors[ref] = nil
        defer {
            if historyPageRequestIDs[ref] == requestID {
                historyPageRequestIDs[ref] = nil
                historyPageLoading.remove(ref)
            }
        }
        do {
            let page = try await client.sessionHistoryPage(ref.sessionID, cursor: cursor)
            try page.validate(sessionID: ref.sessionID, epoch: initial.epoch, cursor: anchorCursor,
                              anchorEndPosition: anchorEnd, totalItems: totalItems, previousStartPosition: start)
            try Task.checkCancellation()
            guard historyPageRequestIDs[ref] == requestID, clients[ref.serverID] === client,
                  sessionOpenIDs[ref] == generation, var current = openSessions[ref],
                  current.transcriptWindow.olderCursor == cursor,
                  current.transcriptWindow.anchorEndPosition == anchorEnd,
                  current.transcriptWindow.historyGeneration == initial.transcriptWindow.historyGeneration else {
                throw CancellationError()
            }
            let olderProjection = try page.project(ref: ref, current: current)
            let merged = Self.mergeOlderHistory(older: olderProjection.transcript, current: current.transcript)
            current.transcript = Self.linkHistoricalToolRows(merged)
            let newRanges = (page.history.omittedRanges ?? []).map { TranscriptOmittedRange(startPosition: $0.startPosition, endPosition: $0.endPosition, reason: $0.reason) }
            let rangesByPosition = Dictionary((current.transcriptWindow.omittedRanges + newRanges).map { ($0.startPosition, $0) }, uniquingKeysWith: { _, newer in newer })
            current.transcriptWindow = TranscriptWindow(isComplete: true, omittedItemCount: max(0, page.history.startPosition), olderCursor: page.history.nextCursor,
                                                        historyGeneration: initial.transcriptWindow.historyGeneration, anchorEndPosition: anchorEnd,
                                                        anchorCursor: anchorCursor, startPosition: page.history.startPosition, totalItemCount: totalItems,
                                                        containsTruncatedItems: !rangesByPosition.isEmpty, omittedRanges: rangesByPosition.values.sorted { $0.startPosition < $1.startPosition })
            Self.enforceHistoryTranscriptBounds(&current)
            openSessions[ref] = current
        } catch {
            if historyPageRequestIDs[ref] == requestID, let apiError = error as? APIError,
               apiError.statusCode == 409, apiError.code == "history_cursor_stale" {
                restartTailObservationAfterHistoryStale(ref, client: client, sessionGeneration: generation,
                    requestID: requestID)
            } else if historyPageRequestIDs[ref] == requestID, !(error is CancellationError) {
                historyPageErrors[ref] = error.localizedDescription
            }
            throw error
        }
    }

    private func restartTailObservationAfterHistoryStale(_ ref: SessionRef, client: APIClient,
                                                         sessionGeneration: UUID, requestID: UUID) {
        guard historyPageRequestIDs[ref] == requestID, clients[ref.serverID] === client,
              sessionOpenIDs[ref] == sessionGeneration, openSessions[ref] != nil else { return }
        historyPageRequestIDs[ref] = nil
        historyPageLoading.remove(ref)
        historyRebaselinePending.insert(ref)
        historyPageErrors[ref] = "History changed on the server. Reconnecting to a fresh bounded history snapshot."
        streamGenerations[ref] = nil
        streamTasks[ref]?.cancel()
        streamTasks[ref] = nil
        if var current = openSessions[ref] {
            current.connection = .reconnecting
            openSessions[ref] = current
        }
        startObserving(ref, client: client)
    }

    static func mergeOlderHistory(older: [TranscriptItem], current: [TranscriptItem]) -> [TranscriptItem] {
        func sourceKey(_ item: TranscriptItem) -> String? {
            guard let position = item.sourcePosition, let part = item.sourcePartIndex else { return nil }
            return "\(item.sourceHistoryGeneration ?? "canonical"):\(position):\(part)"
        }
        var merged: [TranscriptItem] = []
        let currentIDs = Set(current.map(\.id))
        var indexes: [String: Int] = [:]
        var turns: [String: Int] = [:]
        var toolCalls: [String: Int] = [:]
        for item in older + current {
            let key = sourceKey(item)
            let toolKey = item.kind == .tool && key == nil ? item.toolCallID.map {
                "\(item.role ?? "")|\(item.owner ?? "")|\(item.turnID ?? "")|\($0)"
            } : nil
            let turnKey = item.sourcePosition == nil || item.sourcePartIndex == nil
                ? (item.role == "user" && item.turnID?.isEmpty == false ? "\(item.owner ?? "")|\(item.turnID!)" : nil)
                : nil
            if let existing = key.flatMap({ indexes[$0] }) {
                if currentIDs.contains(item.id) { merged[existing] = item }
                continue
            }
            if let toolKey, let existing = toolCalls[toolKey] {
                if currentIDs.contains(item.id) { merged[existing] = item }
                continue
            }
            if let turnKey, let existing = turns[turnKey] {
                if currentIDs.contains(item.id) { merged[existing] = item }
                continue
            }
            if let key { indexes[key] = merged.count }
            if let toolKey { toolCalls[toolKey] = merged.count }
            if let turnKey { turns[turnKey] = merged.count }
            merged.append(item)
        }
        return merged.enumerated().sorted { lhs, rhs in
            switch (lhs.element.sourcePosition, rhs.element.sourcePosition) {
            case let (left?, right?) where left != right: return left < right
            case (_?, nil): return true
            case (nil, _?): return false
            default: return lhs.offset < rhs.offset
            }
        }.map(\.element)
    }

    static func linkHistoricalToolRows(_ items: [TranscriptItem]) -> [TranscriptItem] {
        struct CallKey: Hashable { let owner: String?; let callID: String }
        var calls: [CallKey: [TranscriptItem]] = [:]
        for item in items where item.kind == .tool && item.role == "assistant" {
            guard let id = item.toolCallID, !id.isEmpty else { continue }
            calls[CallKey(owner: item.owner, callID: id), default: []].append(item)
        }
        return items.map { item in
            guard item.kind == .tool, item.role == "tool", let callID = item.toolCallID, !callID.isEmpty,
                  let candidates = calls[CallKey(owner: item.owner, callID: callID)], candidates.count == 1,
                  let call = candidates.first else { return item }
            return TranscriptItem(id: item.id, kind: .tool, owner: item.owner ?? call.owner, role: item.role,
                                  text: call.toolName ?? call.text, isStreaming: item.isStreaming, origin: item.origin,
                                  toolName: call.toolName, toolArguments: call.toolArguments, toolOutput: item.toolOutput,
                                  toolStatus: item.toolStatus, toolCallID: item.toolCallID, turnID: item.turnID,
                                  userInputState: item.userInputState, sourcePosition: item.sourcePosition,
                                  sourcePartIndex: item.sourcePartIndex, sourceHistoryGeneration: item.sourceHistoryGeneration,
                                  child: item.child, attachments: item.attachments, generatedMedia: item.generatedMedia,
                                  userMessageIndex: nil, branchAnchor: nil)
        }
    }

    private static func enforceHistoryTranscriptBounds(_ projection: inout OpenSessionProjection) {
        func estimate(_ item: TranscriptItem) -> Int {
            item.id.utf8.count + item.text.utf8.count + (item.owner?.utf8.count ?? 0) + (item.role?.utf8.count ?? 0) +
            (item.origin?.utf8.count ?? 0) + (item.toolName?.utf8.count ?? 0) + (item.toolArguments?.utf8.count ?? 0) +
            (item.toolOutput?.utf8.count ?? 0) + (item.toolStatus?.utf8.count ?? 0) + (item.turnID?.utf8.count ?? 0) +
            item.attachments.reduce(0) { $0 + $1.id.utf8.count + $1.name.utf8.count + $1.mimeType.utf8.count + 64 } +
            item.generatedMedia.reduce(0) { $0 + $1.id.utf8.count + $1.name.utf8.count + $1.path.utf8.count + $1.mimeType.utf8.count + 96 } + 256
        }
        var bytes = projection.transcript.reduce(0) { $0 + estimate($1) }
        while projection.transcript.count > SessionEventProjector.maximumTranscriptItems || bytes > 8 * 1024 * 1024 {
            guard !projection.transcript.isEmpty else { break }
            let removed = projection.transcript.removeFirst()
            bytes -= estimate(removed)
            projection.omittedTranscriptItemCount += 1
            if projection.transcriptWindow.historyGeneration != nil { projection.transcriptWindow.isComplete = false }
        }
    }

    @discardableResult public func refreshSession(_ ref: SessionRef, generation: UUID? = nil) async throws -> SessionSnapshotWire {
        guard let client = clients[ref.serverID] else { throw APIError("Server is not connected.") }
        let current = openSessions[ref]
        let snapshot: SessionSnapshotWire
        let rawProof: RawSnapshotProof?
        if historyPagingSupported[ref] == true {
            snapshot = try await client.sessionHistoryTail(ref.sessionID)
            try snapshot.validateTail(expectedSessionID: ref.sessionID)
            rawProof = nil
        } else {
            (snapshot, rawProof) = try await verifiedRawSessionSnapshot(client: client, ref: ref)
        }
        guard clients[ref.serverID] === client, openSessions[ref] != nil,
              generation == nil || streamGenerations[ref] == generation,
              !Task.isCancelled else { throw APIError("Session observation changed while refreshing.") }
        var value = snapshot.project(ref: ref, connection: openSessions[ref]?.connection ?? .live, rawProof: rawProof, branchInstanceID: servers.first(where: { $0.id == ref.serverID })?.identity?.instanceID)
        value.capabilities = openSessions[ref]?.capabilities ?? .init()
        if let old = current {
            value.capabilities = old.capabilities
            if snapshot.projection == "tail" {
                let anchorEnd = value.transcriptWindow.anchorEndPosition ?? 0
                let ids = Set(value.transcript.map(\.id))
                let live = old.transcript.filter { item in item.userInputState == .queued || item.sourcePosition.map { $0 >= anchorEnd } == true || item.sourcePosition == nil }
                value.transcript.append(contentsOf: live.filter { !ids.contains($0.id) })
                if !old.transcriptWindow.hasCompleteCanonicalCoverage,
                   old.epoch == value.epoch,
                   let oldAnchor = old.transcriptWindow.anchorEndPosition,
                   value.transcriptWindow.anchorEndPosition ?? 0 >= oldAnchor {
                    let preserveOld = old.transcript.filter { $0.sourcePosition.map { $0 < (value.transcriptWindow.startPosition ?? oldAnchor) } == true }
                    let combined = Self.mergeOlderHistory(older: preserveOld, current: value.transcript)
                    value.transcript = combined
                    value.transcriptWindow = old.transcriptWindow
                }
                Self.enforceHistoryTranscriptBounds(&value)
            } else {
                value.transcript = stableTranscript(old.transcript, value.transcript)
            }
        }
        openSessions[ref] = value
        historyPageErrors[ref] = nil
        return snapshot
    }

    func consumeStreamSnapshot(_ snapshot: SessionSnapshotWire, rawProof: RawSnapshotProof? = nil, ref: SessionRef, cursor: inout SessionEventCursor?, projector: inout SessionEventProjector?, publishProjection: Bool = true) throws {
        if snapshot.projection == "tail" { try snapshot.validateTail(expectedSessionID: ref.sessionID) }
        var currentCursor = cursor ?? SessionEventCursor(expectedSessionID: ref.sessionID)
        guard case .snapshot = currentCursor.accept(snapshot: snapshot) else { throw StreamResnapshotRequired() }
        if let oldEpoch = openSessions[ref]?.epoch, let newEpoch = snapshot.epoch, !oldEpoch.isEmpty, oldEpoch != newEpoch {
            historyPageRequestIDs[ref] = nil
            historyPageLoading.remove(ref)
            historyPageErrors[ref] = nil
            markAcceptedSkillOperationsUnknown(ref, reason: "The server restarted before this operation reached a terminal event; completion is unknown.")
        }
        var current = projector ?? SessionEventProjector(projection: snapshot.project(ref: ref, connection: openSessions[ref]?.connection ?? .live))
        current.replace(with: snapshot, rawProof: rawProof, branchInstanceID: servers.first(where: { $0.id == ref.serverID })?.identity?.instanceID)
        historyPageRequestIDs[ref] = nil
        historyPageLoading.remove(ref)
        historyPageErrors[ref] = nil
        if historyRebaselinePending.remove(ref) != nil {
            historyPageErrors[ref] = "History changed on the server. The latest bounded history is ready; load older again."
        }
        cursor = currentCursor
        projector = current
        if publishProjection { openSessions[ref] = current.projection }
    }

    func consumeStreamEvent(_ type: String, envelope: StreamEnvelope?, ref: SessionRef, cursor: inout SessionEventCursor?, projector: inout SessionEventProjector?, publishProjection: Bool = true) throws {
        guard var currentCursor = cursor else { throw APIError("Session event cursor is unavailable.") }
        switch currentCursor.accept(type: type, envelope: envelope) {
        case .ready:
            cursor = currentCursor
            return
        case .snapshot, .duplicate, .ignored: return
        case .resnapshotRequired: throw StreamResnapshotRequired()
        case .seed(let envelope):
            applySkillOperation(envelope, to: ref)
            guard projector != nil else { throw APIError("Session event projector is unavailable.") }
            projector!.apply(envelope)
            cursor = currentCursor
            if publishProjection, let current = projector { openSessions[ref] = current.projection }
        case .apply(let envelope):
            applySkillOperation(envelope, to: ref)
            guard projector != nil else { throw APIError("Session event projector is unavailable.") }
            projector!.apply(envelope)
            projector!.updateCursor(sequence: currentCursor.sequence, epoch: currentCursor.epoch ?? projector!.projection.epoch)
            if envelope.event?["type"]?.stringValue == "session_title",
               envelope.event?["session_id"]?.stringValue == ref.sessionID,
               let title = envelope.event?["title"]?.stringValue, !title.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty,
               let index = sessions.firstIndex(where: { $0.ref == ref }) {
                sessions[index].title = title
            }
            cursor = currentCursor
            if publishProjection, let current = projector { openSessions[ref] = current.projection }
        }
    }

    private func follow(_ ref: SessionRef, client: APIClient, generation: UUID) async {
        var failures = 0
        var cursor: SessionEventCursor?
        var projector: SessionEventProjector?
        let publication = ProjectionPublicationThrottle { [weak self] projection in
            guard let self, self.observationIsCurrent(ref, client: client, generation: generation) else { return }
            self.openSessions[ref] = projection
        }

        func publish(_ projection: OpenSessionProjection, immediately: Bool = false) {
            if immediately { publication.publishImmediately(projection) }
            else { publication.schedule(projection) }
        }

        defer { publication.cancel() }
        while observationIsCurrent(ref, client: client, generation: generation) {
            guard var projection = openSessions[ref] else { finishObservation(ref, generation: generation); return }
            if cursor == nil {
                cursor = SessionEventCursor(expectedSessionID: ref.sessionID, epoch: projection.epoch.isEmpty ? nil : projection.epoch, sequence: projection.cursor, isReady: true)
                projector = SessionEventProjector(projection: projection)
            }
            projection.connection = failures == 0 ? .live : .reconnecting
            projection.refreshedAt = .now
            openSessions[ref] = projection
            do {
                let tailStream = historyPagingSupported[ref] == true
                let (bytes, _) = try await client.eventStream(sessionID: ref.sessionID,
                                                              since: tailStream ? nil : (cursor?.epoch == nil ? nil : cursor?.sequence),
                                                              epoch: tailStream ? nil : cursor?.epoch,
                                                              tailProjection: tailStream)
                guard observationIsCurrent(ref, client: client, generation: generation) else { finishObservation(ref, generation: generation); return }
                var parser = SSEParser()
                var assembler = SnapshotAssembler(maximumSnapshotBytes: tailStream ? 2 * 1024 * 1024 : RawSnapshotProof.maximumSnapshotBytes)
                var networkChunk: [UInt8] = []
                networkChunk.reserveCapacity(16 * 1024)
                for try await byte in bytes {
                    guard observationIsCurrent(ref, client: client, generation: generation) else { finishObservation(ref, generation: generation); return }
                    networkChunk.append(byte)
                    guard byte == 0x0A || networkChunk.count >= networkChunk.capacity else { continue }
                    let frames = try parser.append(networkChunk)
                    networkChunk.removeAll(keepingCapacity: true)
                    for frame in frames {
                        guard let data = frame.data.data(using: .utf8) else { throw APIError("Invalid session event frame.") }
                        if let payload = try assembler.acceptRawJSON(data) {
                            let proof = payload.snapshot.projection == "tail" ? nil : try? RawSnapshotProof.extract(from: payload.rawJSON, expectedSessionID: ref.sessionID)
                            try consumeStreamSnapshot(payload.snapshot, rawProof: proof, ref: ref, cursor: &cursor, projector: &projector, publishProjection: false)
                            guard let current = projector?.projection else { throw APIError("Session snapshot projection is unavailable.") }
                            projection = current
                            publish(current, immediately: true)
                            continue
                        }
                        guard let message = try? JSONDecoder().decode(StreamMessage.self, from: data) else { throw APIError("Invalid session event frame.") }
                        try consumeStreamEvent(message.type, envelope: message.envelope, ref: ref, cursor: &cursor, projector: &projector, publishProjection: false)
                        guard let current = projector?.projection else { continue }
                        projection = current
                        let type = message.envelope?.event?["type"]?.stringValue ?? ""
                        let urgent = ["user_message", "pending_user_message_accepted", "pending_user_message_edited", "pending_user_message_promoted", "pending_user_message_canceled", "turn_settled", "stream_stopped", "stream_started", "error", "interaction_resolved", "tool_call_confirmation", "elicitation_request", "max_iterations_reached"].contains(type)
                        publish(current, immediately: urgent)
                    }
                }
                if !networkChunk.isEmpty {
                    for frame in try parser.append(networkChunk) {
                        guard let data = frame.data.data(using: .utf8) else { throw APIError("Invalid session event frame.") }
                        if let payload = try assembler.acceptRawJSON(data) {
                            let proof = payload.snapshot.projection == "tail" ? nil : try? RawSnapshotProof.extract(from: payload.rawJSON, expectedSessionID: ref.sessionID)
                            try consumeStreamSnapshot(payload.snapshot, rawProof: proof, ref: ref, cursor: &cursor, projector: &projector, publishProjection: false)
                            if let current = projector?.projection { publish(current, immediately: true) }
                            continue
                        }
                        guard let message = try? JSONDecoder().decode(StreamMessage.self, from: data) else { throw APIError("Invalid session event frame.") }
                        try consumeStreamEvent(message.type, envelope: message.envelope, ref: ref, cursor: &cursor, projector: &projector, publishProjection: false)
                        if let current = projector?.projection {
                            let type = message.envelope?.event?["type"]?.stringValue ?? ""
                            let urgent = ["user_message", "pending_user_message_accepted", "pending_user_message_edited", "pending_user_message_promoted", "pending_user_message_canceled", "turn_settled", "stream_stopped", "stream_started", "error", "interaction_resolved", "tool_call_confirmation", "elicitation_request", "max_iterations_reached"].contains(type)
                            publish(current, immediately: urgent)
                        }
                    }
                }
                try assembler.finish()
                let endFrames = try parser.finish()
                if !endFrames.isEmpty { throw APIError("Incomplete event stream frame.") }
                throw APIError("Session event stream ended.")
            } catch is CancellationError {
                publication.flush()
                finishObservation(ref, generation: generation)
                return
            } catch let error as APIError where error.statusCode == 401 || error.statusCode == 403 {
                if observationIsCurrent(ref, client: client, generation: generation) {
                    publication.flush()
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
            } catch is StreamResnapshotRequired {
                guard observationIsCurrent(ref, client: client, generation: generation) else { finishObservation(ref, generation: generation); return }
                publication.flush()
                markAcceptedSkillOperationsUnknown(ref, reason: "Event replay lost its previous epoch or cursor; completion is unknown.")
                failures = 0
                cursor = SessionEventCursor(expectedSessionID: ref.sessionID)
                projector = nil
                if var current = openSessions[ref] { current.cursor = 0; current.epoch = ""; current.connection = .reconnecting; openSessions[ref] = current }
                continue
            } catch {
                guard observationIsCurrent(ref, client: client, generation: generation) else { finishObservation(ref, generation: generation); return }
                publication.flush()
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
                        publish(current.projection, immediately: true)
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
        guard clients[serverID] === client else { return }
        needsYou = current
    }

    private func stableTranscript(_ old: [TranscriptItem], _ new: [TranscriptItem]) -> [TranscriptItem] {
        func sameUserTurn(_ lhs: TranscriptItem, _ rhs: TranscriptItem) -> Bool {
            guard lhs.role == "user", rhs.role == "user",
                  let lhsTurnID = lhs.turnID, !lhsTurnID.isEmpty,
                  lhsTurnID == rhs.turnID else { return false }
            let sameOrigin = lhs.origin == nil || rhs.origin == nil || lhs.origin == rhs.origin
            let sameOwner = lhs.owner == nil || rhs.owner == nil || lhs.owner == rhs.owner
            return sameOrigin && sameOwner
        }

        let oldByFingerprint = Dictionary(grouping: old, by: { "\($0.kind.rawValue)|\($0.owner ?? "")|\($0.role ?? "")|\($0.text)" })
        return new.map { item in
            if let candidate = old.first(where: { sameUserTurn($0, item) }) {
                return TranscriptItem(id: candidate.id, kind: item.kind, owner: item.owner ?? candidate.owner, role: item.role, text: item.text, isStreaming: item.isStreaming, origin: item.origin ?? candidate.origin, toolName: item.toolName, toolArguments: item.toolArguments, toolOutput: item.toolOutput, toolStatus: item.toolStatus, toolCallID: item.toolCallID, turnID: item.turnID ?? candidate.turnID, userInputState: item.userInputState, sourcePosition: item.sourcePosition ?? candidate.sourcePosition, sourcePartIndex: item.sourcePartIndex ?? candidate.sourcePartIndex, sourceHistoryGeneration: item.sourceHistoryGeneration ?? candidate.sourceHistoryGeneration, child: item.child, attachments: item.attachments, generatedMedia: item.generatedMedia, userMessageIndex: item.userMessageIndex ?? candidate.userMessageIndex, branchAnchor: item.branchAnchor ?? candidate.branchAnchor)
            }
            let key = "\(item.kind.rawValue)|\(item.owner ?? "")|\(item.role ?? "")|\(item.text)"
            guard let candidate = oldByFingerprint[key]?.first(where: { $0.id.hasPrefix("snapshot:") }) else { return item }
            return TranscriptItem(id: candidate.id, kind: item.kind, owner: item.owner ?? candidate.owner, role: item.role, text: item.text, isStreaming: item.isStreaming, origin: item.origin ?? candidate.origin, toolName: item.toolName, toolArguments: item.toolArguments, toolOutput: item.toolOutput, toolStatus: item.toolStatus, toolCallID: item.toolCallID, turnID: item.turnID ?? candidate.turnID, userInputState: item.userInputState, sourcePosition: item.sourcePosition ?? candidate.sourcePosition, sourcePartIndex: item.sourcePartIndex ?? candidate.sourcePartIndex, sourceHistoryGeneration: item.sourceHistoryGeneration ?? candidate.sourceHistoryGeneration, child: item.child, attachments: item.attachments, generatedMedia: item.generatedMedia, userMessageIndex: item.userMessageIndex ?? candidate.userMessageIndex, branchAnchor: item.branchAnchor ?? candidate.branchAnchor)
        }
    }

    func loadSavedCredentials() async {
        for config in configStore.load() {
            do {
                let token = try await tokenStore.read(for: config.id) ?? ""
                let client = try APIClient(config: config, token: token, session: apiSession)
                let identity: ServerIdentity = try await client.request(ServerIdentity.self, path: "api/v2/server")
                guard identity.version == 1, identity.sessionAPIVersion == 2 else {
                    throw APIError("Server API version is unsupported.")
                }
                guard config.instanceID == nil || config.instanceID == identity.instanceID else {
                    markServerIdentityChanged(config.id)
                    throw APIError(ServerConnectionIssue.identityChanged.message)
                }
                tokens[config.id] = token
                clients[config.id] = client
                if let index = servers.firstIndex(where: { $0.id == config.id }) {
                    servers[index].identity = identity; servers[index].state = .live; servers[index].connectionIssue = nil; servers[index].refreshedAt = .now
                }

            } catch { markServer(config.id, state: .offline) }
        }
        await refreshForeground()
    }

    private func markServer(_ id: UUID, state: ConnectionState) {
        guard let i = servers.firstIndex(where: { $0.id == id }) else { return }; servers[i].state = state; servers[i].refreshedAt = .now
    }

    private func markServerIdentityChanged(_ id: UUID) {
        guard let i = servers.firstIndex(where: { $0.id == id }) else { return }
        servers[i].state = .offline
        servers[i].connectionIssue = .identityChanged
        servers[i].refreshedAt = .now
    }

    private func clearServerConnectionIssue(_ id: UUID) {
        guard let i = servers.firstIndex(where: { $0.id == id }) else { return }
        servers[i].connectionIssue = nil
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
