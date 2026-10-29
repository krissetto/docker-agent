import SwiftUI
import AgentKit
import CoreFoundation

struct UserTurnCard: View {
    let item: TranscriptItem

    /// Agent-to-agent input is part of the work log and reads full width; what *you* said sits on the
    /// trailing side, like a reply, and hugs its own length. Unknown origins stay neutral and full width.
    private var isHuman: Bool { UserTurnLayout.isHuman(origin: item.origin) }
    private var isAgent: Bool { item.origin == "agent" }

    /// One short line hugs its measured width; anything longer or multi-line wraps inside the cap. The width
    /// comes from the text, not from probing layout, so prose never truncates and nothing collapses.
    private var cap: CGFloat {
        guard item.attachments.isEmpty, UserTurnLayout.hugsWidth(item.text) else { return Metrics.userTurnMaxWidth }
        return min(Metrics.userTurnMaxWidth, UserTurnLayout.naturalWidth(of: item.text) + 2 * Metrics.m)
    }

    var body: some View {
        if isHuman {
            card
                .frame(maxWidth: cap, alignment: .trailing)
                .frame(maxWidth: .infinity, alignment: .trailing)
        } else {
            card.frame(maxWidth: .infinity, alignment: .leading)
        }
    }

    private var card: some View {
        VStack(alignment: .leading, spacing: Metrics.xs) {
            if isAgent {
                HStack(spacing: Metrics.xs) {
                    Text("from")
                    if let owner = AgentMessageAttribution.namedOwner(item.owner) {
                        AgentLabel(name: owner, font: .caption.weight(.semibold))
                    } else {
                        Text("another agent").font(.caption.weight(.semibold))
                    }
                }
                .font(.caption)
                .foregroundStyle(.secondary)
            }
            if !item.text.isEmpty {
                // Prose, lists and code stay leading-aligned inside the card; only the card moves.
                AssistantTextView(text: item.text)
            }
            if !item.attachments.isEmpty {
                AttachmentSummaryList(attachments: item.attachments)
            }
            if isQueued {
                // Accepted by the server, not started yet; the badge clears when the turn is promoted.
                Label("Queued", systemImage: "tray.full")
                    .font(.caption.weight(.medium))
                    .foregroundStyle(.secondary)
                    .frame(maxWidth: .infinity, alignment: .trailing)
            }
        }
        .frame(maxWidth: isHuman ? nil : .infinity, alignment: .leading)
        .padding(Metrics.m)
        // Harbor is your colour; agent-to-agent input keeps the neutral fill so it never reads as yours.
        // A queued message is drawn lighter until the agent picks it up.
        .cardBackground(isHuman ? AnyShapeStyle(Palette.harbor.opacity(isQueued ? 0.05 : 0.1)) : AnyShapeStyle(.fill.tertiary))
        .overlay {
            if isQueued {
                RoundedRectangle(cornerRadius: Metrics.cardRadius, style: .continuous)
                    .strokeBorder(Palette.harbor.opacity(0.35), style: StrokeStyle(lineWidth: 1, dash: [4, 3]))
            }
        }
        .accessibilityElement(children: .combine)
        .accessibilityLabel(isHuman ? (isQueued ? "You, queued" : "You") : isAgent ? "Message from \(AgentMessageAttribution.accessibilityName(item.owner))" : "Message")
        .accessibilityValue(item.text)
    }

    private var isQueued: Bool { item.userInputState == .queued }
}

enum UserTurnLayout {
    /// Only input the server attributes to a person (no origin, or "user") reads as yours; agent and
    /// runtime input never do, and unrecognised origins stay neutral rather than guessing.
    static func isHuman(origin: String?) -> Bool {
        guard let origin = origin?.trimmingCharacters(in: .whitespacesAndNewlines), !origin.isEmpty else { return true }
        return origin == "user"
    }

    /// Short single-line text (fits one line even on the mini) keeps its natural width.
    static let hugCharacterLimit = 36
    static func hugsWidth(_ text: String) -> Bool {
        !text.contains("\n") && text.count <= hugCharacterLimit
    }

    /// Body-font width of one line of text, with a little slack for inline Markdown emphasis.
    static func naturalWidth(of text: String) -> CGFloat {
        #if canImport(UIKit)
        let font = UIFont.preferredFont(forTextStyle: .body)
        #else
        let font = NSFont.preferredFont(forTextStyle: .body)
        #endif
        let width = (text as NSString).size(withAttributes: [.font: font]).width
        return (width * 1.04).rounded(.up) + 2
    }
}

enum AgentMessageAttribution {
    static func namedOwner(_ owner: String?) -> String? {
        guard let owner = owner?.trimmingCharacters(in: .whitespacesAndNewlines), !owner.isEmpty else { return nil }
        return owner
    }

    static func accessibilityName(_ owner: String?) -> String {
        namedOwner(owner) ?? "another agent"
    }
}

struct AssistantTextView: View {
    let text: String
    var isStreaming = false

    @Environment(\.accessibilityReduceMotion) private var reduceMotion
    @State private var reveal = StreamingTextReveal()
    @State private var displayedText = ""
    @State private var streamingBlocks: [MarkdownCache.Rendered] = []
    @State private var parsedStreamingText = ""
    @State private var completedBlocks: [MarkdownCache.Rendered] = []
    @State private var parsedCompletedText = ""
    @State private var streamingMarkdown = MarkdownStreamingBlocks()
    @State private var committedStreamingBlocks: [MarkdownCache.Rendered] = []

    private var showsStreamingText: Bool { isStreaming && !reduceMotion }

    var body: some View {
        VStack(alignment: .leading, spacing: Metrics.s) {
            if isStreaming {
                ForEach(Array(streamingBlocks.enumerated()), id: \.offset) { _, block in
                    blockView(block)
                }
                Image(systemName: "ellipsis")
                    .symbolEffect(.variableColor.iterative, options: .repeating, isActive: !reduceMotion)
                    .foregroundStyle(.secondary)
                    .accessibilityLabel("Writing")
            } else {
                ForEach(Array(completedBlocks.enumerated()), id: \.offset) { _, block in
                    blockView(block)
                }
            }
        }
        .onAppear {
            displayedText = reveal.reset(text)
            if isStreaming { refreshStreamingMarkdown(text) } else {
                refreshCompletedMarkdown(text)
                clearStreamingState()
            }
        }
        .onChange(of: text) { synchronizeReveal() }
        .onChange(of: isStreaming) { synchronizeReveal() }
        .onChange(of: reduceMotion) { synchronizeReveal() }
        .task(id: showsStreamingText) {
            guard showsStreamingText else { return }
            while !Task.isCancelled && showsStreamingText {
                try? await Task.sleep(for: StreamingTextReveal.frameInterval)
                guard !Task.isCancelled else { return }
                if showsStreamingText, reveal.hasPendingText {
                    let next = reveal.advance()
                    if next != displayedText { displayedText = next }
                }
            }
        }
        .task(id: isStreaming) {
            guard isStreaming else { return }
            while !Task.isCancelled {
                try? await Task.sleep(for: .milliseconds(140))
                guard !Task.isCancelled else { return }
                refreshStreamingMarkdown(showsStreamingText ? displayedText : text)
            }
        }
    }

    private func synchronizeReveal() {
        displayedText = reveal.update(text, isStreaming: isStreaming, reduceMotion: reduceMotion)
        if isStreaming {
            refreshStreamingMarkdown(showsStreamingText ? displayedText : text)
        } else {
            refreshCompletedMarkdown(text)
            clearStreamingState()
        }
    }

    private func refreshCompletedMarkdown(_ value: String) {
        guard value != parsedCompletedText else { return }
        completedBlocks = MarkdownCache.rendered(value)
        parsedCompletedText = value
    }

    private func clearStreamingState() {
        reveal.discardTransientBuffers()
        displayedText = ""
        streamingBlocks.removeAll(keepingCapacity: false)
        parsedStreamingText = ""
        streamingMarkdown.reset(releasingCapacity: true)
        committedStreamingBlocks.removeAll(keepingCapacity: false)
    }

    private func refreshStreamingMarkdown(_ value: String) {
        guard value != parsedStreamingText else { return }
        if !value.hasPrefix(parsedStreamingText) {
            streamingMarkdown.reset()
            committedStreamingBlocks = []
        }
        // Commit only closed paragraphs/fenced blocks. Their markdown is parsed once; only the
        // small live tail is reparsed at the throttle interval. No token prefixes enter the cache.
        let update = streamingMarkdown.update(value)
        if !update.newlyClosed.isEmpty {
            committedStreamingBlocks.append(contentsOf: MarkdownCache.rendered(update.newlyClosed, cache: false))
        }
        let tail = update.liveTail
        let tailBlocks: [MarkdownCache.Rendered]
        if tail.utf8.count <= MarkdownStreamingBoundary.maximumLiveTailBytes {
            tailBlocks = MarkdownCache.rendered(tail, cache: false)
        } else {
            let styledPrefix = String(tail.prefixCharacters(nearUTF8ByteCount: MarkdownStreamingBoundary.maximumLiveTailBytes))
            let remainder = String(tail.dropFirst(styledPrefix.count))
            tailBlocks = MarkdownCache.rendered(styledPrefix, cache: false) + [.prose(AttributedString(remainder))]
        }
        streamingBlocks = committedStreamingBlocks + tailBlocks
        parsedStreamingText = value
    }

    @ViewBuilder
    private func blockView(_ block: MarkdownCache.Rendered) -> some View {
        switch block {
        case .prose(let prose):
            Text(prose)
                .textSelection(.enabled)
                .lineSpacing(2)
                .frame(maxWidth: .infinity, alignment: .leading)
        case .heading(let level, let title):
            Text(title)
                .font(level == 1 ? .title2.weight(.semibold) : level == 2 ? .title3.weight(.semibold) : .headline)
                .padding(.top, level <= 2 ? Metrics.s : Metrics.xs)
                .textSelection(.enabled)
                .frame(maxWidth: .infinity, alignment: .leading)
                .accessibilityAddTraits(.isHeader)
        case .list(let items):
            VStack(alignment: .leading, spacing: Metrics.xs) {
                ForEach(Array(items.enumerated()), id: \.offset) { _, item in
                    HStack(alignment: .firstTextBaseline, spacing: Metrics.s) {
                        Text(item.marker)
                            .foregroundStyle(.secondary)
                            .monospacedDigit()
                            .frame(minWidth: Metrics.l, alignment: .trailing)
                        Text(item.text)
                            .textSelection(.enabled)
                            .frame(maxWidth: .infinity, alignment: .leading)
                    }
                    .padding(.leading, CGFloat(item.depth) * Metrics.l)
                }
            }
        case .quote(let quote):
            Text(quote)
                .foregroundStyle(.secondary)
                .textSelection(.enabled)
                .padding(.leading, Metrics.m)
                .frame(maxWidth: .infinity, alignment: .leading)
                .overlay(alignment: .leading) {
                    RoundedRectangle(cornerRadius: 1).fill(.quaternary).frame(width: 2)
                }
        case .rule:
            Divider().padding(.vertical, Metrics.xs)
        case .table(let rows):
            MarkdownTable(rows: rows)
        case .code(let language, let code):
            CodeBlock(language: language, code: code)
        }
    }
}

struct StreamingTextReveal {
    static let frameInterval: Duration = .milliseconds(34)
    static let idleFlushDelay: Duration = .milliseconds(180)
    static let maximumPendingBytes = 4 * 1024
    private static let maximumCharactersPerFrame = 48
    private static let adaptiveCharactersPerFrame = 160

    private(set) var displayedText = ""
    private var authoritativeText: String?
    private var pendingCharacters: [Character] = []
    private var pendingIndex = 0
    private var pendingBytes = 0
    private var lastAppendAt: ContinuousClock.Instant?

    var hasPendingText: Bool { pendingIndex < pendingCharacters.count }
    var retainedByteCount: Int { displayedText.utf8.count + (authoritativeText?.utf8.count ?? 0) + pendingBytes }

    mutating func discardTransientBuffers() {
        displayedText = ""
        authoritativeText = nil
        pendingCharacters.removeAll(keepingCapacity: false)
        pendingIndex = 0
        pendingBytes = 0
        lastAppendAt = nil
    }

    mutating func reset(_ text: String) -> String { revealAll(text) }

    mutating func update(_ text: String, isStreaming: Bool, reduceMotion: Bool,
                         now: ContinuousClock.Instant = .now) -> String {
        guard isStreaming, !reduceMotion else { return revealAll(text) }

        guard let previous = authoritativeText else {
            authoritativeText = text
            return revealAll(text)
        }
        guard previous != text else { return displayedText }
        guard text.utf8.starts(with: previous.utf8) else { return revealAll(text) }

        let suffix = String(decoding: text.utf8.dropFirst(previous.utf8.count), as: UTF8.self)
        authoritativeText = text
        guard !suffix.isEmpty else { return displayedText }
        pendingCharacters.append(contentsOf: suffix)
        pendingBytes += suffix.utf8.count
        lastAppendAt = now
        if pendingBytes > Self.maximumPendingBytes { return revealAll(text) }
        return displayedText
    }

    @discardableResult
    mutating func advance(now: ContinuousClock.Instant = .now) -> String {
        guard hasPendingText else { return displayedText }
        if let lastAppendAt, now - lastAppendAt >= Self.idleFlushDelay {
            return revealAll(authoritativeText ?? displayedText)
        }

        let maximum = pendingBytes > 1_024 ? Self.adaptiveCharactersPerFrame : Self.maximumCharactersPerFrame
        let end = min(pendingCharacters.count, pendingIndex + maximum)
        let chunk = String(pendingCharacters[pendingIndex..<end])
        displayedText.append(contentsOf: chunk)
        pendingBytes -= chunk.utf8.count
        pendingIndex = end
        if pendingIndex == pendingCharacters.count {
            pendingCharacters.removeAll(keepingCapacity: true)
            pendingIndex = 0
            lastAppendAt = nil
        } else if pendingIndex >= 1_024 {
            pendingCharacters.removeFirst(pendingIndex)
            pendingIndex = 0
        }
        return displayedText
    }

    private mutating func revealAll(_ text: String) -> String {
        displayedText = text
        authoritativeText = text
        pendingCharacters.removeAll(keepingCapacity: true)
        pendingIndex = 0
        pendingBytes = 0
        lastAppendAt = nil
        return text
    }
}

private struct MarkdownTable: View {
    let rows: [[AttributedString]]

    var body: some View {
        ScrollView(.horizontal) {
            Grid(alignment: .leading, horizontalSpacing: Metrics.l, verticalSpacing: Metrics.xs) {
                ForEach(Array(rows.enumerated()), id: \.offset) { index, row in
                    GridRow {
                        ForEach(Array(row.enumerated()), id: \.offset) { _, cell in
                            Text(cell)
                                .font(index == 0 ? .callout.weight(.semibold) : .callout)
                                .textSelection(.enabled)
                        }
                    }
                    if index == 0 { Divider() }
                }
            }
            .padding(Metrics.s)
        }
        .cardBackground(.fill.quaternary)
    }
}

/// Parsed markdown per message text. Bodies re-evaluate far more often than texts change,
/// and attributed parsing of a long transcript on every evaluation stalls the main thread.
extension String {
    func prefixCharacters(nearUTF8ByteCount byteLimit: Int) -> Substring {
        var end = startIndex
        var bytes = 0
        while end < endIndex {
            let next = index(after: end)
            let nextBytes = self[end..<next].utf8.count
            guard bytes + nextBytes <= byteLimit else { break }
            bytes += nextBytes
            end = next
        }
        return self[..<end]
    }
}

enum MarkdownStreamingBoundary {
    static let maximumLiveTailBytes = 8 * 1024

    struct Tracker {
        private(set) var scannedBytes = 0
        private(set) var closedBytes = 0
        private var insideFence = false
        private var line: [UInt8] = []
        private var lineTooLong = false
        private let maximumLineBytes = 8 * 1024

        var retainedByteCount: Int { line.count }

        mutating func reset(releasingCapacity: Bool = false) {
            scannedBytes = 0
            closedBytes = 0
            insideFence = false
            line.removeAll(keepingCapacity: !releasingCapacity)
            lineTooLong = false
        }

        mutating func closedPrefixEnd(in text: String) -> Int {
            let bytes = text.utf8
            guard bytes.count >= scannedBytes else { reset(); return closedPrefixEnd(in: text) }
            var index = bytes.index(bytes.startIndex, offsetBy: scannedBytes)
            while index < bytes.endIndex {
                let byte = bytes[index]
                scannedBytes += 1
                if byte == 10 {
                    if !lineTooLong {
                        let trimmed = String(decoding: line, as: UTF8.self).trimmingCharacters(in: .whitespacesAndNewlines)
                        if trimmed.hasPrefix("```") {
                            insideFence.toggle()
                            if !insideFence { closedBytes = scannedBytes }
                        } else if trimmed.isEmpty && !insideFence {
                            closedBytes = scannedBytes
                        }
                    }
                    line.removeAll(keepingCapacity: true)
                    lineTooLong = false
                } else if !lineTooLong {
                    line.append(byte)
                    if line.count > maximumLineBytes {
                        line.removeAll(keepingCapacity: false)
                        lineTooLong = true
                    }
                }
                index = bytes.index(after: index)
            }
            return closedBytes
        }
    }

    static func closedPrefixEnd(in text: String) -> Int {
        var tracker = Tracker()
        return tracker.closedPrefixEnd(in: text)
    }
}

struct MarkdownStreamingBlocks {
    struct Update {
        let newlyClosed: String
        let liveTail: String
    }

    private var tracker = MarkdownStreamingBoundary.Tracker()
    private(set) var committedPrefix = ""

    var retainedByteCount: Int { committedPrefix.utf8.count + tracker.retainedByteCount }

    mutating func reset(releasingCapacity: Bool = false) {
        tracker.reset(releasingCapacity: releasingCapacity)
        committedPrefix = ""
    }

    mutating func update(_ text: String) -> Update {
        let stableEnd = tracker.closedPrefixEnd(in: text)
        let stablePrefix = String(decoding: text.utf8.prefix(stableEnd), as: UTF8.self)
        let previousByteCount = committedPrefix.utf8.count
        let newlyClosed = stableEnd > previousByteCount
            ? String(decoding: text.utf8.dropFirst(previousByteCount).prefix(stableEnd - previousByteCount), as: UTF8.self)
            : ""
        committedPrefix = stablePrefix
        return Update(newlyClosed: newlyClosed,
                      liveTail: String(decoding: text.utf8.dropFirst(stableEnd), as: UTF8.self))
    }
}

@MainActor
enum MarkdownCache {
    enum Rendered {
        case prose(AttributedString)
        case heading(level: Int, AttributedString)
        case list([ListItem])
        case quote(AttributedString)
        case rule
        case table([[AttributedString]])
        case code(language: String, code: String)
    }

    struct ListItem {
        let marker: String
        let depth: Int
        let text: AttributedString
    }

    private static var store: [String: [Rendered]] = [:]
    private static var storedBytes = 0
    private(set) static var parseCount = 0
    private(set) static var cacheHitCount = 0
    /// Byte budget, not entry count: streaming produces a new, longer key per token.
    private static let byteLimit = 4_000_000
    private static let maxCacheableBytes = 128_000

    static func rendered(_ text: String, cache: Bool = true) -> [Rendered] {
        if cache, let hit = store[text] {
            cacheHitCount += 1
            return hit
        }
        parseCount += 1
        let result = MarkdownSegment.split(text).map(render)
        let bytes = text.utf8.count
        guard cache, bytes <= maxCacheableBytes else { return result }
        if storedBytes + bytes > byteLimit {
            store.removeAll(keepingCapacity: true)
            storedBytes = 0
        }
        store[text] = result
        storedBytes += bytes
        return result
    }

    static func resetPerformanceCounters() { parseCount = 0; cacheHitCount = 0 }

    private static func render(_ segment: MarkdownSegment) -> Rendered {
        switch segment {
        case .prose(let prose): .prose(MarkdownSegment.attributed(prose))
        case .heading(let level, let title): .heading(level: level, MarkdownSegment.attributed(title))
        case .list(let items): .list(items.map { ListItem(marker: $0.marker, depth: $0.depth, text: MarkdownSegment.attributed($0.text)) })
        case .quote(let quote): .quote(MarkdownSegment.attributed(quote))
        case .rule: .rule
        case .table(let rows): .table(rows.map { $0.map(MarkdownSegment.attributed) })
        case .code(let language, let code): .code(language: language, code: code)
        }
    }
}

enum MarkdownSegment: Equatable {
    struct ListItem: Equatable {
        let marker: String
        let depth: Int
        let text: String
    }

    case prose(String)
    case heading(level: Int, String)
    case list([ListItem])
    case quote(String)
    case rule
    case table([[String]])
    case code(language: String, code: String)

    /// Block structure only: fences, headings, lists, quotes, rules and pipe tables.
    /// Inline emphasis, links and code spans are handled by `attributed`.
    static func split(_ text: String) -> [MarkdownSegment] {
        var segments: [MarkdownSegment] = []
        var prose: [Substring] = []
        var code: [Substring] = []
        var language: String?
        var quote: [String] = []
        var list: [ListItem] = []
        var table: [Substring] = []

        func flushProse() {
            let rest = prose.joined(separator: "\n").trimmingCharacters(in: .newlines)
            if !rest.isEmpty { segments.append(.prose(rest)) }
            prose.removeAll()
        }
        func flushQuote() {
            if !quote.isEmpty { segments.append(.quote(quote.joined(separator: "\n"))) }
            quote.removeAll()
        }
        func flushList() {
            if !list.isEmpty { segments.append(.list(list)) }
            list.removeAll()
        }
        func flushTable() {
            if let parsed = parseTable(table) { segments.append(parsed) } else { prose.append(contentsOf: table); flushProse() }
            table.removeAll()
        }
        func flushBlocks() { flushQuote(); flushList(); flushTable(); flushProse() }

        for line in text.split(separator: "\n", omittingEmptySubsequences: false) {
            let trimmed = line.trimmingCharacters(in: .whitespaces)
            if trimmed.hasPrefix("```") {
                if let lang = language {
                    segments.append(.code(language: lang, code: code.joined(separator: "\n")))
                    code.removeAll()
                    language = nil
                } else {
                    flushBlocks()
                    language = String(trimmed.dropFirst(3))
                }
                continue
            }
            if language != nil { code.append(line); continue }

            if !table.isEmpty {
                if trimmed.hasPrefix("|") { table.append(line); continue }
                flushTable()
            }
            if trimmed.hasPrefix(">") {
                flushList(); flushTable(); flushProse()
                quote.append(String(trimmed.dropFirst().drop(while: { $0 == " " })))
                continue
            } else if !quote.isEmpty {
                flushQuote()
            }
            if let item = listItem(line) {
                flushQuote(); flushProse()
                list.append(item)
                continue
            } else if !list.isEmpty {
                if !trimmed.isEmpty, line.first?.isWhitespace == true, let last = list.last {
                    list[list.count - 1] = ListItem(marker: last.marker, depth: last.depth, text: last.text + " " + trimmed)
                    continue
                }
                flushList()
                if trimmed.isEmpty { continue }
            }
            if let level = headingLevel(trimmed) {
                flushBlocks()
                segments.append(.heading(level: level, String(trimmed.drop(while: { $0 == "#" }).trimmingCharacters(in: .whitespaces))))
                continue
            }
            if isRule(trimmed) {
                flushBlocks()
                segments.append(.rule)
                continue
            }
            if trimmed.hasPrefix("|") && trimmed.hasSuffix("|") {
                flushProse()
                table.append(line)
                continue
            }
            prose.append(line)
        }
        // An unterminated fence is still streaming; show it as code.
        if let lang = language { segments.append(.code(language: lang, code: code.joined(separator: "\n"))) }
        flushBlocks()
        return segments
    }

    private static func headingLevel(_ trimmed: String) -> Int? {
        let hashes = trimmed.prefix(while: { $0 == "#" }).count
        guard (1...6).contains(hashes), trimmed.dropFirst(hashes).first == " " else { return nil }
        return hashes
    }

    private static func isRule(_ trimmed: String) -> Bool {
        guard trimmed.count >= 3, let first = trimmed.first, "-*_".contains(first) else { return false }
        return trimmed.allSatisfy { $0 == first || $0 == " " } && trimmed.filter({ $0 != " " }).count >= 3
    }

    private static func listItem(_ line: Substring) -> ListItem? {
        let indent = line.prefix(while: { $0 == " " || $0 == "\t" }).count
        let body = line.dropFirst(indent)
        guard let space = body.firstIndex(of: " ") else { return nil }
        let marker = body[..<space]
        let rest = body[space...].trimmingCharacters(in: .whitespaces)
        guard !rest.isEmpty else { return nil }
        if marker == "-" || marker == "*" || marker == "+" {
            if rest.hasPrefix("[ ] ") { return ListItem(marker: "☐", depth: indent / 2, text: String(rest.dropFirst(4))) }
            if rest.lowercased().hasPrefix("[x] ") { return ListItem(marker: "☑", depth: indent / 2, text: String(rest.dropFirst(4))) }
            return ListItem(marker: "•", depth: indent / 2, text: rest)
        }
        if marker.count <= 4, let last = marker.last, last == "." || last == ")", marker.dropLast().allSatisfy(\.isNumber), !marker.dropLast().isEmpty {
            return ListItem(marker: String(marker.dropLast()) + ".", depth: indent / 2, text: rest)
        }
        return nil
    }

    /// Header, separator, rows. Returns nil for anything that doesn't look like a GFM table.
    private static func parseTable(_ lines: [Substring]) -> MarkdownSegment? {
        guard lines.count >= 2 else { return nil }
        let separator = lines[1].trimmingCharacters(in: .whitespaces)
        guard separator.allSatisfy({ "|-: ".contains($0) }), separator.contains("-") else { return nil }
        func cells(_ line: Substring) -> [String] {
            var trimmed = line.trimmingCharacters(in: .whitespaces)
            if trimmed.hasPrefix("|") { trimmed.removeFirst() }
            if trimmed.hasSuffix("|") { trimmed.removeLast() }
            return trimmed.split(separator: "|", omittingEmptySubsequences: false).map { $0.trimmingCharacters(in: .whitespaces) }
        }
        let header = cells(lines[0])
        let rows = lines.dropFirst(2).map(cells).map { row in
            row.count == header.count ? row : Array((row + Array(repeating: "", count: max(0, header.count - row.count))).prefix(header.count))
        }
        return .table([header] + rows)
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

struct ToolPresentation {
    enum Family: Equatable {
        case command, fileRead, fileWrite, fileEdit, directory, search, fetch, git
        case delegation, communication, todo, task, plan, code, memory, schedule, session, skill, configuration, integration, prompt, reasoning, generic
    }

    enum Status: Equatable {
        case preparing, running, succeeded, failed, approval, denied, pending
    }

    struct Field: Identifiable {
        let id: String
        let name: String
        let value: String
        init(_ name: String, _ value: String) { id = name; self.name = name; self.value = value }
        init(id: String, name: String, value: String) { self.id = id; self.name = name; self.value = value }
    }

    struct ResultLine: Identifiable {
        let id: Int
        let kind: Kind
        let text: String
        enum Kind: Equatable { case plain, addition, removal, heading, path, match, success, failure, active }
    }

    struct Entity: Identifiable {
        let id: String
        let title: String
        let subtitle: String?
        let detail: String?
        let state: String?
        let facts: [Field]
    }

    struct EditRequest: Identifiable {
        let id: Int
        let oldText: String
        let newText: String
    }

    struct Receipt {
        let target: String?
        let accepted: Bool?
        let durable: Bool?
        let queued: Bool?
        let disposition: String?
        let rejection: String?
        let detail: String?
    }

    let name: String
    let displayName: String
    let family: Family
    let status: Status
    let statusLabel: String
    let symbol: String
    let verb: String
    let target: String?
    let command: String?
    let query: String?
    let context: String?
    let fields: [Field]
    let result: [ResultLine]
    let resultLabel: String
    let resultSummary: String?
    let diagnosticArguments: String?
    let diagnosticOutput: String?
    let outputWasTruncated: Bool
    let items: [Entity]
    let metrics: [Field]
    let patch: [ResultLine]
    let contentPreview: String?
    let fullDiagnosticOutput: String?
    let requestedEdits: [EditRequest]
    let requestReceipt: Receipt?

    static let maximumItems = 40
    static let maximumValueCharacters = 2_000
    static let maximumResultLineCharacters = 2_000

    init(name: String?, arguments: String?, output: String?, status rawStatus: String?) {
        let toolName = name?.isEmpty == false ? name! : "tool"
        let args = Self.decode(arguments)
        let family = Self.family(for: toolName)
        let status = Self.status(rawStatus)
        let command = Self.firstString(args, keys: ["cmd", "command"])
        let target = Self.firstString(args, keys: ["path", "paths", "file_path", "file", "uri", "urls", "url", "job_id", "task_id", "agent", "to", "subagent_id", "session_id", "sessionId", "id", "name", "plan_name", "model", "skill_name", "memory", "query", "prompt", "title"])
        let query = Self.firstString(args, keys: ["query", "pattern", "search"])
        let context = Self.firstString(args, keys: ["cwd", "directory", "root", "branch", "scope"])
        let outputString = output ?? ""
        let decodedOutput = Self.decodeOutput(outputString)
        let resultObject = decodedOutput.object ?? [:]

        self.name = toolName
        self.displayName = Self.displayName(toolName)
        self.family = family
        let resultFailed = resultObject["isError"] as? Bool == true || (resultObject["error"] as? String)?.isEmpty == false
        self.status = resultFailed ? .failed : status
        self.statusLabel = Self.statusLabel(resultFailed ? .failed : status)
        self.symbol = Self.symbol(for: family)
        self.verb = Self.verb(for: family, toolName: toolName)
        self.target = target
        self.command = family == .command ? command : nil
        self.query = query ?? (family == .search ? Self.firstString(args, keys: ["memory", "prompt", "term"]) : nil)
        self.context = context
        self.diagnosticArguments = arguments
        self.diagnosticOutput = output
        self.fullDiagnosticOutput = output
        self.requestedEdits = Self.requestedEdits(args)
        self.requestReceipt = Self.receipt(resultObject)
        self.resultLabel = Self.resultLabel(for: family, toolName: toolName)
        let structuredMetricObject = resultObject.isEmpty ? (decodedOutput.array?.first as? [String: Any] ?? [:]) : resultObject
        var metrics = Self.metrics(arguments: args, result: structuredMetricObject)
        if family == .command, !metrics.contains(where: { $0.name == "Exit code" }), let exitCode = Self.explicitExitCode(in: outputString) {
            metrics.append(Field("Exit code", String(exitCode)))
        }
        self.metrics = metrics

        var fields = Self.argumentFields(args, family: family)
        if family == .generic {
            if toolName == "Unlinked tool result" {
                fields.append(Field("Linkage", "Unlinked tool result — no unambiguous matching call"))
            }
            fields.append(contentsOf: Self.structuredOutputFields(resultObject))
        }
        if let context, !fields.contains(where: { $0.value == context }) { fields.append(Field("In", context)) }
        self.fields = Array(fields.prefix(12))

        let parsedStructured = Self.entities(from: decodedOutput.value, family: family)
        let parsed = parsedStructured.isEmpty ? Self.textEntities(outputString, family: family) : parsedStructured
        self.items = Array(parsed.prefix(Self.maximumItems))

        let rawLines: [ResultLine]
        if let body = Self.firstOutputString(resultObject, keys: ["content", "output", "text", "response", "message", "body", "thought", "transcript"]) {
            rawLines = Self.resultLines(body, family: family)
        } else if outputString.isEmpty || decodedOutput.object != nil || decodedOutput.array != nil {
            rawLines = []
        } else {
            rawLines = Self.resultLines(outputString, family: family)
        }
        self.result = Array(rawLines.prefix(Self.maximumItems))
        let patchLines = family == .fileEdit ? [] : rawLines.filter { [.addition, .removal, .heading, .path].contains($0.kind) }
        self.patch = Array(patchLines.prefix(Self.maximumItems))
        let preview: String?
        if let outputContent = Self.firstOutputString(resultObject, keys: ["content", "output", "text", "response", "message", "body", "thought", "transcript"]) {
            preview = Self.bounded(outputContent, limit: Self.maximumItems * Self.maximumResultLineCharacters)
        } else if !outputString.isEmpty && decodedOutput.object == nil && decodedOutput.array == nil {
            preview = Self.bounded(outputString, limit: Self.maximumItems * Self.maximumResultLineCharacters)
        } else if family == .generic, let decodedObject = decodedOutput.object, Self.structuredOutputFields(decodedObject).isEmpty, (decodedOutput.array?.isEmpty ?? true) {
            preview = "Structured result is available in Raw details."
        } else {
            preview = nil
        }
        self.contentPreview = preview ?? Self.contentArgument(args, family: family)
        let visibleLines = Self.boundedLines(outputString, limit: Self.maximumItems + 1)
        let hasLongLine = visibleLines.contains { $0.count > Self.maximumResultLineCharacters }
        let hasMoreLines = visibleLines.count > Self.maximumItems
        self.outputWasTruncated = decodedOutput.wasTruncated || rawLines.count > Self.maximumItems || parsed.count > Self.maximumItems || hasLongLine || hasMoreLines || (preview.map { $0.count < (Self.firstOutputString(resultObject, keys: ["content", "output", "text", "response", "message", "body", "thought", "transcript"])?.count ?? outputString.count) } ?? false)
        let semanticSummary = Self.summary(family: family, toolName: toolName, args: args, result: resultObject, items: parsed, output: outputString)
        self.resultSummary = semanticSummary ?? (toolName == "Unlinked tool result" ? "Unlinked result" : nil)
    }

    private static func decode(_ json: String?) -> [String: Any] {
        guard let json, let data = json.data(using: .utf8),
              let object = try? JSONSerialization.jsonObject(with: data, options: [.fragmentsAllowed]) as? [String: Any] else { return [:] }
        return object
    }

    private static func decodeOutput(_ output: String) -> (value: Any?, object: [String: Any]?, array: [Any]?, wasTruncated: Bool) {
        guard !output.isEmpty, let data = output.data(using: .utf8),
              let value = try? JSONSerialization.jsonObject(with: data, options: [.fragmentsAllowed]) else { return (nil, nil, nil, false) }
        return (value, value as? [String: Any], value as? [Any], false)
    }

    private static func family(for name: String) -> Family {
        switch name {
        case "shell", "list_running_processes", "inspect_running_process", "kill_running_process": .command
        case "read_file", "read_multiple_files": .fileRead
        case "write_file", "create_directory", "remove_directory": .fileWrite
        case "edit_file": .fileEdit
        case "directory_tree", "list_directory": .directory
        case "search_files_content", "rag": .search
        case "fetch", "open_url": .fetch
        case "git_status", "git_log", "git_branches", "git_show", "git_blame": .git
        case "transfer_task", "handoff", "spawn_subagent", "stop_subagent", "run_background_agent", "list_background_agents", "view_background_agent", "stop_background_agent", "run_skill": .delegation
        case "send_message", "read_subagent", "list_agents", "send_webhook": .communication
        case "create_todo", "create_todos", "update_todos", "list_todos": .todo
        case "create_task", "get_task", "update_task", "delete_task", "list_tasks", "next_task", "add_dependency", "remove_dependency": .task
        case "write_plan", "read_plan", "list_plans", "delete_plan", "update_plan_from_file", "export_plan_to_file", "set_plan_status", "get_plan_status": .plan
        case "lsp_workspace", "lsp_hover", "lsp_definition", "lsp_references", "lsp_document_symbols", "lsp_workspace_symbols", "lsp_diagnostics", "lsp_rename", "lsp_code_actions", "lsp_format", "lsp_call_hierarchy", "lsp_type_hierarchy", "lsp_implementations", "lsp_signature_help", "lsp_inlay_hints": .code
        case "add_memory", "get_memories", "delete_memory", "search_memories", "update_memory": .memory
        case "create_schedule", "list_schedules", "cancel_schedule": .schedule
        case "list_sessions", "read_session": .session
        case "read_skill", "read_skill_file": .skill
        case "change_model", "revert_model", "team_edit", "team_edit_persist", "team_edit_export", "get_environment_info": .configuration
        case "search_remote_mcp_servers", "enable_remote_mcp_server", "disable_remote_mcp_server", "list_remote_mcp_servers", "reset_remote_mcp_server_auth", "search_tool", "add_tool": .integration
        case "user_prompt": .prompt
        case "think": .reasoning
        default: .generic
        }
    }

    private static func status(_ raw: String?) -> Status {
        switch raw?.lowercased() {
        case "preparing", "partial", "started": .preparing
        case "running", "in_progress": .running
        case "done", "complete", "completed", "success", "succeeded", "ok": .succeeded
        case "error", "failed", "failure": .failed
        case "approval", "confirmation": .approval
        case "rejected", "denied", "canceled", "cancelled": .denied
        default: .pending
        }
    }

    private static func statusLabel(_ status: Status) -> String {
        switch status {
        case .preparing: "Preparing"
        case .running: "Running"
        case .succeeded: "Done"
        case .failed: "Failed"
        case .approval: "Needs approval"
        case .denied: "Denied"
        case .pending: "Pending"
        }
    }

    private static func displayName(_ name: String) -> String {
        switch name {
        case "shell": "Command"
        case "list_running_processes": "List running processes"
        case "inspect_running_process": "Inspect running process"
        case "kill_running_process": "Stop running process"
        case "read_file": "Read file"
        case "read_multiple_files": "Read files"
        case "write_file": "Write file"
        case "edit_file": "Edit file"
        case "directory_tree": "Explore directory tree"
        case "list_directory": "List directory"
        case "search_files_content": "Find matches"
        case "transfer_task": "Delegate task"
        case "spawn_subagent": "Start agent"
        case "send_message": "Message agent"
        case "read_subagent": "Read agent update"
        case "list_agents": "List agents"
        case "stop_subagent": "Stop agent"
        case "fetch": "Fetch web page"
        case "git_status": "Check Git status"
        case "git_log": "Read Git history"
        case "git_branches": "List Git branches"
        case "git_show": "Inspect commit"
        case "git_blame": "Trace file history"
        case "create_todo": "Add task to list"
        case "create_todos": "Add tasks to list"
        case "update_todos": "Update task progress"
        case "list_todos": "Review task list"
        case "run_background_agent": "Start background agent"
        case "list_background_agents": "List background agents"
        case "view_background_agent": "View background agent"
        case "stop_background_agent": "Stop background agent"
        case "add_memory": "Save memory"
        case "get_memories": "Review memories"
        case "search_memories": "Find memories"
        case "update_memory": "Update memory"
        case "delete_memory": "Delete memory"
        case "create_schedule": "Schedule task"
        case "list_schedules": "Review schedules"
        case "cancel_schedule": "Cancel schedule"
        case "list_sessions": "Find past sessions"
        case "read_session": "Read past session"
        case "read_skill": "Read skill"
        case "read_skill_file": "Read skill file"
        case "run_skill": "Run skill"
        case "change_model": "Change model"
        case "revert_model": "Restore model"
        case "team_edit": "Edit team setup"
        case "team_edit_persist": "Save team setup"
        case "team_edit_export": "Export team setup"
        case "get_environment_info": "Inspect environment"
        case "search_remote_mcp_servers": "Find integrations"
        case "list_remote_mcp_servers": "Review integrations"
        case "enable_remote_mcp_server": "Enable integration"
        case "disable_remote_mcp_server": "Disable integration"
        case "reset_remote_mcp_server_auth": "Reset integration access"
        case "search_tool": "Find a tool"
        case "add_tool": "Enable a tool"
        case "send_webhook": "Send webhook"
        case "user_prompt": "Ask you"
        case "think": "Thinking"
        case "open_url": "Open link"
        case name where name.hasPrefix("lsp_"): String(name.dropFirst(4)).replacingOccurrences(of: "_", with: " ").capitalized
        default: name.replacingOccurrences(of: "_", with: " ").capitalized
        }
    }

    private static func symbol(for family: Family) -> String {
        switch family {
        case .command: "terminal"
        case .fileRead: "doc.text"
        case .fileWrite: "doc.badge.plus"
        case .fileEdit: "arrow.trianglehead.2.clockwise.rotate.90"
        case .directory: "folder"
        case .search: "magnifyingglass"
        case .fetch: "globe"
        case .git: "point.3.connected.trianglepath.dotted"
        case .delegation: "arrow.triangle.branch"
        case .communication: "bubble.left.and.bubble.right"
        case .todo: "checklist"
        case .task: "checklist.checked"
        case .plan: "map"
        case .code: "chevron.left.forwardslash.chevron.right"
        case .memory: "brain.head.profile"
        case .schedule: "calendar"
        case .session: "clock.arrow.circlepath"
        case .skill: "sparkles"
        case .configuration: "slider.horizontal.3"
        case .integration: "puzzlepiece.extension"
        case .prompt: "questionmark.bubble"
        case .reasoning: "brain"
        case .generic: "wrench.and.screwdriver"
        }
    }

    private static func verb(for family: Family, toolName: String) -> String {
        switch family {
        case .command: toolName == "shell" ? "Run command" : displayName(toolName)
        case .fileRead: "Read file"
        case .fileWrite: toolName == "remove_directory" ? "Remove directory" : toolName == "create_directory" ? "Create directory" : "Write file"
        case .fileEdit: "Edit file"
        case .directory: toolName == "directory_tree" ? "Explore directory" : "List directory"
        case .search: toolName == "search_tool" ? "Find a tool" : toolName == "rag" ? "Search knowledge" : toolName == "search_memories" ? "Find memories" : "Find matches"
        case .fetch: toolName == "open_url" ? "Open link" : "Fetch page"
        case .git: displayName(toolName)
        case .delegation: displayName(toolName)
        case .communication: displayName(toolName)
        case .todo: displayName(toolName)
        case .task: displayName(toolName)
        case .plan: displayName(toolName)
        case .code: displayName(toolName)
        case .memory: displayName(toolName)
        case .schedule: displayName(toolName)
        case .session: displayName(toolName)
        case .skill: displayName(toolName)
        case .configuration: displayName(toolName)
        case .integration: displayName(toolName)
        case .prompt: "Ask you"
        case .reasoning: "Thinking"
        case .generic: displayName(toolName)
        }
    }

    private static func resultLabel(for family: Family, toolName: String) -> String {
        switch family {
        case .command: "Terminal output"
        case .fileRead: "File contents"
        case .fileWrite: "Result"
        case .fileEdit: "Changes"
        case .directory: "Directory entries"
        case .search: toolName == "search_memories" ? "Memories" : toolName == "search_tool" ? "Available tools" : toolName == "rag" ? "Knowledge" : "Matches"
        case .fetch: toolName == "open_url" ? "Link status" : "Page content"
        case .git: "Git result"
        case .delegation: "Agent update"
        case .communication: "Message"
        case .todo, .task: "Progress"
        case .plan: "Plan"
        case .code: "Code insight"
        case .memory: "Memory"
        case .schedule: "Schedule"
        case .session: "Past session"
        case .skill: "Skill"
        case .configuration: "Change"
        case .integration: "Integration"
        case .prompt: "Response"
        case .reasoning: "Thought"
        case .generic: "Result"
        }
    }

    private static func firstString(_ object: [String: Any], keys: [String]) -> String? {
        for key in keys {
            if let value = object[key] as? String, !value.isEmpty { return bounded(value, limit: maximumValueCharacters) }
            if let values = object[key] as? [String], let value = values.first { return bounded(value, limit: maximumValueCharacters) }
        }
        return nil
    }

    private static func firstOutputString(_ object: [String: Any], keys: [String]) -> String? {
        for key in keys { if let value = object[key] as? String, !value.isEmpty { return value } }
        return nil
    }

    private static func argumentFields(_ args: [String: Any], family: Family) -> [Field] {
        let hidden = Set(["content", "oldtext", "newtext", "edits", "todos", "descriptions", "updates", "task", "message", "expected_output", "authorization", "token", "password", "secret", "api_key", "schema", "yaml", "instruction", "thought"])
        var fields: [Field] = []
        for key in args.keys.sorted() where !hidden.contains(key.lowercased()) {
            guard let value = args[key] else { continue }
            let displayKey = key.replacingOccurrences(of: "_", with: " ").capitalized
            if let string = value as? String, !string.isEmpty {
                fields.append(Field(displayKey, bounded(string, limit: maximumValueCharacters)))
            } else if let number = value as? NSNumber, CFGetTypeID(number) != CFBooleanGetTypeID() {
                fields.append(Field(displayKey, number.stringValue))
            } else if let bool = value as? Bool {
                fields.append(Field(displayKey, bool ? "Yes" : "No"))
            } else if let values = value as? [String], !values.isEmpty {
                fields.append(Field(displayKey, "\(values.count) items: " + values.prefix(4).joined(separator: ", ")))
            }
        }
        if family == .prompt, let schema = args["schema"] as? [String: Any] {
            fields.append(Field("Expected response", schema["type"] as? String ?? "structured input"))
            if let properties = schema["properties"] as? [String: Any] { fields.append(Field("Fields", properties.keys.sorted().prefix(8).joined(separator: ", "))) }
        }
        if family == .search, let query = firstString(args, keys: ["query", "pattern"]) {
            return [Field("Query", query)] + fields.filter { $0.name.lowercased() != "query" }
        }
        return fields
    }

    private static func structuredOutputFields(_ output: [String: Any]) -> [Field] {
        let hidden = Set(["iserror", "content", "body", "output", "transcript", "token", "secret", "password", "authorization"])
        return output.keys.sorted().compactMap { key in
            guard !hidden.contains(key.lowercased()), let value = output[key] else { return nil }
            if let rendered = scalarString(value) {
                return Field("Result · " + key.replacingOccurrences(of: "_", with: " ").capitalized, bounded(rendered, limit: maximumValueCharacters))
            }
            if let values = value as? [String] {
                return Field("Result · " + key.replacingOccurrences(of: "_", with: " ").capitalized,
                             "\(values.count) items: " + values.prefix(4).map { bounded($0, limit: 240) }.joined(separator: ", "))
            }
            return nil
        }
    }

    private static func metrics(arguments: [String: Any], result: [String: Any]) -> [Field] {
        let names = ["exit_code": "Exit code", "exitCode": "Exit code", "duration": "Duration", "duration_ms": "Duration", "line": "Line", "line_number": "Line", "start_line": "Starts at line", "end_line": "Ends at line", "limit": "Line limit", "matchCount": "Matches", "fileCount": "Files", "statusCode": "HTTP status", "contentLength": "Content length", "revision": "Revision", "bytesWritten": "Bytes written", "num_messages": "Messages", "numMessages": "Messages", "timeout": "Timeout (s)"]
        var fields: [Field] = []
        for (key, label) in names {
            if let value = result[key] ?? arguments[key], let rendered = scalarString(value) { fields.append(Field(label, bounded(rendered, limit: 80))) }
        }
        return fields
    }

    private static func entities(from value: Any?, family: Family) -> [Entity] {
        guard let value else { return [] }
        let root = value as? [String: Any]
        let candidates: [(value: Any, category: String?)]
        if let array = value as? [Any] {
            candidates = array.prefix(maximumItems + 1).map { ($0, nil) }
        } else if let root {
            if family == .directory, (root["type"] as? String) == "directory" {
                return directoryEntities(from: root)
            }
            if root["id"] != nil, [.task, .plan, .session, .memory, .schedule, .skill, .integration, .configuration].contains(family) {
                return entity(from: root, category: familyName(family), index: 0).map { [$0] } ?? []
            }
            let keys: [String]
            switch family {
            case .communication, .delegation: keys = ["agents", "children", "tasks", "items"]
            case .todo:
                let authoritative = ["all_todos", "todos"].first(where: { root[$0] is [Any] })
                keys = authoritative.map { [$0] } ?? ["created", "updated"]
            case .task: keys = ["tasks", "items", "dependencies"]
            case .plan: keys = ["plans", "items"]
            case .directory: keys = ["dirs", "directories", "files", "entries", "items"]
            case .search: keys = ["matches", "results", "items"]
            case .fileRead: keys = ["files", "results", "items"]
            case .git: keys = ["commits", "branches", "files", "items"]
            case .code: keys = ["diagnostics", "locations", "symbols", "references", "items", "results", "actions"]
            case .memory: keys = ["memories", "items", "results"]
            case .schedule: keys = ["schedules", "items", "results"]
            case .session: keys = ["sessions", "items", "results"]
            case .skill: keys = ["skills", "files", "items", "results"]
            case .configuration: keys = ["changes", "agents", "items", "results"]
            case .integration: keys = ["servers", "tools", "results", "items"]
            case .prompt, .reasoning: keys = ["items", "results"]
            case .generic:
                let conventional = ["items", "results", "all_todos", "todos", "created", "updated", "matches", "files", "dirs", "agents", "children"]
                let actualKeys = conventional.filter { root[$0] is [Any] }
                    + root.keys.filter { !conventional.contains($0) && root[$0] is [Any] }.sorted()
                keys = Array(actualKeys.prefix(8))
            default: keys = ["items", "results"]
            }
            candidates = keys.flatMap { key -> [(value: Any, category: String?)] in
                guard let array = root[key] as? [Any] else { return [] }
                return array.prefix(maximumItems + 1).map { ($0, key) }
            }
            if candidates.isEmpty && (root["id"] != nil || root["title"] != nil) && ([.task, .plan, .session, .memory, .schedule, .skill, .integration, .configuration].contains(family)) {
                return entity(from: root, category: familyName(family), index: 0).map { [$0] } ?? []
            }
        } else { return [] }

        return candidates.prefix(maximumItems + 1).enumerated().compactMap { index, candidate in
            let category = candidate.category
            if let text = candidate.value as? String, !text.isEmpty {
                return Entity(id: "\(category ?? "item")-\(text)", title: bounded(text, limit: maximumValueCharacters), subtitle: category?.capitalized, detail: nil, state: nil, facts: [])
            }
            guard let object = candidate.value as? [String: Any] else { return nil }
            return entity(from: object, category: category, index: index)
        }
    }

    private static func familyName(_ family: Family) -> String {
        switch family {
        case .task: "Task"
        case .plan: "Plan"
        case .session: "Session"
        case .memory: "Memory"
        case .schedule: "Schedule"
        case .skill: "Skill"
        case .integration: "Integration"
        case .configuration: "Configuration"
        default: "Item"
        }
    }

    private static func entity(from object: [String: Any], category: String?, index: Int) -> Entity? {
        let title = firstString(object, keys: ["description", "title", "name", "path", "file", "text", "agent", "id", "hash", "commit", "query", "uri", "symbol", "message", "url", "model", "memory", "prompt"]) ?? "Item \(index + 1)"
        let state = firstString(object, keys: ["effectiveStatus", "effective_status", "state", "status", "tool_status", "action"])
            ?? ((object["effectiveStatus"] != nil || object["status"] != nil) ? "unknown" : nil)
        let normalizedState: String?
        switch state?.lowercased() {
        case "done", "complete", "completed": normalizedState = "completed"
        case "in_progress", "in-progress": normalizedState = "in-progress"
        case "pending": normalizedState = "pending"
        case "blocked": normalizedState = "blocked"
        default: normalizedState = state
        }
        let explicitSubtitle = firstString(object, keys: ["role", "summary", "author", "branch", "parent", "url", "severity", "priority", "category", "contentType", "content_type", "when", "statusCode"])
        let subtitle = explicitSubtitle ?? category.map { $0 == "dirs" || $0 == "directories" ? "Folder" : $0 == "files" ? "File" : $0.capitalized }
        let detail = firstString(object, keys: ["snippet", "match", "content", "output", "message", "task", "expected_output", "body", "detail", "error", "memory", "prompt"])
        let factKeys = ["line", "character", "column", "parent_id", "id", "messageable", "dependencies", "completed", "added", "removed", "count", "revision", "priority", "effectiveStatus", "effective_status", "severity", "statusCode", "status_code", "contentLength", "content_length", "when", "createdAt", "created_at", "num_messages", "starred"]
        let facts = factKeys.compactMap { key -> Field? in
            guard let value = object[key] else { return nil }
            if let rendered = scalarString(value) { return Field(key.replacingOccurrences(of: "_", with: " ").capitalized, bounded(rendered, limit: 100)) }
            if let values = value as? [String] { return Field(key.replacingOccurrences(of: "_", with: " ").capitalized, values.prefix(8).joined(separator: ", ")) }
            return nil
        }
        return Entity(id: firstString(object, keys: ["id", "path", "name"]) ?? "\(index)-\(title)", title: title, subtitle: subtitle, detail: detail, state: normalizedState, facts: facts)
    }

    private static func directoryEntities(from root: [String: Any]) -> [Entity] {
        var entities: [Entity] = []
        var stack: [(node: [String: Any], depth: Int)] = [(root, 0)]
        while let (node, depth) = stack.popLast(), entities.count <= maximumItems {
            if let children = node["children"] as? [[String: Any]] {
                for child in children.reversed() { stack.append((child, depth + 1)) }
            }
            guard depth > 0 else { continue }
            let title = firstString(node, keys: ["path", "name", "title"]) ?? "Directory entry"
            let type = firstString(node, keys: ["type", "kind"])
            entities.append(Entity(id: firstString(node, keys: ["path", "name"]) ?? "entry-\(entities.count)",
                                   title: title, subtitle: type?.capitalized, detail: nil, state: nil, facts: []))
        }
        return entities
    }

    private static func textEntities(_ output: String, family: Family) -> [Entity] {
        guard family == .search || family == .directory else { return [] }
        return boundedLines(output, limit: maximumItems + 1).enumerated().compactMap { index, raw in
            let line = String(raw).trimmingCharacters(in: .whitespaces)
            guard !line.isEmpty else { return nil }
            if family == .directory {
                if line.hasPrefix("DIR  ") { return Entity(id: "dir-\(index)", title: String(line.dropFirst(5)), subtitle: "Folder", detail: nil, state: nil, facts: []) }
                if line.hasPrefix("FILE ") { return Entity(id: "file-\(index)", title: String(line.dropFirst(5)), subtitle: "File", detail: nil, state: nil, facts: []) }
                return nil
            }
            let parts = line.split(separator: ":", maxSplits: 3, omittingEmptySubsequences: false)
            guard parts.count == 4, let lineNumber = Int(parts[1]), let column = Int(parts[2]) else { return nil }
            let path = String(parts[0])
            let snippet = String(parts[3]).trimmingCharacters(in: .whitespaces)
            return Entity(id: "\(path):\(lineNumber):\(column)", title: path, subtitle: nil, detail: snippet, state: nil,
                          facts: [Field("Line", String(lineNumber)), Field("Column", String(column))])
        }
    }

    private static func requestedEdits(_ args: [String: Any]) -> [EditRequest] {
        guard let edits = args["edits"] as? [[String: Any]] else { return [] }
        return edits.prefix(maximumItems).enumerated().map { index, edit in
            EditRequest(id: index, oldText: bounded(edit["oldText"] as? String ?? "", limit: maximumResultLineCharacters),
                        newText: bounded(edit["newText"] as? String ?? "", limit: maximumResultLineCharacters))
        }
    }

    private static func receipt(_ object: [String: Any]) -> Receipt? {
        guard ["accepted", "durable", "queued", "disposition", "rejection", "detail", "target"].contains(where: { object[$0] != nil }) else { return nil }
        return Receipt(target: object["target"] as? String, accepted: object["accepted"] as? Bool,
                       durable: object["durable"] as? Bool, queued: object["queued"] as? Bool,
                       disposition: object["disposition"] as? String, rejection: object["rejection"] as? String,
                       detail: object["detail"] as? String)
    }

    private static func contentArgument(_ args: [String: Any], family: Family) -> String? {
        let keys: [String]
        switch family {
        case .delegation: keys = ["task", "expected_output"]
        case .communication: keys = ["message", "task", "value2", "value3"]
        case .todo: keys = ["description", "descriptions"]
        case .task: keys = ["title", "description", "task"]
        case .plan: keys = ["content", "description"]
        case .memory: keys = ["memory"]
        case .session: keys = ["transcript", "content"]
        case .skill: keys = ["content", "body"]
        case .prompt: keys = ["message"]
        case .reasoning: keys = ["thought"]
        case .configuration: keys = ["instruction", "yaml"]
        case .fileRead, .fileWrite, .fileEdit, .directory, .search, .fetch, .git, .command, .code, .schedule, .integration, .generic: keys = []
        }
        for key in keys {
            if let string = args[key] as? String, !string.isEmpty { return bounded(string, limit: maximumItems * maximumResultLineCharacters) }
            if let strings = args[key] as? [String], !strings.isEmpty { return strings.prefix(maximumItems).map { "• " + bounded($0, limit: maximumResultLineCharacters) }.joined(separator: "\n") }
        }
        return nil
    }

    private static func boundedLines(_ text: String, limit: Int) -> [Substring] {
        var lines: [Substring] = []
        var start = text.startIndex
        while lines.count < limit, start < text.endIndex {
            let end = text[start...].firstIndex(of: "\n") ?? text.endIndex
            lines.append(text[start..<end])
            if end == text.endIndex { return lines }
            start = text.index(after: end)
        }
        if lines.count < limit, text.last == "\n" { lines.append(text[text.endIndex..<text.endIndex]) }
        return lines
    }

    private static func resultLines(_ output: String, family: Family) -> [ResultLine] {
        boundedLines(output, limit: maximumItems + 1).enumerated().map { index, raw in
            let line = bounded(String(raw), limit: maximumResultLineCharacters)
            let trimmed = line.trimmingCharacters(in: .whitespaces)
            let kind: ResultLine.Kind
            if trimmed.hasPrefix("+++") || trimmed.hasPrefix("---") || trimmed.hasPrefix("diff --git") { kind = .path }
            else if trimmed.hasPrefix("@@") || trimmed.hasPrefix("###") { kind = .heading }
            else if trimmed.hasPrefix("+") && family == .fileEdit { kind = .addition }
            else if trimmed.hasPrefix("-") && family == .fileEdit { kind = .removal }
            else if family == .search && (trimmed.contains(":") || trimmed.contains("match")) { kind = .match }
            else if trimmed.lowercased().hasPrefix("error:") || trimmed.lowercased().contains("failed") { kind = .failure }
            else if trimmed.lowercased().hasPrefix("success") || trimmed.lowercased().hasPrefix("created ") { kind = .success }
            else if family == .todo || family == .task, ["in-progress", "pending", "completed"].contains(where: trimmed.lowercased().contains) { kind = .active }
            else { kind = .plain }
            return ResultLine(id: index, kind: kind, text: line)
        }
    }

    private static func summary(family: Family, toolName: String, args: [String: Any], result: [String: Any], items: [Entity], output: String) -> String? {
        switch family {
        case .search:
            let matches = scalarInt(result["matchCount"] ?? result["match_count"])
            let files = scalarInt(result["fileCount"] ?? result["file_count"])
            if let matches, let files { return "\(matches) \(matches == 1 ? "match" : "matches") in \(files) \(files == 1 ? "file" : "files")" }
            if let matches { return "\(matches) \(matches == 1 ? "match" : "matches")" }
            if !items.isEmpty { return "\(items.count) \(toolName == "search_tool" ? (items.count == 1 ? "tool" : "tools") : (items.count == 1 ? "match" : "matches"))" }
        case .directory:
            let files = (result["files"] as? [Any])?.count ?? scalarInt(result["fileCount"])
            let dirs = (result["dirs"] as? [Any])?.count ?? scalarInt(result["dirCount"])
            if let files, let dirs { return "\(files) files, \(dirs) folders" }
            if !items.isEmpty { return "\(items.count) entries" }
        case .todo, .task:
            let explicit = result["all_todos"] as? [[String: Any]] ?? result["todos"] as? [[String: Any]] ?? result["tasks"] as? [[String: Any]]
            if let explicit {
                let done = explicit.filter { ["completed", "complete", "done"].contains((($0["effectiveStatus"] as? String) ?? ($0["effective_status"] as? String) ?? ($0["status"] as? String))?.lowercased() ?? "") }.count
                return "\(done) of \(explicit.count) done"
            }
            if let descriptions = args["descriptions"] as? [Any] { return "\(descriptions.count) added" }
            if !items.isEmpty { return "\(items.count) items" }
        case .communication:
            if let accepted = result["accepted"] as? Bool { return accepted ? "Accepted" : (result["rejection"] as? String ?? "Rejected") }
            if !items.isEmpty { return "\(items.count) agents" }
        case .prompt:
            if let action = result["action"] as? String { return action == "accept" ? "Response received" : "Response: \(action)" }
        case .session:
            if !items.isEmpty { return "\(items.count) sessions" }
        case .fetch:
            if let status = scalarInt(result["statusCode"] ?? result["status_code"]) { return "HTTP \(status)" }
        case .code:
            if !items.isEmpty { return "\(items.count) results" }
        case .memory:
            if !items.isEmpty { return "\(items.count) memories" }
        case .schedule:
            if !items.isEmpty { return "\(items.count) schedules" }
        case .skill:
            if !items.isEmpty { return "\(items.count) skill items" }
        case .integration:
            if let accepted = result["accepted"] as? Bool { return accepted ? "Accepted" : "Not accepted" }
            if !items.isEmpty { return "\(items.count) results" }
        case .configuration:
            if !items.isEmpty { return "\(items.count) changes" }
        case .reasoning: return nil
        case .fileRead:
            if let lines = scalarInt(result["lineCount"] ?? result["line_count"]) { return "\(lines) lines" }
        case .fileEdit:
            if !requestedEditsForSummary(args).isEmpty {
                let count = requestedEditsForSummary(args).count
                return "\(count) requested \(count == 1 ? "edit" : "edits")"
            }
        case .git, .plan:
            if !items.isEmpty { return "\(items.count) \(family == .git ? "records" : "plans")" }
        case .command:
            if let code = result["exit_code"] ?? result["exitCode"], let value = scalarString(code) { return "Exit code \(value)" }
            if let code = explicitExitCode(in: output) { return "Exit code \(code)" }
            if output.contains("Background job started with ID:") { return "Background job started" }
        case .generic:
            if !items.isEmpty { return "\(items.count) structured results" }
        case .fileWrite, .delegation: break
        }
        let isPlainText = (try? JSONSerialization.jsonObject(with: Data(output.utf8), options: [.fragmentsAllowed])) == nil
        if isPlainText, !output.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty {
            let firstLine = output.split(separator: "\n", omittingEmptySubsequences: true).first.map(String.init)
            if let firstLine, firstLine.count <= 120 { return firstLine }
        }
        return nil
    }

    private static func requestedEditsForSummary(_ args: [String: Any]) -> [[String: Any]] {
        (args["edits"] as? [[String: Any]]) ?? []
    }

    private static func explicitExitCode(in output: String) -> Int? {
        let pattern = #"(?i)\bexit code\s+(-?\d+)\b"#
        guard let regex = try? NSRegularExpression(pattern: pattern),
              let match = regex.firstMatch(in: output, range: NSRange(output.startIndex..., in: output)),
              let range = Range(match.range(at: 1), in: output) else { return nil }
        return Int(output[range])
    }

    private static func scalarString(_ value: Any) -> String? {
        if let value = value as? String { return value }
        if let value = value as? NSNumber { return CFGetTypeID(value) == CFBooleanGetTypeID() ? (value.boolValue ? "Yes" : "No") : value.stringValue }
        return nil
    }

    private static func scalarInt(_ value: Any?) -> Int? {
        if let value = value as? Int { return value }
        if let value = value as? NSNumber { return value.intValue }
        return nil
    }

    private static func bounded(_ text: String, limit: Int) -> String {
        guard text.count > limit else { return text }
        return String(text.prefix(limit)) + "…"
    }
}

struct ToolCallPresentation {
    let shellCommand: String?
    let target: String?

    init(name: String?, arguments: String?, status: String?) {
        let object = Self.decode(arguments)
        let command = object["cmd"] ?? object["command"]
        shellCommand = PermissionPattern.isCommandTool(name ?? "") ? command : nil
        target = ["path", "file_path", "url", "query", "pattern", "name"].compactMap { object[$0] }.first
    }

    static func decode(_ arguments: String?) -> [String: String] {
        guard let arguments, let data = arguments.data(using: .utf8),
              let object = try? JSONSerialization.jsonObject(with: data) as? [String: String] else { return [:] }
        return object
    }
}

private struct ToolPresentationKey: Equatable {
    let name: String
    let arguments: String
    let output: String
    let status: String
}

struct ToolRow: View {
    let item: TranscriptItem
    var onBranch: (() -> Void)? = nil
    @State private var presentation: ToolPresentation?

    private var presentationKey: ToolPresentationKey {
        ToolPresentationKey(name: item.toolName ?? item.text, arguments: item.toolArguments ?? "", output: item.toolOutput ?? "", status: item.toolStatus ?? "")
    }

    var body: some View {
        Group {
            if let presentation {
                ToolContentView(presentation: presentation, isExpanded: expanded, onToggle: toggle, onBranch: onBranch)
            } else {
                Label("Preparing tool details", systemImage: "ellipsis.circle")
                    .font(.caption)
                    .foregroundStyle(.secondary)
            }
        }
        .onAppear { updatePresentation() }
        .onChange(of: presentationKey) { updatePresentation() }
        .accessibilityElement(children: .contain)
    }

    @State private var expanded = false

    private func toggle() {
        withAnimation(.snappy) { expanded.toggle() }
    }

    private func updatePresentation() {
        presentation = ToolPresentation(name: item.toolName ?? item.text, arguments: item.toolArguments, output: item.toolOutput, status: item.toolStatus)
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
                AssistantTextView(text: item.text, isStreaming: item.isStreaming)
                    .foregroundStyle(.secondary)
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
            Image(systemName: "arrow.turn.down.right")
                .font(.caption2.weight(.semibold))
                .foregroundStyle(.tertiary)
            if let owner = item.owner { AgentLabel(name: owner, font: .caption.weight(.semibold)) }
            Text(item.text)
                .font(.caption)
                .foregroundStyle(.secondary)
                .lineLimit(1)
                .layoutPriority(-1)
            if let child = item.child {
                Button("Open") { openSession(child) }
                    .buttonStyle(.borderless)
                    .font(.caption.weight(.medium))
            }
            line.layoutPriority(-2)
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
    static func isCommandTool(_ name: String) -> Bool { name == "shell" }

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
