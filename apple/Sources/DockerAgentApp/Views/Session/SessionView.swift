import SwiftUI
import AgentKit

enum SessionMode: String, CaseIterable, Identifiable {
    case conversation = "Conversation"
    case review = "Review"
    case files = "Files"
    var id: Self { self }

    var symbol: String {
        switch self {
        case .conversation: "bubble.left.and.text.bubble.right"
        case .review: "plus.forwardslash.minus"
        case .files: "folder"
        }
    }
}

struct SessionView: View {
    @Environment(AppModel.self) private var model
    @Environment(\.openSession) private var openSession
    @Environment(\.sessionDeleted) private var sessionDeleted
    @Environment(\.composerHandoff) private var composerHandoff
    @Environment(\.horizontalSizeClass) private var sizeClass
    let ref: SessionRef

    @State private var mode: SessionMode = .conversation
    @State private var showsInspector = false
    @State private var inspectorTab: InspectorTab = .agents
    @State private var composerText = ""
    @State private var composerAttachments: [SessionDocumentAttachment] = []
    @State private var openError: String?
    @State private var restoringWorkspace = false
    @State private var showingHistory = false
    @State private var waitTask: Task<Void, Never>?
    @State private var waitNote: String?
    @State private var pendingPrefill: String?
    @State private var waitLabel = ""
    @State private var exportDocument: SessionExportDocument?
    @State private var confirmingDelete = false
    @State private var actionError: String?
    /// This view instance's lease on the open session; released exactly once when the view goes away.
    @State private var lease: SessionViewLease?

    private var session: OpenSessionProjection? { model.openSessions[ref] }
    private var controls: SessionControlsProjection? { model.sessionControls[ref] }
    private var summary: SessionProjection? { model.sessions.first { $0.ref == ref } }

    var body: some View {
        withActions(chrome)
    }

    private var isCompact: Bool { sizeClass == .compact }

    private var chrome: some View {
        content
            // Identifier only: no `.accessibilityElement(children:)` so every row and control keeps its own AX node.
            .accessibilityIdentifier(SessionAccessibility.identity(ref))
            .navigationTitle(session?.title.nonEmpty ?? summary?.title.nonEmpty ?? "Session")
            .navigationSubtitle(session?.agentName ?? summary?.agentName ?? "")
            #if os(iOS)
            .navigationBarTitleDisplayMode(.inline)
            #endif
            .toolbar { toolbar }
            .inspector(isPresented: $showsInspector) {
                if let session {
                    SessionInspector(session: session, controls: controls, tab: $inspectorTab)
                        .inspectorColumnWidth(min: 240, ideal: 300, max: 400)
                }
            }
            .task(id: ref) { await open() }
            .onDisappear {
                waitTask?.cancel()
                // Keep an unsent draft so returning to this session restores it.
                composerHandoff?.save(text: composerText, attachments: composerAttachments, for: ref)
                releaseLease()
            }
    }

    private func withActions(_ view: some View) -> some View {
        view
            .sheet(isPresented: $showingHistory) { RecentHistorySheet(ref: ref) }
            .sheet(isPresented: $restoringWorkspace) {
                WorkspaceRestoreSheet(
                    sessionTitle: session?.title.nonEmpty ?? "This session",
                    workspaceLabel: model.reviewSnapshots[ref]?.scope.label.nonEmpty ?? "session workspace",
                    isIdle: !(session?.state.isActive ?? true),
                    load: { try await model.loadWorkspaceSnapshots(ref) },
                    undo: { proof in try await model.undoWorkspace(ref, proof: proof, confirmed: true).restoredFiles },
                    reset: { keep, proof in try await model.resetWorkspace(ref, keep: keep, proof: proof, confirmed: true).restoredFiles }
                )
            }
            .fileExporter(
                isPresented: Binding(get: { exportDocument != nil }, set: { if !$0 { exportDocument = nil } }),
                document: exportDocument,
                contentType: .json,
                defaultFilename: (session?.title.nonEmpty ?? "session") + ".json"
            ) { result in
                if case .failure(let error) = result { actionError = error.localizedDescription }
                exportDocument = nil
            }
            .confirmationDialog("Delete “\(session?.title.nonEmpty ?? "this session")”?", isPresented: $confirmingDelete, titleVisibility: .visible) {
                Button("Delete Session", role: .destructive) { Task { await deleteSession() } }
            } message: {
                Text("The session and its history are deleted from the server. This can't be undone.")
            }
            .alert("Couldn't Complete Action", isPresented: Binding(get: { actionError != nil }, set: { if !$0 { actionError = nil } })) {
                Button("OK", role: .cancel) {}
            } message: {
                Text(actionError ?? "")
            }
    }

    @ViewBuilder
    private var content: some View {
        if let session {
            switch mode {
            case .conversation:
                ConversationPane(session: session, composerText: $composerText, composerAttachments: $composerAttachments, findEnabled: true, banners: banners) {
                    inspectorTab = .agents
                    showsInspector = true
                }
            case .review:
                ReviewWorkspaceView(session: session, composerText: $composerText, composerAttachments: $composerAttachments)
            case .files:
                FilesBrowserView(session: session)
            }
        } else if let openError {
            ContentUnavailableView {
                Label("Couldn't Open Session", systemImage: "exclamationmark.triangle")
            } description: {
                Text(openError)
            } actions: {
                Button("Try Again") { Task { await open() } }
            }
        } else {
            ProgressView("Opening session…")
                .frame(maxWidth: .infinity, maxHeight: .infinity)
        }
    }

    @ToolbarContentBuilder
    private var toolbar: some ToolbarContent {
        if let parent = session?.parentSession {
            ToolbarItem(placement: .navigation) {
                Button {
                    openSession(parent)
                } label: {
                    if isCompact {
                        Image(systemName: "arrow.up.left")
                    } else {
                        Label(parentTitle(parent), systemImage: "arrow.up.left")
                    }
                }
                .help("Open the session that started this one")
                .accessibilityLabel("Open parent session \(parentTitle(parent))")
            }
        }
        if isCompact {
            // The principal slot would replace the title on iPhone. The mode picker rides in the session menu
            // once controls are loaded; until then a plain menu keeps Review and Files reachable.
            if controls == nil {
                ToolbarItem(placement: .primaryAction) {
                    Menu {
                        Picker("View", selection: $mode) {
                            ForEach(SessionMode.allCases) { mode in
                                Label(mode.rawValue, systemImage: mode.symbol).tag(mode)
                            }
                        }
                    } label: {
                        Label(mode.rawValue, systemImage: mode.symbol)
                    }
                    .disabled(session == nil)
                    .accessibilityLabel("View, \(mode.rawValue)")
                }
            }
        } else {
            ToolbarItem(placement: .principal) {
                Picker("Mode", selection: $mode) {
                    ForEach(SessionMode.allCases) { mode in
                        Label(mode.rawValue, systemImage: mode.symbol).tag(mode)
                    }
                }
                .pickerStyle(.segmented)
                .labelStyle(.titleOnly)
                .fixedSize()
                .disabled(session == nil)
            }
        }
        if let session {
            if !isCompact {
                ToolbarItem(placement: .status) {
                    ConnectionStatusView(state: session.connection, refreshedAt: session.refreshedAt)
                }
            }
            if let controls {
                ToolbarItem(placement: .primaryAction) {
                    SessionMenu(
                        title: session.title,
                        starred: controls.starred,
                        currentAgent: session.agentName,
                        agents: model.teamRosterBySession[ref] ?? [],
                        readOnly: controls.isReadOnly,
                        actions: menuActions(controls),
                        mode: isCompact ? $mode : nil
                    )
                }
            }
        }
        ToolbarItem(placement: .primaryAction) {
            Button {
                showsInspector.toggle()
            } label: {
                Label("Inspector", systemImage: "sidebar.trailing")
            }
            .keyboardShortcut("i", modifiers: [.command, .option])
            .disabled(session == nil || mode != .conversation)
            .help("Show agents and usage")
        }
    }

    private func open() async {
        if composerText.isEmpty && composerAttachments.isEmpty, let saved = composerHandoff?.restore(for: ref) {
            composerText = saved.text
            composerAttachments = saved.attachments
        }
        if let prefill = composerHandoff?.take(for: ref) {
            // Never overwrite an existing draft; offer the handed-off text instead.
            if composerText.isEmpty { composerText = prefill } else { pendingPrefill = prefill }
        }
        openError = nil
        do {
            let token = try await model.acquireSessionView(ref)
            // A token that arrives after this view's task was cancelled is released at once, never kept.
            if Task.isCancelled { model.releaseSessionView(token); return }
            if let lease, lease.ref == ref {
                // Try Again while still holding a lease: keep one, never two.
                model.releaseSessionView(token)
            } else {
                releaseLease()
                lease = token
            }
        } catch is CancellationError {
            // Only this view's wait was cancelled; the shared open continues for other owners.
            return
        } catch {
            openError = error.localizedDescription
            return
        }
        _ = try? await model.loadSessionControls(ref)
        _ = try? await model.loadTeamRoster(ref)
    }

    /// Idempotent: releasing a token twice, or a token the model no longer tracks, is harmless by contract.
    private func releaseLease() {
        if let lease { model.releaseSessionView(lease) }
        lease = nil
    }

    private func menuActions(_ controls: SessionControlsProjection) -> SessionMenuActions {
        let ref = ref
        let open = openSession
        return SessionMenuActions(
            rename: controls.allows(SessionCapability.editing) ? { try await model.setSessionTitle(ref, title: $0) } : nil,
            setStarred: { try await model.setSessionStarred(ref, starred: $0) },
            compact: controls.allows(SessionCapability.compaction) ? { try await model.compactSession(ref, additionalPrompt: $0) } : nil,
            branch: controls.allows(SessionCapability.branching) ? { open(try await model.branchSession(ref)) } : nil,
            switchAgent: controls.allows(SessionCapability.executionAvailable) ? { open(try await model.switchSessionAgent(ref, agentName: $0)) } : nil,
            openParent: session?.parentSession.map { parent in { open(parent) } },
            pause: controls.allows(SessionCapability.pause) ? (pauseTitle(controls), { togglePause() }) : nil,
            waitForTurn: currentTurnID.flatMap { turnID in waitTask == nil ? { startWaiting(turnID: turnID) } : nil },
            waitForAgents: (session?.state.isActive ?? false) && waitTask == nil ? { startWaiting() } : nil,
            history: { showingHistory = true },
            refresh: { Task { do { try await model.refreshSession(ref) } catch { actionError = error.localizedDescription } } },
            restoreWorkspace: controls.allows(SessionCapability.snapshots) ? { restoringWorkspace = true } : nil,
            exportJSON: inCatalog ? { Task { await exportJSON() } } : nil,
            delete: inCatalog ? { confirmingDelete = true } : nil
        )
    }

    private func pauseTitle(_ controls: SessionControlsProjection) -> String {
        if controls.paused { return "Resume" }
        return controls.pauseRequested ? "Cancel Pause Request" : "Pause"
    }

    private func togglePause() {
        Task {
            do {
                _ = try await model.pauseSession(ref)
            } catch {
                actionError = error.localizedDescription
            }
        }
    }

    private var currentTurnID: String? {
        guard let id = session?.turnID?.trimmingCharacters(in: .whitespaces), !id.isEmpty, session?.state.isActive == true else { return nil }
        return id
    }

    // Waiting only observes; cancelling it never stops the agents.
    private func startWaiting(turnID: String? = nil) {
        waitNote = nil
        waitLabel = turnID == nil ? "Waiting for this session and its agents to finish" : "Waiting for the current turn to finish"
        waitTask = Task {
            do {
                if let turnID {
                    try await model.waitForTurn(ref, turnID: turnID)
                    waitNote = "The turn finished."
                } else {
                    _ = try await model.waitForWorkflow(ref)
                    waitNote = "All agents finished."
                }
            } catch is CancellationError {
            } catch {
                // A failed or timed-out wait says nothing about the agents themselves.
                let timedOut = (error as? URLError)?.code == .timedOut
                waitNote = timedOut
                    ? "Stopped waiting after the connection timed out. The agents may still be running."
                    : "Stopped waiting: \(error.localizedDescription) The agents weren't affected."
            }
            waitTask = nil
        }
    }

    private var banners: [SessionBanner] {
        var result: [SessionBanner] = []
        if isCompact, let session, session.connection != .live, session.connection != .unknown {
            let when = Format.relative(session.refreshedAt).map { " since \($0)" } ?? ""
            result.append(SessionBanner(symbol: "dot.radiowaves.left.and.right", text: session.connection.label + when, tint: session.connection.tint))
        }
        if let pendingPrefill {
            result.append(SessionBanner(symbol: "text.insert", text: "A message from the original session is ready to insert.", tint: .accentColor, actionTitle: "Insert") {
                composerText += (composerText.hasSuffix("\n") ? "\n" : "\n\n") + pendingPrefill
                self.pendingPrefill = nil
            })
        }
        if let controls, controls.paused {
            result.append(SessionBanner(symbol: "pause.circle.fill", text: "Paused", tint: .orange, actionTitle: controls.allows(SessionCapability.pause) ? "Resume" : nil) { togglePause() })
        } else if controls?.pauseRequested == true {
            result.append(SessionBanner(symbol: "pause.circle", text: "Pause requested — waiting for a safe point", tint: .orange, actionTitle: "Cancel") { togglePause() })
        }
        if waitTask != nil {
            result.append(SessionBanner(symbol: "hourglass", text: waitLabel, tint: .secondary, actionTitle: "Stop Waiting") {
                waitTask?.cancel()
                waitTask = nil
            })
        } else if let waitNote {
            result.append(SessionBanner(symbol: waitNote.hasPrefix("All agents") || waitNote.hasPrefix("The turn") ? "checkmark.circle" : "hourglass.bottomhalf.filled", text: waitNote, tint: .secondary, actionTitle: "Dismiss") { self.waitNote = nil })
        }
        return result
    }

    private func parentTitle(_ parent: SessionRef) -> String {
        model.sessions.first { $0.ref == parent }?.title.nonEmpty ?? model.openSessions[parent]?.title.nonEmpty ?? "Parent session"
    }

    private var inCatalog: Bool { model.sessions.contains { $0.ref == ref } }

    private func exportJSON() async {
        do {
            let export = try await model.exportSessionsJSON(serverID: ref.serverID, sessionIDs: [ref.sessionID], confirmedSessionIDs: [ref.sessionID])
            guard export.exportedSessionIDs.contains(ref.sessionID) else {
                actionError = "The server didn't include this session in the export."
                return
            }
            exportDocument = SessionExportDocument(data: try export.encodedJSON())
        } catch {
            actionError = error.localizedDescription
        }
    }

    private func deleteSession() async {
        do {
            let result = try await model.deleteSessions(serverID: ref.serverID, sessionIDs: [ref.sessionID], confirmedSessionIDs: [ref.sessionID])
            if result.deletedIDs.contains(ref.sessionID) {
                sessionDeleted(ref)
            } else {
                actionError = "The server couldn't delete this session."
            }
        } catch {
            actionError = error.localizedDescription
        }
    }
}

/// Transcript with its live bottom stack; reused beside the review workspace.
struct ConversationPane: View {
    @Environment(AppModel.self) private var model
    let session: OpenSessionProjection
    @Binding var composerText: String
    @Binding var composerAttachments: [SessionDocumentAttachment]
    var findEnabled = false
    var banners: [SessionBanner] = []
    var onShowAgents: () -> Void = {}

    @State private var showingPrompts = false
    @State private var forkTarget: TranscriptItem?
    @State private var branchTarget: SessionBranchAnchor?
    @Environment(\.openWindow) private var openWindow
    @Environment(\.supportsMultipleWindows) private var supportsMultipleWindows
    @State private var forkError: String?
    @State private var forkErrorOffersRefresh = false
    @Environment(\.openSession) private var openSessionAction
    @Environment(\.accessibilityReduceMotion) private var reduceMotion
    @State private var finding = false
    @State private var findQuery = ""
    @State private var matchIndex = 0
    @FocusState private var findFocused: Bool

    private var ref: SessionRef { session.ref }

    /// Searches only the transcript already loaded in this window.
    private var matches: [String] {
        let query = findQuery.trimmingCharacters(in: .whitespaces)
        guard finding, !query.isEmpty else { return [] }
        return session.transcript.filter { item in
            item.origin != "runtime" && [item.text, item.toolOutput ?? "", item.toolArguments ?? ""].contains { $0.localizedCaseInsensitiveContains(query) }
        }.map(\.id)
    }

    private var currentMatch: String? {
        matches.isEmpty ? nil : matches[min(matchIndex, matches.count - 1)]
    }
    private var canFork: Bool {
        model.sessionControls[ref]?.allows(SessionCapability.branching) == true
    }

    private func fork(_ item: TranscriptItem) {
        guard let index = item.userMessageIndex else { return }
        Task {
            do {
                let result = try await model.forkBeforeUserMessage(ref, index: index, confirmed: true)
                openSessionAction(result.ref, prefill: result.prefilledText)
            } catch {
                forkError = error.localizedDescription
            }
        }
    }

    private func branch(_ anchor: SessionBranchAnchor) {
        Task {
            do {
                let newRef = try await model.branchSessionItem(ref, anchor: anchor, confirmed: true)
                // A separate window leaves this composer untouched; otherwise the draft is saved on navigation.
                if supportsMultipleWindows { openWindow(value: newRef) } else { openSessionAction(newRef) }
            } catch let api as APIError where api.statusCode == 409 {
                forkErrorOffersRefresh = true
                forkError = "The conversation changed since it was loaded. Refresh it, then choose the point again."
            } catch {
                forkError = error.localizedDescription
            }
        }
    }

    /// The model owns paging errors (matched to its own request ID) and their generation guard; the view only asks.
    private func loadOlder() async {
        try? await model.loadOlderSessionHistory(ref)
    }

    private var promptsAvailable: Bool {
        guard let controls = model.sessionControls[ref] else { return false }
        return controls.allows(SessionCapability.mcpPrompts) || controls.allows(SessionCapability.forkSkills)
    }

    private var pendingRecords: [DurableSendRecord] {
        (model.pendingSends[ref] ?? []).filter { $0.state != .accepted }
    }

    var body: some View {
        let matches = matches
        TranscriptView(sessionRef: ref, items: session.transcript, canRetry: session.state == .failed, onRetry: {
            Task { try? await model.retry(ref) }
        }, resolveMedia: { reference in
            try await model.resolveGeneratedMedia(ref, reference: reference)
        }, matchIDs: Set(matches), currentMatchID: currentMatch, onContinueFromHere: canFork ? { forkTarget = $0 } : nil, onBranchHere: canFork ? { branchTarget = $0.branchAnchor } : nil, omittedCount: session.omittedTranscriptItemCount,
                       history: session.transcriptWindow,
                       isLoadingOlder: model.historyPageLoading.contains(ref),
                       loadOlderError: model.historyPageErrors[ref],
                       onLoadOlder: { await loadOlder() })
        .safeAreaInset(edge: .top, spacing: 0) {
            if finding { findBar(matches.count) }
        }
        .sheet(isPresented: $showingPrompts) { promptsSheet }
        .confirmationDialog("Continue from here in a new session?", isPresented: Binding(get: { forkTarget != nil }, set: { if !$0 { forkTarget = nil } }), titleVisibility: .visible, presenting: forkTarget) { item in
            Button("Continue in New Session") { fork(item) }
        } message: { _ in
            Text("The new session copies the conversation before this message. The message goes into its composer for you to edit and send; this session stays unchanged.")
        }
        .confirmationDialog("Start a new session up to here?", isPresented: Binding(get: { branchTarget != nil }, set: { if !$0 { branchTarget = nil } }), titleVisibility: .visible, presenting: branchTarget) { anchor in
            Button("Create Branch") { branch(anchor) }
        } message: { _ in
            Text("The new session copies the conversation through this point and starts with an empty composer. This session and its draft stay as they are.")
        }
        .alert("Couldn't Continue from Here", isPresented: Binding(get: { forkError != nil }, set: { if !$0 { forkError = nil; forkErrorOffersRefresh = false } })) {
            if forkErrorOffersRefresh {
                Button("Refresh Conversation") { Task { try? await model.refreshSession(ref) } }
            }
            Button("OK", role: .cancel) {}
        } message: {
            Text(forkError ?? "")
        }
        .background {
            if findEnabled {
                Button("Find") {
                    finding = true
                    findFocused = true
                }
                .keyboardShortcut("f", modifiers: .command)
                .hidden()
            }
        }
        .overlay {
            if session.transcript.isEmpty {
                ContentUnavailableView {
                    Label("Ready", systemImage: "sparkles")
                } description: {
                    Text("Nothing has happened here yet. Tell \(session.agentName.nonEmpty ?? "the agent") what to work on.")
                }
            }
        }
        .safeAreaInset(edge: .bottom, spacing: 0) {
            VStack(spacing: Metrics.s) {
                if teamLineVisible {
                    ActivityStrip(session: session, onShowAgents: onShowAgents, onStop: { try await model.cancel(ref) })
                        .frame(minHeight: Metrics.target)
                        .transition(reduceMotion ? .opacity : .move(edge: .bottom).combined(with: .opacity))
                }
                ForEach(banners) { banner in
                    SessionBannerView(banner: banner)
                }
                if let operations = model.skillOperations[ref]?.filter({ $0.state != .completed }), !operations.isEmpty {
                    SkillOperationsView(operations: operations)
                }
                if let controls = model.sessionControls[ref], !controls.pendingInputs.isEmpty {
                    QueuedInputsView(
                        inputs: controls.pendingInputs,
                        shownInTranscript: QueuedTurns.shown(in: session.transcript),
                        edit: controls.allows(SessionCapability.editing) ? { input, text in
                            try await model.editPendingInput(ref, turnID: input.turnID, content: text, expectedContent: input.content)
                        } : nil,
                        cancelAll: controls.allows(SessionCapability.pendingRemoval) ? { try await model.cancelPendingInputs(ref) } : nil
                    )
                }
                if !pendingRecords.isEmpty {
                    PendingSendsView(records: pendingRecords) { record in
                        try await model.retryPendingSend(ref, requestID: record.requestID)
                    } discard: { record in
                        // The row has already asked for confirmation when delivery is unknown.
                        try model.discardPendingSend(ref, requestID: record.requestID, confirmUnknownDelivery: record.state == .deliveryUnknown)
                    }
                }
                if !session.interactions.isEmpty {
                    InteractionStack(interactions: session.interactions) { interaction, reply in
                        try await respond(interaction, reply)
                    }
                }
                composer
            }
            .padding(.horizontal, Metrics.s)
            .padding(.bottom, Metrics.xs)
            .frame(maxWidth: Metrics.readingWidth + 2 * Metrics.m)
            .frame(maxWidth: .infinity)
            // Animate the team line's insertion/removal only; nothing else in the stack moves on state ticks.
            .animation(Motion.reveal(reduceMotion), value: teamLineVisible)
        }
    }

    /// Authoritative, state-derived: the root is working (running or stopping), a sub-agent in the shown
    /// tree is running, or a person is needed (open interaction or an agent flagged for attention).
    /// Queued or waiting alone is not "working". No timers.
    private var teamLineVisible: Bool {
        let tally = TeamTally(session.agentTree)
        return TeamLineVisibility.isVisible(rootState: session.state, interactions: session.interactions.count,
                                            runningSubagents: tally.runningSubagents, attention: tally.needsYou)
    }

    private var composer: some View {
        ComposerView(
            text: $composerText,
            attachments: $composerAttachments,
            agentName: session.agentName,
            isRunning: session.state.isActive,
            pendingCount: (model.sessionControls[ref]?.pendingInputs.isEmpty ?? true) ? session.pendingInputCount : 0,
            canSend: session.connection != .unauthorized,
            openPrompts: promptsAvailable ? { showingPrompts = true } : nil
        ) { content, files, steer in
            let requestID = UUID().uuidString
            do {
                try await model.send(ref, content: content, attachments: files, mode: steer ? "steer" : "submit", requestID: requestID)
            } catch {
                // Delivery unknown: the pending row owns the message so the composer can't duplicate it.
                // Definitely not delivered: drop the outbox copy and keep the draft in the composer.
                if let record = pendingRecords.first(where: { $0.requestID == requestID }) {
                    if record.state == .deliveryUnknown { return }
                    if record.state == .prepared { try? model.discardPendingSend(ref, requestID: record.requestID) }
                }
                throw error
            }
        } stop: {
            try await model.cancel(ref)
        } unknownDuplicate: { content in
            pendingRecords.first { $0.payload == content && $0.state == .deliveryUnknown }?.requestID
        } retryExisting: { requestID in
            try await model.retryPendingSend(ref, requestID: requestID)
        }
    }

    private var promptsSheet: some View {
        let controls = model.sessionControls[ref]
        return PromptsAndSkillsView(
            loadPrompts: controls?.allows(SessionCapability.mcpPrompts) == true ? { try await model.loadPrompts(ref) } : nil,
            loadSkills: controls?.allows(SessionCapability.forkSkills) == true ? { try await model.loadForkSkills(ref) } : nil,
            expand: { prompt, args in try await model.expandPrompt(ref, key: prompt.key, args: args) },
            insert: { text in
                composerText = composerText.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty ? text : composerText + "\n\n" + text
            },
            runSkill: { skill, task in try await model.runForkSkill(ref, name: skill.name, task: task) }
        )
    }

    private func findBar(_ count: Int) -> some View {
        HStack(spacing: Metrics.s) {
            Image(systemName: "magnifyingglass").foregroundStyle(.secondary)
            TextField("Find in this session", text: $findQuery)
                .textFieldStyle(.plain)
                .focused($findFocused)
                .onSubmit { step(1, count) }
                .onChange(of: findQuery) { matchIndex = 0 }
            if !findQuery.isEmpty {
                Text(count == 0 ? "No matches" : "\(min(matchIndex, count - 1) + 1) of \(count)")
                    .font(.caption.monospacedDigit())
                    .foregroundStyle(.secondary)
            }
            Button { step(-1, count) } label: { Image(systemName: "chevron.up") }
                .keyboardShortcut("g", modifiers: [.command, .shift])
                .disabled(count == 0)
                .accessibilityLabel("Previous match")
            Button { step(1, count) } label: { Image(systemName: "chevron.down") }
                .keyboardShortcut("g", modifiers: .command)
                .disabled(count == 0)
                .accessibilityLabel("Next match")
            Button("Done") {
                finding = false
                findQuery = ""
            }
            .keyboardShortcut(.cancelAction)
        }
        .buttonStyle(.borderless)
        .padding(.horizontal, Metrics.l)
        .padding(.vertical, Metrics.s)
        .background(.bar)
    }

    private func step(_ delta: Int, _ count: Int) {
        guard count > 0 else { return }
        matchIndex = (min(matchIndex, count - 1) + delta + count) % count
    }

    private func respond(_ interaction: InteractionProjection, _ reply: InteractionReply) async throws {
        switch (interaction, reply) {
        case let (.confirmation(id, _, toolName, arguments, _), .confirmation(decision, reason)):
            // Like the TUI, "always allow" grants a scoped pattern (e.g. shell:cmd=ls*), never the whole shell tool.
            let pattern = decision == .approveTool ? PermissionPattern.build(toolName: toolName, arguments: arguments) : nil
            if decision == .approveTool && pattern == nil { throw APIError("This command can't be read, so it can't be always-allowed. Allow it once instead.") }
            try await model.respond(ref, interactionID: id, kind: "confirmation", confirmation: decision, toolName: pattern, reason: reason)
        case let (.elicitation(id, _, _, _, elicitationID), .elicitation(action, content)):
            try await model.respond(ref, interactionID: id, kind: "elicitation", elicitationID: elicitationID, action: action, content: content)
        case let (.maxIterations(id, _, _), .maxIterations(proceed)):
            try await model.respond(ref, interactionID: id, kind: "max_iterations", confirmation: proceed ? .approve : .reject)
        default:
            break
        }
    }
}

extension String {
    var nonEmpty: String? { isEmpty ? nil : self }
}

/// Turn IDs whose user message already appears in the transcript as a queued card (exact IDs, no text matching).
enum QueuedTurns {
    static func shown(in transcript: [TranscriptItem]) -> Set<String> {
        Set(transcript.compactMap { $0.userInputState == .queued ? $0.turnID : nil })
    }
}

/// Pure rule for the team line, shared with tests.
enum TeamLineVisibility {
    static func isVisible(rootState: SessionState, interactions: Int, runningSubagents: Int, attention: Int = 0) -> Bool {
        rootState == .running || rootState == .cancelling || interactions > 0 || runningSubagents > 0 || attention > 0
    }
}

struct SessionBanner: Identifiable {
    let symbol: String
    let text: String
    let tint: Color
    var actionTitle: String?
    var action: () -> Void = {}
    var id: String { symbol + text }
}

struct SessionBannerView: View {
    let banner: SessionBanner

    var body: some View {
        HStack(spacing: Metrics.s) {
            Image(systemName: banner.symbol).foregroundStyle(banner.tint)
            Text(banner.text).font(.callout)
            Spacer(minLength: Metrics.s)
            if let title = banner.actionTitle {
                Button(title, action: banner.action)
                    .buttonStyle(.borderless)
                    .font(.callout.weight(.medium))
            }
        }
        .padding(.horizontal, Metrics.m)
        .padding(.vertical, Metrics.s)
        .floatingChip()
        .accessibilityElement(children: .combine)
    }
}
