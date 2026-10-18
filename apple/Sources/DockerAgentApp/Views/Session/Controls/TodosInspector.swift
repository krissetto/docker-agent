import SwiftUI
import AgentKit

struct TodoActions {
    var setStatus: ((SessionTodoWire, SessionTodoStatus) async throws -> Void)?
    var updateDescription: ((SessionTodoWire, String) async throws -> Void)?
    var remove: ((SessionTodoWire) async throws -> Void)?
}

struct TodosInspector: View {
    let todos: [SessionTodoWire]
    let readOnly: Bool
    let actions: TodoActions

    @State private var editing: SessionTodoWire?
    @State private var draft = ""
    @State private var error: String?
    @State private var showsCompleted = false

    private func todos(_ status: SessionTodoStatus) -> [SessionTodoWire] {
        todos.filter { SessionTodoStatus(rawValue: $0.status) == status }
    }

    var body: some View {
        List {
            if todos.isEmpty {
                ContentUnavailableView("No Todos", systemImage: "checklist", description: Text("Todos the agents create for this session appear here."))
                    .listRowSeparator(.hidden)
            }
            section("In Progress", todos(.inProgress))
            section("To Do", todos(.pending))
            let done = todos(.completed)
            if !done.isEmpty {
                Section(isExpanded: $showsCompleted) {
                    ForEach(done) { row($0) }
                } header: {
                    Text("Completed · \(done.count)")
                }
            }
            if let error {
                Label(error, systemImage: "exclamationmark.triangle.fill")
                    .font(.caption)
                    .foregroundStyle(.red)
            }
        }
        .listStyle(.sidebar)
        .alert("Edit Todo", isPresented: Binding(get: { editing != nil }, set: { if !$0 { editing = nil } })) {
            TextField("Description", text: $draft)
            Button("Save") {
                guard let todo = editing, let update = actions.updateDescription else { return }
                let value = draft.trimmingCharacters(in: .whitespacesAndNewlines)
                guard !value.isEmpty, value != todo.description else { return }
                run { try await update(todo, value) }
            }
            Button("Cancel", role: .cancel) {}
        } message: {
            Text("If an agent changed this todo meanwhile, the edit is rejected and the latest text is shown.")
        }
    }

    @ViewBuilder
    private func section(_ title: String, _ items: [SessionTodoWire]) -> some View {
        if !items.isEmpty {
            Section(title) { ForEach(items) { row($0) } }
        }
    }

    private func row(_ todo: SessionTodoWire) -> some View {
        let status = SessionTodoStatus(rawValue: todo.status)
        return HStack(alignment: .firstTextBaseline, spacing: Metrics.s) {
            Button {
                guard let setStatus = actions.setStatus else { return }
                run { try await setStatus(todo, next(status)) }
            } label: {
                Image(systemName: symbol(status))
                    .foregroundStyle(status == .completed ? AnyShapeStyle(.green) : status == .inProgress ? AnyShapeStyle(.tint) : AnyShapeStyle(.secondary))
            }
            .buttonStyle(.borderless)
            .disabled(readOnly || actions.setStatus == nil)
            .accessibilityLabel(label(status))
            .accessibilityHint("Changes status")
            Text(todo.description)
                .strikethrough(status == .completed, color: .secondary)
                .foregroundStyle(status == .completed ? .secondary : .primary)
                .textSelection(.enabled)
        }
        .contextMenu {
            if !readOnly, let setStatus = actions.setStatus {
                ForEach(SessionTodoStatus.allCases, id: \.self) { value in
                    Button(label(value)) { run { try await setStatus(todo, value) } }
                        .disabled(value == status)
                }
            }
            if !readOnly, actions.updateDescription != nil {
                Button("Edit…") {
                    draft = todo.description
                    editing = todo
                }
            }
            if !readOnly, let remove = actions.remove {
                Divider()
                Button("Delete", role: .destructive) { run { try await remove(todo) } }
            }
        }
        .swipeActions {
            if !readOnly, let remove = actions.remove {
                Button("Delete", role: .destructive) { run { try await remove(todo) } }
            }
        }
    }

    private func next(_ status: SessionTodoStatus?) -> SessionTodoStatus {
        switch status {
        case .pending, .none: .inProgress
        case .inProgress: .completed
        case .completed: .pending
        }
    }

    private func symbol(_ status: SessionTodoStatus?) -> String {
        switch status {
        case .completed: "checkmark.circle.fill"
        case .inProgress: "circle.lefthalf.filled"
        default: "circle"
        }
    }

    private func label(_ status: SessionTodoStatus?) -> String {
        switch status {
        case .completed: "Completed"
        case .inProgress: "In progress"
        default: "To do"
        }
    }

    private func run(_ action: @escaping () async throws -> Void) {
        error = nil
        Task {
            do { try await action() } catch { self.error = error.localizedDescription }
        }
    }
}
