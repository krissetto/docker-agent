import SwiftUI
import AgentKit

struct SessionListView: View {
    @Environment(AppModel.self) private var model
    let filter: SidebarItem
    @Binding var selection: SessionRef?
    @State private var query = ""
    @State private var starredOnly = false
    @State private var creatingOn: NewSessionTarget?
    @State private var multiSelection: Set<SessionRef> = []
    @State private var confirmingBatchDelete = false
    @State private var batchExport: SessionExportDocument?
    @State private var batchMessage: String?
    @State private var batchWorking = false
    #if os(iOS)
    @Environment(\.editMode) private var editMode
    #endif

    private struct Group: Identifiable {
        let id: String
        let title: String
        let sessions: [SessionProjection]
    }

    private var title: String {
        switch filter {
        case .needsYou: "Needs You"
        case .running: "Running"
        case .server(let id): model.servers.first { $0.id == id }?.name ?? "Server"
        }
    }

    private var serverID: UUID? {
        if case .server(let id) = filter { id } else { nil }
    }

    private var scoped: [SessionProjection] {
        let base: [SessionProjection]
        switch filter {
        case .needsYou:
            let refs = Set(model.needsYou.map(\.id))
            base = model.sessions.filter { refs.contains($0.ref) || $0.needsYouReason != nil }
        case .running:
            base = model.sessions.filter { $0.state.isActive }
        case .server(let id):
            base = model.sessions.filter { $0.ref.serverID == id && $0.parentID == nil }
        }
        return base.filter { session in
            (!starredOnly || session.starred)
                && (query.isEmpty || session.title.localizedCaseInsensitiveContains(query) || session.agentName.localizedCaseInsensitiveContains(query))
        }
    }

    private var groups: [Group] {
        let sorted = scoped.sorted { ($0.updatedAt ?? .distantPast) > ($1.updatedAt ?? .distantPast) }
        let waiting = sorted.filter { $0.needsYouReason != nil || $0.state == .waiting || $0.state == .failed }
        let running = sorted.filter { $0.state == .running && $0.needsYouReason == nil }
        let rest = sorted.filter { !$0.state.isActive && $0.state != .failed && $0.needsYouReason == nil }
        return [
            Group(id: "waiting", title: "Waiting", sessions: waiting),
            Group(id: "running", title: "Running", sessions: running),
            Group(id: "recent", title: "Recent", sessions: rest),
        ].filter { !$0.sessions.isEmpty }
    }

    var body: some View {
        List(selection: $multiSelection) {
            ForEach(groups) { group in
                Section(group.title) {
                    ForEach(group.sessions) { session in
                        row(session)
                    }
                }
            }
            if let serverID, model.catalogHasMore.contains(serverID) {
                Button("Load More") { Task { await model.loadMoreSessions(serverID: serverID) } }
                    .frame(maxWidth: .infinity)
                    .buttonStyle(.borderless)
            }
        }
        .navigationTitle(title)
        .searchable(text: $query, prompt: "Search loaded sessions")
        .refreshable {
            if let serverID { await model.refreshCatalog(serverID) } else { await model.refreshForeground() }
        }
        .toolbar {
            ToolbarItemGroup {
                Toggle(isOn: $starredOnly) {
                    Label("Starred Only", systemImage: starredOnly ? "star.fill" : "star")
                }
                .help("Show starred sessions only")
                Button {
                    creatingOn = NewSessionTarget(serverID: serverID ?? model.servers.first?.id)
                } label: {
                    Label("New Session", systemImage: "square.and.pencil")
                }
                .keyboardShortcut("n", modifiers: .command)
                .disabled(model.servers.isEmpty)
            }
        }
        .overlay {
            if groups.isEmpty {
                emptyState
            }
        }
        .sheet(item: $creatingOn) { target in
            NewSessionSheet(initialServerID: target.serverID) { ref in
                selection = ref
            }
        }
        .onChange(of: multiSelection) { _, refs in
            if refs.count == 1, let ref = refs.first, ref != selection { selection = ref }
        }
        .onChange(of: selection, initial: true) { _, ref in
            if let ref, !multiSelection.contains(ref) || multiSelection.count == 1 { multiSelection = [ref] }
            if ref == nil && multiSelection.count == 1 { multiSelection = [] }
        }
        .safeAreaInset(edge: .bottom, spacing: 0) {
            if showsBatchBar { batchBar }
        }
        .confirmationDialog(batchDeleteTitle, isPresented: $confirmingBatchDelete, titleVisibility: .visible) {
            Button("Delete \(batchRefs.count) Sessions", role: .destructive) { Task { await deleteBatch() } }
        } message: {
            Text(batchTitles + "\nThe sessions and their history are deleted from the server. This can't be undone.")
        }
        .fileExporter(
            isPresented: Binding(get: { batchExport != nil }, set: { if !$0 { batchExport = nil } }),
            document: batchExport,
            contentType: .json,
            defaultFilename: "sessions-\(Date.now.formatted(.iso8601.year().month().day())).json"
        ) { result in
            if case .failure(let error) = result { batchMessage = error.localizedDescription }
            batchExport = nil
        }
        .alert("Sessions", isPresented: Binding(get: { batchMessage != nil }, set: { if !$0 { batchMessage = nil } })) {
            Button("OK", role: .cancel) {}
        } message: {
            Text(batchMessage ?? "")
        }
        #if os(iOS)
        .toolbar {
            ToolbarItem(placement: .topBarLeading) { EditButton() }
        }
        #endif
    }

    // MARK: Batch

    private var batchRefs: [SessionRef] {
        model.sessions.map(\.ref).filter { multiSelection.contains($0) }
    }

    private var showsBatchBar: Bool {
        #if os(iOS)
        if editMode?.wrappedValue.isEditing == true { return true }
        #endif
        return multiSelection.count > 1
    }

    private var batchServerID: UUID? {
        let servers = Set(batchRefs.map(\.serverID))
        return servers.count == 1 ? servers.first : nil
    }

    private var batchDeleteTitle: String { "Delete \(batchRefs.count) sessions?" }

    private var batchTitles: String {
        let titles = batchRefs.prefix(5).map { ref in model.sessions.first { $0.ref == ref }?.title.nonEmpty ?? "Untitled session" }
        let more = batchRefs.count > 5 ? "\n…and \(batchRefs.count - 5) more" : ""
        return titles.map { "• \($0)" }.joined(separator: "\n") + more
    }

    private var batchBar: some View {
        HStack(spacing: Metrics.m) {
            Text("\(batchRefs.count) selected").font(.callout.weight(.medium))
            if batchServerID == nil && !batchRefs.isEmpty {
                Text("Select sessions from one server").font(.caption).foregroundStyle(.secondary)
            }
            Spacer()
            if batchWorking { ProgressView().controlSize(.small) }
            Button("Export…") { Task { await exportBatch() } }
                .disabled(batchServerID == nil || batchWorking)
            Button("Delete…", role: .destructive) { confirmingBatchDelete = true }
                .disabled(batchServerID == nil || batchWorking)
        }
        .padding(.horizontal, Metrics.l)
        .padding(.vertical, Metrics.s)
        .background(.bar)
    }

    private func exportBatch() async {
        guard let serverID = batchServerID else { return }
        let ids = batchRefs.map(\.sessionID)
        batchWorking = true
        defer { batchWorking = false }
        do {
            let export = try await model.exportSessionsJSON(serverID: serverID, sessionIDs: ids, confirmedSessionIDs: Set(ids))
            if export.exportedSessionIDs.count < ids.count {
                batchMessage = "The server exported \(export.exportedSessionIDs.count) of \(ids.count) sessions."
            }
            batchExport = SessionExportDocument(data: try export.encodedJSON())
        } catch {
            batchMessage = error.localizedDescription
        }
    }

    private func deleteBatch() async {
        guard let serverID = batchServerID else { return }
        let refs = batchRefs
        let ids = refs.map(\.sessionID)
        let titles = Dictionary(uniqueKeysWithValues: refs.map { ref in (ref.sessionID, model.sessions.first { $0.ref == ref }?.title.nonEmpty ?? ref.sessionID) })
        batchWorking = true
        defer { batchWorking = false }
        do {
            let result = try await model.deleteSessions(serverID: serverID, sessionIDs: ids, confirmedSessionIDs: Set(ids))
            multiSelection.subtract(refs.filter { result.deletedIDs.contains($0.sessionID) })
            if let selected = selection, result.deletedIDs.contains(selected.sessionID), selected.serverID == serverID { selection = nil }
            if !result.failedIDs.isEmpty {
                batchMessage = "Deleted \(result.deletedIDs.count). Couldn't delete:\n" + result.failedIDs.map { "• " + (titles[$0] ?? $0) }.joined(separator: "\n")
            }
        } catch {
            batchMessage = error.localizedDescription
        }
    }

    private func row(_ session: SessionProjection) -> some View {
        let reason = model.needsYou.first { $0.id == session.ref }?.reason ?? session.needsYouReason
        let serverName: String? = serverID == nil ? model.servers.first { $0.id == session.ref.serverID }?.name : nil
        return SessionRow(session: session, reason: reason, serverName: serverName)
            .tag(session.ref)
    }

    @ViewBuilder
    private var emptyState: some View {
        if !query.isEmpty {
            ContentUnavailableView.search(text: query)
        } else {
            switch filter {
            case .needsYou:
                ContentUnavailableView("Nothing Needs You", systemImage: "checkmark.circle", description: Text("Approvals, questions and failures from every server appear here."))
            case .running:
                ContentUnavailableView("Nothing Running", systemImage: "moon.zzz", description: Text("Sessions that are working right now appear here."))
            case .server:
                ContentUnavailableView {
                    Label("No Sessions", systemImage: "bubble.left.and.bubble.right")
                } description: {
                    Text("Start a session with one of this server's agents.")
                } actions: {
                    Button("New Session") { creatingOn = NewSessionTarget(serverID: serverID) }
                        .buttonStyle(.borderedProminent)
                }
            }
        }
    }
}

struct NewSessionTarget: Identifiable {
    let id = UUID()
    let serverID: UUID?
}
