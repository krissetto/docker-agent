import SwiftUI

/// One tool call in the transcript: a one-line account of what the agent did, to what, and how it went;
/// expanded, the artifact itself (command and output, diff, hits, roster, checklist). Never raw JSON first.
struct ToolContentView: View {
    let presentation: ToolPresentation
    let isExpanded: Bool
    let onToggle: () -> Void
    var onBranch: (() -> Void)? = nil

    @Environment(\.accessibilityReduceMotion) private var reduceMotion
    @State private var showsRawDetails = false

    var body: some View {
        VStack(alignment: .leading, spacing: Metrics.s) {
            Button {
                withAnimation(Motion.reveal(reduceMotion)) { onToggle() }
            } label: {
                ToolHeader(presentation: presentation, isExpanded: isExpanded)
                    .frame(minHeight: Metrics.target)
                    .contentShape(.rect)
            }
            .buttonStyle(.plain)
            .accessibilityLabel(presentation.accessibilityTitle)
            .accessibilityValue(presentation.accessibilityStatus)
            .accessibilityHint(isExpanded ? "Hides details" : "Shows details")
            .accessibilityIdentifier("tool-row-header-\(presentation.familyIdentifier)")

            if isExpanded {
                ToolBody(presentation: presentation, showsRawDetails: $showsRawDetails)
                    .padding(.leading, ToolHeader.glyphColumn + Metrics.s)
                    // A container element, so UI tests can find the body and VoiceOver keeps its children.
                    .accessibilityElement(children: .contain)
                    .accessibilityIdentifier("tool-row-body")
                    .transition(reduceMotion ? .opacity : .opacity.combined(with: .offset(y: -Metrics.s)))
            }
        }
        .contextMenu { ToolActions(presentation: presentation, isExpanded: isExpanded, showsRawDetails: $showsRawDetails, onToggle: toggle, onBranch: onBranch) }
        .sensoryFeedback(.selection, trigger: isExpanded)
    }

    private func toggle() {
        withAnimation(Motion.reveal(reduceMotion)) { onToggle() }
    }
}

// MARK: - Header

private struct ToolHeader: View {
    let presentation: ToolPresentation
    let isExpanded: Bool

    static let glyphColumn: CGFloat = 16

    var body: some View {
        HStack(spacing: Metrics.s) {
            Group {
                if presentation.family == .reasoning, presentation.status == .succeeded {
                    Image(systemName: "brain").foregroundStyle(.secondary)
                } else {
                    StatusGlyph(status: presentation.status)
                }
            }
            .frame(width: Self.glyphColumn)
            headline
            Spacer(minLength: Metrics.s)
            trailing
            Image(systemName: "chevron.right")
                .font(.caption2.weight(.semibold))
                .foregroundStyle(.tertiary)
                .rotationEffect(.degrees(isExpanded ? 90 : 0))
        }
        .font(.callout)
        .lineLimit(1)
    }

    @ViewBuilder
    private var headline: some View {
        switch presentation.family {
        case .command:
            if let command = presentation.command {
                let preview = CommandText.preview(command)
                Text("$").foregroundStyle(.tertiary).font(.mono)
                Text(preview.firstLine)
                    .font(.mono)
                    .truncationMode(.tail)
                if preview.moreLines > 0 {
                    Text("+\(preview.moreLines) lines")
                        .font(.figureSmall)
                        .foregroundStyle(.tertiary)
                        .layoutPriority(1)
                }
            } else {
                verb
                targetText
            }
        case .delegation, .communication:
            verb
            if let agent = presentation.target {
                HStack(spacing: Metrics.xs) {
                    AgentDot(name: agent)
                    Text(agent).fontWeight(.semibold)
                }
                .layoutPriority(1)
            }
            if let lead = presentation.contentPreview?.firstMeaningfulLine {
                Text(lead)
                    .foregroundStyle(.secondary)
                    .truncationMode(.tail)
                    .layoutPriority(-1)
            }
        case .search, .code:
            verb
            if let query = presentation.query {
                Text(query).font(.mono).layoutPriority(1)
            } else {
                targetText
            }
        case .prompt, .reasoning:
            verb
            if let lead = presentation.contentPreview?.firstMeaningfulLine {
                Text(lead)
                    .foregroundStyle(.secondary)
                    .truncationMode(.tail)
                    .layoutPriority(-1)
            }
        case .generic:
            verb
            if presentation.showsNameBesideVerb {
                Text(presentation.name)
                    .font(.monoSmall)
                    .foregroundStyle(.tertiary)
                    .truncationMode(.middle)
                    .layoutPriority(-1)
            } else {
                targetText
            }
        default:
            verb
            targetText
        }
    }

    private var verb: some View {
        Text(presentation.verb).fixedSize()
    }

    @ViewBuilder
    private var targetText: some View {
        if let target = presentation.target {
            Text(target)
                .font(.monoSmall)
                .foregroundStyle(.secondary)
                .truncationMode(.middle)
                .layoutPriority(-1)
        }
    }

    /// Failure and denial are written in the status colour; success stays quiet and shows the summary instead.
    @ViewBuilder
    private var trailing: some View {
        switch presentation.status {
        case .failed, .denied, .approval:
            Text(presentation.statusLabel)
                .font(.caption.weight(.medium))
                .foregroundStyle(presentation.status.tint)
                .layoutPriority(1)
        case .running, .preparing, .pending:
            EmptyView()
        case .succeeded:
            if let summary = presentation.resultSummary {
                Text(summary)
                    .font(.figureSmall)
                    .foregroundStyle(.secondary)
                    .layoutPriority(1)
            }
        }
    }
}

private struct StatusGlyph: View {
    let status: ToolPresentation.Status

    var body: some View {
        switch status {
        case .running:
            RunningGlyph()
        case .preparing:
            Image(systemName: "circle.dashed").foregroundStyle(.tertiary)
        case .pending:
            Image(systemName: "circle").foregroundStyle(.tertiary)
        case .succeeded:
            Image(systemName: "checkmark").font(.caption.weight(.semibold)).foregroundStyle(.secondary)
        case .failed:
            Image(systemName: "xmark.circle.fill").foregroundStyle(status.tint)
        case .denied:
            Image(systemName: "hand.raised.fill").foregroundStyle(status.tint)
        case .approval:
            Image(systemName: "exclamationmark.circle.fill").foregroundStyle(status.tint)
        }
    }
}

/// Live work: a dotted ring cycling in Harbor; a still ring under Reduce Motion.
private struct RunningGlyph: View {
    @Environment(\.accessibilityReduceMotion) private var reduceMotion

    var body: some View {
        Image(systemName: "progress.indicator")
            .foregroundStyle(Palette.harbor)
            .symbolEffect(.variableColor.iterative, options: .repeating, isActive: !reduceMotion)
            .accessibilityHidden(true)
    }
}

extension ToolPresentation.Status {
    var tint: Color {
        switch self {
        case .failed: .red
        case .denied, .approval: Palette.signal
        case .running: Palette.harbor
        default: .secondary
        }
    }
}

// MARK: - Body

/// Family bodies are compositions of a few shared blocks; each family picks the artifact it is about.
private struct ToolBody: View {
    let presentation: ToolPresentation
    @Binding var showsRawDetails: Bool

    var body: some View {
        VStack(alignment: .leading, spacing: Metrics.s) {
            switch presentation.family {
            case .command: CommandBody(presentation: presentation)
            case .fileRead:
                ArtifactBody(presentation: presentation, items: .paths, preview: .code, runningText: "Reading…")
            case .fileWrite:
                ArtifactBody(presentation: presentation, items: .paths, preview: .code, previewTitle: "Written", runningText: "Writing…")
            case .fileEdit: EditBody(presentation: presentation)
            case .directory:
                ArtifactBody(presentation: presentation, items: .paths, preview: .code, runningText: "Listing…")
            case .search: SearchBody(presentation: presentation)
            case .fetch: FetchBody(presentation: presentation)
            case .git:
                ArtifactBody(presentation: presentation, items: .commits, preview: .code, runningText: "Running…", patchFirst: true)
            case .delegation: PeerBody(presentation: presentation, title: "Task", runningText: "Working…")
            case .communication: PeerBody(presentation: presentation, title: "Message", runningText: "Delivering…")
            case .todo, .task:
                ArtifactBody(presentation: presentation, items: .checklist, preview: .prose, runningText: "Updating…")
            case .plan:
                ArtifactBody(presentation: presentation, items: .plans, preview: .prose, previewTitle: "Plan", runningText: "Saving…")
            case .code: CodeBody(presentation: presentation)
            case .memory:
                ArtifactBody(presentation: presentation, items: .notes, preview: .prose, previewTitle: "Memory", runningText: "Working…")
            case .schedule:
                ArtifactBody(presentation: presentation, items: .schedules, preview: .prose, previewTitle: "Prompt", runningText: "Scheduling…")
            case .session:
                ArtifactBody(presentation: presentation, items: .sessions, preview: .prose, previewTitle: "Transcript", runningText: "Reading…")
            case .skill:
                ArtifactBody(presentation: presentation, items: .paths, preview: .code, previewTitle: presentation.contentPreviewReadsAsTask ? "Task" : "Skill", runningText: "Working…")
            case .configuration:
                ArtifactBody(presentation: presentation, items: .plain, preview: .code, previewTitle: "Configuration", runningText: "Applying…", factsAsGrid: true)
            case .integration:
                ArtifactBody(presentation: presentation, items: .catalog, preview: .prose, runningText: "Working…")
            case .prompt: PromptBody(presentation: presentation)
            case .reasoning: ReasoningBody(presentation: presentation)
            case .generic: GenericBody(presentation: presentation)
            }
            // A thought is its own payload; raw arguments would only repeat it.
            if presentation.family != .reasoning {
                RawDetails(presentation: presentation, isOn: $showsRawDetails)
            }
        }
    }
}

private extension ToolPresentation {
    /// The header already carries the summary for quiet successes; bodies repeat it only when the header can't.
    var bodySummary: String? { status == .succeeded ? nil : resultSummary }

    /// Facts worth a line of their own: anything not already said by the header summary.
    var bodyFacts: [Field] {
        (metrics + fields).filter { field in
            !(resultSummary?.contains(field.value) ?? false)
                && field.value != contentPreview
                && field.value != target
                && field.value != requestReceipt?.disposition
                && field.value != requestReceipt?.target
        }
    }

    var contentPreviewReadsAsTask: Bool { fields.contains { $0.name.lowercased() == "task" } }

    var normalizedName: String { name.replacingOccurrences(of: "_", with: " ").lowercased() }

    var showsNameBesideVerb: Bool {
        normalizedName != verb.lowercased() && normalizedName != displayName.lowercased()
    }
}

/// The common shape: facts, then the artifact (list, patch or preview), else the result or progress.
private struct ArtifactBody: View {
    enum Preview { case code, prose }

    let presentation: ToolPresentation
    let items: EntityList.Style
    let preview: Preview
    var previewTitle: String? = nil
    let runningText: String
    var patchFirst = false
    var factsAsGrid = false

    var body: some View {
        if factsAsGrid { FactsGrid(fields: presentation.bodyFacts) } else { FactsLine(fields: presentation.bodyFacts) }
        if patchFirst, !presentation.patch.isEmpty {
            PatchSection(presentation: presentation)
        }
        if !presentation.items.isEmpty {
            SectionLine(title: presentation.resultLabel, detail: presentation.bodySummary, copyText: presentation.items.copyText)
            EntityList(items: presentation.items, style: items)
            TruncationNote(presentation: presentation)
        }
        if let text = presentation.contentPreview, !text.isEmpty {
            switch preview {
            case .code:
                CodePreview(text: text, title: previewTitle ?? presentation.resultLabel, truncated: presentation.outputWasTruncated, full: presentation.fullDiagnosticOutput)
            case .prose:
                ProseBlock(title: previewTitle ?? presentation.resultLabel, text: text, truncated: presentation.outputWasTruncated, full: presentation.fullDiagnosticOutput)
            }
        }
        if !patchFirst, !presentation.patch.isEmpty {
            PatchSection(presentation: presentation)
        }
        ResultSection(presentation: presentation, emptyRunningText: runningText,
                      hasArtifact: !presentation.items.isEmpty || !presentation.patch.isEmpty || presentation.contentPreview?.isEmpty == false)
    }
}

private struct PatchSection: View {
    let presentation: ToolPresentation

    var body: some View {
        VStack(alignment: .leading, spacing: Metrics.xs) {
            SectionLine(title: presentation.resultLabel, detail: presentation.bodySummary, copyText: presentation.patch.map(\.text).joined(separator: "\n"))
            DiffView(lines: presentation.patch)
        }
    }
}

// MARK: Command

private struct CommandBody: View {
    let presentation: ToolPresentation

    var body: some View {
        if let command = presentation.command {
            VStack(alignment: .leading, spacing: Metrics.xs) {
                HStack(alignment: .firstTextBaseline, spacing: Metrics.s) {
                    Text("$").foregroundStyle(.tertiary)
                    Text(command)
                        .textSelection(.enabled)
                        .frame(maxWidth: .infinity, alignment: .leading)
                }
                .font(.mono)
                if let cwd = presentation.context {
                    Text("in \(cwd)")
                        .font(.monoSmall)
                        .foregroundStyle(.secondary)
                        .lineLimit(1)
                        .truncationMode(.middle)
                }
            }
        }
        FactsLine(fields: presentation.bodyFacts)
        if !presentation.items.isEmpty {
            SectionLine(title: presentation.resultLabel, detail: presentation.bodySummary, copyText: presentation.items.copyText)
            EntityList(items: presentation.items, style: .plain)
        }
        ResultSection(presentation: presentation, emptyRunningText: "Running…", hasArtifact: !presentation.items.isEmpty)
    }
}

// MARK: Edits — the backend confirms "replaced N characters", never a diff, so the view shows what was asked for

private struct EditBody: View {
    let presentation: ToolPresentation

    var body: some View {
        if !presentation.patch.isEmpty {
            PatchSection(presentation: presentation)
        } else if !presentation.requestedEdits.isEmpty {
            VStack(alignment: .leading, spacing: Metrics.xs) {
                SectionLine(title: "Requested edits", detail: presentation.requestedEdits.count > 1 ? "\(presentation.requestedEdits.count)" : nil,
                            copyText: presentation.requestedEdits.map { "- \($0.oldText)\n+ \($0.newText)" }.joined(separator: "\n\n"))
                DiffView(lines: Self.lines(presentation.requestedEdits))
            }
        }
        FactsLine(fields: presentation.bodyFacts)
        ResultSection(presentation: presentation, emptyRunningText: "Editing…", hasArtifact: !presentation.patch.isEmpty || !presentation.requestedEdits.isEmpty)
    }

    /// Old text above new text per edit, numbered only when there are several.
    static func lines(_ edits: [ToolPresentation.EditRequest]) -> [ToolPresentation.ResultLine] {
        var result: [ToolPresentation.ResultLine] = []
        for (index, edit) in edits.enumerated() {
            if edits.count > 1 {
                result.append(ToolPresentation.ResultLine(id: result.count, kind: .heading, text: "Edit \(index + 1)"))
            }
            for line in edit.oldText.split(omittingEmptySubsequences: false, whereSeparator: \.isNewline) {
                result.append(ToolPresentation.ResultLine(id: result.count, kind: .removal, text: String(line)))
            }
            for line in edit.newText.split(omittingEmptySubsequences: false, whereSeparator: \.isNewline) {
                result.append(ToolPresentation.ResultLine(id: result.count, kind: .addition, text: String(line)))
            }
        }
        return result
    }
}

// MARK: Search

private struct SearchBody: View {
    let presentation: ToolPresentation

    var body: some View {
        if let query = presentation.query {
            HStack(spacing: Metrics.s) {
                Text(query)
                    .font(.mono)
                    .textSelection(.enabled)
                    .padding(.horizontal, Metrics.s)
                    .padding(.vertical, Metrics.xs)
                    .background(.fill.quaternary, in: .rect(cornerRadius: Metrics.chipRadius, style: .continuous))
                if let context = presentation.context {
                    Text("in \(context)").font(.monoSmall).foregroundStyle(.secondary).lineLimit(1).truncationMode(.middle)
                }
            }
        }
        FactsLine(fields: presentation.bodyFacts)
        if !presentation.items.isEmpty {
            SectionLine(title: presentation.resultLabel, detail: presentation.bodySummary, copyText: presentation.items.copyText)
            EntityList(items: presentation.items, style: .hits)
            TruncationNote(presentation: presentation)
        }
        if let text = presentation.contentPreview, !text.isEmpty, presentation.items.isEmpty {
            ProseBlock(title: presentation.resultLabel, text: text, truncated: presentation.outputWasTruncated, full: presentation.fullDiagnosticOutput)
        }
        ResultSection(presentation: presentation, emptyRunningText: "Searching…", hasArtifact: !presentation.items.isEmpty || presentation.contentPreview?.isEmpty == false)
    }
}

// MARK: Fetch

private struct FetchBody: View {
    let presentation: ToolPresentation

    var body: some View {
        if let url = presentation.target {
            Text(url)
                .font(.monoSmall)
                .foregroundStyle(.secondary)
                .textSelection(.enabled)
                .lineLimit(3)
                .truncationMode(.middle)
        }
        FactsLine(fields: presentation.bodyFacts)
        if !presentation.items.isEmpty {
            SectionLine(title: presentation.resultLabel, detail: presentation.bodySummary, copyText: presentation.items.copyText)
            EntityList(items: presentation.items, style: .plain)
        }
        if let text = presentation.contentPreview, !text.isEmpty {
            ProseBlock(title: presentation.resultLabel, text: text, truncated: presentation.outputWasTruncated, full: presentation.fullDiagnosticOutput)
        }
        ResultSection(presentation: presentation, emptyRunningText: "Fetching…", hasArtifact: !presentation.items.isEmpty || presentation.contentPreview?.isEmpty == false)
    }
}

// MARK: Peers (delegation, messages, background agents, webhooks)

private struct PeerBody: View {
    let presentation: ToolPresentation
    let title: String
    let runningText: String

    var body: some View {
        if let text = presentation.contentPreview, !text.isEmpty {
            ProseBlock(title: title, text: text, truncated: presentation.outputWasTruncated, full: presentation.fullDiagnosticOutput)
        }
        FactsLine(fields: presentation.bodyFacts)
        if let receipt = presentation.requestReceipt {
            ReceiptView(receipt: receipt)
        }
        if !presentation.items.isEmpty {
            SectionLine(title: presentation.resultLabel, detail: presentation.bodySummary, copyText: nil)
            EntityList(items: presentation.items, style: .agents)
            TruncationNote(presentation: presentation)
        }
        ResultSection(presentation: presentation, emptyRunningText: runningText,
                      hasArtifact: !presentation.items.isEmpty || presentation.requestReceipt != nil, previewAlreadyShown: true)
    }
}

/// What the inbox said: admission only. Nothing here claims the message was read.
private struct ReceiptView: View {
    let receipt: ToolPresentation.Receipt

    var body: some View {
        VStack(alignment: .leading, spacing: Metrics.xs) {
            HStack(alignment: .firstTextBaseline, spacing: Metrics.s) {
                switch receipt.accepted {
                case true?:
                    Image(systemName: "tray.and.arrow.down").foregroundStyle(.secondary)
                    Text(receipt.target.map { "Accepted into \($0)’s inbox" } ?? "Accepted into inbox")
                case false?:
                    Image(systemName: "tray.and.arrow.down").foregroundStyle(.red)
                    Text(receipt.rejection.map { "Rejected: \($0)" } ?? "Rejected").foregroundStyle(.red)
                case nil:
                    Image(systemName: "tray").foregroundStyle(.tertiary)
                    Text("No receipt").foregroundStyle(.secondary)
                }
            }
            .font(.callout)
            FactsLine(fields: facts)
            if let detail = receipt.detail, !detail.isEmpty {
                Text(detail).font(.caption).foregroundStyle(.secondary).textSelection(.enabled)
            }
        }
        .accessibilityElement(children: .combine)
    }

    private var facts: [ToolPresentation.Field] {
        var facts: [ToolPresentation.Field] = []
        if let disposition = receipt.disposition, !disposition.isEmpty { facts.append(ToolPresentation.Field("as", disposition)) }
        if receipt.queued == true { facts.append(ToolPresentation.Field("queued", "yes")) }
        if receipt.durable == true { facts.append(ToolPresentation.Field("durable", "yes")) }
        return facts
    }
}

// MARK: Code intelligence (LSP): locations and diagnostics

private struct CodeBody: View {
    let presentation: ToolPresentation

    var body: some View {
        if let query = presentation.query {
            Text(query)
                .font(.mono)
                .textSelection(.enabled)
                .padding(.horizontal, Metrics.s)
                .padding(.vertical, Metrics.xs)
                .background(.fill.quaternary, in: .rect(cornerRadius: Metrics.chipRadius, style: .continuous))
        }
        FactsLine(fields: presentation.bodyFacts)
        if !presentation.items.isEmpty {
            SectionLine(title: presentation.resultLabel, detail: presentation.bodySummary, copyText: presentation.items.copyText)
            EntityList(items: presentation.items, style: .locations)
            TruncationNote(presentation: presentation)
        }
        if !presentation.patch.isEmpty { PatchSection(presentation: presentation) }
        if let text = presentation.contentPreview, !text.isEmpty {
            CodePreview(text: text, title: presentation.resultLabel, truncated: presentation.outputWasTruncated, full: presentation.fullDiagnosticOutput)
        }
        ResultSection(presentation: presentation, emptyRunningText: "Asking the language server…",
                      hasArtifact: !presentation.items.isEmpty || !presentation.patch.isEmpty || presentation.contentPreview?.isEmpty == false)
    }
}

// MARK: Question to the person (user_prompt) — display only; the decision card does the asking

private struct PromptBody: View {
    let presentation: ToolPresentation

    var body: some View {
        if let question = presentation.contentPreview, !question.isEmpty {
            HStack(alignment: .top, spacing: Metrics.s) {
                Image(systemName: "questionmark.circle")
                    .foregroundStyle(Palette.signal)
                    .padding(.top, 2)
                VStack(alignment: .leading, spacing: Metrics.xs) {
                    if let title = presentation.target { Text(title).font(.callout.weight(.semibold)) }
                    AssistantTextView(text: question).font(.callout)
                }
            }
            .padding(Metrics.m)
            .frame(maxWidth: .infinity, alignment: .leading)
            .background(.fill.quaternary, in: .rect(cornerRadius: Metrics.cardRadius, style: .continuous))
        }
        FactsGrid(fields: presentation.bodyFacts.filter { $0.name != "Linkage" })
        if !presentation.items.isEmpty {
            SectionLine(title: presentation.resultLabel, detail: presentation.bodySummary, copyText: nil)
            EntityList(items: presentation.items, style: .plain)
        }
        ResultSection(presentation: presentation, emptyRunningText: "Waiting for an answer", hasArtifact: !presentation.items.isEmpty, previewAlreadyShown: true)
    }
}

// MARK: Reasoning (think): a thought, set like the reasoning rows, not like a tool result

private struct ReasoningBody: View {
    let presentation: ToolPresentation

    var body: some View {
        if let thought = presentation.contentPreview, !thought.isEmpty {
            AssistantTextView(text: thought)
                .font(.callout)
                .foregroundStyle(.secondary)
                .padding(.leading, Metrics.m)
                .frame(maxWidth: .infinity, alignment: .leading)
                .overlay(alignment: .leading) { RoundedRectangle(cornerRadius: 1).fill(.quaternary).frame(width: 2) }
        }
        ResultSection(presentation: presentation, emptyRunningText: "Thinking…", hasArtifact: presentation.contentPreview?.isEmpty == false, previewAlreadyShown: true)
    }
}

// MARK: Generic (user, MCP, API and other tools without a family)

private struct GenericBody: View {
    let presentation: ToolPresentation

    var body: some View {
        if let unlinked = presentation.fields.first(where: { $0.name == "Linkage" }) {
            Label(unlinked.value, systemImage: "link.badge.plus")
                .font(.caption)
                .foregroundStyle(.secondary)
                .accessibilityElement(children: .combine)
        }
        if presentation.showsNameBesideVerb {
            Text(presentation.name)
                .font(.monoSmall)
                .foregroundStyle(.secondary)
                .textSelection(.enabled)
        }
        FactsGrid(fields: presentation.bodyFacts.filter { $0.name != "Linkage" })
        if !presentation.items.isEmpty {
            SectionLine(title: presentation.resultLabel, detail: presentation.bodySummary, copyText: nil)
            EntityList(items: presentation.items, style: .plain)
            TruncationNote(presentation: presentation)
        }
        if let text = presentation.contentPreview, !text.isEmpty {
            ProseBlock(title: presentation.resultLabel, text: text, truncated: presentation.outputWasTruncated, full: presentation.fullDiagnosticOutput)
        }
        ResultSection(presentation: presentation, emptyRunningText: "Running…", hasArtifact: !presentation.items.isEmpty || presentation.contentPreview?.isEmpty == false)
    }
}

private extension Array where Element == ToolPresentation.Entity {
    var copyText: String {
        map { [$0.title, $0.subtitle, $0.detail].compactMap { $0 }.joined(separator: "  ") }.joined(separator: "\n")
    }
}

// MARK: - Shared blocks

/// Semantic result lines as one selectable block; running without output is progress, not an empty terminal.
/// When the body already shows an artifact, a silent success says nothing more.
private struct ResultSection: View {
    let presentation: ToolPresentation
    let emptyRunningText: String
    var hasArtifact = false
    var previewAlreadyShown = false

    var body: some View {
        if !presentation.result.isEmpty {
            VStack(alignment: .leading, spacing: Metrics.xs) {
                SectionLine(title: presentation.resultLabel, detail: presentation.bodySummary,
                            copyText: presentation.result.map(\.text).joined(separator: "\n"),
                            fullCopyText: presentation.outputWasTruncated ? presentation.fullDiagnosticOutput : nil)
                LineBlock(lines: presentation.result)
                if !hasArtifact { TruncationNote(presentation: presentation) }
            }
        } else {
            switch presentation.status {
            case .running:
                HStack(spacing: Metrics.s) {
                    RunningGlyph()
                    Text(emptyRunningText)
                }
                .font(.caption)
                .foregroundStyle(.secondary)
            case .preparing:
                Text("Preparing").font(.caption).foregroundStyle(.secondary)
            case .approval:
                Text("Waiting for your approval").font(.caption).foregroundStyle(Palette.signal)
            case .denied:
                Text("Not run").font(.caption).foregroundStyle(.secondary)
            case .failed:
                if !hasArtifact { Text("Failed without output").font(.caption).foregroundStyle(.red) }
            case .succeeded, .pending:
                // Say nothing rather than "No output": history can carry the result in a separate row.
                EmptyView()
            }
        }
    }
}

/// Title row above a block: what the block is, a truthful count, and the visible copy control.
private struct SectionLine: View {
    let title: String
    let detail: String?
    let copyText: String?
    var fullCopyText: String? = nil

    var body: some View {
        HStack(alignment: .center, spacing: Metrics.s) {
            Text(title).font(.caption).foregroundStyle(.secondary)
            if let detail {
                Text(detail).font(.figureSmall).foregroundStyle(.tertiary)
            }
            Spacer(minLength: 0)
            if let copyText, !copyText.isEmpty {
                CopyControl(text: fullCopyText ?? copyText, label: fullCopyText != nil ? "Copy full output" : "Copy")
            }
        }
    }
}

/// Icon-only copy in an honest 44pt slot; the section title sits beside it, so the row is the control's height.
private struct CopyControl: View {
    let text: String
    let label: String
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
            Image(systemName: copied ? "checkmark" : "doc.on.doc")
                .contentTransition(.symbolEffect(.replace))
                .font(.caption)
                .frame(width: Metrics.target, height: Metrics.target)
                .contentShape(.rect)
        }
        .buttonStyle(.borderless)
        .accessibilityLabel(copied ? "Copied" : label)
        .help(label)
    }
}

private struct LineBlock: View {
    let lines: [ToolPresentation.ResultLine]

    var body: some View {
        Text(attributed)
            .font(.monoSmall)
            .textSelection(.enabled)
            .frame(maxWidth: .infinity, alignment: .leading)
            .padding(Metrics.s)
            .background(.fill.quaternary, in: .rect(cornerRadius: Metrics.cardRadius, style: .continuous))
    }

    private var attributed: AttributedString {
        var result = AttributedString()
        for (index, line) in lines.enumerated() {
            var piece = AttributedString(line.text)
            switch line.kind {
            case .failure, .removal: piece.foregroundColor = .red
            case .success, .addition: piece.foregroundColor = .green
            case .heading, .path: piece.foregroundColor = .secondary
            case .match: piece.font = .monoSmall.weight(.semibold)
            case .active: piece.foregroundColor = Palette.harbor
            case .plain: break
            }
            if index > 0 { result.append(AttributedString("\n")) }
            result.append(piece)
        }
        return result
    }
}

/// File contents or a written body: real code, so it scrolls sideways instead of wrapping.
private struct CodePreview: View {
    let text: String
    let title: String
    let truncated: Bool
    let full: String?

    var body: some View {
        VStack(alignment: .leading, spacing: Metrics.xs) {
            SectionLine(title: title, detail: nil, copyText: text, fullCopyText: truncated ? full : nil)
            ScrollView(.horizontal) {
                Text(text)
                    .font(.monoSmall)
                    .textSelection(.enabled)
                    .padding(Metrics.s)
            }
            .scrollBounceBehavior(.basedOnSize)
            .background(.fill.quaternary, in: .rect(cornerRadius: Metrics.cardRadius, style: .continuous))
            if truncated {
                Text("Showing the beginning; copy for the full contents").font(.caption2).foregroundStyle(.tertiary)
            }
        }
    }
}

/// Task text, messages, plan bodies and fetched pages read as prose, in the body face.
private struct ProseBlock: View {
    let title: String
    let text: String
    let truncated: Bool
    let full: String?

    var body: some View {
        VStack(alignment: .leading, spacing: Metrics.xs) {
            SectionLine(title: title, detail: nil, copyText: text, fullCopyText: truncated ? full : nil)
            // Settled Markdown through the message renderer's cache; the copy above stays the literal text.
            AssistantTextView(text: text)
                .font(.callout)
                .frame(maxWidth: .infinity, alignment: .leading)
                .padding(Metrics.m)
                .background(.fill.quaternary, in: .rect(cornerRadius: Metrics.cardRadius, style: .continuous))
            if truncated {
                Text("Showing the beginning; copy for the full text").font(.caption2).foregroundStyle(.tertiary)
            }
        }
    }
}

private struct DiffView: View {
    let lines: [ToolPresentation.ResultLine]

    var body: some View {
        VStack(alignment: .leading, spacing: 0) {
            ForEach(lines) { line in
                DiffRow(line: line)
            }
        }
        .padding(.vertical, Metrics.xs)
        .background(.fill.quaternary, in: .rect(cornerRadius: Metrics.cardRadius, style: .continuous))
        .clipShape(.rect(cornerRadius: Metrics.cardRadius, style: .continuous))
    }
}

private struct DiffRow: View {
    let line: ToolPresentation.ResultLine

    var body: some View {
        let tint: Color? = Self.tint(line.kind)
        let band: Color = tint?.opacity(0.12) ?? .clear
        let signColor: Color = tint ?? .secondary
        let textColor: Color = line.kind == .heading ? .secondary : .primary
        HStack(alignment: .firstTextBaseline, spacing: Metrics.s) {
            Text(Self.sign(line.kind))
                .foregroundStyle(signColor)
                .frame(width: 10)
            Text(line.text)
                .textSelection(.enabled)
                .foregroundStyle(textColor)
                .frame(maxWidth: .infinity, alignment: .leading)
        }
        .font(.monoSmall)
        .padding(.horizontal, Metrics.s)
        .padding(.vertical, 1)
        .background(band)
        .accessibilityElement(children: .combine)
        .accessibilityLabel(Self.prefix(line.kind) + line.text)
    }

    static func sign(_ kind: ToolPresentation.ResultLine.Kind) -> String {
        switch kind {
        case .addition: "+"
        case .removal: "\u{2212}"
        case .heading: "@"
        default: " "
        }
    }

    static func tint(_ kind: ToolPresentation.ResultLine.Kind) -> Color? {
        switch kind {
        case .addition: .green
        case .removal: .red
        default: nil
        }
    }

    static func prefix(_ kind: ToolPresentation.ResultLine.Kind) -> String {
        switch kind {
        case .addition: "Added: "
        case .removal: "Removed: "
        case .heading: "Hunk: "
        default: ""
        }
    }
}

/// Short facts in one wrapping line: "lines 10–30", "status 200", "exit 1".
private struct FactsLine: View {
    let fields: [ToolPresentation.Field]

    var body: some View {
        if !fields.isEmpty {
            ViewThatFits(in: .horizontal) {
                HStack(spacing: Metrics.m) { facts }
                VStack(alignment: .leading, spacing: Metrics.xxs) { facts }
            }
            .font(.caption)
            .lineLimit(1)
        }
    }

    private var facts: some View {
        ForEach(fields) { field in
            HStack(spacing: Metrics.xs) {
                Text(field.name).foregroundStyle(.secondary)
                Text(field.value).monospacedDigit().truncationMode(.middle)
            }
            .accessibilityElement(children: .combine)
        }
    }
}

/// Generic tools: readable argument fields as a two-column grid, values selectable.
private struct FactsGrid: View {
    let fields: [ToolPresentation.Field]

    var body: some View {
        if !fields.isEmpty {
            Grid(alignment: .topLeading, horizontalSpacing: Metrics.m, verticalSpacing: Metrics.xs) {
                ForEach(fields) { field in
                    GridRow {
                        Text(field.name)
                            .font(.caption)
                            .foregroundStyle(.secondary)
                            .gridColumnAlignment(.trailing)
                        Text(field.value)
                            .font(.callout)
                            .textSelection(.enabled)
                            .lineLimit(6)
                            .frame(maxWidth: .infinity, alignment: .leading)
                    }
                    .accessibilityElement(children: .combine)
                }
            }
        }
    }
}

private struct TruncationNote: View {
    let presentation: ToolPresentation

    var body: some View {
        if presentation.outputWasTruncated {
            HStack(spacing: Metrics.s) {
                Text("Showing the first \(presentation.items.isEmpty ? presentation.result.count : presentation.items.count)")
                    .font(.caption2)
                    .foregroundStyle(.tertiary)
                if let full = presentation.fullDiagnosticOutput {
                    Button {
                        Clipboard.copy(full)
                    } label: {
                        Text("Copy full output")
                            .font(.caption2.weight(.medium))
                            .frame(minHeight: Metrics.target)
                            .contentShape(.rect)
                    }
                    .buttonStyle(.borderless)
                }
            }
        }
    }
}

// MARK: Entity lists

private struct EntityList: View {
    enum Style { case paths, hits, locations, agents, checklist, commits, plans, notes, schedules, sessions, catalog, plain }

    let items: [ToolPresentation.Entity]
    let style: Style

    var body: some View {
        VStack(alignment: .leading, spacing: style == .hits ? Metrics.s : Metrics.xs) {
            ForEach(items) { item in
                row(item)
            }
        }
    }

    @ViewBuilder
    private func row(_ item: ToolPresentation.Entity) -> some View {
        switch style {
        case .paths:
            HStack(alignment: .firstTextBaseline, spacing: Metrics.s) {
                Text(item.title).font(.monoSmall).textSelection(.enabled).lineLimit(1).truncationMode(.middle)
                if let subtitle = item.subtitle { Text(subtitle).font(.caption).foregroundStyle(.secondary).lineLimit(1) }
                Spacer(minLength: 0)
                factsTrail(item.facts)
            }
            .accessibilityElement(children: .combine)
        case .hits, .locations:
            VStack(alignment: .leading, spacing: Metrics.xxs) {
                HStack(alignment: .firstTextBaseline, spacing: Metrics.s) {
                    if style == .locations, let state = item.state { SeverityGlyph(state: state) }
                    Text(item.title).font(.monoSmall).foregroundStyle(.secondary).lineLimit(1).truncationMode(.middle)
                    if let subtitle = item.subtitle { Text(subtitle).font(.caption).foregroundStyle(.secondary).lineLimit(1) }
                    Spacer(minLength: 0)
                    if style == .locations, let state = item.state { StateTag(state: state) }
                }
                if let detail = item.detail {
                    Text(detail)
                        .font(.monoSmall)
                        .textSelection(.enabled)
                        .lineLimit(4)
                        .frame(maxWidth: .infinity, alignment: .leading)
                        .padding(.horizontal, Metrics.s)
                        .padding(.vertical, Metrics.xs)
                        .background(.fill.quaternary, in: .rect(cornerRadius: Metrics.chipRadius, style: .continuous))
                }
            }
            .accessibilityElement(children: .combine)
        case .agents:
            HStack(alignment: .firstTextBaseline, spacing: Metrics.s) {
                AgentDot(name: item.title)
                Text(item.title).font(.callout.weight(.medium)).lineLimit(1)
                if let subtitle = item.subtitle { Text(subtitle).font(.caption).foregroundStyle(.secondary).lineLimit(1) }
                Spacer(minLength: 0)
                if let state = item.state { StateTag(state: state) }
            }
            .accessibilityElement(children: .combine)
        case .checklist:
            HStack(alignment: .firstTextBaseline, spacing: Metrics.s) {
                ChecklistGlyph(state: item.state)
                VStack(alignment: .leading, spacing: Metrics.xxs) {
                    Text(item.title)
                        .font(.callout)
                        .strikethrough(ChecklistGlyph.isDone(item.state), color: .secondary)
                        .foregroundStyle(ChecklistGlyph.isDone(item.state) ? AnyShapeStyle(.secondary) : AnyShapeStyle(.primary))
                    if let subtitle = item.subtitle { Text(subtitle).font(.caption).foregroundStyle(.secondary) }
                    if let detail = item.detail { Text(detail).font(.caption).foregroundStyle(.secondary).lineLimit(3) }
                }
                Spacer(minLength: 0)
                factsTrail(item.facts)
            }
            .accessibilityElement(children: .combine)
            .accessibilityValue(item.state ?? "")
        case .commits:
            HStack(alignment: .firstTextBaseline, spacing: Metrics.s) {
                Text(item.title).font(.monoSmall).foregroundStyle(.secondary).lineLimit(1)
                Text(item.subtitle ?? item.detail ?? "").font(.callout).lineLimit(2)
                Spacer(minLength: 0)
                factsTrail(item.facts)
            }
            .accessibilityElement(children: .combine)
        case .plans:
            HStack(alignment: .firstTextBaseline, spacing: Metrics.s) {
                Text(item.title).font(.callout.weight(.medium)).lineLimit(1)
                if let subtitle = item.subtitle { Text(subtitle).font(.caption).foregroundStyle(.secondary).lineLimit(1) }
                Spacer(minLength: 0)
                if let state = item.state { StateTag(state: state) }
            }
            .accessibilityElement(children: .combine)
        case .notes, .schedules, .sessions, .catalog:
            HStack(alignment: .firstTextBaseline, spacing: Metrics.s) {
                Image(systemName: Self.symbol(for: style))
                    .font(.caption)
                    .foregroundStyle(.secondary)
                    .frame(width: 16)
                VStack(alignment: .leading, spacing: Metrics.xxs) {
                    Text(item.title).font(.callout).lineLimit(2)
                    if let subtitle = item.subtitle { Text(subtitle).font(.caption).foregroundStyle(.secondary).lineLimit(2) }
                    if let detail = item.detail { Text(detail).font(.caption).foregroundStyle(.secondary).lineLimit(3) }
                }
                Spacer(minLength: 0)
                if let state = item.state { StateTag(state: state) } else { factsTrail(item.facts) }
            }
            .accessibilityElement(children: .combine)
        case .plain:
            VStack(alignment: .leading, spacing: Metrics.xxs) {
                HStack(alignment: .firstTextBaseline, spacing: Metrics.s) {
                    Text(item.title).font(.callout).lineLimit(2)
                    if let subtitle = item.subtitle { Text(subtitle).font(.caption).foregroundStyle(.secondary).lineLimit(1) }
                    Spacer(minLength: 0)
                    if let state = item.state { StateTag(state: state) }
                }
                if let detail = item.detail { Text(detail).font(.caption).foregroundStyle(.secondary).lineLimit(3) }
                if !item.facts.isEmpty { FactsLine(fields: item.facts) }
            }
            .accessibilityElement(children: .combine)
        }
    }

    private static func symbol(for style: Style) -> String {
        switch style {
        case .notes: "text.quote"
        case .schedules: "clock"
        case .sessions: "bubble.left.and.bubble.right"
        case .catalog: "puzzlepiece.extension"
        default: "circle"
        }
    }

    @ViewBuilder
    private func factsTrail(_ facts: [ToolPresentation.Field]) -> some View {
        if let first = facts.first {
            Text(first.value)
                .font(.figureSmall)
                .foregroundStyle(.tertiary)
                .lineLimit(1)
                .accessibilityLabel("\(first.name) \(first.value)")
        }
    }
}

/// Source state as written, with colour only where the vocabulary is unmistakable.
private struct StateTag: View {
    let state: String

    var body: some View {
        Text(state)
            .font(.caption)
            .foregroundStyle(tint)
            .lineLimit(1)
    }

    private var tint: Color {
        switch state.lowercased() {
        case "running", "working", "in-progress", "in_progress", "active": Palette.harbor
        case "failed", "error", "errored": .red
        case "warning", "needs attention", "waiting", "blocked", "attention": Palette.signal
        case "enabled", "accepted", "done", "completed", "succeeded": .secondary
        default: .secondary
        }
    }
}

/// Diagnostic severity as the language server states it.
private struct SeverityGlyph: View {
    let state: String

    var body: some View {
        switch state.lowercased() {
        case "error": Image(systemName: "xmark.octagon.fill").foregroundStyle(.red)
        case "warning": Image(systemName: "exclamationmark.triangle.fill").foregroundStyle(.yellow)
        case "information", "info": Image(systemName: "info.circle").foregroundStyle(Palette.harbor)
        case "hint": Image(systemName: "lightbulb").foregroundStyle(.secondary)
        default: EmptyView()
        }
    }
}

private struct ChecklistGlyph: View {
    let state: String?

    static func isDone(_ state: String?) -> Bool {
        ["completed", "complete", "done", "closed"].contains(state?.lowercased() ?? "")
    }

    var body: some View {
        switch state?.lowercased() {
        case let s? where Self.isDone(s):
            Image(systemName: "checkmark.circle.fill").foregroundStyle(.secondary)
        case "in-progress", "in_progress", "active", "running", "started":
            Image(systemName: "circle.lefthalf.filled").foregroundStyle(Palette.harbor)
        case "failed", "error", "cancelled", "canceled":
            Image(systemName: "xmark.circle").foregroundStyle(.red)
        case "blocked":
            Image(systemName: "pause.circle").foregroundStyle(Palette.signal)
        default:
            Image(systemName: "circle").foregroundStyle(.tertiary)
        }
    }
}

// MARK: Raw details

/// Opt-in diagnostics: the arguments and output exactly as provided, never the primary reading.
private struct RawDetails: View {
    let presentation: ToolPresentation
    @Binding var isOn: Bool
    @Environment(\.accessibilityReduceMotion) private var reduceMotion

    private static let displayLimit = 8_000

    private var hasAnything: Bool {
        presentation.diagnosticArguments?.isEmpty == false || presentation.diagnosticOutput?.isEmpty == false
    }

    var body: some View {
        if hasAnything {
            VStack(alignment: .leading, spacing: Metrics.s) {
                Button {
                    withAnimation(Motion.reveal(reduceMotion)) { isOn.toggle() }
                } label: {
                    HStack(spacing: Metrics.xs) {
                        Text("Raw details")
                        Image(systemName: "chevron.right")
                            .font(.caption2.weight(.semibold))
                            .rotationEffect(.degrees(isOn ? 90 : 0))
                    }
                    .font(.caption)
                    .foregroundStyle(.tertiary)
                    .frame(minHeight: Metrics.target)
                    .contentShape(.rect)
                }
                .buttonStyle(.plain)
                .accessibilityHint(isOn ? "Hides raw arguments and output" : "Shows raw arguments and output")
                .accessibilityIdentifier("tool-row-diagnostics")

                if isOn {
                    VStack(alignment: .leading, spacing: Metrics.s) {
                        HStack(spacing: Metrics.xs) {
                            Text("Tool").font(.caption).foregroundStyle(.secondary)
                            Text(presentation.name).font(.monoSmall).textSelection(.enabled)
                        }
                        if let arguments = presentation.diagnosticArguments, !arguments.isEmpty {
                            rawBlock("Arguments", arguments)
                        }
                        if let output = presentation.diagnosticOutput, !output.isEmpty {
                            rawBlock("Output", output)
                        }
                    }
                    .transition(.opacity)
                }
            }
        }
    }

    private func rawBlock(_ title: String, _ text: String) -> some View {
        VStack(alignment: .leading, spacing: Metrics.xs) {
            SectionLine(title: title, detail: text.count > Self.displayLimit ? "first \(Self.displayLimit.formatted()) characters shown" : nil, copyText: text)
            ScrollView(.horizontal) {
                Text(text.count > Self.displayLimit ? String(text.prefix(Self.displayLimit)) + "…" : text)
                    .font(.monoSmall)
                    .textSelection(.enabled)
                    .padding(Metrics.s)
            }
            .scrollBounceBehavior(.basedOnSize)
            .background(.fill.quaternary, in: .rect(cornerRadius: Metrics.cardRadius, style: .continuous))
        }
    }
}

// MARK: - Context menu

/// Long-press / right-click: copies of the meaningful parts, the same expand and raw toggles, and branching.
/// Nothing here runs anything.
private struct ToolActions: View {
    let presentation: ToolPresentation
    let isExpanded: Bool
    @Binding var showsRawDetails: Bool
    let onToggle: () -> Void
    let onBranch: (() -> Void)?

    var body: some View {
        if let command = presentation.command {
            Button("Copy Command", systemImage: "terminal") { Clipboard.copy(command) }
        }
        if let target = presentation.target {
            Button(targetCopyTitle, systemImage: targetCopySymbol) { Clipboard.copy(target) }
        }
        if let query = presentation.query, presentation.family == .search || presentation.family == .code {
            Button("Copy Query", systemImage: "magnifyingglass") { Clipboard.copy(query) }
        }
        if let output = copyableOutput {
            Button(output.title, systemImage: "doc.on.doc") { Clipboard.copy(output.text) }
        }
        Divider()
        Button(isExpanded ? "Collapse" : "Expand", systemImage: isExpanded ? "chevron.up" : "chevron.down", action: onToggle)
        if presentation.diagnosticArguments?.isEmpty == false || presentation.diagnosticOutput?.isEmpty == false {
            Toggle("Raw Details", systemImage: "curlybraces", isOn: $showsRawDetails)
        }
        if let onBranch {
            Divider()
            Button("New Session Up to Here…", systemImage: "arrow.triangle.branch", action: onBranch)
        }
    }

    private var targetCopyTitle: String {
        switch presentation.family {
        case .fetch: "Copy URL"
        case .delegation, .communication: "Copy Agent Name"
        case .fileRead, .fileWrite, .fileEdit, .directory, .search, .code, .skill: "Copy Path"
        default: "Copy Target"
        }
    }

    private var targetCopySymbol: String {
        switch presentation.family {
        case .fetch: "link"
        case .delegation, .communication: "person"
        default: "folder"
        }
    }

    private var copyableOutput: (title: String, text: String)? {
        if let full = presentation.fullDiagnosticOutput, !full.isEmpty {
            return (presentation.outputWasTruncated ? "Copy Full Output" : "Copy Output", full)
        }
        if !presentation.patch.isEmpty { return ("Copy Diff", presentation.patch.map(\.text).joined(separator: "\n")) }
        if !presentation.requestedEdits.isEmpty {
            return ("Copy Requested Edits", presentation.requestedEdits.map { "- \($0.oldText)\n+ \($0.newText)" }.joined(separator: "\n\n"))
        }
        if !presentation.result.isEmpty { return ("Copy Output", presentation.result.map(\.text).joined(separator: "\n")) }
        if let preview = presentation.contentPreview, !preview.isEmpty { return ("Copy Shown Text", preview) }
        if let raw = presentation.diagnosticOutput, !raw.isEmpty { return ("Copy Output", raw) }
        return nil
    }
}

// MARK: - Helpers

private enum CommandText {
    /// First meaningful line of a script plus how many more follow, so a multi-line command still scans as one row.
    static func preview(_ command: String) -> (firstLine: String, moreLines: Int) {
        let lines = command.split(omittingEmptySubsequences: false, whereSeparator: \.isNewline)
            .map { $0.trimmingCharacters(in: .whitespaces) }
            .filter { !$0.isEmpty && !$0.hasPrefix("#!") }
        guard let first = lines.first else { return (command.trimmingCharacters(in: .whitespacesAndNewlines), 0) }
        return (first, lines.count - 1)
    }
}

private extension String {
    var firstMeaningfulLine: String? {
        split(whereSeparator: \.isNewline).map { $0.trimmingCharacters(in: .whitespaces) }.first { !$0.isEmpty }
    }
}

extension ToolPresentation {
    /// Stable, content-free token for UI tests: the family case name (e.g. "command", "fileEdit").
    var familyIdentifier: String { String(describing: family) }

    var accessibilityTitle: String {
        switch family {
        case .command: command.map { "Run command \($0)" } ?? verb
        case .delegation, .communication: [verb, target].compactMap { $0 }.joined(separator: " ")
        case .search, .code: [verb, query ?? target, context.map { "in \($0)" }].compactMap { $0 }.joined(separator: " ")
        case .prompt, .reasoning: [verb, contentPreview?.firstMeaningfulLine].compactMap { $0 }.joined(separator: " ")
        default: [verb, target ?? (family == .generic && showsNameBesideVerb ? name : nil)].compactMap { $0 }.joined(separator: " ")
        }
    }

    var accessibilityStatus: String {
        [statusLabel, status == .succeeded ? resultSummary : nil].compactMap { $0 }.joined(separator: ", ")
    }
}
