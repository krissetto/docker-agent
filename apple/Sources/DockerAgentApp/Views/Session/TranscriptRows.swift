import SwiftUI
import AgentKit

struct UserTurnCard: View {
    let item: TranscriptItem

    var body: some View {
        VStack(alignment: .leading, spacing: Metrics.xs) {
            if item.origin == "agent", let owner = item.owner {
                HStack(spacing: Metrics.xs) {
                    Text("from")
                    AgentLabel(name: owner, font: .caption.weight(.semibold))
                }
                .font(.caption)
                .foregroundStyle(.secondary)
            }
            if !item.text.isEmpty {
                Text(item.text)
                    .textSelection(.enabled)
                    .frame(maxWidth: .infinity, alignment: .leading)
            }
            if !item.attachments.isEmpty {
                AttachmentSummaryList(attachments: item.attachments)
            }
        }
        .frame(maxWidth: .infinity, alignment: .leading)
        .padding(Metrics.m)
        .cardBackground(.fill.tertiary)
        .accessibilityElement(children: .combine)
        .accessibilityLabel(item.origin == "agent" ? "Message from \(item.owner ?? "agent")" : "You")
        .accessibilityValue(item.text)
    }
}

struct AssistantTextView: View {
    let text: String
    var isStreaming = false

    var body: some View {
        VStack(alignment: .leading, spacing: Metrics.s) {
            ForEach(Array(MarkdownSegment.split(text).enumerated()), id: \.offset) { _, segment in
                switch segment {
                case .prose(let prose):
                    Text(MarkdownSegment.attributed(prose))
                        .textSelection(.enabled)
                        .frame(maxWidth: .infinity, alignment: .leading)
                case .code(let language, let code):
                    CodeBlock(language: language, code: code)
                }
            }
            if isStreaming {
                Image(systemName: "ellipsis")
                    .symbolEffect(.variableColor.iterative, options: .repeating)
                    .foregroundStyle(.secondary)
                    .accessibilityLabel("Writing")
            }
        }
    }
}

enum MarkdownSegment: Equatable {
    case prose(String)
    case code(language: String, code: String)

    static func split(_ text: String) -> [MarkdownSegment] {
        var segments: [MarkdownSegment] = []
        var prose: [Substring] = []
        var code: [Substring] = []
        var language: String?
        for line in text.split(separator: "\n", omittingEmptySubsequences: false) {
            let trimmed = line.trimmingCharacters(in: .whitespaces)
            if trimmed.hasPrefix("```") {
                if let lang = language {
                    segments.append(.code(language: lang, code: code.joined(separator: "\n")))
                    code.removeAll()
                    language = nil
                } else {
                    if !prose.isEmpty { segments.append(.prose(prose.joined(separator: "\n"))) }
                    prose.removeAll()
                    language = String(trimmed.dropFirst(3))
                }
            } else if language != nil {
                code.append(line)
            } else {
                prose.append(line)
            }
        }
        // An unterminated fence is still streaming; show it as code.
        if let lang = language { segments.append(.code(language: lang, code: code.joined(separator: "\n"))) }
        let rest = prose.joined(separator: "\n").trimmingCharacters(in: .newlines)
        if !rest.isEmpty { segments.append(.prose(rest)) }
        return segments
    }

    static func attributed(_ prose: String) -> AttributedString {
        let options = AttributedString.MarkdownParsingOptions(interpretedSyntax: .inlineOnlyPreservingWhitespace, failurePolicy: .returnPartiallyParsedIfPossible)
        return (try? AttributedString(markdown: prose, options: options)) ?? AttributedString(prose)
    }
}

struct CodeBlock: View {
    let language: String
    let code: String

    var body: some View {
        VStack(alignment: .leading, spacing: 0) {
            HStack {
                Text(language.isEmpty ? "code" : language)
                    .font(.caption)
                    .foregroundStyle(.secondary)
                Spacer()
                CopyButton(text: code)
            }
            .padding(.horizontal, Metrics.m)
            .padding(.vertical, Metrics.xs)
            ScrollView(.horizontal) {
                Text(code)
                    .font(.mono)
                    .textSelection(.enabled)
                    .padding(.horizontal, Metrics.m)
                    .padding(.bottom, Metrics.m)
            }
        }
        .cardBackground(.fill.quaternary)
    }
}

struct CopyButton: View {
    let text: String
    @State private var copied = false

    var body: some View {
        Button {
            Clipboard.copy(text)
            copied = true
            Task {
                try? await Task.sleep(for: .seconds(1.5))
                copied = false
            }
        } label: {
            Label(copied ? "Copied" : "Copy", systemImage: copied ? "checkmark" : "doc.on.doc")
                .labelStyle(.iconOnly)
                .contentTransition(.symbolEffect(.replace))
        }
        .buttonStyle(.borderless)
        .font(.caption)
        .help("Copy")
    }
}

enum Clipboard {
    @MainActor static func copy(_ text: String) {
        #if os(macOS)
        NSPasteboard.general.clearContents()
        NSPasteboard.general.setString(text, forType: .string)
        #else
        UIPasteboard.general.string = text
        #endif
    }
}

struct ToolCallPresentation {
    let name: String
    let target: String?
    let shellCommand: String?
    let status: Status

    enum Status {
        case pending, running, succeeded, failed, rejected

        var symbol: String {
            switch self {
            case .pending: "circle.dashed"
            case .running: "circle.dotted.circle"
            case .succeeded: "checkmark.circle"
            case .failed: "xmark.circle"
            case .rejected: "hand.raised.slash"
            }
        }

        var tint: Color {
            switch self {
            case .failed: .red
            case .rejected: .orange
            case .running: .accentColor
            default: .secondary
            }
        }

        var label: String {
            switch self {
            case .pending: "Pending"
            case .running: "Running"
            case .succeeded: "Done"
            case .failed: "Failed"
            case .rejected: "Denied"
            }
        }
    }

    init(name: String?, arguments: String?, status: String?) {
        self.name = name ?? "tool"
        let args = ToolCallPresentation.decode(arguments)
        let isShell = PermissionPattern.isCommandTool(self.name)
        let command = PermissionPattern.command(in: args)
        shellCommand = isShell ? command : nil
        target = ["path", "file_path", "url", "query", "pattern", "name"].lazy.compactMap { args[$0] }.first ?? (isShell ? nil : command)
        switch status?.lowercased() {
        case "running", "in_progress", "started": self.status = .running
        case "done", "complete", "completed", "success", "succeeded", "ok": self.status = .succeeded
        case "error", "failed", "failure": self.status = .failed
        case "rejected", "denied", "canceled", "cancelled": self.status = .rejected
        default: self.status = .pending
        }
    }

    static func decode(_ json: String?) -> [String: String] {
        guard let data = json?.data(using: .utf8),
              let object = try? JSONSerialization.jsonObject(with: data) as? [String: Any] else { return [:] }
        return object.compactMapValues { $0 as? String }
    }

    var displayName: String {
        name.replacingOccurrences(of: "_", with: " ")
    }

    var symbol: String {
        switch name {
        case let n where n.contains("read") || n.contains("view"): "doc.text"
        case let n where n.contains("write") || n.contains("edit"): "pencil"
        case let n where n.contains("search") || n.contains("grep") || n.contains("find"): "magnifyingglass"
        case let n where n.contains("fetch") || n.contains("http") || n.contains("web"): "globe"
        case let n where n.contains("todo"): "checklist"
        case let n where n.contains("transfer") || n.contains("handoff"): "arrow.triangle.branch"
        case let n where n.contains("list") || n.contains("directory"): "folder"
        default: "wrench.and.screwdriver"
        }
    }
}

struct ToolRow: View {
    let item: TranscriptItem
    @State private var expanded = false

    var body: some View {
        let tool = ToolCallPresentation(name: item.toolName, arguments: item.toolArguments, status: item.toolStatus)
        VStack(alignment: .leading, spacing: Metrics.xs) {
            Button {
                withAnimation(.snappy) { expanded.toggle() }
            } label: {
                HStack(spacing: Metrics.s) {
                    if let command = tool.shellCommand {
                        Text("$").foregroundStyle(.tertiary)
                        Text(command)
                            .lineLimit(1)
                            .truncationMode(.tail)
                            .frame(maxWidth: .infinity, alignment: .leading)
                    } else {
                        Image(systemName: tool.symbol)
                            .frame(width: 16)
                            .foregroundStyle(.secondary)
                        Text(tool.displayName)
                            .foregroundStyle(.primary)
                        if let target = tool.target {
                            Text(target)
                                .font(.monoSmall)
                                .foregroundStyle(.secondary)
                                .lineLimit(1)
                                .truncationMode(.middle)
                        }
                        Spacer(minLength: Metrics.s)
                    }
                    Image(systemName: tool.status.symbol)
                        .foregroundStyle(tool.status.tint)
                        .symbolEffect(.rotate, isActive: tool.status == .running)
                    Image(systemName: "chevron.right")
                        .font(.caption2.weight(.semibold))
                        .foregroundStyle(.tertiary)
                        .rotationEffect(.degrees(expanded ? 90 : 0))
                }
                .font(tool.shellCommand != nil ? .monoSmall : .callout)
                .foregroundStyle(tool.shellCommand != nil ? AnyShapeStyle(.secondary) : AnyShapeStyle(.primary))
                .contentShape(.rect)
            }
            .buttonStyle(.plain)
            .accessibilityLabel(tool.shellCommand.map { "Shell command \($0)" } ?? "\(tool.displayName) \(tool.target ?? "")")
            .accessibilityValue(tool.status.label)
            .accessibilityHint(expanded ? "Hides details" : "Shows details")

            if expanded {
                VStack(alignment: .leading, spacing: Metrics.s) {
                    if tool.shellCommand == nil, let args = item.toolArguments, !args.isEmpty {
                        DetailBlock(title: "Arguments", text: args)
                    }
                    if let output = item.toolOutput, !output.isEmpty {
                        DetailBlock(title: tool.shellCommand == nil ? "Result" : "Output", text: output)
                    } else if tool.status == .running {
                        Text("Waiting for output…").font(.caption).foregroundStyle(.secondary)
                    }
                }
                .padding(.leading, Metrics.l + Metrics.s)
                .transition(.opacity)
            }
        }
    }
}

struct DetailBlock: View {
    let title: String
    let text: String
    private let limit = 4_000

    var body: some View {
        VStack(alignment: .leading, spacing: Metrics.xs) {
            HStack {
                Text(title).font(.caption).foregroundStyle(.secondary)
                Spacer()
                CopyButton(text: text)
            }
            ScrollView(.horizontal) {
                Text(text.count > limit ? String(text.prefix(limit)) + "\n…" : text)
                    .font(.monoSmall)
                    .textSelection(.enabled)
                    .lineLimit(40)
            }
            if text.count > limit {
                Text("Showing the first \(limit.formatted()) characters").font(.caption2).foregroundStyle(.tertiary)
            }
        }
        .padding(Metrics.s)
        .cardBackground(.fill.quaternary)
    }
}

struct ReasoningRow: View {
    let item: TranscriptItem
    @State private var expanded = false

    var body: some View {
        VStack(alignment: .leading, spacing: Metrics.xs) {
            Button {
                withAnimation(.snappy) { expanded.toggle() }
            } label: {
                HStack(spacing: Metrics.xs) {
                    Image(systemName: "brain")
                        .symbolEffect(.pulse, isActive: item.isStreaming)
                    Text(item.isStreaming ? "Thinking…" : "Thought")
                    Image(systemName: "chevron.right")
                        .font(.caption2.weight(.semibold))
                        .rotationEffect(.degrees(expanded ? 90 : 0))
                }
                .font(.callout)
                .foregroundStyle(.secondary)
                .contentShape(.rect)
            }
            .buttonStyle(.plain)
            .accessibilityHint(expanded ? "Hides reasoning" : "Shows reasoning")

            if expanded {
                Text(item.text)
                    .font(.callout)
                    .foregroundStyle(.secondary)
                    .textSelection(.enabled)
                    .padding(.leading, Metrics.l)
                    .overlay(alignment: .leading) {
                        Rectangle().fill(.quaternary).frame(width: 2)
                    }
            }
        }
    }
}

struct DelegationDivider: View {
    let item: TranscriptItem
    @Environment(\.openSession) private var openSession

    var body: some View {
        HStack(spacing: Metrics.s) {
            line
            if let owner = item.owner { AgentLabel(name: owner, font: .caption.weight(.semibold)) }
            Image(systemName: "arrow.right").font(.caption2).foregroundStyle(.tertiary)
            Text(item.text)
                .font(.caption)
                .foregroundStyle(.secondary)
                .lineLimit(1)
            if let child = item.child {
                Button("Open") { openSession(child) }
                    .buttonStyle(.borderless)
                    .font(.caption.weight(.medium))
            }
            line
        }
        .padding(.vertical, Metrics.xs)
        .accessibilityElement(children: .combine)
    }

    private var line: some View {
        Rectangle().fill(.separator).frame(height: 1).frame(maxWidth: .infinity)
    }
}

struct NoticeRow: View {
    let item: TranscriptItem

    var body: some View {
        Label(item.text, systemImage: "info.circle")
            .font(.caption)
            .foregroundStyle(.secondary)
            .lineLimit(2)
    }
}

struct ErrorRow: View {
    let item: TranscriptItem
    var onRetry: (() -> Void)?

    var body: some View {
        HStack(alignment: .firstTextBaseline, spacing: Metrics.s) {
            Image(systemName: "exclamationmark.triangle.fill").foregroundStyle(.red)
            Text(item.text)
                .font(.callout)
                .textSelection(.enabled)
                .frame(maxWidth: .infinity, alignment: .leading)
            if let onRetry {
                Button("Retry", action: onRetry)
                    .buttonStyle(.bordered)
                    .controlSize(.small)
            }
        }
        .padding(Metrics.m)
        .cardBackground(Color.red.opacity(0.08))
    }
}

/// Mirrors the TUI's "always allow" grant (toolconfirm.BuildPermissionPattern):
/// command tools are scoped to the command's first word, other tools to their name.
enum PermissionPattern {
    static func isCommandTool(_ name: String) -> Bool { name == "shell" || name == "run_background_job" }

    static func command(in args: [String: String]) -> String? {
        if let cmd = args["cmd"], !cmd.trimmingCharacters(in: .whitespaces).isEmpty { return cmd }
        return args["command"] ?? args["cmd"]
    }

    /// nil when a command tool's command can't be read (e.g. truncated arguments):
    /// falling back to the bare tool name would grant every command.
    static func build(toolName: String, arguments: String) -> String? {
        guard isCommandTool(toolName) else { return toolName }
        guard let first = command(in: ToolCallPresentation.decode(arguments))?.split(whereSeparator: \.isWhitespace).first else { return nil }
        return "\(toolName):cmd=\(first)*"
    }

    static func label(_ pattern: String) -> String {
        let shown = pattern.range(of: ":cmd=").map { String(pattern[$0.upperBound...]) } ?? pattern
        return shown.split(whereSeparator: \.isWhitespace).joined(separator: " ")
    }
}

/// Metadata-only rows for attachments already in the transcript; bytes are never shown here.
struct AttachmentSummaryList: View {
    let attachments: [MessageAttachmentSummary]

    var body: some View {
        VStack(alignment: .leading, spacing: Metrics.xs) {
            ForEach(attachments) { attachment in
                HStack(spacing: Metrics.s) {
                    Image(systemName: MediaKind(mimeType: attachment.mimeType).symbol)
                        .foregroundStyle(.secondary)
                        .frame(width: 16)
                    Text(attachment.name).font(.callout).lineLimit(1).truncationMode(.middle)
                    if let size = attachment.byteCount {
                        Text(size.formatted(.byteCount(style: .file))).font(.caption).foregroundStyle(.secondary)
                    }
                }
                .accessibilityElement(children: .combine)
                .accessibilityLabel("Attachment \(attachment.name)")
            }
        }
    }
}
