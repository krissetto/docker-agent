import SwiftUI
import AgentKit

/// Registers an agent configuration that lives in the server's workspace; nothing is uploaded from this device.
struct RegisterSourceSheet: View {
    @Environment(AppModel.self) private var model
    @Environment(\.dismiss) private var dismiss
    let serverID: UUID
    let onRegistered: (String) -> Void

    @State private var path = ""
    @State private var workingDir = ""
    @State private var working = false
    @State private var error: String?

    private var trimmedPath: String { path.trimmingCharacters(in: .whitespacesAndNewlines) }

    var body: some View {
        NavigationStack {
            Form {
                Section {
                    TextField("Configuration path on server", text: $path, prompt: Text("agents/coder.yaml"))
                        .font(.mono)
                        .autocorrectionDisabled()
                        #if os(iOS)
                        .textInputAutocapitalization(.never)
                        #endif
                    TextField("Working directory on server", text: $workingDir, prompt: Text("Optional"))
                        .font(.mono)
                        .autocorrectionDisabled()
                        #if os(iOS)
                        .textInputAutocapitalization(.never)
                        #endif
                } footer: {
                    Text("Paths are relative to the server's workspace. Files on this device can't be registered.")
                }
                if let error {
                    Label(error, systemImage: "exclamationmark.triangle.fill").foregroundStyle(.red)
                }
            }
            .formStyle(.grouped)
            .navigationTitle("Register Source")
            #if os(iOS)
            .navigationBarTitleDisplayMode(.inline)
            #endif
            .toolbar {
                ToolbarItem(placement: .cancellationAction) { Button("Cancel") { dismiss() } }
                ToolbarItem(placement: .confirmationAction) {
                    Button("Register") { Task { await register() } }
                        .disabled(trimmedPath.isEmpty || trimmedPath.hasPrefix("/") || working)
                }
            }
        }
        .frame(minWidth: 420, minHeight: 280)
    }

    private func register() async {
        working = true
        error = nil
        defer { working = false }
        let directory = workingDir.trimmingCharacters(in: .whitespacesAndNewlines)
        do {
            let result = try await model.registerSource(serverID: serverID, path: trimmedPath, workingDir: directory.isEmpty ? nil : directory)
            onRegistered(result.sourceID)
            dismiss()
        } catch let api as APIError where api.statusCode == 404 {
            error = "This server doesn't support registering sources."
        } catch {
            self.error = error.localizedDescription
        }
    }
}
