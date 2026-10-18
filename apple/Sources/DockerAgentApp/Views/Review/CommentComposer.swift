import SwiftUI
import AgentKit

struct CommentComposer: View {
    let title: String
    let path: String
    let anchorLabel: String
    let context: [String]
    var initialBody = ""
    var saveLabel = "Add Comment"
    let onSave: (String) throws -> Void
    let onCancel: () -> Void

    @State private var body_ = ""
    @State private var error: String?
    @FocusState private var focused: Bool

    private var bytes: Int { body_.trimmingCharacters(in: .whitespacesAndNewlines).utf8.count }
    private var canSave: Bool { bytes > 0 && bytes <= ReviewLimits.bodyBytes }

    var body: some View {
        VStack(alignment: .leading, spacing: Metrics.m) {
            HStack(alignment: .firstTextBaseline) {
                Text(title).font(.headline)
                Spacer()
                Text(anchorLabel).font(.caption).foregroundStyle(.secondary)
            }
            Text(path)
                .font(.monoSmall)
                .foregroundStyle(.secondary)
                .lineLimit(1)
                .truncationMode(.middle)
            if !context.isEmpty {
                VStack(alignment: .leading, spacing: 0) {
                    ForEach(Array(context.prefix(6).enumerated()), id: \.offset) { _, line in
                        Text(line.isEmpty ? " " : line).lineLimit(1)
                    }
                    if context.count > 6 {
                        Text("… \(context.count - 6) more lines").foregroundStyle(.tertiary)
                    }
                }
                .font(.monoSmall)
                .padding(Metrics.s)
                .frame(maxWidth: .infinity, alignment: .leading)
                .cardBackground(.fill.quaternary)
                .accessibilityLabel("Selected lines")
            }
            TextEditor(text: $body_)
                .font(.body)
                .focused($focused)
                .frame(minHeight: 110)
                .scrollContentBackground(.hidden)
                .padding(Metrics.xs)
                .cardBackground(.fill.tertiary)
                .accessibilityLabel("Comment")
            HStack {
                if let error {
                    Label(error, systemImage: "exclamationmark.triangle.fill")
                        .font(.caption)
                        .foregroundStyle(.red)
                } else if bytes > ReviewLimits.bodyBytes * 3 / 4 {
                    Text("\(bytes.formatted(.byteCount(style: .memory))) of 16 KB")
                        .font(.caption)
                        .foregroundStyle(bytes > ReviewLimits.bodyBytes ? .red : .secondary)
                }
                Spacer()
                Button("Cancel", role: .cancel, action: onCancel)
                    .keyboardShortcut(.cancelAction)
                Button(saveLabel) {
                    do { try onSave(body_) } catch { self.error = error.localizedDescription }
                }
                .buttonStyle(.borderedProminent)
                .keyboardShortcut(.return, modifiers: .command)
                .disabled(!canSave)
            }
        }
        .padding(Metrics.l)
        .frame(minWidth: 360, idealWidth: 440)
        .presentationDetents([.medium, .large])
        .onAppear {
            body_ = initialBody
            focused = true
        }
    }
}
