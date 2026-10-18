import SwiftUI
import AgentKit

struct DiffLinesView: View {
    let lines: [ReviewDocumentLine]
    let layout: DiffLayout
    @Binding var selection: ClosedRange<Int>?
    var extendsByTap = false
    let inlineComments: [Int: [ReviewComment]]
    let matches: [Int]
    let currentMatch: Int?
    let onComment: () -> Void
    let onEditComment: (ReviewComment) -> Void

    @State private var selectionAnchor: Int?
    @FocusState private var focused: Bool

    private var gutterWidth: CGFloat {
        let maxLine = lines.reduce(0) { max($0, $1.oldLine, $1.newLine) }
        return CGFloat(max(3, String(maxLine).count)) * 7.5 + 8
    }

    var body: some View {
        let matchSet = Set(matches)
        let splitRows = layout == .split ? SplitRow.build(lines) : []
        ScrollViewReader { proxy in
            ScrollView {
                LazyVStack(alignment: .leading, spacing: 0) {
                    if layout == .split {
                        ForEach(splitRows) { row in
                            splitRow(row, matches: matchSet)
                                .id(primaryIndex(row))
                        }
                    } else {
                        ForEach(lines.indices, id: \.self) { index in
                            unifiedRow(index, matched: matchSet.contains(index))
                                .id(index)
                            commentsAfter(index)
                        }
                    }
                }
                .padding(.bottom, 80)
            }
            .focusable()
            .focused($focused)
            .focusEffectDisabled()
            .onKeyPress(keys: [.upArrow, .downArrow]) { press in
                move(press.key == .upArrow ? -1 : 1, extend: press.modifiers.contains(.shift))
                if let selection { proxy.scrollTo(press.key == .upArrow ? selection.lowerBound : selection.upperBound) }
                return .handled
            }
            .onKeyPress(.return) {
                guard selection != nil else { return .ignored }
                onComment()
                return .handled
            }
            .onKeyPress(.escape) {
                guard selection != nil else { return .ignored }
                selection = nil
                return .handled
            }
            .onChange(of: currentMatch) { _, match in
                guard let match else { return }
                withAnimation(.snappy) { proxy.scrollTo(match, anchor: .center) }
            }
        }
    }

    // MARK: Selection

    private func select(_ index: Int, extend: Bool) {
        guard lines.indices.contains(index), lines[index].kind.isSelectable else { return }
        focused = true
        if extend, let anchor = selectionAnchor {
            selection = min(anchor, index)...max(anchor, index)
        } else if selection == index...index {
            selection = nil
            selectionAnchor = nil
        } else {
            selection = index...index
            selectionAnchor = index
        }
    }

    private func move(_ delta: Int, extend: Bool) {
        let current = selection.map { delta < 0 ? $0.lowerBound : $0.upperBound } ?? (delta < 0 ? lines.count : -1)
        var next = current + delta
        while lines.indices.contains(next), !lines[next].kind.isSelectable { next += delta }
        guard lines.indices.contains(next) else { return }
        if extend, selectionAnchor != nil {
            select(next, extend: true)
        } else {
            selection = next...next
            selectionAnchor = next
        }
    }

    private func isSelected(_ index: Int) -> Bool { selection?.contains(index) ?? false }

    // MARK: Rows

    private func unifiedRow(_ index: Int, matched: Bool) -> some View {
        let line = lines[index]
        return HStack(alignment: .firstTextBaseline, spacing: 0) {
            if line.kind == .hunk || line.kind == .note {
                Text(line.text)
                    .font(line.kind == .note ? .monoSmall.italic() : .monoSmall)
                    .foregroundStyle(.secondary)
                    .padding(.leading, gutterWidth * 2 + 20)
                    .padding(.vertical, Metrics.xs)
            } else {
                lineNumber(line.kind == .add ? nil : line.oldLine)
                lineNumber(line.kind == .remove ? nil : line.newLine)
                Text(line.kind.marker)
                    .frame(width: 20)
                    .foregroundStyle(line.kind == .context ? AnyShapeStyle(.tertiary) : AnyShapeStyle(line.kind == .add ? Color.green : Color.red))
                Text(line.text.isEmpty ? " " : line.text)
                    .padding(.trailing, Metrics.s)
            }
            Spacer(minLength: 0)
        }
        .font(.mono)
        .frame(maxWidth: .infinity, alignment: .leading)
        .background(rowBackground(line.kind, selected: isSelected(index), matched: matched))
        .contentShape(.rect)
        .modifier(SelectableLine(index: index, line: line, isSelected: isSelected(index), extendsByTap: extendsByTap, select: select, comment: commentFromMenu))
    }

    private func splitRow(_ row: SplitRow, matches: Set<Int>) -> some View {
        VStack(spacing: 0) {
            switch row.content {
            case .full(let index):
                unifiedRow(index, matched: matches.contains(index))
                commentsAfter(index)
            case .pair(let left, let right):
                HStack(spacing: 0) {
                    splitCell(left, side: .old, matches: matches)
                    Divider()
                    splitCell(right, side: .new, matches: matches)
                }
                if let right { commentsAfter(right) }
                if let left, left != right { commentsAfter(left) }
            }
        }
    }

    private enum Side { case old, new }

    @ViewBuilder
    private func splitCell(_ index: Int?, side: Side, matches: Set<Int>) -> some View {
        if let index {
            let line = lines[index]
            HStack(alignment: .firstTextBaseline, spacing: 0) {
                lineNumber(side == .old ? line.oldLine : line.newLine)
                Text(line.kind == .context ? " " : line.kind.marker)
                    .frame(width: 16)
                    .foregroundStyle(line.kind == .add ? Color.green : Color.red)
                Text(line.text.isEmpty ? " " : line.text)
                    .padding(.trailing, Metrics.s)
                Spacer(minLength: 0)
            }
            .font(.mono)
            .frame(maxWidth: .infinity, maxHeight: .infinity, alignment: .topLeading)
            .background(rowBackground(line.kind, selected: isSelected(index), matched: matches.contains(index)))
            .contentShape(.rect)
            .modifier(SelectableLine(index: index, line: line, isSelected: isSelected(index), extendsByTap: extendsByTap, select: select, comment: commentFromMenu))
        } else {
            Color.secondary.opacity(0.06)
                .frame(maxWidth: .infinity, maxHeight: .infinity)
                .accessibilityHidden(true)
        }
    }

    private func primaryIndex(_ row: SplitRow) -> Int {
        switch row.content {
        case .full(let i): i
        case .pair(let l, let r): l ?? r ?? 0
        }
    }

    private func lineNumber(_ value: Int?) -> some View {
        Text(value.map { $0 > 0 ? String($0) : "" } ?? "")
            .font(.monoSmall)
            .foregroundStyle(.tertiary)
            .frame(width: gutterWidth, alignment: .trailing)
            .padding(.trailing, Metrics.xs)
    }

    private func rowBackground(_ kind: ReviewLineKind, selected: Bool, matched: Bool) -> some View {
        ZStack {
            kind.background
            if matched { Color.yellow.opacity(0.22) }
            if selected { Color.accentColor.opacity(0.22) }
        }
        .overlay(alignment: .leading) {
            if selected { Rectangle().fill(Color.accentColor).frame(width: 3) }
        }
    }

    private func commentFromMenu(_ index: Int) {
        if !isSelected(index) { select(index, extend: false) }
        onComment()
    }

    @ViewBuilder
    private func commentsAfter(_ index: Int) -> some View {
        if let comments = inlineComments[index] {
            VStack(alignment: .leading, spacing: Metrics.s) {
                ForEach(comments) { comment in
                    InlineCommentBubble(comment: comment) { onEditComment(comment) }
                }
            }
            .padding(.leading, gutterWidth * 2 + 20)
            .padding(.trailing, Metrics.l)
            .padding(.vertical, Metrics.s)
        }
    }
}

private struct SelectableLine: ViewModifier {
    let index: Int
    let line: ReviewDocumentLine
    let isSelected: Bool
    let extendsByTap: Bool
    let select: (Int, Bool) -> Void
    let comment: (Int) -> Void

    func body(content: Content) -> some View {
        if line.kind.isSelectable {
            content
                #if os(macOS)
                .gesture(TapGesture().modifiers(.shift).onEnded { select(index, true) })
                #endif
                .onTapGesture { select(index, extendsByTap) }
                .contextMenu {
                    Button("Comment…", systemImage: "text.bubble") { comment(index) }
                    Button("Select Line", systemImage: "selection.pin.in.out") { select(index, false) }
                    Button("Extend Selection to Here", systemImage: "arrow.up.and.down.text.horizontal") { select(index, true) }
                    Divider()
                    Button("Copy Line", systemImage: "doc.on.doc") { Clipboard.copy(line.text) }
                }
                .accessibilityElement(children: .ignore)
                .accessibilityLabel(accessibilityText)
                .accessibilityAddTraits(isSelected ? [.isSelected, .isButton] : .isButton)
                .accessibilityAction(named: "Select line") { select(index, false) }
                .accessibilityAction(named: "Extend selection to here") { select(index, true) }
                .accessibilityAction(named: "Comment") { comment(index) }
        } else {
            content
                .accessibilityElement(children: .combine)
        }
    }

    private var accessibilityText: String {
        let kind = switch line.kind {
        case .add: "Added"
        case .remove: "Removed"
        default: "Unchanged"
        }
        let number = line.kind == .remove ? "old line \(line.oldLine)" : "line \(line.newLine)"
        return "\(kind), \(number): \(line.text)"
    }
}

struct InlineCommentBubble: View {
    let comment: ReviewComment
    let edit: () -> Void

    var body: some View {
        HStack(alignment: .top, spacing: Metrics.s) {
            Image(systemName: "text.bubble.fill")
                .foregroundStyle(.tint)
            Text(comment.body)
                .font(.callout)
                .textSelection(.enabled)
                .frame(maxWidth: .infinity, alignment: .leading)
            Button("Edit", action: edit)
                .buttonStyle(.borderless)
                .font(.caption)
        }
        .padding(Metrics.m)
        .background(.background, in: .rect(cornerRadius: Metrics.chipRadius, style: .continuous))
        .overlay {
            RoundedRectangle(cornerRadius: Metrics.chipRadius, style: .continuous)
                .strokeBorder(.tint.opacity(0.35))
        }
        .accessibilityElement(children: .combine)
        .accessibilityLabel("Draft comment: \(comment.body)")
    }
}
