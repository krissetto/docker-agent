import SwiftUI
import AgentKit

struct TranscriptView: View {
    let sessionRef: SessionRef
    let items: [TranscriptItem]
    var canRetry: Bool
    var onRetry: () -> Void
    var resolveMedia: (GeneratedMediaReference) async throws -> Data
    var matchIDs: Set<String> = []
    var currentMatchID: String?
    var onContinueFromHere: ((TranscriptItem) -> Void)?
    var onBranchHere: ((TranscriptItem) -> Void)?

    @State private var position = ScrollPosition(edge: .bottom)
    @State private var atBottom = true

    private struct Row: Identifiable {
        let item: TranscriptItem
        let showsHeader: Bool
        var id: String { item.id }
    }

    private var rows: [Row] {
        var result: [Row] = []
        var lastOwner: String?
        for item in items where item.origin != "runtime" {
            let isUser = item.kind == .message && item.role == "user"
            let owner = isUser ? nil : item.owner
            let showsHeader = !isUser && item.kind != .delegation && owner != nil && owner != lastOwner
            result.append(Row(item: item, showsHeader: showsHeader))
            if isUser || item.kind == .delegation { lastOwner = nil } else if let owner { lastOwner = owner }
        }
        return result
    }

    var body: some View {
        let rows = rows
        let lastErrorID = items.last(where: { $0.kind == .error })?.id
        ScrollView {
            LazyVStack(alignment: .leading, spacing: Metrics.m) {
                ForEach(rows) { row in
                    VStack(alignment: .leading, spacing: Metrics.s) {
                        if row.showsHeader, let owner = row.item.owner {
                            AgentLabel(name: owner)
                                .padding(.top, Metrics.s)
                                .accessibilityAddTraits(.isHeader)
                        }
                        itemView(row.item, retry: canRetry && row.item.id == lastErrorID)
                            .contextMenu { branchMenu(row.item) }
                            .padding(matchIDs.contains(row.id) ? Metrics.xs : 0)
                            .background {
                                if matchIDs.contains(row.id) {
                                    RoundedRectangle(cornerRadius: Metrics.chipRadius, style: .continuous)
                                        .fill(Color.yellow.opacity(row.id == currentMatchID ? 0.3 : 0.12))
                                }
                            }
                    }
                }
            }
            .scrollTargetLayout()
            .padding(.horizontal, Metrics.l)
            .padding(.vertical, Metrics.l)
            .frame(maxWidth: Metrics.readingWidth)
            .frame(maxWidth: .infinity)
        }
        .scrollPosition($position)
        .defaultScrollAnchor(.bottom)
        .onScrollGeometryChange(for: Bool.self) { geometry in
            geometry.contentOffset.y + geometry.containerSize.height >= geometry.contentSize.height - 40
        } action: { _, isAtBottom in
            atBottom = isAtBottom
        }
        .onChange(of: items.last) {
            if atBottom && currentMatchID == nil { position.scrollTo(edge: .bottom) }
        }
        .onChange(of: currentMatchID) { _, id in
            guard let id else { return }
            withAnimation(.snappy) { position.scrollTo(id: id, anchor: .center) }
        }
        .overlay(alignment: .bottom) {
            if !atBottom {
                Button {
                    withAnimation(.snappy) { position.scrollTo(edge: .bottom) }
                } label: {
                    Label("Jump to Latest", systemImage: "arrow.down")
                        .font(.callout.weight(.medium))
                }
                .buttonStyle(.glass)
                .padding(.bottom, Metrics.m)
                .transition(.move(edge: .bottom).combined(with: .opacity))
            }
        }
        .animation(.snappy, value: atBottom)
    }

    @ViewBuilder
    private func branchMenu(_ item: TranscriptItem) -> some View {
        if !item.text.isEmpty {
            Button("Copy", systemImage: "doc.on.doc") { Clipboard.copy(item.text) }
        }
        if let onContinueFromHere, item.role == "user", item.userMessageIndex != nil {
            Button("Continue from Here in New Session…", systemImage: "arrow.uturn.backward") { onContinueFromHere(item) }
        }
        if let onBranchHere, item.branchAnchor != nil {
            Button("New Session Up to Here…", systemImage: "arrow.triangle.branch") { onBranchHere(item) }
        }
    }

    @ViewBuilder
    private func itemView(_ item: TranscriptItem, retry: Bool) -> some View {
        switch item.kind {
        case .message where item.role == "user":
            UserTurnCard(item: item)
        case .message:
            VStack(alignment: .leading, spacing: Metrics.s) {
                if !item.text.isEmpty || item.isStreaming {
                    AssistantTextView(text: item.text, isStreaming: item.isStreaming)
                }
                ForEach(item.generatedMedia) { reference in
                    // Owner/root/path can repeat across servers, so identity includes the full session ref.
                    let identity = "\(sessionRef.serverID.uuidString)|\(sessionRef.sessionID)|\(reference.id)"
                    GeneratedMediaTile(identity: identity, name: reference.name, mimeType: reference.mimeType) {
                        try await resolveMedia(reference)
                    }
                    .id(identity)
                }
            }
        case .tool:
            ToolRow(item: item)
        case .reasoning:
            ReasoningRow(item: item)
        case .delegation:
            DelegationDivider(item: item)
        case .error:
            ErrorRow(item: item, onRetry: retry ? onRetry : nil)
        case .activity:
            NoticeRow(item: item)
        }
    }
}
