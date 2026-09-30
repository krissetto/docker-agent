# Dialog coverage audit

Shared contracts: `bounds_matrix_test.go` exercises 1×1 through 120×40 bounds,
positions, and standard-size title dividers. `interaction_response_test.go`
retains correlated answers, safe dismissals, and enabled-only action targets.
`picker_footer_test.go` checks unboxed auxiliary cells, exact click regions,
wrapping, keyboard selection, hover leases, and disabled targets.

| Family | State-specific coverage |
| --- | --- |
| Tool confirmation / rejection adapter | Six described policies, No default, reason custom/cancel, duplicate response, narrow scroll, metadata, static proposed-call preview |
| Exit / close-root / max iterations | Safe negative default, shortcuts, close/outside rejection, centered exit spacing and cell targets |
| Tour offer | Telemetry variants and explicit choices; shared lifecycle fixtures |
| OAuth / URL | Correlated authorization, negative dismissal, pending URL authorization |
| Elicitation | Schema/nested validation, required fields, disabled submission, correlated result |
| MCP prompt | Argument fields, validation, submit/cancel |
| Pending message edit | Draft, multiline editing, saving/error, Ctrl+Enter behavior, close guards |
| Multi-choice | Empty/custom/secondary choices, rejection reason correlation |
| Settings | Four categories, visual/nonvisual, complete target traversal, disabled children, staged apply/cancel, theme-independent save, panel reorder, wrapped categories |
| Sessions | Empty/filter/selection, star/filter/copy/delete/workspace modifiers, Load primary |
| Model | Empty/error/refresh, row details, Use model primary |
| File | File/directory, hidden/ignored, errors/empty, actual double-click and stable primary label width |
| Working directory | Browse/recent/pinned, visible tabs/hit offsets, Use/Open directory primary |
| Plan browser / status / name / delete | Empty/filter/version-disabled management, validated forms and safe delete |
| Plan detail | Versioned/versionless management, read-only body and plain auxiliary shortcuts |
| Commands / theme / effort | Empty/filter selection, exact activation and theme lifecycle |
| Panes | Search/identity, Apply primary, keyboard/double-click exact-once |
| Subagents | Empty/tree folding/identity, Attach primary, hover/selection |
| Todos | Empty/normal/remove-armed/saving/error, Unicode wrapped body, bounded navigation help |
| Snapshots | Empty/populated and reset semantics |
| Context / cost | Empty/live rows, selection-specific commands, Copy and other management hints without fake primary |
| Help | Context/reference categories; existing category guide retained without duplicate footer |
| Tools / skills / permissions | Empty/populated, permissions YOLO, read-only height cap and navigation |
| Agent / panel details | Optional sections, read-only cap and scroll |
| Attachment text / fallback | Literal whitespace, binary/control rejection, read errors, cap/scroll |
| Native image preview | Native/no-upscale fit, cell metrics, title divider, narrow bounds, animation suppression, placement clipping/deletion |

Dedicated tests accompany each family. Standard-size divider coverage is uniform;
when a terminal cannot physically fit header, content and safe actions, shared
compact chrome may omit the title/divider before making decisions unreachable.
Native image fitting remains independent of reader height caps. No functional
keypress, permission default, request identity, or destructive-action contract was
changed by this audit.
