import SwiftUI
import AgentKit

struct ContextBreakdownView: View {
    let breakdown: SessionContextBreakdown
    var stopIncluding: ((String) async throws -> Void)?
    @State private var confirming: String?
    @State private var error: String?

    private var rows: [(label: String, category: SessionContextCategory, color: Color)] {
        [
            ("System prompt", breakdown.systemPrompt, .blue),
            ("Tool definitions", breakdown.toolDefinitions, .purple),
            ("Prompt files", breakdown.promptFiles, .teal),
            ("Messages", breakdown.messages, .orange),
            ("Tool results", breakdown.toolResults, .pink),
            ("Compaction summary", breakdown.compactionSummary, .brown),
        ]
    }

    private var total: Int64 { max(1, rows.reduce(0) { $0 + $1.category.tokens }) }

    var body: some View {
        Section {
            GeometryReader { geometry in
                HStack(spacing: 1) {
                    ForEach(rows, id: \.label) { row in
                        if row.category.tokens > 0 {
                            row.color.frame(width: max(2, geometry.size.width * CGFloat(row.category.tokens) / CGFloat(max(total, breakdown.contextLimit))))
                        }
                    }
                    Spacer(minLength: 0)
                }
                .background(.fill.tertiary)
                .clipShape(.capsule)
            }
            .frame(height: 8)
            .accessibilityHidden(true)
            ForEach(rows, id: \.label) { row in
                HStack {
                    Circle().fill(row.color).frame(width: 8, height: 8).accessibilityHidden(true)
                    Text(row.label)
                    Spacer()
                    Text("\(Format.tokens(row.category.tokens)) · \(row.category.items)")
                        .font(.caption.monospacedDigit())
                        .foregroundStyle(.secondary)
                }
                .accessibilityElement(children: .combine)
            }
            LabeledContent("Estimated total", value: "\(Format.tokens(rows.reduce(0) { $0 + $1.category.tokens })) of \(Format.tokens(breakdown.contextLimit))")
                .monospacedDigit()
            LabeledContent("Model") { Text(breakdown.model).font(.monoSmall) }
        } header: {
            Text("Context (estimated)")
        } footer: {
            Text("Estimates of the next request's composition, not provider token accounting.")
        }
        if let files = breakdown.attachedFiles, !files.isEmpty {
            Section {
                ForEach(files) { file in
                    HStack {
                        Text((file.path as NSString).lastPathComponent)
                            .help(file.path)
                        if file.missing == true {
                            Text("missing").font(.caption2).foregroundStyle(.orange)
                        }
                        Spacer()
                        Text(Format.tokens(file.tokens)).font(.caption.monospacedDigit()).foregroundStyle(.secondary)
                        if stopIncluding != nil {
                            Button("Stop Including") { confirming = file.path }
                                .buttonStyle(.borderless)
                                .font(.caption)
                        }
                    }
                }
                if let error {
                    Text(error).font(.caption).foregroundStyle(.red)
                }
            } header: {
                Text("Attached Files")
            } footer: {
                Text("Stopping only keeps the file out of future prompts and sub-agents. It doesn't delete the file or the attachment already in the conversation.")
            }
            .confirmationDialog("Stop including this file?", isPresented: Binding(get: { confirming != nil }, set: { if !$0 { confirming = nil } }), titleVisibility: .visible, presenting: confirming) { path in
                Button("Stop Including") {
                    guard let stopIncluding else { return }
                    Task {
                        do { try await stopIncluding(path) } catch { self.error = error.localizedDescription }
                    }
                }
            } message: { path in
                Text((path as NSString).lastPathComponent)
            }
        }
    }
}
