import SwiftUI
import AgentKit

enum SidebarItem: Hashable {
    case all
    case needsYou
    case running
    case server(UUID)
}

/// Compact (iPhone) routing rules, kept pure so they can be tested without a window.
enum CompactRouting {
    /// Remembered sessions are restored only where the detail sits beside the list; on iPhone a restored
    /// selection would be invisible and could not be re-opened by tapping its already selected row.
    static func restoresRememberedSession(isCompact: Bool) -> Bool { !isCompact }

    /// The compact stack after a row tap: one canonical destination, so Back always returns to the workbench.
    static func path(afterTap ref: SessionRef) -> [SessionRef] { [ref] }

    /// The compact stack after an in-app open (child, parent, branch): pushed so Back returns to where you were;
    /// re-opening the top of the stack changes nothing.
    static func path(afterOpen ref: SessionRef, current: [SessionRef]) -> [SessionRef] {
        current.last == ref ? current : current + [ref]
    }

    /// Selection mirrors the top of the compact stack; an empty stack means nothing is selected, so the same
    /// row opens again with one tap.
    static func selection(forPath path: [SessionRef]) -> SessionRef? { path.last }

    /// A row tap opens its session unless the list is in edit mode or a multi-selection is in progress.
    static func rowTapOpens(isEditing: Bool, selectedCount: Int) -> Bool { !isEditing && selectedCount <= 1 }
}

public struct RootView: View {
    @Environment(AppModel.self) private var model
    @Environment(\.scenePhase) private var scenePhase
    @SceneStorage("root.activeServerID") private var storedServerID = ""
    /// Last server the user chose explicitly; outlives scene state (e.g. a reinstall) so the app
    /// reopens on that server instead of whichever profile happens to sort first.
    @AppStorage("root.preferredServerID") private var preferredServerID = ""
    @SceneStorage("root.sessionsByServer") private var storedSessions = "{}"
    @State private var sidebarSelection: SidebarItem = .all
    @State private var sessionSelection: SessionRef?
    @State private var editor: ServerEditorTarget?
    @State private var showServerSettings = false
    @State private var sceneID = UUID()
    @State private var handoff = ComposerDraftHandoff()
    @State private var restoringServers = Set<UUID>()
    /// iPhone navigates with one explicit stack; the split view's compact collapse re-hosted the detail mid-push,
    /// which fired onDisappear on a visible view and cancelled its open.
    @State private var compactPath: [SessionRef] = []
    @Environment(\.horizontalSizeClass) private var sizeClass

    private var isCompact: Bool { sizeClass == .compact }

    private var activeServerID: Binding<UUID?> {
        Binding(get: { UUID(uuidString: storedServerID) }, set: {
            storedServerID = $0?.uuidString ?? ""
            if let id = $0 { preferredServerID = id.uuidString }
        })
    }

    private var fallbackServerID: UUID? {
        if let preferred = UUID(uuidString: preferredServerID), model.servers.contains(where: { $0.id == preferred }) { return preferred }
        return model.servers.first?.id
    }

    private var sessionsByServer: [String: String] {
        (try? JSONDecoder().decode([String: String].self, from: Data(storedSessions.utf8))) ?? [:]
    }

    public init() {}

    public var body: some View {
        Group {
            if isCompact {
                NavigationStack(path: $compactPath) {
                    sessionList
                        .navigationDestination(for: SessionRef.self) { ref in
                            SessionView(ref: ref)
                        }
                }
            } else {
                NavigationSplitView {
                    sessionList
                        .navigationSplitViewColumnWidth(min: 300, ideal: 370)
                } detail: {
                    if let sessionSelection {
                        SessionView(ref: sessionSelection).id(sessionSelection)
                    } else {
                        ContentUnavailableView("No Session Selected", systemImage: "bubble.left.and.text.bubble.right", description: Text("Start a session or choose one from this server."))
                    }
                }
            }
        }
        .tint(Palette.harbor)
        .environment(\.openSession, OpenSessionAction { ref, prefill in
            if let prefill { handoff.put(prefill, for: ref) }
            activeServerID.wrappedValue = ref.serverID
            sidebarSelection = .all
            if isCompact {
                compactPath = CompactRouting.path(afterOpen: ref, current: compactPath)
            } else {
                sessionSelection = ref
            }
            remember(ref)
        })
        .environment(\.composerHandoff, handoff)
        .environment(\.sessionDeleted, CloseSessionAction { ref in
            if sessionSelection == ref { sessionSelection = nil }
            compactPath.removeAll { $0 == ref }
            forget(ref)
        })
        .sheet(item: $editor) { target in
            ServerEditorSheet(target: target) { id in
                activeServerID.wrappedValue = id
                Task { await model.refreshCatalog(id) }
            }
        }
        .sheet(isPresented: $showServerSettings) {
            ServerSettingsView(onServerAdded: { id in
                activeServerID.wrappedValue = id
                Task { await model.refreshCatalog(id) }
            })
        }
        .onAppear {
            if activeServerID.wrappedValue == nil { storedServerID = fallbackServerID?.uuidString ?? "" }
            if let id = activeServerID.wrappedValue { restoreRememberedSession(serverID: id) }
        }
        .onChange(of: model.servers) { _, servers in
            var remembered = sessionsByServer
            for key in Array(remembered.keys) where !servers.contains(where: { $0.id.uuidString == key }) { remembered[key] = nil }
            persistSessions(remembered)
            guard !servers.isEmpty else { storedServerID = ""; sessionSelection = nil; return }
            if let active = activeServerID.wrappedValue, servers.contains(where: { $0.id == active }) {
                restoreRememberedSession(serverID: active)
                return
            }
            storedServerID = fallbackServerID?.uuidString ?? ""
        }
        .onChange(of: storedServerID) { _, _ in
            sidebarSelection = .all
            if sessionSelection?.serverID != activeServerID.wrappedValue { sessionSelection = nil }
            if let id = activeServerID.wrappedValue { restoreRememberedSession(serverID: id) }
        }
        .onChange(of: model.sessions) { _, _ in
            if let id = activeServerID.wrappedValue { restoreRememberedSession(serverID: id) }
        }
        .onChange(of: model.catalogLoadedServers) { _, loaded in
            if let id = activeServerID.wrappedValue, loaded.contains(id) { restoreRememberedSession(serverID: id) }
        }
        .onChange(of: sessionSelection) { _, ref in
            if let ref { remember(ref) }
        }
        .onChange(of: compactPath) { _, path in
            // The list's selection mirrors the stack: Back empties it so the same row opens again with one tap.
            let next = CompactRouting.selection(forPath: path)
            if next != sessionSelection { sessionSelection = next }
        }
        .onChange(of: sizeClass) { _, _ in
            // iPad multitasking can switch layouts mid-flight: carry the current session across.
            if isCompact {
                compactPath = sessionSelection.map { [$0] } ?? []
            } else if let top = compactPath.last {
                sessionSelection = top
            }
        }
        .onAppear { model.setSceneActive(scenePhase != .background, for: sceneID) }
        .onChange(of: scenePhase) { _, phase in
            // Inactive (e.g. switching apps on Mac) still counts as foreground.
            model.setSceneActive(phase != .background, for: sceneID)
            guard phase == .active else { return }
            Task { await model.refreshForeground() }
        }
        .onDisappear { model.setSceneActive(false, for: sceneID) }
    }

    private var sessionList: some View {
        SessionListView(filter: $sidebarSelection, serverID: activeServerID, selection: $sessionSelection,
                        onAddServer: { editor = .add }, onManageServers: { showServerSettings = true },
                        onEditServer: { id in editor = .edit(id) },
                        onOpen: { ref in
                            if isCompact {
                                compactPath = CompactRouting.path(afterTap: ref)
                            } else {
                                sessionSelection = ref
                            }
                        })
    }

    private func restoreRememberedSession(serverID: UUID) {
        guard CompactRouting.restoresRememberedSession(isCompact: isCompact) else { return }
        guard sessionSelection == nil, let sessionID = sessionsByServer[serverID.uuidString] else { return }
        let ref = SessionRef(serverID: serverID, sessionID: sessionID)
        if model.sessions.contains(where: { $0.ref == ref }) {
            sessionSelection = ref
            return
        }
        guard model.catalogLoadedServers.contains(serverID) else { return }
        if model.catalogHasMore.contains(serverID), restoringServers.insert(serverID).inserted {
            let previousCursor = model.nextCatalogCursor[serverID]
            Task {
                await model.loadMoreSessions(serverID: serverID)
                restoringServers.remove(serverID)
                guard activeServerID.wrappedValue == serverID,
                      model.servers.first(where: { $0.id == serverID })?.state == .live else { return }
                if model.sessions.contains(where: { $0.ref == ref }) { restoreRememberedSession(serverID: serverID); return }
                guard model.catalogHasMore.contains(serverID), model.nextCatalogCursor[serverID] != previousCursor else { return }
                restoreRememberedSession(serverID: serverID)
            }
        } else if !model.catalogHasMore.contains(serverID), model.servers.first(where: { $0.id == serverID })?.state == .live {
            forget(ref)
        }
    }

    private func remember(_ ref: SessionRef) {
        var values = sessionsByServer
        values[ref.serverID.uuidString] = ref.sessionID
        persistSessions(values)
    }

    private func forget(_ ref: SessionRef) {
        var values = sessionsByServer
        guard values[ref.serverID.uuidString] == ref.sessionID else { return }
        values[ref.serverID.uuidString] = nil
        persistSessions(values)
    }

    private func persistSessions(_ values: [String: String]) {
        guard let data = try? JSONEncoder().encode(values) else { return }
        storedSessions = String(decoding: data, as: UTF8.self)
    }
}

/// Root of a standalone session window (`WindowGroup(for: SessionRef.self)`).
public struct SessionWindowView: View {
    let ref: SessionRef
    @State private var current: SessionRef
    @State private var sceneID = UUID()
    @State private var handoff = ComposerDraftHandoff()
    @Environment(AppModel.self) private var model
    @Environment(\.scenePhase) private var scenePhase
    @Environment(\.dismiss) private var dismiss

    public init(ref: SessionRef) {
        self.ref = ref
        _current = State(initialValue: ref)
    }

    public var body: some View {
        NavigationStack {
            SessionView(ref: current)
                .id(current)
        }
        .tint(Palette.harbor)
        .environment(\.openSession, OpenSessionAction { ref, prefill in
            if let prefill { handoff.put(prefill, for: ref) }
            current = ref
        })
        .environment(\.composerHandoff, handoff)
        .environment(\.sessionDeleted, CloseSessionAction { ref in
            if ref == current { dismiss() }
        })
        .onChange(of: scenePhase, initial: true) { _, phase in
            model.setSceneActive(phase != .background, for: sceneID)
        }
        .onDisappear { model.setSceneActive(false, for: sceneID) }
    }
}
