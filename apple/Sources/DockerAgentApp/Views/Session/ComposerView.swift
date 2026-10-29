import SwiftUI
import AgentKit
import UniformTypeIdentifiers

struct ComposerView: View {
    @Binding var text: String
    @Binding var attachments: [SessionDocumentAttachment]
    let agentName: String
    let isRunning: Bool
    let pendingCount: Int
    let canSend: Bool
    var openPrompts: (() -> Void)? = nil
    let send: (String, [SessionDocumentAttachment], _ steer: Bool) async throws -> Void
    let stop: () async throws -> Void
    /// Request ID of an unacknowledged send with identical text, if any.
    var unknownDuplicate: (String) -> String? = { _ in nil }
    var retryExisting: (String) async throws -> Void = { _ in }

    @State private var sending = false
    @State private var error: String?
    @State private var duplicate: (requestID: String, steer: Bool)?
    @State private var acceptedSends = 0
    @FocusState private var focused: Bool

    private var trimmed: String { text.trimmingCharacters(in: .whitespacesAndNewlines) }
    // Estimate for the live label; SessionSendPayload enforces the exact encoded size at send.
    private var estimatedBytes: Int { attachments.reduce(text.utf8.count + 256) { $0 + $1.encodedByteCount } }
    private var withinLimit: Bool { estimatedBytes <= SessionSendPayload.maximumEncodedRequestBytes }
    private var hasContent: Bool { !trimmed.isEmpty || !attachments.isEmpty }

    var body: some View {
        VStack(alignment: .leading, spacing: Metrics.xs) {
            if pendingCount > 0 || error != nil {
                HStack(spacing: Metrics.s) {
                    if pendingCount > 0 {
                        Label("\(pendingCount) queued", systemImage: "tray.full")
                            .font(.caption)
                            .foregroundStyle(.secondary)
                    }
                    if let error {
                        Label(error, systemImage: "exclamationmark.triangle.fill")
                            .font(.caption)
                            .foregroundStyle(.red)
                            .lineLimit(2)
                    }
                }
                .padding(.horizontal, Metrics.s)
            }
            if !attachments.isEmpty {
                AttachmentChips(attachments: attachments, usedBytes: estimatedBytes) { removed in
                    attachments.removeAll { $0.id == removed.id }
                }
                .padding(.horizontal, Metrics.s)
            }
            HStack(alignment: .bottom, spacing: Metrics.s) {
                AttachmentMenu(allowedTypes: [.item], addFiles: addFiles, addPhoto: addPhoto, openPrompts: openPrompts)
                    .disabled(!canSend)
                TextField(isRunning ? "Queue for \(agentName.nonEmpty ?? "the agent")…" : "Message \(agentName.nonEmpty ?? "the agent")…", text: $text, axis: .vertical)
                    .lineLimit(1...8)
                    .textFieldStyle(.plain)
                    .focused($focused)
                    .padding(.vertical, Metrics.s)
                    .frame(minHeight: Metrics.target)
                    .onSubmit { submit(steer: false) }
                    .disabled(!canSend)
                // Stop lives on the team line above, next to the work it stops; this slot is always Send.
                sendButton
            }
            .padding(.horizontal, Metrics.xs)
            .glassEffect(.regular, in: .rect(cornerRadius: 22, style: .continuous))
        }
        .sensoryFeedback(.success, trigger: acceptedSends)
        .dropDestination(for: URL.self) { urls, _ in
            let files = urls.filter(\.isFileURL)
            guard !files.isEmpty, canSend else { return false }
            addFiles(files)
            return true
        }
        .confirmationDialog(
            "This message may already have been delivered",
            isPresented: Binding(get: { duplicate != nil }, set: { if !$0 { duplicate = nil } }),
            titleVisibility: .visible
        ) {
            Button("Retry Exact Send") {
                guard let requestID = duplicate?.requestID else { return }
                perform { try await retryExisting(requestID) }
            }
            Button("Send as New Message") {
                let steer = duplicate?.steer ?? false
                let content = trimmed
                let files = attachments
                perform { try await send(content, files, steer) }
            }
            Button("Cancel", role: .cancel) {}
        } message: {
            Text("An earlier send of the same text has an unknown delivery state. Retrying it can't create a duplicate; sending again can.")
        }
        .background {
            Group {
                Button("Focus Composer") { focused = true }
                    .keyboardShortcut("l", modifiers: .command)
                Button("Steer") { submit(steer: true) }
                    .keyboardShortcut(.return, modifiers: .option)
                    .disabled(!isRunning)
                // ⌘. always reaches Stop, even while Send is showing because a message is typed.
                Button("Stop") { Task { try? await stop() } }
                    .keyboardShortcut(".", modifiers: .command)
                    .disabled(!isRunning)
            }
            .hidden()
        }
    }

    /// A plain button, not a menu: the menu's indicator leaked beside the glyph on iPhone.
    /// Steer stays one press-and-hold (or right-click, or ⌥⏎) away.
    private var sendButton: some View {
        Button {
            submit(steer: false)
        } label: {
            Image(systemName: isRunning ? "text.line.last.and.arrowtriangle.forward" : "arrow.up.circle.fill")
                .font(.title2)
                .contentTransition(.symbolEffect(.replace))
                .frame(width: Metrics.target, height: Metrics.target)
                .contentShape(.rect)
        }
        .buttonStyle(.borderless)
        .fixedSize()
        .disabled(!hasContent || !withinLimit || sending || !canSend)
        .keyboardShortcut(.return, modifiers: .command)
        .contextMenu {
            Button("Steer current turn", systemImage: "arrow.turn.down.right") { submit(steer: true) }
                .disabled(!isRunning || !hasContent)
            if isRunning {
                Button("Stop current turn", systemImage: "stop.circle") { Task { try? await stop() } }
            }
        }
        .help(isRunning ? "Queue after the current turn (hold for Steer)" : "Send")
        .accessibilityLabel(isRunning ? "Queue message" : "Send message")
        .accessibilityHint(isRunning ? "Press and hold to steer the current turn instead" : "")
    }

    private func submit(steer: Bool) {
        let content = trimmed
        let files = attachments
        guard hasContent, withinLimit, !sending, canSend else { return }
        if files.isEmpty, let requestID = unknownDuplicate(content) {
            duplicate = (requestID, steer)
            return
        }
        perform { try await send(content, files, steer) }
    }

    private func perform(_ action: @escaping () async throws -> Void) {
        let content = trimmed
        let sentIDs = Set(attachments.map(\.id))
        sending = true
        error = nil
        Task {
            do {
                try await action()
                if text.trimmingCharacters(in: .whitespacesAndNewlines) == content { text = "" }
                attachments.removeAll { sentIDs.contains($0.id) }
                acceptedSends += 1
            } catch {
                self.error = error.localizedDescription
            }
            sending = false
        }
    }

    private func addFiles(_ urls: [URL]) {
        for url in urls {
            Task {
                do {
                    let attachment = try await Task.detached { try SessionDocumentAttachment.fromFile(url) }.value
                    attachments.append(attachment)
                } catch {
                    self.error = "\(url.lastPathComponent): \(error.localizedDescription)"
                }
            }
        }
    }

    private func addPhoto(_ data: Data, name: String, mimeType: String) {
        Task {
            do {
                let attachment = try await Task.detached {
                    if data.count <= SessionDocumentAttachment.maximumDocumentBytes {
                        return try SessionDocumentAttachment.fromData(data, name: name, mimeType: mimeType)
                    }
                    guard let jpeg = ImageDownscaler.jpeg(from: data, maxBytes: SessionDocumentAttachment.maximumDocumentBytes) else {
                        throw SessionMediaError.documentTooLarge
                    }
                    let base = (name as NSString).deletingPathExtension
                    return try SessionDocumentAttachment.fromData(jpeg, name: base + ".jpg", mimeType: "image/jpeg")
                }.value
                attachments.append(attachment)
            } catch {
                self.error = "\(name): \(error.localizedDescription)"
            }
        }
    }
}
