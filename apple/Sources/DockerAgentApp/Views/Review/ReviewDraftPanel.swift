import SwiftUI
import AgentKit

struct ReviewDraftPanel: View {
    @Environment(AppModel.self) private var model
    let session: OpenSessionProjection
    let snapshot: ReviewSnapshot?
    let onShow: (ReviewDraftScope, ReviewComment) -> Void
    let onReanchor: (ReviewDraftScope, ReviewComment) -> Void

    @State private var preparing: ReviewDraftScope?
    @State private var preview: PreviewItem?
    @State private var errors: [ReviewDraftScope: String] = [:]
    @State private var editing: (scope: ReviewDraftScope, comment: ReviewComment)?

    private var drafts: [ReviewDraftProjection] {
        model.reviewDrafts.drafts.values
            .filter { $0.scope.serverID == session.ref.serverID && $0.scope.sessionID == session.ref.sessionID && !$0.comments.isEmpty }
            .sorted { $0.scope.worktreeID < $1.scope.worktreeID }
    }

    var body: some View {
        Group {
            if drafts.isEmpty {
                ContentUnavailableView {
                    Label("No Draft Comments", systemImage: "text.bubble")
                } description: {
                    Text("Select lines in a diff and add comments. They're sent together to \(session.agentName) as one message.")
                }
            } else {
                List {
                    ForEach(drafts, id: \.scope) { draft in
                        Section {
                            ForEach(draft.comments) { comment in
                                DraftCommentRow(comment: comment) {
                                    onShow(draft.scope, comment)
                                } edit: {
                                    editing = (draft.scope, comment)
                                } delete: {
                                    model.deleteReviewComment(scope: draft.scope, commentID: comment.id)
                                } reanchor: {
                                    onReanchor(draft.scope, comment)
                                }
                            }
                            sendFooter(draft)
                        } header: {
                            scopeHeader(draft)
                        }
                    }
                }
                .listStyle(.inset)
            }
        }
        .sheet(item: $preview) { item in
            let attempt = item.attempt
            ReviewSendPreview(attempt: attempt, sessionTitle: session.title, pending: model.reviewDrafts.attempts[attempt.scope] != nil) {
                try await model.confirmReviewSend(scope: attempt.scope, focusedSession: attempt.focusedSession)
            }
        }
        .sheet(isPresented: Binding(get: { editing != nil }, set: { if !$0 { editing = nil } })) {
            if let editing {
                CommentComposer(
                    title: "Edit Comment",
                    path: editing.comment.anchor.path,
                    anchorLabel: AnchorBuilder.label(editing.comment.anchor),
                    context: editing.comment.anchor.context,
                    initialBody: editing.comment.body,
                    saveLabel: "Save"
                ) { body in
                    try model.editReviewComment(scope: editing.scope, commentID: editing.comment.id, body: body)
                    self.editing = nil
                } onCancel: {
                    self.editing = nil
                }
            }
        }
    }

    private func scopeHeader(_ draft: ReviewDraftProjection) -> some View {
        let repository = snapshot?.repositories.first { $0.id == draft.scope.repositoryID }
        let worktree = repository?.worktrees.first { $0.id == draft.scope.worktreeID }
        return HStack {
            VStack(alignment: .leading, spacing: 1) {
                Text(repository?.name ?? draft.scope.workspaceLabel)
                if let worktree {
                    Text(worktree.branch.nonEmpty ?? worktree.path).font(.caption2).foregroundStyle(.secondary)
                } else {
                    Text("Worktree not in the latest scan").font(.caption2).foregroundStyle(.orange)
                }
            }
            Spacer()
            Text("\(draft.comments.count)/\(ReviewLimits.comments)")
                .font(.caption.monospacedDigit())
                .foregroundStyle(draft.comments.count >= ReviewLimits.comments ? .red : .secondary)
        }
    }

    @ViewBuilder
    private func sendFooter(_ draft: ReviewDraftProjection) -> some View {
        let staleCount = draft.comments.filter(\.stale).count
        let pending = model.reviewDrafts.attempts[draft.scope]
        VStack(alignment: .leading, spacing: Metrics.s) {
            HStack(spacing: Metrics.xs) {
                Text("To")
                    .foregroundStyle(.secondary)
                AgentLabel(name: pending?.expectedAgent ?? session.agentName, font: .callout.weight(.semibold))
                Text("· \(session.title.nonEmpty ?? "this session")")
                    .foregroundStyle(.secondary)
                    .lineLimit(1)
            }
            .font(.callout)
            if staleCount > 0 && pending == nil {
                Label("\(staleCount) comment\(staleCount == 1 ? " needs" : "s need") re-anchoring before sending.", systemImage: "clock.arrow.circlepath")
                    .font(.caption)
                    .foregroundStyle(.orange)
            }
            if let error = errors[draft.scope] ?? draft.error {
                Label(error, systemImage: "exclamationmark.triangle.fill")
                    .font(.caption)
                    .foregroundStyle(.red)
            }
            Button {
                Task { await prepare(draft.scope) }
            } label: {
                HStack {
                    Text(pending != nil ? "Review Pending Send…" : "Review & Send…")
                    if preparing == draft.scope || draft.status == .sending { ProgressView().controlSize(.small) }
                }
                .frame(maxWidth: .infinity)
            }
            .buttonStyle(.borderedProminent)
            .disabled(preparing != nil || draft.status == .sending || (staleCount > 0 && pending == nil))
            .help("Rechecks every comment against a fresh scan and the session's current agent")
        }
        .padding(.vertical, Metrics.s)
    }

    private func prepare(_ scope: ReviewDraftScope) async {
        preparing = scope
        errors[scope] = nil
        defer { preparing = nil }
        do {
            preview = PreviewItem(attempt: try await model.prepareReviewSend(scope: scope, focusedSession: session.ref))
        } catch {
            errors[scope] = error.localizedDescription
        }
    }
}

private struct PreviewItem: Identifiable {
    let attempt: ReviewSendAttempt
    var id: String { attempt.requestID }
}

private struct DraftCommentRow: View {
    let comment: ReviewComment
    let show: () -> Void
    let edit: () -> Void
    let delete: () -> Void
    let reanchor: () -> Void
    @State private var showsContext = false

    var body: some View {
        VStack(alignment: .leading, spacing: Metrics.xs) {
            HStack(spacing: Metrics.xs) {
                Text((comment.anchor.path as NSString).lastPathComponent)
                    .font(.caption.monospaced().weight(.medium))
                Text(AnchorBuilder.label(comment.anchor))
                    .font(.caption)
                    .foregroundStyle(.secondary)
                Spacer()
                if comment.stale {
                    Text("Changed")
                        .font(.caption2.weight(.semibold))
                        .foregroundStyle(.orange)
                        .padding(.horizontal, Metrics.xs)
                        .background(Color.orange.opacity(0.12), in: .capsule)
                        .help("The file changed after this comment was written. Its original lines are kept below.")
                }
            }
            Text(comment.body)
                .font(.callout)
                .lineLimit(5)
                .textSelection(.enabled)
            if comment.stale {
                DisclosureGroup("Original lines", isExpanded: $showsContext) {
                    Text(comment.anchor.context.prefix(12).joined(separator: "\n"))
                        .font(.monoSmall)
                        .foregroundStyle(.secondary)
                        .textSelection(.enabled)
                }
                .font(.caption)
            }
            HStack(spacing: Metrics.m) {
                Button("Show", action: show)
                if comment.stale { Button("Re-anchor…", action: reanchor) }
                Button("Edit", action: edit)
                Spacer()
                Button("Delete", role: .destructive, action: delete)
            }
            .buttonStyle(.borderless)
            .font(.caption)
        }
        .padding(.vertical, Metrics.xs)
        .contextMenu {
            Button("Show in Diff", action: show)
            Button("Edit…", action: edit)
            if comment.stale { Button("Re-anchor…", action: reanchor) }
            Button("Delete", role: .destructive, action: delete)
        }
    }
}

private struct ReviewSendPreview: View {
    @Environment(\.dismiss) private var dismiss
    let attempt: ReviewSendAttempt
    let sessionTitle: String
    let pending: Bool
    let confirm: () async throws -> Void

    @State private var sending = false
    @State private var error: String?
    @State private var showsPayload = false

    var body: some View {
        NavigationStack {
            Form {
                Section {
                    LabeledContent("Recipient") { AgentLabel(name: attempt.expectedAgent) }
                    LabeledContent("Session", value: sessionTitle.nonEmpty ?? attempt.focusedSession.sessionID)
                    LabeledContent("Workspace", value: attempt.scope.workspaceLabel)
                    LabeledContent("Comments", value: "\(attempt.comments.count)")
                } footer: {
                    Text(pending
                        ? "This retries the earlier send with the same request and recipient, so it can't be delivered twice."
                        : "Sent as one ordinary message to the session's current agent. Comments were rechecked against a fresh scan; files can still change before the agent reads them.")
                }
                Section {
                    DisclosureGroup("Message", isExpanded: $showsPayload) {
                        Text(attempt.payload)
                            .font(.monoSmall)
                            .textSelection(.enabled)
                            .lineLimit(200)
                    }
                }
                if let error {
                    Label(error, systemImage: "exclamationmark.triangle.fill").foregroundStyle(.red)
                }
            }
            .formStyle(.grouped)
            .navigationTitle("Send Review")
            #if os(iOS)
            .navigationBarTitleDisplayMode(.inline)
            #endif
            .toolbar {
                ToolbarItem(placement: .cancellationAction) { Button("Cancel") { dismiss() } }
                ToolbarItem(placement: .confirmationAction) {
                    Button("Send to \(attempt.expectedAgent)") { Task { await send() } }
                        .disabled(sending)
                }
            }
        }
        .frame(minWidth: 440, minHeight: 360)
    }

    private func send() async {
        sending = true
        defer { sending = false }
        do {
            try await confirm()
            dismiss()
        } catch {
            self.error = error.localizedDescription
        }
    }
}
