import SwiftUI
import AgentKit

enum Metrics {
    static let xxs: CGFloat = 2
    static let xs: CGFloat = 4
    static let s: CGFloat = 8
    static let m: CGFloat = 12
    static let l: CGFloat = 16
    static let xl: CGFloat = 20
    static let xxl: CGFloat = 32
    static let readingWidth: CGFloat = 720
    static let cardRadius: CGFloat = 12
    static let chipRadius: CGFloat = 8
    static let conversationColumn: CGFloat = 380
    static let navigatorColumn: CGFloat = 280
}

enum AgentPalette {
    // Red and green are reserved for status, so agents never use them.
    private static let hues: [Color] = [.blue, .purple, .pink, .orange, .teal, .indigo, .mint, .brown]

    static func color(for name: String) -> Color {
        guard !name.isEmpty else { return .secondary }
        // FNV-1a: String.hashValue is seeded per launch and would reshuffle colors.
        var hash: UInt64 = 0xcbf29ce484222325
        for byte in name.utf8 {
            hash ^= UInt64(byte)
            hash &*= 0x100000001b3
        }
        return hues[Int(hash % UInt64(hues.count))]
    }
}

extension Font {
    static let mono = Font.system(.callout, design: .monospaced)
    static let monoSmall = Font.system(.caption, design: .monospaced)
}

extension View {
    func cardBackground(_ style: some ShapeStyle = .background.secondary) -> some View {
        background(style, in: .rect(cornerRadius: Metrics.cardRadius, style: .continuous))
    }
}

enum Format {
    static func cost(_ value: Double) -> String {
        value.formatted(.currency(code: "USD").precision(.fractionLength(value < 1 ? 3 : 2)))
    }

    static func tokens(_ value: Int64) -> String {
        value.formatted(.number.notation(.compactName))
    }

    static func relative(_ date: Date?) -> String? {
        date.map { $0.formatted(.relative(presentation: .named, unitsStyle: .abbreviated)) }
    }
}

struct OpenSessionAction {
    var handler: @MainActor (SessionRef, String?) -> Void = { _, _ in }
    /// `prefill` seeds the destination composer once (e.g. the message a fork stopped before); it is never sent.
    @MainActor func callAsFunction(_ ref: SessionRef, prefill: String? = nil) { handler(ref, prefill) }
}

/// Per-window composer state: one-shot drafts handed to a session that is about to open,
/// and unsent drafts kept while the user navigates to another session (memory only).
@Observable @MainActor
final class ComposerDraftHandoff {
    struct Saved {
        let text: String
        let attachments: [SessionDocumentAttachment]
    }

    private var drafts: [SessionRef: String] = [:]
    private var saved: [SessionRef: Saved] = [:]

    func put(_ text: String, for ref: SessionRef) { drafts[ref] = text }

    func take(for ref: SessionRef) -> String? { drafts.removeValue(forKey: ref) }

    func save(text: String, attachments: [SessionDocumentAttachment], for ref: SessionRef) {
        if text.isEmpty && attachments.isEmpty { saved[ref] = nil } else { saved[ref] = Saved(text: text, attachments: attachments) }
    }

    func restore(for ref: SessionRef) -> Saved? { saved.removeValue(forKey: ref) }
}

struct CloseSessionAction {
    var handler: @MainActor (SessionRef) -> Void = { _ in }
    @MainActor func callAsFunction(_ ref: SessionRef) { handler(ref) }
}

extension EnvironmentValues {
    @Entry var openSession = OpenSessionAction()
    @Entry var composerHandoff: ComposerDraftHandoff? = nil
    /// Called after a session is deleted so its container can drop the selection.
    @Entry var sessionDeleted = CloseSessionAction()
}
