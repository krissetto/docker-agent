import SwiftUI
import AgentKit

/// Read-only workspace browser, separate from Git review.
struct FilesBrowserView: View {
    @Environment(AppModel.self) private var model
    @Environment(\.horizontalSizeClass) private var sizeClass
    let session: OpenSessionProjection

    @State private var worktreeID: String?
    @State private var path: [String] = []
    @State private var directories: [String: ReviewDirectory] = [:]
    @State private var loadingDirectory = false
    @State private var directoryError: String?
    @State private var selectedFile: FileItem?
    @State private var document: ReviewDocument?
    @State private var documentError: String?
    @State private var scanning = false
    @State private var scanError: String?

    struct FileItem: Hashable, Identifiable {
        let worktreeID: String
        let path: String
        var id: String { worktreeID + "|" + path }
        var name: String { (path as NSString).lastPathComponent }
    }

    private var ref: SessionRef { session.ref }
    private var snapshot: ReviewSnapshot? { model.reviewSnapshots[ref] }
    private var allWorktrees: [(repository: ReviewRepository, worktree: ReviewWorktree)] {
        (snapshot?.repositories ?? []).flatMap { repo in repo.worktrees.map { (repo, $0) } }
    }
    private var currentPath: String { path.last ?? "" }
    private var directoryKey: String { "\(worktreeID ?? "")|\(currentPath)" }

    var body: some View {
        Group {
            if sizeClass == .compact {
                browser
                    .navigationDestination(item: $selectedFile) { file in
                        FileDocumentView(file: file, document: document, error: documentError)
                            .navigationTitle(file.name)
                    }
            } else {
                HStack(spacing: 0) {
                    browser.frame(width: 300)
                    Divider()
                    if let selectedFile {
                        FileDocumentView(file: selectedFile, document: document, error: documentError)
                    } else {
                        ContentUnavailableView("Select a File", systemImage: "doc.text", description: Text("Browse the session's workspace. Files are read-only here."))
                            .frame(maxWidth: .infinity, maxHeight: .infinity)
                    }
                }
            }
        }
        .task(id: ref) { await prepare() }
        .task(id: directoryKey) { await loadDirectory() }
        .task(id: selectedFile) { await loadDocument() }
    }

    @ViewBuilder
    private var browser: some View {
        if snapshot == nil {
            if let scanError {
                ContentUnavailableView {
                    Label("Couldn't Read Workspace", systemImage: "exclamationmark.triangle")
                } description: {
                    Text(scanError)
                } actions: {
                    Button("Try Again") { Task { await prepare(force: true) } }
                }
            } else {
                ProgressView("Reading workspace…").frame(maxWidth: .infinity, maxHeight: .infinity)
            }
        } else if let unavailable = snapshot?.scope.unavailable, !unavailable.isEmpty {
            ContentUnavailableView("Files Unavailable", systemImage: "eye.slash", description: Text(unavailable))
        } else {
            VStack(spacing: 0) {
                header
                Divider()
                list
            }
        }
    }

    private var header: some View {
        VStack(alignment: .leading, spacing: Metrics.s) {
            if allWorktrees.count > 1 {
                Picker("Worktree", selection: $worktreeID) {
                    ForEach(allWorktrees, id: \.worktree.id) { item in
                        Text("\(item.repository.name) · \(item.worktree.branch.nonEmpty ?? item.worktree.path)").tag(Optional(item.worktree.id))
                    }
                }
                .labelsHidden()
                .onChange(of: worktreeID) { path = [] }
            }
            HStack(spacing: Metrics.xs) {
                Button {
                    if !path.isEmpty { path.removeLast() }
                } label: {
                    Image(systemName: "chevron.left")
                }
                .buttonStyle(.borderless)
                .disabled(path.isEmpty)
                .keyboardShortcut("[", modifiers: .command)
                .accessibilityLabel("Parent folder")
                Text(currentPath.isEmpty ? "/" : currentPath)
                    .font(.callout.monospaced())
                    .lineLimit(1)
                    .truncationMode(.head)
                Spacer()
                Text("Read-only")
                    .font(.caption2.weight(.medium))
                    .foregroundStyle(.secondary)
                    .padding(.horizontal, Metrics.s)
                    .padding(.vertical, Metrics.xxs)
                    .background(.fill.tertiary, in: .capsule)
            }
        }
        .padding(.horizontal, Metrics.l)
        .padding(.vertical, Metrics.s)
    }

    @ViewBuilder
    private var list: some View {
        let directory = directories[directoryKey]
        if let directoryError {
            ContentUnavailableView {
                Label("Couldn't Open Folder", systemImage: "exclamationmark.triangle")
            } description: {
                Text(directoryError)
            } actions: {
                Button("Try Again") { Task { await loadDirectory(force: true) } }
            }
        } else if let directory {
            List {
                ForEach(sorted(directory.entries)) { entry in
                    let fullPath = currentPath.isEmpty ? entry.name : currentPath + "/" + entry.name
                    Button {
                        if entry.isDirectory {
                            path.append(fullPath)
                        } else if let worktreeID {
                            selectedFile = FileItem(worktreeID: worktreeID, path: fullPath)
                        }
                    } label: {
                        Label(entry.name, systemImage: entry.isDirectory ? "folder" : "doc")
                            .foregroundStyle(entry.isDirectory ? AnyShapeStyle(.primary) : AnyShapeStyle(.primary))
                            .frame(maxWidth: .infinity, alignment: .leading)
                            .contentShape(.rect)
                    }
                    .buttonStyle(.plain)
                    .listRowBackground(selectedFile?.path == fullPath ? Color.accentColor.opacity(0.15) : nil)
                }
                if directory.truncated {
                    Label("Only the first entries are shown; the server limits folder listings.", systemImage: "scissors")
                        .font(.caption)
                        .foregroundStyle(.secondary)
                }
                if directory.entries.isEmpty {
                    Text("Empty folder").foregroundStyle(.secondary)
                }
            }
            .listStyle(.plain)
        } else {
            ProgressView().frame(maxWidth: .infinity, maxHeight: .infinity)
        }
    }

    private func sorted(_ entries: [ReviewDirectoryEntry]) -> [ReviewDirectoryEntry] {
        entries.sorted { a, b in
            a.isDirectory != b.isDirectory ? a.isDirectory : a.name.localizedStandardCompare(b.name) == .orderedAscending
        }
    }

    private func prepare(force: Bool = false) async {
        scanError = nil
        if snapshot == nil || force {
            scanning = true
            defer { scanning = false }
            do { _ = try await model.reviewSnapshot(ref, refresh: true) } catch { scanError = error.localizedDescription; return }
        }
        if worktreeID == nil {
            worktreeID = (allWorktrees.first { $0.worktree.current } ?? allWorktrees.first)?.worktree.id
        }
    }

    private func loadDirectory(force: Bool = false) async {
        guard let worktreeID, let generation = snapshot?.generation else { return }
        if directories[directoryKey] != nil && !force { return }
        directoryError = nil
        loadingDirectory = true
        defer { loadingDirectory = false }
        do {
            directories[directoryKey] = try await model.reviewDirectory(ref, generation: generation, worktreeID: worktreeID, path: currentPath)
        } catch is CancellationError {
        } catch let error as APIError where error.statusCode == 409 {
            directories = [:]
            await prepare(force: true)
            if let generation = snapshot?.generation {
                directories[directoryKey] = try? await model.reviewDirectory(ref, generation: generation, worktreeID: worktreeID, path: currentPath)
            }
        } catch {
            directoryError = error.localizedDescription
        }
    }

    private func loadDocument() async {
        guard let selectedFile, let generation = snapshot?.generation else { return }
        document = nil
        documentError = nil
        do {
            document = try await model.reviewDocument(ref, generation: generation, key: ReviewDocumentKey(mode: .files, worktreeID: selectedFile.worktreeID, path: selectedFile.path))
        } catch is CancellationError {
        } catch {
            documentError = error.localizedDescription
        }
    }
}

private struct FileDocumentView: View {
    let file: FilesBrowserView.FileItem
    let document: ReviewDocument?
    let error: String?

    var body: some View {
        VStack(spacing: 0) {
            HStack {
                Text(file.path)
                    .font(.callout.monospaced().weight(.medium))
                    .lineLimit(1)
                    .truncationMode(.head)
                    .textSelection(.enabled)
                Spacer()
                if let document, !document.binary {
                    CopyButton(text: document.lines.map(\.text).joined(separator: "\n"))
                }
            }
            .padding(.horizontal, Metrics.l)
            .padding(.vertical, Metrics.s)
            Divider()
            content.frame(maxWidth: .infinity, maxHeight: .infinity)
        }
    }

    @ViewBuilder
    private var content: some View {
        if let error {
            ContentUnavailableView("Couldn't Open File", systemImage: "exclamationmark.triangle", description: Text(error))
        } else if let document {
            if document.state == .error {
                ContentUnavailableView("File Unavailable", systemImage: "exclamationmark.triangle", description: Text(document.error.nonEmpty ?? "The server couldn't read this file."))
            } else if document.binary {
                ContentUnavailableView("Binary File", systemImage: "doc.zipper", description: Text("Binary content has no text preview."))
            } else {
                VStack(spacing: 0) {
                    if document.truncated {
                        Banner(symbol: "scissors", text: "Truncated at the server's size limit.", tint: .orange)
                    }
                    ScrollView {
                        LazyVStack(alignment: .leading, spacing: 0) {
                            ForEach(Array(document.lines.enumerated()), id: \.offset) { index, line in
                                HStack(alignment: .firstTextBaseline, spacing: Metrics.s) {
                                    Text(line.newLine > 0 ? String(line.newLine) : String(index + 1))
                                        .font(.monoSmall)
                                        .foregroundStyle(.tertiary)
                                        .frame(width: 44, alignment: .trailing)
                                    Text(line.text.isEmpty ? " " : line.text)
                                        .font(.mono)
                                        .foregroundStyle(line.kind == .note ? AnyShapeStyle(.secondary) : AnyShapeStyle(.primary))
                                    Spacer(minLength: 0)
                                }
                            }
                        }
                        .textSelection(.enabled)
                        .padding(.vertical, Metrics.s)
                    }
                }
            }
        } else {
            ProgressView()
        }
    }
}
