import Testing
@testable import DockerAgentApple

@Test func toolPresentationClassifiesAndSummarizesUsefulCommands() {
    let command = ToolPresentation(name: "shell", arguments: #"{"cmd":"git status","cwd":"/repo"}"#, output: "", status: "running")
    #expect(command.family == .command)
    #expect(command.verb == "Run command")
    #expect(command.command == "git status")
    #expect(command.context == "/repo")
    #expect(command.status == .running)
    #expect(command.statusLabel == "Running")

    let search = ToolPresentation(name: "search_files_content", arguments: #"{"path":"pkg","query":"ToolRow","is_regex":false}"#, output: #"{"matchCount":2,"fileCount":1,"matches":[{"path":"a.swift","line":12,"snippet":"struct ToolRow"}]}"#, status: "complete")
    #expect(search.family == .search)
    #expect(search.target == "pkg")
    #expect(search.query == "ToolRow")
    #expect(search.resultSummary == "2 matches in 1 file")
    #expect(search.items.first?.title == "a.swift")
    #expect(search.items.first?.detail == "struct ToolRow")
    #expect(search.items.first?.facts.contains(where: { $0.name == "Line" && $0.value == "12" }) == true)
    #expect(search.result.isEmpty, "Structured JSON must not fall back to a raw JSON body")
}

@Test func toolPresentationExtractsAgentAndTodoStructure() {
    let agents = ToolPresentation(name: "list_agents", arguments: #"{"scope":"tree"}"#, output: #"{"agents":[{"id":"a1","agent":"worker","name":"researcher","state":"running","parent_id":"root","messageable":true}]}"#, status: "complete")
    #expect(agents.family == .communication)
    #expect(agents.items.first?.title == "researcher")
    #expect(agents.items.first?.state == "running")
    #expect(agents.items.first?.facts.contains(where: { $0.name == "Parent Id" && $0.value == "root" }) == true)

    let todos = ToolPresentation(name: "list_todos", arguments: "{}", output: #"{"todos":[{"id":"1","description":"Implement renderer","status":"completed"},{"id":"2","description":"Review","status":"in-progress"}]}"#, status: "complete")
    #expect(todos.family == .todo)
    #expect(todos.resultSummary == "1 of 2 done")
    #expect(todos.items.map(\.state) == ["completed", "in-progress"])
}

@Test func toolPresentationMakesMeaningfulPatchAndPreservesDiagnostics() {
    let diff = "--- a/file.swift\n+++ b/file.swift\n@@ -1 +1 @@\n-old\n+new"
    let presentation = ToolPresentation(name: "edit_file", arguments: #"{"path":"file.swift","edits":[{"oldText":"old","newText":"new"}]}"#, output: diff, status: "failed")
    #expect(presentation.family == .fileEdit)
    #expect(presentation.target == "file.swift")
    #expect(presentation.patch.isEmpty, "edit_file output prose is not a unified diff")
    #expect(presentation.fullDiagnosticOutput == diff)
    #expect(presentation.status == .failed)
    #expect(presentation.resultSummary == "1 requested edit")
}

@Test func toolPresentationBoundsLargePayloadAndKeepsUnknownTruthful() {
    let entities = (0..<45).map { "{\"path\":\"file\($0).swift\"}" }.joined(separator: ",")
    let oversizedResult = "[\(entities)]"
    let presentation = ToolPresentation(name: "custom_catalog_lookup", arguments: #"{"term":"needle","limit":8}"#, output: oversizedResult, status: nil)
    #expect(presentation.family == .generic)
    #expect(presentation.name == "custom_catalog_lookup")
    #expect(presentation.fields.contains(where: { $0.name == "Term" && $0.value == "needle" }))
    #expect(presentation.items.count == ToolPresentation.maximumItems)
    #expect(presentation.outputWasTruncated)
    #expect(presentation.fullDiagnosticOutput == oversizedResult)
    #expect(presentation.status == .pending)
}

@Test func markdownStreamingOnlyCommitsClosedBlocksAndBoundsLiveTail() {
    #expect(MarkdownStreamingBoundary.closedPrefixEnd(in: "first paragraph\n\npartial") == "first paragraph\n\n".count)
    #expect(MarkdownStreamingBoundary.closedPrefixEnd(in: "```swift\nlet a = 1\n```\ntail") == "```swift\nlet a = 1\n```\n".count)
    #expect(MarkdownStreamingBoundary.maximumLiveTailBytes == 8 * 1024)

    let multiScalarTail = String(repeating: "x", count: 8_191) + "👩🏽‍💻"
    let styled = multiScalarTail.prefixCharacters(nearUTF8ByteCount: MarkdownStreamingBoundary.maximumLiveTailBytes)
    #expect(String(styled) == String(multiScalarTail.prefix(8_191)))
    #expect(String(multiScalarTail.dropFirst(styled.count)) == "👩🏽‍💻")
}

@Test func auditedToolFamiliesUseRealInputSchemasAndTruthfulResults() {
    let read = ToolPresentation(name: "read_file", arguments: #"{"path":"src/main.swift","line":8,"limit":12}"#, output: "line 8", status: "complete")
    #expect(read.target == "src/main.swift")
    #expect(read.fields.contains(where: { $0.name == "Line" && $0.value == "8" }))
    #expect(read.fields.contains(where: { $0.name == "Limit" && $0.value == "12" }))

    let fetch = ToolPresentation(name: "fetch", arguments: #"{"urls":["https://example.test"],"timeout":4,"format":"markdown"}"#, output: ##"[{"url":"https://example.test","statusCode":200,"status":"OK","contentType":"text/html","contentLength":50,"body":"Page\nHi"}]"##, status: "complete")
    #expect(fetch.family == .fetch)
    #expect(fetch.target == "https://example.test")
    #expect(fetch.items.first?.title == "https://example.test")
    #expect(fetch.metrics.contains(where: { $0.name == "HTTP status" && $0.value == "200" }))

    let receipt = ToolPresentation(name: "send_message", arguments: #"{"to":"worker","message":"Check this"}"#, output: #"{"target":"worker","accepted":true,"idempotent":false,"durable":true,"queued":false,"disposition":"guidance"}"#, status: "complete")
    #expect(receipt.requestReceipt?.accepted == true)
    #expect(receipt.requestReceipt?.durable == true)
    #expect(receipt.resultSummary == "Accepted")

    let edit = ToolPresentation(name: "edit_file", arguments: #"{"path":"src/main.swift","edits":[{"oldText":"let a = 1","newText":"let a = 2"}]}"#, output: "File edited successfully. Replaced 9 characters", status: "complete")
    #expect(edit.requestedEdits.count == 1)
    #expect(edit.requestedEdits[0].oldText == "let a = 1")
    #expect(edit.requestedEdits[0].newText == "let a = 2")
    #expect(edit.patch.isEmpty, "Successful prose must never be mistaken for an applied unified diff")

    let task = ToolPresentation(name: "get_task", arguments: #"{"id":"t1"}"#, output: #"{"id":"t1","title":"Build UI","status":"pending","effectiveStatus":"blocked","dependencies":["t0"]}"#, status: "complete")
    #expect(task.family == .task)
    #expect(task.items.first?.state == "blocked")
    #expect(task.items.first?.facts.contains(where: { $0.name == "Dependencies" }) == true)
}

@Test func auditedFamiliesClassifyEveryAddedBuiltinAndKeepPromptsAndThoughtsPurposeful() {
    let names: [(String, ToolPresentation.Family)] = [
        ("lsp_diagnostics", .code), ("add_memory", .memory), ("create_schedule", .schedule),
        ("list_sessions", .session), ("read_skill_file", .skill), ("change_model", .configuration),
        ("enable_remote_mcp_server", .integration), ("user_prompt", .prompt), ("think", .reasoning),
        ("run_background_agent", .delegation), ("send_webhook", .communication), ("rag", .search),
        ("open_url", .fetch), ("search_tool", .integration),
        ("shell", .command),
        ("list_running_processes", .command), ("inspect_running_process", .command), ("kill_running_process", .command),
        ("read_file", .fileRead), ("read_multiple_files", .fileRead), ("write_file", .fileWrite), ("create_directory", .fileWrite), ("remove_directory", .fileWrite), ("edit_file", .fileEdit), ("directory_tree", .directory), ("list_directory", .directory), ("search_files_content", .search),
        ("git_status", .git), ("git_log", .git), ("git_branches", .git), ("git_show", .git), ("git_blame", .git),
        ("transfer_task", .delegation), ("handoff", .delegation), ("spawn_subagent", .delegation), ("stop_subagent", .delegation), ("list_agents", .communication), ("read_subagent", .communication), ("send_message", .communication),
        ("create_todo", .todo), ("create_todos", .todo), ("update_todos", .todo), ("list_todos", .todo),
        ("create_task", .task), ("get_task", .task), ("update_task", .task), ("delete_task", .task), ("list_tasks", .task), ("next_task", .task), ("add_dependency", .task), ("remove_dependency", .task),
        ("write_plan", .plan), ("read_plan", .plan), ("list_plans", .plan), ("delete_plan", .plan), ("update_plan_from_file", .plan), ("export_plan_to_file", .plan), ("set_plan_status", .plan), ("get_plan_status", .plan),
        ("lsp_workspace", .code), ("lsp_hover", .code), ("lsp_definition", .code), ("lsp_references", .code), ("lsp_document_symbols", .code), ("lsp_workspace_symbols", .code), ("lsp_diagnostics", .code), ("lsp_rename", .code), ("lsp_code_actions", .code), ("lsp_format", .code), ("lsp_call_hierarchy", .code), ("lsp_type_hierarchy", .code), ("lsp_implementations", .code), ("lsp_signature_help", .code), ("lsp_inlay_hints", .code),
        ("get_environment_info", .configuration), ("team_edit", .configuration), ("team_edit_persist", .configuration), ("team_edit_export", .configuration), ("change_model", .configuration), ("revert_model", .configuration),
        ("read_skill", .skill), ("read_skill_file", .skill), ("run_skill", .delegation), ("run_background_agent", .delegation), ("list_background_agents", .delegation), ("view_background_agent", .delegation), ("stop_background_agent", .delegation),
        ("add_memory", .memory), ("search_memories", .memory), ("get_memories", .memory), ("update_memory", .memory), ("delete_memory", .memory),
        ("create_schedule", .schedule), ("list_schedules", .schedule), ("cancel_schedule", .schedule), ("list_sessions", .session), ("read_session", .session),
        ("search_remote_mcp_servers", .integration), ("list_remote_mcp_servers", .integration), ("enable_remote_mcp_server", .integration), ("disable_remote_mcp_server", .integration), ("reset_remote_mcp_server_auth", .integration), ("add_tool", .integration),
        ("user_prompt", .prompt), ("think", .reasoning), ("send_webhook", .communication), ("open_url", .fetch)
    ]
    for (name, family) in names {
        #expect(ToolPresentation(name: name, arguments: "{}", output: "", status: nil).family == family)
    }

    let prompt = ToolPresentation(name: "user_prompt", arguments: #"{"message":"Choose a direction","schema":{"type":"object","properties":{"path":{"type":"string"}}}}"#, output: #"{"action":"accept","content":{"path":"src"}}"#, status: "complete")
    #expect(prompt.contentPreview == "Choose a direction")
    #expect(prompt.fields.contains(where: { $0.name == "Fields" && $0.value == "path" }))
    #expect(prompt.resultSummary == "Response received")

    let thought = ToolPresentation(name: "think", arguments: #"{"thought":"Consider options"}"#, output: "", status: "complete")
    #expect(thought.family == .reasoning)
    #expect(thought.contentPreview == "Consider options")
}

@Test @MainActor func markdownStreamingReusesClosedMarkdownAndBoundsIncrementalScanWork() {
    MarkdownCache.resetPerformanceCounters()
    let largeHistory = String(repeating: "# Cached history\n\nBody with **markdown**.\n\n", count: 1_400)
    _ = MarkdownCache.rendered(largeHistory)
    let afterLargeParse = MarkdownCache.parseCount
    #expect(largeHistory.utf8.count > 48_000 && largeHistory.utf8.count < 128_000)
    _ = MarkdownCache.rendered(largeHistory)
    #expect(MarkdownCache.parseCount == afterLargeParse)
    #expect(MarkdownCache.cacheHitCount == 1)
    let history = String(repeating: "## Finished work\n\nStable **markdown** block.\n\n", count: 1_400) // ~64 KiB
    var streaming = MarkdownStreamingBlocks()
    let first = streaming.update(history + "partial")
    #expect(first.newlyClosed.utf8.count == history.utf8.count)
    #expect(first.liveTail == "partial")
    _ = MarkdownCache.rendered(first.newlyClosed, cache: false)
    let countAfterStableParse = MarkdownCache.parseCount

    var content = history + "partial"
    let began = ContinuousClock.now
    for _ in 0..<400 {
        content += "x"
        let update = streaming.update(content)
        #expect(update.newlyClosed.isEmpty)
        #expect(update.liveTail.utf8.count <= MarkdownStreamingBoundary.maximumLiveTailBytes + 400)
        _ = MarkdownCache.rendered(update.liveTail, cache: false)
    }
    let elapsed = began.duration(to: .now)
    #expect(streaming.committedPrefix == history)
    #expect(MarkdownCache.parseCount == countAfterStableParse + 400, "Closed history must not be reparsed for each tail update")
    print("Markdown parse budget: 1 committed 64KiB history parse + 400 throttled tail parses in \(elapsed)")
}

@Test func agentMessageAttributionNeverInventsAnEmptyAgentIdentity() {
    #expect(AgentMessageAttribution.namedOwner("reviewer") == "reviewer")
    #expect(AgentMessageAttribution.accessibilityName("reviewer") == "reviewer")
    #expect(AgentMessageAttribution.namedOwner(nil) == nil)
    #expect(AgentMessageAttribution.namedOwner("") == nil)
    #expect(AgentMessageAttribution.namedOwner("  \n") == nil)
    #expect(AgentMessageAttribution.accessibilityName(nil) == "another agent")
    #expect(AgentMessageAttribution.accessibilityName("") == "another agent")
}

@Test func settledStreamingBuffersReleaseTheirLargeAuthoritativeCopiesAndCanRestart() {
    var reveal = StreamingTextReveal()
    var markdown = MarkdownStreamingBlocks()
    let large = String(repeating: "x", count: 16 * 1024)
    _ = reveal.update(large, isStreaming: true, reduceMotion: false)
    let streamed = markdown.update(large + "\n\n")
    #expect(!streamed.newlyClosed.isEmpty)
    #expect(reveal.retainedByteCount > large.utf8.count)
    #expect(markdown.retainedByteCount >= large.utf8.count)

    reveal.discardTransientBuffers()
    markdown.reset(releasingCapacity: true)
    #expect(reveal.retainedByteCount == 0)
    #expect(markdown.retainedByteCount == 0)

    #expect(reveal.update("next turn", isStreaming: true, reduceMotion: false) == "next turn")
    #expect(markdown.update("next turn").liveTail == "next turn")
}

@Test func historicalAllTodosResultExtractsActualChecklistDescriptionsAndStatuses() {
    let result = ToolPresentation(name: "update_todos", arguments: #"{"updates":[{"id":"todo-1","status":"completed"}]}"#, output: #"{"updated":[{"id":"todo-1","status":"completed"}],"all_todos":[{"id":"todo-1","description":"Close pairing gap","status":"completed"}]}"#, status: "complete")
    #expect(result.family == .todo)
    #expect(result.items.count == 1)
    #expect(result.items[0].title == "Close pairing gap")
    #expect(result.items[0].state == "completed")
    #expect(result.resultSummary == "1 of 1 done")
}
