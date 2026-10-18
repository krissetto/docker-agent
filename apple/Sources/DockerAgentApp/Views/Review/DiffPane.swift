import SwiftUI
import AgentKit

struct DiffPane: View {
    let selection: ReviewFileSelection
    let document: ReviewDocument?
    let generation: UInt64
    let loading: Bool
    let error: String?
    let comments: [ReviewComment]
    let canComment: Bool
    let commentBlockedReason: String?
    let reanchoring: ReviewComment?
    let forceUnified: Bool
    let onRefresh: () -> Void
    let onAdd: (ReviewAnchor, String) throws -> Void
    let onEdit: (ReviewComment, String) throws -> Void
    let onReanchor: (ReviewComment, ReviewAnchor) -> Void
    let onCancelReanchor: () -> Void

    @AppStorage("review.diffLayout") private var storedLayout: DiffLayout = .unified
    @State private var lineSelection: ClosedRange<Int>?
    @State private var composing = false
    @State private var editing: ReviewComment?
    @State private var query = ""
    @State private var matchIndex = 0
    @State private var paneWidth: CGFloat = 1000
    @State private var extendsByTap = false
    @FocusState private var searchFocused: Bool

    private static let splitMinimumWidth: CGFloat = 760
    private var splitFits: Bool { !forceUnified && paneWidth >= Self.splitMinimumWidth }
    private var layout: DiffLayout { splitFits ? storedLayout : .unified }
    private var file: ReviewFileChange { selection.file }

    private var fingerprint: String? { document.map(reviewDocumentFingerprint) }

    private var currentComments: [ReviewComment] {
        comments.filter { !$0.stale && $0.anchor.documentFingerprint == fingerprint }
    }

    private var earlierComments: Int { comments.count - currentComments.count }

    private var inlineComments: [Int: [ReviewComment]] {
        guard let lines = document?.lines else { return [:] }
        var result: [Int: [ReviewComment]] = [:]
        for comment in currentComments {
            if let row = AnchorBuilder.row(for: comment.anchor, in: lines) { result[row, default: []].append(comment) }
        }
        return result
    }

    private var matches: [Int] {
        guard !query.isEmpty, let lines = document?.lines else { return [] }
        return lines.indices.filter { lines[$0].text.localizedCaseInsensitiveContains(query) }
    }

    var body: some View {
        VStack(spacing: 0) {
            header
            Divider()
            content
                .frame(maxWidth: .infinity, maxHeight: .infinity)
        }
        .overlay(alignment: .bottom) { selectionBar }
        .onGeometryChange(for: CGFloat.self) { $0.size.width } action: { paneWidth = $0 }
        .onChange(of: document) {
            lineSelection = nil
            extendsByTap = false
            matchIndex = 0
        }
        .onChange(of: lineSelection) { _, range in
            if range == nil { extendsByTap = false }
        }
        .sheet(item: $editing) { comment in
            CommentComposer(
                title: "Edit Comment",
                path: comment.anchor.path,
                anchorLabel: AnchorBuilder.label(comment.anchor),
                context: comment.anchor.context,
                initialBody: comment.body,
                saveLabel: "Save"
            ) { body in
                try onEdit(comment, body)
                editing = nil
            } onCancel: {
                editing = nil
            }
        }
    }

    // MARK: Header

    private var header: some View {
        HStack(spacing: Metrics.m) {
            VStack(alignment: .leading, spacing: 1) {
                Text(file.path)
                    .font(.callout.monospaced().weight(.medium))
                    .lineLimit(1)
                    .truncationMode(.head)
                    .textSelection(.enabled)
                HStack(spacing: Metrics.xs) {
                    Text(file.group.title).foregroundStyle(file.group.tint)
                    if !file.status.isEmpty { Text("· \(file.status)") }
                    if file.isRename { Text("· renamed from \(file.oldPath)").lineLimit(1).truncationMode(.middle) }
                    if !file.binary { DiffStat(added: file.added, removed: file.removed) }
                }
                .font(.caption)
                .foregroundStyle(.secondary)
            }
            Spacer(minLength: Metrics.s)
            searchField
            if !forceUnified {
                Picker("Layout", selection: $storedLayout) {
                    ForEach(DiffLayout.allCases) { Text($0.rawValue).tag($0) }
                }
                .pickerStyle(.segmented)
                .labelsHidden()
                .fixedSize()
                .disabled(!splitFits)
                .help(splitFits ? "Unified or side-by-side diff" : "Widen the diff (hide files or conversation) for side-by-side")
            }
        }
        .padding(.horizontal, Metrics.l)
        .padding(.vertical, Metrics.s)
    }

    private var searchField: some View {
        HStack(spacing: Metrics.xs) {
            Image(systemName: "magnifyingglass").foregroundStyle(.secondary)
            TextField("Find in file", text: $query)
                .textFieldStyle(.plain)
                .focused($searchFocused)
                .frame(minWidth: 80, maxWidth: 160)
                .onSubmit { step(1) }
                .onChange(of: query) { matchIndex = 0 }
            if !query.isEmpty {
                Text(matches.isEmpty ? "0" : "\(matchIndex + 1)/\(matches.count)")
                    .font(.caption.monospacedDigit())
                    .foregroundStyle(.secondary)
                Button { step(-1) } label: { Image(systemName: "chevron.up") }
                    .keyboardShortcut("g", modifiers: [.command, .shift])
                    .accessibilityLabel("Previous match")
                Button { step(1) } label: { Image(systemName: "chevron.down") }
                    .keyboardShortcut("g", modifiers: .command)
                    .accessibilityLabel("Next match")
            }
        }
        .buttonStyle(.borderless)
        .font(.callout)
        .padding(.horizontal, Metrics.s)
        .padding(.vertical, Metrics.xs)
        .background(.fill.tertiary, in: .capsule)
        .background {
            Button("Find") { searchFocused = true }
                .keyboardShortcut("f", modifiers: .command)
                .hidden()
        }
    }

    private func step(_ delta: Int) {
        guard !matches.isEmpty else { return }
        matchIndex = (matchIndex + delta + matches.count) % matches.count
    }

    // MARK: Content

    @ViewBuilder
    private var content: some View {
        if loading && document == nil {
            ProgressView()
        } else if let error {
            ContentUnavailableView {
                Label("Couldn't Load Diff", systemImage: "exclamationmark.triangle")
            } description: {
                Text(error)
            } actions: {
                Button("Refresh", action: onRefresh)
            }
        } else if let document {
            documentContent(document)
        } else {
            ProgressView()
        }
    }

    @ViewBuilder
    private func documentContent(_ document: ReviewDocument) -> some View {
        let hasDiffLines = document.lines.contains { $0.kind != .note }
        if document.state == .error {
            ContentUnavailableView("Diff Unavailable", systemImage: "exclamationmark.triangle", description: Text(document.error.nonEmpty ?? "The server couldn't read this file."))
        } else if document.binary || file.binary {
            ContentUnavailableView("Binary File", systemImage: "doc.zipper", description: Text("Binary content has no text preview."))
        } else if file.group == .conflicts && !hasDiffLines {
            ContentUnavailableView {
                Label("Combined Conflict Diff Unavailable", systemImage: "arrow.triangle.merge")
            } description: {
                Text("This file has unresolved conflicts\(file.status.isEmpty ? "" : " (\(file.status))"). The server can't render combined conflict diffs yet.")
            }
        } else if !hasDiffLines {
            ContentUnavailableView("No Textual Changes", systemImage: "equal.circle", description: Text(document.note.nonEmpty ?? "This change has no line differences to show."))
        } else {
            VStack(spacing: 0) {
                banners(document)
                DiffLinesView(
                    lines: document.lines,
                    layout: layout,
                    selection: $lineSelection,
                    extendsByTap: extendsByTap,
                    inlineComments: inlineComments,
                    matches: matches,
                    currentMatch: matches.isEmpty ? nil : matches[min(matchIndex, matches.count - 1)],
                    onComment: { if canComment { composing = true } },
                    onEditComment: { editing = $0 }
                )
            }
        }
    }

    @ViewBuilder
    private func banners(_ document: ReviewDocument) -> some View {
        VStack(spacing: 0) {
            if document.truncated {
                Banner(symbol: "scissors", text: "Truncated at the server's size limit. Lines past this point aren't shown.", tint: .orange)
            }
            if !document.note.isEmpty {
                Banner(symbol: "info.circle", text: document.note, tint: .secondary)
            }
            if earlierComments > 0 {
                Banner(symbol: "clock.arrow.circlepath", text: "\(earlierComments) draft comment\(earlierComments == 1 ? " was" : "s were") written on an earlier version of this file. Review them in the draft.", tint: .orange)
            }
            if let reanchoring {
                HStack {
                    Banner(symbol: "scope", text: "Select the lines for “\(reanchoring.body.prefix(40))”, then choose Re-anchor Here.", tint: .accentColor)
                    Button("Cancel", action: onCancelReanchor).buttonStyle(.borderless).padding(.trailing, Metrics.l)
                }
            }
        }
    }

    // MARK: Selection bar

    @ViewBuilder
    private var selectionBar: some View {
        if let range = lineSelection, let document, let anchor = AnchorBuilder.anchor(range: range, document: document, file: file, generation: generation) {
            HStack(spacing: Metrics.m) {
                Text(AnchorBuilder.label(anchor)).font(.callout.weight(.medium))
                if let reason = commentBlockedReason {
                    Text(reason).font(.caption).foregroundStyle(.secondary).lineLimit(2)
                }
                Spacer(minLength: Metrics.s)
                #if os(iOS)
                Toggle(isOn: $extendsByTap) {
                    Label("Extend", systemImage: "arrow.up.and.down.text.horizontal")
                }
                .toggleStyle(.button)
                .help("Tap another line to extend the selection")
                #endif
                Button("Clear") { lineSelection = nil }
                    .buttonStyle(.borderless)
                if let reanchoring {
                    Button("Re-anchor Here") {
                        onReanchor(reanchoring, anchor)
                        lineSelection = nil
                    }
                    .buttonStyle(.borderedProminent)
                } else {
                    Button {
                        composing = true
                    } label: {
                        Label("Comment", systemImage: "text.bubble")
                    }
                    .buttonStyle(.borderedProminent)
                    .disabled(!canComment)
                    .popover(isPresented: $composing, arrowEdge: .top) {
                        CommentComposer(title: "Comment", path: anchor.path, anchorLabel: AnchorBuilder.label(anchor), context: anchor.context) { body in
                            try onAdd(anchor, body)
                            composing = false
                            lineSelection = nil
                        } onCancel: {
                            composing = false
                        }
                    }
                }
            }
            .padding(.horizontal, Metrics.l)
            .padding(.vertical, Metrics.s)
            .glassEffect(.regular, in: .capsule)
            .padding(Metrics.m)
            .frame(maxWidth: 640)
            .transition(.move(edge: .bottom).combined(with: .opacity))
        } else if let range = lineSelection, let lines = document?.lines, let problem = AnchorBuilder.validate(range, in: lines) {
            HStack {
                Label(problem.message, systemImage: "exclamationmark.circle")
                    .font(.callout)
                Spacer()
                Button("Clear") { lineSelection = nil }.buttonStyle(.borderless)
            }
            .padding(.horizontal, Metrics.l)
            .padding(.vertical, Metrics.s)
            .glassEffect(.regular, in: .capsule)
            .padding(Metrics.m)
            .frame(maxWidth: 640)
        }
    }
}

struct Banner: View {
    let symbol: String
    let text: String
    let tint: Color

    var body: some View {
        Label(text, systemImage: symbol)
            .font(.caption)
            .foregroundStyle(tint == .secondary ? AnyShapeStyle(.secondary) : AnyShapeStyle(tint))
            .frame(maxWidth: .infinity, alignment: .leading)
            .padding(.horizontal, Metrics.l)
            .padding(.vertical, Metrics.s)
            .background(tint.opacity(0.08))
    }
}
