import SwiftUI
import AgentKit

/// Restores workspace files to a checkpoint. The server can't preview affected paths.
struct WorkspaceRestoreSheet: View {
    @Environment(\.dismiss) private var dismiss
    let sessionTitle: String
    let workspaceLabel: String
    let isIdle: Bool
    let load: () async throws -> SessionWorkspaceSnapshots
    let undo: (_ proof: String) async throws -> Int
    let reset: (_ keep: Int, _ proof: String) async throws -> Int

    @State private var snapshots: SessionWorkspaceSnapshots?
    @State private var selection: Int?
    @State private var confirming: Action?
    @State private var working = false
    @State private var result: String?
    @State private var error: String?

    private enum Action: Identifiable {
        case undo
        case reset(keep: Int)
        var id: String { if case .reset(let keep) = self { "reset\(keep)" } else { "undo" } }
    }

    var body: some View {
        NavigationStack {
            Form {
                if let snapshots {
                    if !snapshots.enabled {
                        Text("Checkpoints aren't enabled for this session.").foregroundStyle(.secondary)
                    } else {
                        Section {
                            checkpointRow(keep: 0, title: "Original workspace", detail: "Before any agent changes")
                            ForEach(Array(snapshots.files.enumerated()), id: \.offset) { index, count in
                                checkpointRow(keep: index + 1, title: "Checkpoint \(index + 1)", detail: "\(count) file\(count == 1 ? "" : "s") changed")
                            }
                        } header: {
                            Text("Restore to")
                        } footer: {
                            Text("Restoring overwrites workspace files changed after the selected point. Affected files can't be previewed.")
                        }
                    }
                } else if error == nil {
                    ProgressView()
                }
                if !isIdle {
                    Label("Wait for the session to finish before restoring.", systemImage: "hourglass").foregroundStyle(.orange)
                }
                if let result {
                    Label(result, systemImage: "checkmark.circle").foregroundStyle(.green)
                }
                if let error {
                    Label(error, systemImage: "exclamationmark.triangle.fill").foregroundStyle(.red)
                }
            }
            .formStyle(.grouped)
            .navigationTitle("Restore Workspace")
            #if os(iOS)
            .navigationBarTitleDisplayMode(.inline)
            #endif
            .toolbar {
                ToolbarItem(placement: .cancellationAction) { Button("Close") { dismiss() } }
                ToolbarItemGroup(placement: .confirmationAction) {
                    Button("Undo Last") { confirming = .undo }
                        .disabled(!canAct || (snapshots?.files.isEmpty ?? true))
                    Button("Restore…") { if let selection { confirming = .reset(keep: selection) } }
                        .disabled(!canAct || selection == nil)
                }
            }
            .confirmationDialog(confirmTitle, isPresented: Binding(get: { confirming != nil }, set: { if !$0 { confirming = nil } }), titleVisibility: .visible, presenting: confirming) { action in
                Button("Restore Files", role: .destructive) { Task { await perform(action) } }
            } message: { _ in
                Text("\(sessionTitle) · \(workspaceLabel)\nFiles changed after this point are overwritten. This can't be previewed or undone from here.")
            }
        }
        .frame(minWidth: 420, minHeight: 380)
        .task { await reload() }
    }

    private var canAct: Bool { isIdle && !working && snapshots?.enabled == true }

    private var confirmTitle: String {
        switch confirming {
        case .undo: "Undo the last checkpoint?"
        case .reset(let keep): keep == 0 ? "Restore the original workspace?" : "Restore to checkpoint \(keep)?"
        case nil: ""
        }
    }

    private func checkpointRow(keep: Int, title: String, detail: String) -> some View {
        Button { selection = keep } label: {
            HStack {
                VStack(alignment: .leading, spacing: 1) {
                    Text(title)
                    Text(detail).font(.caption).foregroundStyle(.secondary)
                }
                Spacer()
                if selection == keep { Image(systemName: "checkmark").foregroundStyle(.tint) }
            }
            .contentShape(.rect)
        }
        .buttonStyle(.plain)
        .accessibilityAddTraits(selection == keep ? .isSelected : [])
    }

    private func reload() async {
        do { snapshots = try await load() } catch { self.error = error.localizedDescription }
    }

    private func perform(_ action: Action) async {
        guard let proof = snapshots?.proof else { return }
        working = true
        error = nil
        result = nil
        defer { working = false }
        do {
            let restored: Int
            switch action {
            case .undo: restored = try await undo(proof)
            case .reset(let keep): restored = try await reset(keep, proof)
            }
            result = "Restored \(restored) file\(restored == 1 ? "" : "s")."
            selection = nil
            await reload()
        } catch {
            // A stale proof means checkpoints changed; reload so the user chooses again.
            self.error = error.localizedDescription
            await reload()
        }
    }
}
