import SwiftUI
import AgentKit

struct SessionListView: View {
    @Environment(AppModel.self) private var model
    @Binding var filter: SidebarItem
    @Binding var serverID: UUID?
    @Binding var selection: SessionRef?
    let onAddServer: () -> Void
    let onManageServers: () -> Void
    /// Opens the existing editor for a server whose pinned identity no longer matches; Test is the only way back.
    var onEditServer: (UUID) -> Void = { _ in }
    /// Explicit open from a row tap or menu; the container decides how to show the detail.
    var onOpen: (SessionRef) -> Void = { _ in }
    @State private var query = ""
    @State private var starredOnly = false
    @State private var showingOptions = false
    @State private var creating = false
    @State private var createError: String?
    @State private var createOperationID = UUID()
    @State private var optionsOperationID = UUID()
    @State private var multiSelection: Set<SessionRef> = []
    @State private var confirmingBatchDelete = false
    @State private var batchExport: SessionExportDocument?
    @State private var batchMessage: String?
    @State private var batchWorking = false
    @State private var pendingDelete: SessionProjection?
    @State private var starFeedback = 0
    @Environment(\.accessibilityReduceMotion) private var reduceMotion

    private struct LogGroup: Identifiable {
        let bucket: LogBucket
        let sessions: [SessionProjection]
        var id: Int { bucket.id }
    }

    private var activeServer: ServerProjection? { model.servers.first { $0.id == serverID } }

    private var title: String {
        switch filter {
        case .all: activeServer?.name ?? "Sessions"
        case .needsYou: "Needs you"
        case .running: "Working"
        case .server(let id): model.servers.first { $0.id == id }?.name ?? "Sessions"
        }
    }

    private var scopedServerID: UUID? {
        if case .server(let id) = filter { return id }
        return serverID
    }

    /// Root sessions on the active server; children are grouped under their parent for the board.
    private var onServer: [SessionProjection] {
        model.sessions.filter { $0.ref.serverID == scopedServerID && $0.parentID == nil }
    }

    private var childrenByParent: [String: [SessionProjection]] {
        Dictionary(grouping: model.sessions.filter { $0.ref.serverID == scopedServerID && $0.parentID != nil }, by: { $0.parentID ?? "" })
    }

    /// A person is needed only when the server says so (interaction, pending input, error) or the run failed.
    /// `.waiting` on its own is waiting on an agent, a tool or a pause and stays on the working side.
    private func reason(for session: SessionProjection) -> String? {
        if let reason = model.needsYou.first(where: { $0.id == session.ref })?.reason ?? session.needsYouReason { return reason }
        return session.state == .failed ? "Failed" : nil
    }

    private var matchesSearch: (SessionProjection) -> Bool {
        let query = query
        let starredOnly = starredOnly
        return { session in
            (!starredOnly || session.starred)
                && (query.isEmpty || session.title.localizedCaseInsensitiveContains(query) || session.agentName.localizedCaseInsensitiveContains(query))
        }
    }

    private var sortedRoots: [SessionProjection] {
        onServer.filter(matchesSearch).sorted { ($0.updatedAt ?? .distantPast) > ($1.updatedAt ?? .distantPast) }
    }

    /// Live work: sessions a person must answer, then sessions that are working. Disjoint from the log.
    private var board: (needsYou: [BoardEntry], working: [BoardEntry]) {
        let children = childrenByParent
        var needsYou: [BoardEntry] = []
        var working: [BoardEntry] = []
        for session in sortedRoots {
            let reason = reason(for: session)
            guard reason != nil || session.state.isActive else { continue }
            let entry = BoardEntry.make(session: session, reason: reason, open: model.openSessions[session.ref], catalogChildren: children[session.ref.sessionID] ?? [])
            if reason != nil { needsYou.append(entry) } else { working.append(entry) }
        }
        return (needsYou, working)
    }

    /// Everything that isn't live, newest first, in recency buckets.
    private var log: [LogGroup] {
        guard filter == .all || isServerFilter else { return [] }
        let rest = sortedRoots.filter { reason(for: $0) == nil && !$0.state.isActive }
        let grouped = Dictionary(grouping: rest) { LogBucket.bucket(for: $0.updatedAt) }
        return LogBucket.allCases.compactMap { bucket in grouped[bucket].map { LogGroup(bucket: bucket, sessions: $0) } }
    }

    private var isServerFilter: Bool {
        if case .server = filter { return true }
        return false
    }

    private var isEmpty: Bool {
        let board = board
        switch filter {
        case .needsYou: return board.needsYou.isEmpty
        case .running: return board.working.isEmpty
        case .all, .server: return board.needsYou.isEmpty && board.working.isEmpty && log.isEmpty
        }
    }

    private var needsYouCount: Int { onServer.count { reason(for: $0) != nil } }
    private var workingCount: Int { onServer.count { $0.state.isActive && reason(for: $0) == nil } }

    @Environment(\.horizontalSizeClass) private var sizeClass
    #if os(iOS)
    @State private var editMode: EditMode = .inactive
    #endif

    var body: some View {
        list
            .navigationTitle(title)
            #if os(iOS)
            .navigationBarTitleDisplayMode(.inline)
            .environment(\.editMode, $editMode)
            #endif
            .searchable(text: $query, prompt: "Search loaded sessions")
            .refreshable {
                if let serverID { await model.refreshCatalog(serverID) } else { await model.refreshForeground() }
            }
            .toolbar { toolbar }
            .overlay { if isEmpty { emptyState } }
            .sheet(isPresented: $showingOptions) {
                NewSessionSheet(initialServerID: serverID, operationID: optionsOperationID) { ref in
                    guard ref.serverID == self.serverID else { return }
                    selection = ref
                    optionsOperationID = UUID()
                }
            }
            .alert("Couldn't Start Session", isPresented: Binding(get: { createError != nil }, set: { if !$0 { createError = nil } })) {
                Button("OK", role: .cancel) { createOperationID = UUID() }
                if serverID != nil { Button("Options…") { createOperationID = UUID(); showingOptions = true } }
            } message: {
                Text(createError ?? "")
            }
            .onChange(of: serverID) { _, newServerID in
                multiSelection = []
                if selection?.serverID != newServerID { selection = nil }
                createOperationID = UUID()
                optionsOperationID = UUID()
            }
            .onChange(of: multiSelection) { _, refs in
                if refs.count == 1, let ref = refs.first, ref != selection { selection = ref }
            }
            .onChange(of: selection, initial: true) { _, ref in
                if let ref, ref.serverID != serverID { selection = nil; return }
                if let ref, !multiSelection.contains(ref) || multiSelection.count == 1 { multiSelection = [ref] }
                if ref == nil && multiSelection.count == 1 { multiSelection = [] }
            }
            .safeAreaInset(edge: .bottom, spacing: 0) {
                if showsBatchBar { batchBar }
            }
            .confirmationDialog("Delete “\(pendingDelete?.title.nonEmpty ?? "this session")”?", isPresented: Binding(get: { pendingDelete != nil }, set: { if !$0 { pendingDelete = nil } }), titleVisibility: .visible, presenting: pendingDelete) { session in
                Button("Delete Session", role: .destructive) { Task { await delete(session) } }
            } message: { _ in
                Text("The session and its history are deleted from the server. This can't be undone.")
            }
            .sensoryFeedback(.success, trigger: starFeedback)
            .sensoryFeedback(.selection, trigger: filter)
            .animation(Motion.reveal(reduceMotion), value: filter)
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
    }

    @ViewBuilder
    private var list: some View {
        let board = board
        let log = log
        // Said once when nothing is live: it rides in the first log group's header so the distance to
        // "Today" is a deliberate 12pt rather than a List section's own padding.
        let quietServer = board.needsYou.isEmpty && board.working.isEmpty && query.isEmpty ? activeServer : nil
        let content = List(selection: $multiSelection) {
            if filter != .running { boardSection(title: "Needs you", entries: board.needsYou) }
            if filter != .needsYou { boardSection(title: "Working", entries: board.working) }
            ForEach(log) { group in
                Section {
                    ForEach(group.sessions) { session in
                        row(session)
                    }
                } header: {
                    if group.id == log.first?.id, let quietServer {
                        VStack(alignment: .leading, spacing: Metrics.m) {
                            quietLine(quietServer)
                            sectionHeader(group.bucket.title, count: group.sessions.count)
                        }
                    } else {
                        sectionHeader(group.bucket.title, count: group.sessions.count)
                    }
                }
            }
            if let serverID, model.catalogHasMore.contains(serverID), filter == .all || isServerFilter {
                Button("Load older sessions") { Task { await model.loadMoreSessions(serverID: serverID) } }
                    .frame(maxWidth: .infinity)
                    .buttonStyle(.borderless)
                    .font(.callout)
                    .listRowSeparator(.hidden)
            }
        }
        if sizeClass == .compact {
            content
                .listStyle(.plain)
                #if os(iOS)
                // Plain-list sections sit ~35pt apart by default; the log reads as one column at 16pt.
                .listSectionSpacing(Metrics.l)
                #endif
        } else {
            content
        }
    }

    @ViewBuilder
    private func boardSection(title: String, entries: [BoardEntry]) -> some View {
        if !entries.isEmpty {
            Section {
                ForEach(entries) { entry in
                    BoardTag(entry: entry)
                        .tag(entry.session.ref)
                        .listRowSeparator(.hidden)
                        .listRowInsets(EdgeInsets(top: Metrics.boardSpacing / 2, leading: Metrics.m, bottom: Metrics.boardSpacing / 2, trailing: Metrics.m))
                        .swipeActions(edge: .leading, allowsFullSwipe: true) { starAction(entry.session) }
                        // Live work gets no trailing swipe: stopping or deleting it is a confirmed menu action.
                        .contextMenu { rowMenu(entry.session, live: true) }
                        .simultaneousGesture(openTap(entry.session.ref))
                }
            } header: {
                sectionHeader(title, count: entries.count, attention: title == "Needs you")
            }
        }
    }

    /// Said once, in words, instead of a row of zero counts. Header text is semibold by default; this is a
    /// sentence, so it takes regular weight and sentence case explicitly.
    private func quietLine(_ server: ServerProjection) -> some View {
        Text("Nothing is running on \(server.name).")
            .font(.footnote)
            .fontWeight(.regular)
            .foregroundStyle(.secondary)
            .textCase(nil)
            .accessibilityAddTraits(.isStaticText)
    }

    private func sectionHeader(_ title: String, count: Int, attention: Bool = false) -> some View {
        HStack(alignment: .firstTextBaseline, spacing: Metrics.xs) {
            Text(title)
                .foregroundStyle(attention ? AnyShapeStyle(Palette.signalText) : AnyShapeStyle(.primary))
            Text(count.formatted())
                .font(.figure)
                .foregroundStyle(.tertiary)
        }
        .font(.footnote.weight(.semibold))
        .textCase(nil)
        .accessibilityElement(children: .combine)
    }

    /// Runs alongside list selection so re-tapping an already selected row still opens it. Edit mode and
    /// multi-selection keep their native tap meaning.
    /// iOS only: on the Mac, list selection already shows the detail and ⌘-click multi-selection must not be
    /// pre-empted by a tap.
    private func openTap(_ ref: SessionRef) -> some Gesture {
        TapGesture().onEnded {
            #if os(iOS)
            guard CompactRouting.rowTapOpens(isEditing: editMode.isEditing, selectedCount: multiSelection.count) else { return }
            onOpen(ref)
            #endif
        }
    }

    /// Swipe → : mark. The same action sits in the context menu, so nothing is hidden behind the gesture.
    private func starAction(_ session: SessionProjection) -> some View {
        Button {
            toggleStar(session)
        } label: {
            Label(session.starred ? "Unstar" : "Star", systemImage: session.starred ? "star.slash" : "star")
        }
        .tint(.yellow)
    }

    private func toggleStar(_ session: SessionProjection) {
        Task {
            do {
                try await model.setSessionStarred(session.ref, starred: !session.starred)
                starFeedback += 1
            } catch {
                batchMessage = error.localizedDescription
            }
        }
    }

    @ViewBuilder
    private func rowMenu(_ session: SessionProjection, live: Bool = false) -> some View {
        Button(session.starred ? "Unstar" : "Star", systemImage: session.starred ? "star.slash" : "star") { toggleStar(session) }
        Button("Open", systemImage: "arrow.right.circle") { onOpen(session.ref) }
        if !live {
            Divider()
            Button("Delete…", systemImage: "trash", role: .destructive) { pendingDelete = session }
        }
    }

    private func delete(_ session: SessionProjection) async {
        do {
            let result = try await model.deleteSessions(serverID: session.ref.serverID, sessionIDs: [session.ref.sessionID], confirmedSessionIDs: [session.ref.sessionID])
            if result.deletedIDs.contains(session.ref.sessionID) {
                if selection == session.ref { selection = nil }
                multiSelection.remove(session.ref)
            } else {
                batchMessage = "The server couldn't delete “\(session.title.nonEmpty ?? "this session")”."
            }
        } catch {
            batchMessage = error.localizedDescription
        }
    }

    @ToolbarContentBuilder
    private var toolbar: some ToolbarContent {
        ToolbarItem(placement: .principal) {
            Menu {
                Section("Active Server") {
                    ForEach(model.servers) { server in
                        Button {
                            serverID = server.id
                            filter = .all
                            selection = nil
                        } label: {
                            Label(serverMenuTitle(server), systemImage: server.id == serverID ? "checkmark.circle.fill" : "server.rack")
                        }
                    }
                }
                Section {
                    Button("Add Server…", systemImage: "plus", action: onAddServer)
                    Button("Manage Servers…", systemImage: "slider.horizontal.3", action: onManageServers)
                    if let activeServer {
                        if activeServer.connectionIssue == .identityChanged {
                            Button("Edit Server…", systemImage: "exclamationmark.lock") { onEditServer(activeServer.id) }
                        } else {
                            Button("Reconnect", systemImage: "arrow.clockwise") { Task { do { try await model.reconnectServer(activeServer.id) } catch { createError = error.localizedDescription } } }
                        }
                    }
                }
            } label: {
                HStack(spacing: Metrics.xs) {
                    if let activeServer { HealthDot(state: activeServer.state) }
                    Text(activeServer?.name ?? "Choose Server").font(.headline).lineLimit(1)
                    Image(systemName: "chevron.down").font(.caption2.weight(.semibold)).foregroundStyle(.secondary)
                }
            }
            .accessibilityLabel("Active server, \(activeServer?.name ?? "none")")
        }
        #if os(iOS)
        if editMode.isEditing {
            ToolbarItem(placement: .topBarLeading) {
                Button("Done") { withAnimation { editMode = .inactive } }
                    .fontWeight(.semibold)
            }
        }
        #endif
        ToolbarItemGroup(placement: .primaryAction) {
            Button {
                Task { await createDefault() }
            } label: {
                if creating { ProgressView() } else { Label("New Session", systemImage: "plus") }
            }
            .keyboardShortcut("n", modifiers: .command)
            .disabled(activeServer == nil || creating || activeServer?.state != .live)
            Menu {
                Button("New Session Options…", systemImage: "slider.horizontal.3") { optionsOperationID = UUID(); showingOptions = true }
                    .keyboardShortcut("n", modifiers: [.command, .shift])
                    .disabled(activeServer == nil)
                Picker("Show", selection: $filter) {
                    Label("Everything", systemImage: "list.bullet").tag(SidebarItem.all)
                    Label("Needs you", systemImage: "hand.raised").tag(SidebarItem.needsYou)
                    Label("Working", systemImage: "bolt.horizontal").tag(SidebarItem.running)
                }
                .pickerStyle(.inline)
                Toggle("Starred only", systemImage: "star", isOn: $starredOnly)
                #if os(iOS)
                if !editMode.isEditing {
                    Button("Select Sessions", systemImage: "checkmark.circle") { withAnimation { editMode = .active } }
                        .disabled(isEmpty)
                }
                #endif
            } label: {
                Label("More", systemImage: "ellipsis.circle")
            }
        }
    }

    private func serverMenuTitle(_ server: ServerProjection) -> String {
        let needs = model.needsYou.filter { $0.id.serverID == server.id }.count
        var parts = [server.name, server.state.label, "\(server.sessionCount) sessions"]
        if needs > 0 { parts.append(needs == 1 ? "1 needs you" : "\(needs) need you") }
        return parts.joined(separator: ", ")
    }

    // MARK: Batch

    private var batchRefs: [SessionRef] {
        model.sessions.map(\.ref).filter { multiSelection.contains($0) }
    }

    private var showsBatchBar: Bool {
        #if os(iOS)
        if editMode.isEditing { return true }
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

    private func createDefault() async {
        guard let serverID, !creating else { return }
        creating = true
        defer { creating = false }
        do {
            let ref = try await model.createDefaultSession(serverID: serverID, operationID: createOperationID)
            guard self.serverID == serverID else { return }
            selection = ref
            createOperationID = UUID()
        } catch {
            createError = error.localizedDescription
        }
    }

    private func row(_ session: SessionProjection) -> some View {
        SessionRow(session: session, reason: reason(for: session))
            .tag(session.ref)
            .listRowInsets(EdgeInsets(top: Metrics.xs, leading: Metrics.m, bottom: Metrics.xs, trailing: Metrics.l))
            .swipeActions(edge: .leading, allowsFullSwipe: true) { starAction(session) }
            // No full swipe: the trailing action only opens the confirmation the menu uses.
            .swipeActions(edge: .trailing, allowsFullSwipe: false) {
                Button(role: .destructive) { pendingDelete = session } label: { Label("Delete…", systemImage: "trash") }
            }
            .contextMenu { rowMenu(session) }
            .simultaneousGesture(openTap(session.ref))
    }

    @ViewBuilder
    private var emptyState: some View {
        if !query.isEmpty {
            ContentUnavailableView.search(text: query)
        } else if let activeServer, activeServer.connectionIssue == .identityChanged {
            // Fail-closed by design: Reconnect would be rejected, so the only honest action is Edit & Test.
            ContentUnavailableView {
                Label("Server Identity Changed", systemImage: "exclamationmark.lock")
            } description: {
                Text("\(activeServer.name) reports a different identity than the one saved on this device. Edit and test the server to trust it again. Your sessions remain on the server.")
            } actions: {
                Button("Edit Server…") { onEditServer(activeServer.id) }
                    .buttonStyle(.borderedProminent)
            }
        } else if activeServer?.state == .offline || activeServer?.state == .unauthorized {
            ContentUnavailableView {
                Label(activeServer?.state == .unauthorized ? "Reconnect Required" : "Server Unavailable", systemImage: "wifi.exclamationmark")
            } description: {
                Text("\(activeServer?.name ?? "This server") is not currently available. Your sessions remain on the server.")
            } actions: {
                if let activeServer {
                    Button("Reconnect") { Task { do { try await model.reconnectServer(activeServer.id) } catch { createError = error.localizedDescription } } }
                        .buttonStyle(.borderedProminent)
                }
            }
        } else {
            switch filter {
            case .needsYou:
                ContentUnavailableView("Nothing needs you", systemImage: "checkmark.circle", description: Text("Approvals, questions and failures from this server appear here."))
            case .running:
                ContentUnavailableView("Nothing is working", systemImage: "moon.zzz", description: Text("Sessions with an agent at work on this server appear here."))
            case .all, .server:
                ContentUnavailableView {
                    Label("No Sessions", systemImage: "bubble.left.and.bubble.right")
                } description: {
                    Text(activeServer == nil ? "Connect a server to see its sessions." : "Start a session with this server's default agent team.")
                } actions: {
                    if activeServer != nil {
                        Button("New Session") { Task { await createDefault() } }.buttonStyle(.borderedProminent)
                    } else {
                        Button("Add Server…", action: onAddServer).buttonStyle(.borderedProminent)
                    }
                }
            }
        }
    }
}

struct NewSessionTarget: Identifiable {
    let id = UUID()
    let serverID: UUID?
}
