import SwiftUI
import AgentKit

struct ReviewWorkspaceView: View {
    @Environment(AppModel.self) private var model
    @Environment(\.horizontalSizeClass) private var sizeClass
    let session: OpenSessionProjection
    @Binding var composerText: String
    @Binding var composerAttachments: [SessionDocumentAttachment]

    @State private var snapshotLoading = false
    @State private var snapshotError: String?
    @State private var worktrees: [String: ReviewWorktree] = [:]
    @State private var loadingWorktrees: Set<String> = []
    @State private var worktreeErrors: [String: String] = [:]
    @State private var expanded: Set<String> = []
    @State private var selection: ReviewFileSelection?
    @State private var document: ReviewDocument?
    @State private var documentGeneration: UInt64 = 0
    @State private var documentLoading = false
    @State private var documentError: String?
    @State private var showsNavigator = true
    @State private var showsSidePanel = true
    @State private var sidePanel: SidePanel = .conversation
    @State private var reanchoring: (scope: ReviewDraftScope, comment: ReviewComment)?
    @State private var compactSheet: SidePanel?

    enum SidePanel: String, Identifiable {
        case conversation, draft
        var id: Self { self }
    }

    private var ref: SessionRef { session.ref }
    private var snapshot: ReviewSnapshot? { model.reviewSnapshots[ref] }
    private var isCompact: Bool { sizeClass == .compact }
    private var instanceID: String? { model.servers.first { $0.id == ref.serverID }?.identity?.instanceID }

    private var draftCount: Int {
        model.reviewDrafts.drafts.values
            .filter { $0.scope.serverID == ref.serverID && $0.scope.sessionID == ref.sessionID }
            .reduce(0) { $0 + $1.comments.count }
    }

    var body: some View {
        Group {
            if isCompact { compactLayout } else { regularLayout }
        }
        .task(id: ref) {
            if snapshot == nil { await refresh() } else { expandDefault() }
        }
    }

    // MARK: Layouts

    private var regularLayout: some View {
        HStack(spacing: 0) {
            if showsNavigator {
                navigatorColumn
                    .frame(width: Metrics.navigatorColumn)
                Divider()
            }
            diffColumn(forceUnified: false)
                .frame(maxWidth: .infinity, maxHeight: .infinity)
            if showsSidePanel {
                Divider()
                sideColumn
                    .frame(width: Metrics.conversationColumn)
            }
        }
        .toolbar {
            ToolbarItemGroup(placement: .navigation) {
                Button {
                    withAnimation(.snappy) { showsNavigator.toggle() }
                } label: {
                    Label("Files", systemImage: "sidebar.left")
                }
                .help("Show or hide changed files")
            }
            ToolbarItemGroup(placement: .primaryAction) {
                refreshButton
                Button {
                    withAnimation(.snappy) { showsSidePanel.toggle() }
                } label: {
                    Label("Conversation", systemImage: "bubble.right")
                }
                .badge(draftCount)
                .keyboardShortcut("j", modifiers: [.command, .option])
                .help("Show or hide the conversation and review draft")
            }
        }
    }

    private var compactLayout: some View {
        navigatorColumn
            .navigationDestination(item: $selection) { _ in
                diffColumn(forceUnified: true)
                    .navigationTitle(selection?.file.fileName ?? "Diff")
                    .toolbar { compactToolbar }
            }
            .toolbar { compactToolbar }
            .sheet(item: $compactSheet) { panel in
                NavigationStack {
                    Group {
                        switch panel {
                        case .conversation:
                            ConversationPane(session: session, composerText: $composerText, composerAttachments: $composerAttachments)
                        case .draft:
                            draftPanel
                        }
                    }
                    .navigationTitle(panel == .conversation ? session.agentName : "Review Draft")
                    #if os(iOS)
                    .navigationBarTitleDisplayMode(.inline)
                    #endif
                    .toolbar {
                        ToolbarItem(placement: .confirmationAction) { Button("Done") { compactSheet = nil } }
                    }
                }
                .presentationDetents([.medium, .large])
            }
    }

    @ToolbarContentBuilder
    private var compactToolbar: some ToolbarContent {
        ToolbarItemGroup(placement: .primaryAction) {
            Button { compactSheet = .draft } label: { Label("Draft", systemImage: "text.bubble") }
                .badge(draftCount)
            Button { compactSheet = .conversation } label: { Label("Conversation", systemImage: "bubble.right") }
        }
    }

    private var refreshButton: some View {
        Button {
            Task { await refresh() }
        } label: {
            Label("Rescan", systemImage: "arrow.clockwise")
        }
        .keyboardShortcut("r", modifiers: .command)
        .disabled(snapshotLoading)
        .help("Scan the workspace again")
    }

    // MARK: Columns

    @ViewBuilder
    private var navigatorColumn: some View {
        if let snapshot {
            VStack(spacing: 0) {
                scanHeader(snapshot)
                if !snapshot.scope.unavailable.isEmpty {
                    ContentUnavailableView("Review Unavailable", systemImage: "eye.slash", description: Text(snapshot.scope.unavailable))
                } else if snapshot.repositories.isEmpty {
                    ContentUnavailableView("No Git Repositories", systemImage: "shippingbox", description: Text("This session's workspace has no Git repositories to review."))
                } else {
                    ReviewNavigator(
                        snapshot: snapshot,
                        worktrees: worktrees,
                        loadingWorktrees: loadingWorktrees,
                        worktreeErrors: worktreeErrors,
                        commentCount: commentCount,
                        tapToSelect: isCompact,
                        expanded: $expanded,
                        selection: $selection,
                        loadWorktree: { id in Task { await loadWorktree(id) } }
                    )
                }
            }
        } else if let snapshotError {
            ContentUnavailableView {
                Label("Couldn't Scan Workspace", systemImage: "exclamationmark.triangle")
            } description: {
                Text(snapshotError)
            } actions: {
                Button("Try Again") { Task { await refresh() } }
            }
        } else {
            ProgressView("Scanning workspace…")
                .frame(maxWidth: .infinity, maxHeight: .infinity)
        }
    }

    private func scanHeader(_ snapshot: ReviewSnapshot) -> some View {
        VStack(alignment: .leading, spacing: Metrics.xxs) {
            HStack(spacing: Metrics.xs) {
                Text(snapshot.scope.remoteLabel.nonEmpty ?? snapshot.scope.label)
                    .font(.callout.weight(.semibold))
                    .lineLimit(1)
                Spacer()
                if snapshotLoading { ProgressView().controlSize(.small) }
                if isCompact { refreshButton.labelStyle(.iconOnly) }
            }
            if let when = Format.relative(snapshot.capturedAt) {
                Text("Scanned \(when)").font(.caption).foregroundStyle(.secondary)
            }
            if snapshot.maybeOutdated {
                Label("This scan may be outdated.", systemImage: "clock.badge.exclamationmark")
                    .font(.caption)
                    .foregroundStyle(.orange)
            }
            if !snapshot.scope.note.isEmpty {
                Text(snapshot.scope.note).font(.caption).foregroundStyle(.secondary)
            }
            if let snapshotError {
                Label(snapshotError, systemImage: "exclamationmark.triangle").font(.caption).foregroundStyle(.red)
            }
        }
        .padding(.horizontal, Metrics.l)
        .padding(.vertical, Metrics.s)
    }

    @ViewBuilder
    private func diffColumn(forceUnified: Bool) -> some View {
        if let selection {
            let scope = draftScope(for: selection)
            let comments = scope.map { model.reviewDrafts.draft(for: $0).comments.filter { $0.anchor.path == selection.file.path && $0.anchor.group == selection.file.group } } ?? []
            DiffPane(
                selection: selection,
                document: document,
                generation: documentGeneration,
                loading: documentLoading,
                error: documentError,
                comments: comments,
                canComment: commentBlockedReason(scope) == nil,
                commentBlockedReason: commentBlockedReason(scope),
                reanchoring: reanchoring.flatMap { $0.scope == scope ? $0.comment : nil },
                forceUnified: forceUnified,
                onRefresh: { Task { await refresh() } },
                onAdd: { anchor, body in
                    guard let scope else { return }
                    try model.addReviewComment(scope: scope, anchor: anchor, body: body)
                },
                onEdit: { comment, body in
                    guard let scope else { return }
                    try model.editReviewComment(scope: scope, commentID: comment.id, body: body)
                },
                onReanchor: { comment, anchor in
                    guard let scope else { return }
                    model.reanchorReviewComment(scope: scope, commentID: comment.id, anchor: anchor)
                    reanchoring = nil
                },
                onCancelReanchor: { reanchoring = nil }
            )
            .task(id: selection) { await loadDocument(selection) }
        } else {
            ContentUnavailableView("Select a File", systemImage: "plus.forwardslash.minus", description: Text("Choose a changed file to review its diff and comment on lines."))
        }
    }

    private var sideColumn: some View {
        VStack(spacing: 0) {
            Picker("Panel", selection: $sidePanel) {
                Text("Conversation").tag(SidePanel.conversation)
                Text(draftCount > 0 ? "Draft (\(draftCount))" : "Draft").tag(SidePanel.draft)
            }
            .pickerStyle(.segmented)
            .labelsHidden()
            .padding(Metrics.s)
            Divider()
            switch sidePanel {
            case .conversation:
                ConversationPane(session: session, composerText: $composerText, composerAttachments: $composerAttachments)
            case .draft:
                draftPanel
            }
        }
    }

    private var draftPanel: some View {
        ReviewDraftPanel(session: session, snapshot: snapshot) { scope, comment in
            compactSheet = nil
            show(comment, in: scope)
        } onReanchor: { scope, comment in
            compactSheet = nil
            reanchoring = (scope, comment)
            show(comment, in: scope)
        }
    }

    // MARK: Data

    private func commentCount(worktreeID: String, path: String) -> Int {
        model.reviewDrafts.drafts.values
            .filter { $0.scope.serverID == ref.serverID && $0.scope.sessionID == ref.sessionID && $0.scope.worktreeID == worktreeID }
            .reduce(0) { $0 + $1.comments.filter { $0.anchor.path == path }.count }
    }

    private func draftScope(for selection: ReviewFileSelection) -> ReviewDraftScope? {
        guard let instanceID, let snapshot else { return nil }
        return ReviewDraftScope(serverID: ref.serverID, instanceID: instanceID, sessionID: ref.sessionID, repositoryID: selection.repositoryID, worktreeID: selection.worktreeID, workspaceLabel: snapshot.scope.label)
    }

    private func commentBlockedReason(_ scope: ReviewDraftScope?) -> String? {
        guard let scope else { return "Commenting needs the server's identity. Refresh the server and try again." }
        if model.reviewDrafts.draft(for: scope).comments.count >= ReviewLimits.comments { return "This draft has reached \(ReviewLimits.comments) comments." }
        if model.reviewDrafts.attempts[scope] != nil { return "A send is pending for this draft. Finish it before adding comments." }
        return nil
    }

    private func show(_ comment: ReviewComment, in scope: ReviewDraftScope) {
        let worktree = worktrees[scope.worktreeID] ?? snapshot?.repositories.first { $0.id == scope.repositoryID }?.worktrees.first { $0.id == scope.worktreeID }
        if isCompact == false { sidePanel = .draft }
        expanded.insert(scope.worktreeID)
        guard let file = worktree?.files.first(where: { $0.path == comment.anchor.path && $0.group == comment.anchor.group }) ?? worktree?.files.first(where: { $0.path == comment.anchor.path }) else {
            documentError = "\(comment.anchor.path) isn't in the latest scan. Its original lines are kept in the draft."
            return
        }
        selection = ReviewFileSelection(repositoryID: scope.repositoryID, worktreeID: scope.worktreeID, file: file)
    }

    private func expandDefault() {
        guard expanded.isEmpty, let snapshot else { return }
        let all = snapshot.repositories.flatMap { repo in repo.worktrees.map { (repo, $0) } }
        guard let (_, worktree) = all.first(where: { $0.1.current }) ?? all.first else { return }
        expanded.insert(worktree.id)
        if worktree.files.isEmpty && worktree.state != "clean" { Task { await loadWorktree(worktree.id) } }
    }

    private func refresh() async {
        snapshotLoading = true
        snapshotError = nil
        defer { snapshotLoading = false }
        do {
            let fresh = try await model.reviewSnapshot(ref, refresh: true)
            worktrees = [:]
            worktreeErrors = [:]
            expandDefault()
            for id in expanded where fresh.repositories.contains(where: { $0.worktrees.contains { $0.id == id && $0.files.isEmpty && $0.state != "clean" } }) {
                await loadWorktree(id)
            }
            if let selection {
                let files = (worktrees[selection.worktreeID] ?? fresh.repositories.flatMap(\.worktrees).first { $0.id == selection.worktreeID })?.files ?? []
                if let file = files.first(where: { $0.id == selection.file.id }) {
                    let updated = ReviewFileSelection(repositoryID: selection.repositoryID, worktreeID: selection.worktreeID, file: file)
                    self.selection = updated
                    await loadDocument(updated)
                } else {
                    self.selection = nil
                    document = nil
                }
            }
        } catch {
            snapshotError = error.localizedDescription
        }
    }

    private func loadWorktree(_ id: String) async {
        guard let generation = snapshot?.generation, !loadingWorktrees.contains(id) else { return }
        loadingWorktrees.insert(id)
        worktreeErrors[id] = nil
        defer { loadingWorktrees.remove(id) }
        do {
            worktrees[id] = try await model.reviewWorktree(ref, generation: generation, worktreeID: id)
        } catch {
            worktreeErrors[id] = message(for: error)
        }
    }

    private func loadDocument(_ selection: ReviewFileSelection) async {
        guard let generation = snapshot?.generation else { return }
        if document?.key != selection.documentKey || documentGeneration != generation { document = nil }
        documentLoading = true
        documentError = nil
        defer { documentLoading = false }
        do {
            let loaded = try await model.reviewDocument(ref, generation: generation, key: selection.documentKey)
            guard self.selection == selection else { return }
            document = loaded
            documentGeneration = generation
        } catch is CancellationError {
        } catch {
            documentError = message(for: error)
        }
    }

    private func message(for error: Error) -> String {
        if let api = error as? APIError, api.statusCode == 409 || api.code == "review_generation_expired" {
            return "This scan expired. Rescan to continue."
        }
        return error.localizedDescription
    }
}
