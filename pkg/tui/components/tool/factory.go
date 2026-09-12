// Package tool builds the TUI view for a tool call message.
//
// A small lookup table (builders) maps each tool's name to a constructor.
// Lookup order is: exact tool name, then "category:<category>", then a
// defaulttool fallback.
package tool

import (
	"sync"

	subagentpkg "github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/tools/builtin/fetch"
	"github.com/docker/docker-agent/pkg/tools/builtin/filesystem"
	handofftool "github.com/docker/docker-agent/pkg/tools/builtin/handoff"
	"github.com/docker/docker-agent/pkg/tools/builtin/plan"
	shelltool "github.com/docker/docker-agent/pkg/tools/builtin/shell"
	"github.com/docker/docker-agent/pkg/tools/builtin/todo"
	transfertasktool "github.com/docker/docker-agent/pkg/tools/builtin/transfertask"
	userpromptool "github.com/docker/docker-agent/pkg/tools/builtin/userprompt"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/tool/api"
	"github.com/docker/docker-agent/pkg/tui/components/tool/defaulttool"
	"github.com/docker/docker-agent/pkg/tui/components/tool/directorytree"
	"github.com/docker/docker-agent/pkg/tui/components/tool/editfile"
	"github.com/docker/docker-agent/pkg/tui/components/tool/handoff"
	"github.com/docker/docker-agent/pkg/tui/components/tool/listdirectory"
	"github.com/docker/docker-agent/pkg/tui/components/tool/plantool"
	"github.com/docker/docker-agent/pkg/tui/components/tool/readfile"
	"github.com/docker/docker-agent/pkg/tui/components/tool/readmultiplefiles"
	"github.com/docker/docker-agent/pkg/tui/components/tool/searchfilescontent"
	"github.com/docker/docker-agent/pkg/tui/components/tool/shell"
	"github.com/docker/docker-agent/pkg/tui/components/tool/subagenttool"
	"github.com/docker/docker-agent/pkg/tui/components/tool/todotool"
	"github.com/docker/docker-agent/pkg/tui/components/tool/transfertask"
	"github.com/docker/docker-agent/pkg/tui/components/tool/userprompt"
	"github.com/docker/docker-agent/pkg/tui/components/tool/writefile"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/subagentindex"
	"github.com/docker/docker-agent/pkg/tui/types"
)

// Builder constructs the layout.Model for a tool message. Embedders implement
// this to provide a custom view for a tool and register it via Register.
type Builder = func(ar *animation.Runtime, msg *types.Message, sessionState service.SessionStateReader) layout.Model

// builders maps a tool name (or a "category:<name>" key) to its renderer.
// Tools sharing the same visual representation point at the same builder.
var builders = map[string]Builder{
	transfertasktool.ToolNameTransferTask: transfertask.New,
	handofftool.ToolNameHandoff:           handoff.New,
	filesystem.ToolNameEditFile:           editfile.New,
	filesystem.ToolNameWriteFile:          writefile.New,
	filesystem.ToolNameReadFile:           readfile.New,
	filesystem.ToolNameReadMultipleFiles:  readmultiplefiles.New,
	filesystem.ToolNameListDirectory:      listdirectory.New,
	filesystem.ToolNameDirectoryTree:      directorytree.New,
	filesystem.ToolNameSearchFilesContent: searchfilescontent.New,
	shelltool.ToolNameShell:               shell.New,
	userpromptool.ToolNameUserPrompt:      userprompt.New,
	fetch.ToolNameFetch:                   api.New,
	"category:api":                        api.New,
	todo.ToolNameCreateTodo:               todotool.New,
	todo.ToolNameCreateTodos:              todotool.New,
	todo.ToolNameUpdateTodos:              todotool.New,
	todo.ToolNameListTodos:                todotool.New,
	// Single-plan write/status tools surface the plan's status/title in a
	// compact header. read_plan, list_plans and delete_plan intentionally keep
	// the default renderer: read_plan's job is to show the full plan body (the
	// default renderer prints it, with the status still in the JSON), list_plans
	// returns many plans, and delete_plan has no status to show.
	plan.ToolNameWritePlan:          plantool.New,
	plan.ToolNameSetPlanStatus:      plantool.New,
	plan.ToolNameGetPlanStatus:      plantool.New,
	plan.ToolNameUpdatePlanFromFile: plantool.New,
	plan.ToolNameExportPlanToFile:   plantool.New,
}

// custom holds renderers registered by embedders via Register. They are
// consulted before the built-in builders, so an embedder can override a
// built-in tool's view as well as render tools cagent has no view for.
var (
	customMu sync.RWMutex
	custom   = map[string]Builder{}
)

// Register installs a custom renderer for the given key, which is a tool name
// (e.g. "add") or a "category:<name>" key (e.g. "category:compute"). Registering
// a key that already exists replaces the previous renderer. Registered renderers
// take precedence over the built-in ones.
//
// Register is safe for concurrent use, but is normally called once at startup
// (e.g. via tui.WithToolRenderers) before the TUI begins rendering.
func Register(key string, b Builder) {
	customMu.Lock()
	defer customMu.Unlock()
	custom[key] = b
}

func customBuilder(key string) (Builder, bool) {
	customMu.RLock()
	defer customMu.RUnlock()
	b, ok := custom[key]
	return b, ok
}

// resolve returns the renderer for key, preferring a registered custom renderer
// over the built-in one.
func resolve(key string) (Builder, bool) {
	customMu.RLock()
	b, ok := custom[key]
	customMu.RUnlock()
	if ok {
		return b, true
	}
	b, ok = builders[key]
	return b, ok
}

// New returns the appropriate tool view for the given message.
// Lookup order: exact tool name, then "category:<category>", then default.
// At each tier a registered custom renderer wins over the built-in one.
func New(ar *animation.Runtime, msg *types.Message, sessionState service.SessionStateReader, indexes ...*subagentindex.Index) layout.Model {
	var lookup subagenttool.NameLookup
	if len(indexes) > 0 && indexes[0] != nil {
		lookup = indexes[0].Name
	}
	name := msg.ToolCall.Function.Name
	if b, ok := customBuilder(name); ok {
		return withInlineImages(b(ar, msg, sessionState), msg.Images, sessionState)
	}
	switch name {
	case subagentpkg.ToolSpawnSubagent:
		return withInlineImages(subagenttool.NewSpawn(ar, msg, sessionState, lookup), msg.Images, sessionState)
	case subagentpkg.ToolSendMessage:
		return withInlineImages(subagenttool.NewSend(ar, msg, sessionState, lookup), msg.Images, sessionState)
	case subagentpkg.ToolReadSubagent:
		return withInlineImages(subagenttool.NewRead(ar, msg, sessionState, lookup), msg.Images, sessionState)
	case subagentpkg.ToolStopSubagent:
		return withInlineImages(subagenttool.NewStop(ar, msg, sessionState, lookup), msg.Images, sessionState)
	}
	var view layout.Model
	if b, ok := resolve(msg.ToolCall.Function.Name); ok {
		view = b(ar, msg, sessionState)
	} else if cat := msg.ToolDefinition.Category; cat != "" {
		if b, ok := resolve("category:" + cat); ok {
			view = b(ar, msg, sessionState)
		}
	}
	if view == nil {
		view = defaulttool.New(ar, msg, sessionState)
	}
	return withInlineImages(view, msg.Images, sessionState)
}
