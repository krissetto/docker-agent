import SwiftUI
import AgentKit

struct ReviewNavigator: View {
    let snapshot: ReviewSnapshot
    let worktrees: [String: ReviewWorktree]
    let loadingWorktrees: Set<String>
    let worktreeErrors: [String: String]
    let commentCount: (String, String) -> Int
    var tapToSelect = false
    @Binding var expanded: Set<String>
    @Binding var selection: ReviewFileSelection?
    let loadWorktree: (String) -> Void

    var body: some View {
        List(selection: $selection) {
            ForEach(snapshot.repositories) { repository in
                Section {
                    if !repository.error.isEmpty {
                        Label(repository.error, systemImage: "exclamationmark.triangle")
                            .font(.caption)
                            .foregroundStyle(.red)
                    }
                    ForEach(repository.worktrees) { worktree in
                        worktreeGroup(repository: repository, worktree: worktrees[worktree.id] ?? worktree)
                    }
                    if repository.hiddenOutside > 0 {
                        Text("\(repository.hiddenOutside) linked worktree\(repository.hiddenOutside == 1 ? "" : "s") outside this workspace not shown")
                            .font(.caption)
                            .foregroundStyle(.secondary)
                    }
                } header: {
                    HStack(spacing: Metrics.xs) {
                        Image(systemName: "shippingbox")
                        Text(repository.name)
                        if repository.kind != "primary" && !repository.kind.isEmpty && repository.kind != "none" {
                            Text(repository.kind).font(.caption2).foregroundStyle(.secondary)
                        }
                    }
                }
            }
        }
        .listStyle(.sidebar)
    }

    private func worktreeGroup(repository: ReviewRepository, worktree: ReviewWorktree) -> some View {
        DisclosureGroup(isExpanded: Binding(
            get: { expanded.contains(worktree.id) },
            set: { open in
                if open {
                    expanded.insert(worktree.id)
                    if worktree.state == "unscanned" || worktree.state.isEmpty { loadWorktree(worktree.id) }
                } else {
                    expanded.remove(worktree.id)
                }
            }
        )) {
            worktreeContent(repository: repository, worktree: worktree)
        } label: {
            WorktreeLabel(worktree: worktree, loading: loadingWorktrees.contains(worktree.id))
        }
    }

    @ViewBuilder
    private func worktreeContent(repository: ReviewRepository, worktree: ReviewWorktree) -> some View {
        if let error = worktreeErrors[worktree.id] ?? worktree.error.nonEmpty {
            Label(error, systemImage: "exclamationmark.triangle")
                .font(.caption)
                .foregroundStyle(.red)
            Button("Retry") { loadWorktree(worktree.id) }.font(.caption)
        } else if loadingWorktrees.contains(worktree.id) {
            ProgressView().controlSize(.small)
        } else if worktree.files.isEmpty {
            Text(worktree.state == "clean" ? "No changes" : "Not scanned yet")
                .font(.caption)
                .foregroundStyle(.secondary)
        } else {
            ForEach(ReviewFileGroup.allCases, id: \.self) { group in
                let files = worktree.files.filter { $0.group == group }
                if !files.isEmpty {
                    Text("\(group.title) · \(files.count)")
                        .font(.caption.weight(.semibold))
                        .foregroundStyle(group.tint)
                        .selectionDisabled()
                    ForEach(files) { file in
                        fileRow(file, repository: repository, worktree: worktree)
                    }
                }
            }
        }
    }
}

extension ReviewNavigator {
    @ViewBuilder
    fileprivate func fileRow(_ file: ReviewFileChange, repository: ReviewRepository, worktree: ReviewWorktree) -> some View {
        let value = ReviewFileSelection(repositoryID: repository.id, worktreeID: worktree.id, file: file)
        let row = FileChangeRow(file: file, commentCount: commentCount(worktree.id, file.path))
        if tapToSelect {
            Button { selection = value } label: { row.contentShape(.rect) }
                .buttonStyle(.plain)
        } else {
            row.tag(value)
        }
    }
}

private struct WorktreeLabel: View {
    let worktree: ReviewWorktree
    let loading: Bool

    var body: some View {
        HStack(spacing: Metrics.xs) {
            Image(systemName: worktree.detached ? "point.topleft.down.to.point.bottomright.curvepath" : "arrow.triangle.branch")
                .foregroundStyle(.secondary)
            Text(worktree.detached ? (worktree.detachedHead.nonEmpty.map { String($0.prefix(8)) } ?? "detached") : (worktree.branch.nonEmpty ?? "worktree"))
                .lineLimit(1)
            if worktree.current {
                Text("current")
                    .font(.caption2.weight(.medium))
                    .padding(.horizontal, Metrics.xs)
                    .background(.fill.tertiary, in: .capsule)
            }
            if worktree.linked {
                Image(systemName: "link").font(.caption2).foregroundStyle(.secondary).accessibilityLabel("Linked worktree")
            }
            Spacer()
            if loading {
                ProgressView().controlSize(.mini)
            } else if !worktree.files.isEmpty {
                Text("\(worktree.files.count)").font(.caption).foregroundStyle(.secondary)
            }
        }
        .help(worktree.path)
    }
}

struct FileChangeRow: View {
    let file: ReviewFileChange
    var commentCount = 0

    var body: some View {
        HStack(spacing: Metrics.s) {
            Text(file.statusLetter)
                .font(.caption2.monospaced().weight(.bold))
                .foregroundStyle(file.group.tint)
                .frame(width: 18)
                .accessibilityLabel(file.status.isEmpty ? file.group.title : "Status \(file.status)")
            VStack(alignment: .leading, spacing: 1) {
                Text(file.fileName).lineLimit(1)
                if file.isRename {
                    Text("from \(file.oldPath)")
                        .font(.caption2)
                        .foregroundStyle(.secondary)
                        .lineLimit(1)
                        .truncationMode(.middle)
                } else if !file.directory.isEmpty {
                    Text(file.directory)
                        .font(.caption2)
                        .foregroundStyle(.secondary)
                        .lineLimit(1)
                        .truncationMode(.head)
                }
            }
            Spacer(minLength: Metrics.xs)
            if commentCount > 0 {
                Label("\(commentCount)", systemImage: "text.bubble")
                    .font(.caption2)
                    .foregroundStyle(.tint)
                    .accessibilityLabel("\(commentCount) draft comments")
            }
            if file.binary {
                Text("bin").font(.caption2).foregroundStyle(.secondary)
            } else {
                DiffStat(added: file.added, removed: file.removed)
            }
        }
        .help(file.path)
    }
}

struct DiffStat: View {
    let added: Int
    let removed: Int

    var body: some View {
        // The server reports -1 when a count is unavailable.
        HStack(spacing: 3) {
            if added >= 0 { Text("+\(added)").foregroundStyle(.green) }
            if removed >= 0 { Text("−\(removed)").foregroundStyle(.red) }
        }
        .font(.caption2.monospacedDigit())
    }
}
