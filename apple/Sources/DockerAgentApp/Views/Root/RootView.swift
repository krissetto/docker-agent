import SwiftUI
import AgentKit

enum SidebarItem: Hashable {
    case needsYou
    case running
    case server(UUID)
}

public struct RootView: View {
    @Environment(AppModel.self) private var model
    @Environment(\.scenePhase) private var scenePhase
    @State private var sidebarSelection: SidebarItem? = .needsYou
    @State private var sessionSelection: SessionRef?
    @State private var editor: ServerEditorTarget?
    @State private var sceneID = UUID()
    @State private var handoff = ComposerDraftHandoff()

    public init() {}

    public var body: some View {
        NavigationSplitView {
            SidebarView(selection: $sidebarSelection, editor: $editor)
                .navigationSplitViewColumnWidth(min: 200, ideal: 240)
        } content: {
            SessionListView(filter: sidebarSelection ?? .needsYou, selection: $sessionSelection)
                .navigationSplitViewColumnWidth(min: 260, ideal: 320)
        } detail: {
            if let sessionSelection {
                SessionView(ref: sessionSelection)
                    .id(sessionSelection)
            } else {
                ContentUnavailableView("No Session Selected", systemImage: "bubble.left.and.text.bubble.right", description: Text("Choose a session to follow its agents, or start a new one."))
            }
        }
        .environment(\.openSession, OpenSessionAction { ref, prefill in
            if let prefill { handoff.put(prefill, for: ref) }
            sidebarSelection = .server(ref.serverID)
            sessionSelection = ref
        })
        .environment(\.composerHandoff, handoff)
        .environment(\.sessionDeleted, CloseSessionAction { ref in
            if sessionSelection == ref { sessionSelection = nil }
        })
        .sheet(item: $editor) { target in
            ServerEditorSheet(target: target)
        }
        .onChange(of: scenePhase, initial: true) { _, phase in
            // Inactive (e.g. switching apps on Mac) still counts as foreground.
            model.setSceneActive(phase != .background, for: sceneID)
            guard phase == .active else { return }
            Task { await model.refreshForeground() }
        }
        .onDisappear { model.setSceneActive(false, for: sceneID) }
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
