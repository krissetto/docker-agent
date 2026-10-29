import SwiftUI
import AgentKit

enum ServerEditorTarget: Identifiable, Hashable {
    case add
    case edit(UUID)

    var id: String {
        switch self {
        case .add: "add"
        case .edit(let id): id.uuidString
        }
    }
}

struct ServerEditorSheet: View {
    @Environment(AppModel.self) private var model
    @Environment(\.dismiss) private var dismiss
    let target: ServerEditorTarget
    var onSaved: ((UUID) -> Void)? = nil

    @State private var draft = ServerDraft(name: "", baseURL: "")
    @State private var tested: (draft: ServerDraft, identity: ServerIdentity)?
    @State private var testing = false
    @State private var saving = false
    @State private var error: String?
    @FocusState private var focus: Field?

    private enum Field { case name, url, token }

    private var isEditing: Bool { if case .edit = target { true } else { false } }
    private var trimmedURL: String { draft.baseURL.trimmingCharacters(in: .whitespaces) }
    private var isPlainHTTP: Bool { trimmedURL.lowercased().hasPrefix("http:") }
    private var urlProblem: String? {
        guard !trimmedURL.isEmpty else { return nil }
        do {
            _ = try URLPolicy.validate(trimmedURL, allowsLocalHTTP: draft.allowsLocalHTTP)
            return nil
        } catch {
            return error.localizedDescription
        }
    }
    private var canTest: Bool { !draft.name.trimmingCharacters(in: .whitespaces).isEmpty && !trimmedURL.isEmpty && urlProblem == nil }
    private var testedCurrentDraft: Bool { tested?.draft == draft }

    var body: some View {
        NavigationStack {
            Form {
                Section {
                    TextField("Name", text: $draft.name, prompt: Text("Workstation"))
                        .focused($focus, equals: .name)
                    TextField("URL", text: $draft.baseURL, prompt: Text("https://host:8080"))
                        .focused($focus, equals: .url)
                        .textContentType(.URL)
                        .autocorrectionDisabled()
                        #if os(iOS)
                        .keyboardType(.URL)
                        .textInputAutocapitalization(.never)
                        #endif
                    SecureField("Bearer Token", text: $draft.token, prompt: Text(isEditing ? "Leave blank to keep existing token" : "Token"))
                        .focused($focus, equals: .token)
                } footer: {
                    if let urlProblem {
                        Label(urlProblem, systemImage: "lock.trianglebadge.exclamationmark")
                            .foregroundStyle(.red)
                    } else {
                        Text("The token is stored in the Keychain on this device and grants full access to the server.")
                    }
                }

                if isPlainHTTP {
                    Section {
                        Toggle("Allow HTTP for a local development server", isOn: $draft.allowsLocalHTTP)
                    } footer: {
                        Text("Only loopback addresses (localhost, 127.0.0.1, ::1) qualify. Traffic is not encrypted.")
                    }
                }

                Section {
                    Button {
                        Task { await test() }
                    } label: {
                        HStack {
                            Text("Test Connection")
                            Spacer()
                            if testing { ProgressView().controlSize(.small) }
                        }
                    }
                    .disabled(!canTest || testing)

                    if let error {
                        Label(error, systemImage: "xmark.octagon.fill")
                            .foregroundStyle(.red)
                            .textSelection(.enabled)
                    } else if let tested, testedCurrentDraft {
                        IdentitySummary(identity: tested.identity)
                    }
                }
            }
            .formStyle(.grouped)
            .navigationTitle(isEditing ? "Edit Server" : "Add Server")
            #if os(iOS)
            .navigationBarTitleDisplayMode(.inline)
            #endif
            .toolbar {
                ToolbarItem(placement: .cancellationAction) {
                    Button("Cancel") { dismiss() }
                }
                ToolbarItem(placement: .confirmationAction) {
                    Button("Save") { Task { await save() } }
                        .disabled(!testedCurrentDraft || saving)
                }
            }
            .onChange(of: draft) { error = nil }
        }
        .frame(minWidth: 420, idealWidth: 480, minHeight: 380)
        .onAppear {
            if case .edit(let id) = target, let existing = model.servers.first(where: { $0.id == id }) {
                draft = ServerDraft(id: id, name: existing.name, baseURL: existing.baseURL, allowsLocalHTTP: existing.baseURL.lowercased().hasPrefix("http:"))
            }
            focus = isEditing ? .token : .name
        }
    }

    private func test() async {
        testing = true
        defer { testing = false }
        draft.baseURL = trimmedURL
        let snapshot = draft
        do {
            let identity = try await model.testServer(snapshot)
            tested = (snapshot, identity)
            error = nil
        } catch {
            tested = nil
            self.error = error.localizedDescription
        }
    }

    private func save() async {
        saving = true
        defer { saving = false }
        guard let tested, tested.draft == draft else { return }
        do {
            try await model.addOrUpdateServer(draft, identity: tested.identity)
            onSaved?(draft.id)
            dismiss()
        } catch {
            self.error = error.localizedDescription
        }
    }
}

private struct IdentitySummary: View {
    let identity: ServerIdentity

    var body: some View {
        VStack(alignment: .leading, spacing: Metrics.xs) {
            Label(identity.ready ? "Connected" : "Connected · not ready yet", systemImage: identity.ready ? "checkmark.circle.fill" : "clock")
                .foregroundStyle(identity.ready ? .green : .orange)
            LabeledContent("Session API", value: "v\(identity.sessionAPIVersion)")
            LabeledContent("Instance", value: identity.instanceID)
            if let root = identity.workspaceRoot { LabeledContent("Workspace", value: root) }
            if !identity.capabilities.isEmpty {
                LabeledContent("Capabilities", value: "\(identity.capabilities.count)")
                    .help(identity.capabilities.joined(separator: ", "))
            }
        }
        .font(.callout)
    }
}
