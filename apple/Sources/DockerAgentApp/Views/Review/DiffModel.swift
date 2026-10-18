import SwiftUI
import AgentKit

enum ReviewLimits {
    static let comments = 100
    static let bodyBytes = 16 * 1024
    static let contextLines = 400
    static let contextBytes = 256 * 1024
}

struct ReviewFileSelection: Hashable {
    let repositoryID: String
    let worktreeID: String
    let file: ReviewFileChange

    static func == (a: Self, b: Self) -> Bool {
        a.repositoryID == b.repositoryID && a.worktreeID == b.worktreeID && a.file.id == b.file.id
    }

    func hash(into hasher: inout Hasher) {
        hasher.combine(repositoryID)
        hasher.combine(worktreeID)
        hasher.combine(file.id)
    }

    var documentKey: ReviewDocumentKey {
        ReviewDocumentKey(mode: .changes, worktreeID: worktreeID, path: file.path, group: file.group)
    }
}

enum DiffLayout: String, CaseIterable, Identifiable {
    case unified = "Unified"
    case split = "Split"
    var id: Self { self }
}

extension ReviewLineKind {
    var isSelectable: Bool { self == .context || self == .add || self == .remove }

    var marker: String {
        switch self {
        case .add: "+"
        case .remove: "−"
        default: " "
        }
    }

    var background: Color {
        switch self {
        case .add: .green.opacity(0.14)
        case .remove: .red.opacity(0.14)
        case .hunk: .accentColor.opacity(0.08)
        default: .clear
        }
    }
}

extension ReviewFileGroup {
    var title: String {
        switch self {
        case .conflicts: "Conflicts"
        case .staged: "Staged"
        case .unstaged: "Unstaged"
        case .untracked: "Untracked"
        case .none: "Changes"
        }
    }

    var tint: Color {
        switch self {
        case .conflicts: .red
        case .staged: .green
        case .unstaged: .orange
        case .untracked, .none: .secondary
        }
    }
}

extension ReviewFileChange {
    var fileName: String { (path as NSString).lastPathComponent }
    var directory: String {
        let dir = (path as NSString).deletingLastPathComponent
        return dir.isEmpty ? "" : dir + "/"
    }
    var statusLetter: String {
        let trimmed = status.trimmingCharacters(in: .whitespaces)
        return trimmed.isEmpty ? (group == .untracked ? "?" : "M") : String(trimmed.prefix(2))
    }
    var isRename: Bool { !oldPath.isEmpty && oldPath != path }
}

/// Row in the split view: both sides refer back to indices in `document.lines`.
struct SplitRow: Identifiable {
    enum Content {
        case pair(left: Int?, right: Int?)
        case full(Int)
    }
    let id: Int
    let content: Content

    static func build(_ lines: [ReviewDocumentLine]) -> [SplitRow] {
        var rows: [SplitRow] = []
        var removes: [Int] = []
        var adds: [Int] = []
        func flush() {
            for i in 0..<max(removes.count, adds.count) {
                rows.append(SplitRow(id: rows.count, content: .pair(left: i < removes.count ? removes[i] : nil, right: i < adds.count ? adds[i] : nil)))
            }
            removes.removeAll()
            adds.removeAll()
        }
        for (index, line) in lines.enumerated() {
            switch line.kind {
            case .remove:
                if !adds.isEmpty { flush() }
                removes.append(index)
            case .add:
                adds.append(index)
            case .context:
                flush()
                rows.append(SplitRow(id: rows.count, content: .pair(left: index, right: index)))
            case .hunk, .note:
                flush()
                rows.append(SplitRow(id: rows.count, content: .full(index)))
            }
        }
        flush()
        return rows
    }
}

enum AnchorBuilder {
    enum Problem: Equatable {
        case noSelectableLines, tooManyLines, tooLarge

        var message: String {
            switch self {
            case .noSelectableLines: "Select changed or context lines to comment on."
            case .tooManyLines: "Select at most \(ReviewLimits.contextLines) lines."
            case .tooLarge: "The selected lines are too large to quote (256 KiB limit)."
            }
        }
    }

    static func validate(_ range: ClosedRange<Int>, in lines: [ReviewDocumentLine]) -> Problem? {
        let slice = lines[range.clamped(to: 0...(lines.count - 1))]
        if !slice.contains(where: { $0.kind.isSelectable }) { return .noSelectableLines }
        if slice.count > ReviewLimits.contextLines { return .tooManyLines }
        if slice.reduce(0, { $0 + $1.text.utf8.count }) > ReviewLimits.contextBytes { return .tooLarge }
        return nil
    }

    static func anchor(range: ClosedRange<Int>, document: ReviewDocument, file: ReviewFileChange, generation: UInt64) -> ReviewAnchor? {
        guard !document.lines.isEmpty, validate(range, in: document.lines) == nil else { return nil }
        let slice = Array(document.lines[range.clamped(to: 0...(document.lines.count - 1))])
        let olds = slice.filter { $0.kind != .add }.map(\.oldLine).filter { $0 > 0 }
        let news = slice.filter { $0.kind != .remove }.map(\.newLine).filter { $0 > 0 }
        return ReviewAnchor(
            path: document.key.path,
            oldPath: file.oldPath,
            mode: document.key.mode,
            group: document.key.group,
            generation: generation,
            documentFingerprint: reviewDocumentFingerprint(document),
            oldStart: olds.min() ?? 0,
            oldEnd: olds.max() ?? 0,
            newStart: news.min() ?? 0,
            newEnd: news.max() ?? 0,
            context: slice.map(\.text),
            contextLines: slice.map { ReviewAnchorLine(kind: $0.kind, oldLine: $0.oldLine, newLine: $0.newLine, text: $0.text) }
        )
    }

    static func label(_ anchor: ReviewAnchor) -> String {
        func span(_ a: Int, _ b: Int) -> String { a == b ? "\(a)" : "\(a)–\(b)" }
        if anchor.newStart > 0 { return "Lines \(span(anchor.newStart, anchor.newEnd))" }
        if anchor.oldStart > 0 { return "Old lines \(span(anchor.oldStart, anchor.oldEnd))" }
        return "File"
    }

    /// Row index after which a current-version comment is shown inline.
    static func row(for anchor: ReviewAnchor, in lines: [ReviewDocumentLine]) -> Int? {
        if anchor.newEnd > 0, let i = lines.lastIndex(where: { $0.kind != .remove && $0.newLine == anchor.newEnd }) { return i }
        if anchor.oldEnd > 0, let i = lines.lastIndex(where: { $0.kind != .add && $0.oldLine == anchor.oldEnd }) { return i }
        return nil
    }
}
