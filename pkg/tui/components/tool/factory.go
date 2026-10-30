// Package tool builds the TUI view for a tool call message.
//
// A small lookup table (builders) maps each tool's name to a constructor.
// Lookup order is: exact tool name, then "category:<category>", then a
// defaulttool fallback.
package tool

import (
	"slices"
	"sync"

	"github.com/docker/docker-agent/pkg/app/lifecycle"
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
	"github.com/docker/docker-agent/pkg/tui/components/tool/codemode"
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
	"run_tools_with_javascript":           codemode.New,
	filesystem.ToolNameEditFile:           editfile.New,
	filesystem.ToolNameWriteFile:          writefile.New,
	filesystem.ToolNameReadFile:           readfile.New,
	filesystem.ToolNameReadMultipleFiles:  readmultiplefiles.New,
	filesystem.ToolNameListDirectory:      listdirectory.New,
	filesystem.ToolNameDirectoryTree:      directorytree.New,
	filesystem.ToolNameSearchFilesContent: searchfilescontent.New,
	shelltool.ToolNameShell:               shell.New,
	"category:shell":                      shell.New,
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

// Registry resolves a tool renderer for a message. Custom renderers registered
// on an instance take precedence over the built-in table at each tier: exact
// tool name, then "category:<name>", then the generic fallback. Each
// application owns its registry, so renderers registered by one embedder never
// leak into another instance.
type Registry struct {
	mu           sync.RWMutex
	custom       map[string]Builder
	invalidators []func()
}

// NewRegistry returns a registry with no custom renderers. Built-in renderers
// are resolved from the shared builtin table.
func NewRegistry() *Registry {
	return &Registry{custom: map[string]Builder{}}
}

// defaultRegistry serves package-level New for callers that do not own a
// registry. It is never registered into; embedders use their own instance.
var defaultRegistry = NewRegistry()

// DefaultRegistry returns the shared registry with no custom renderers. Do not
// register renderers on it; use NewRegistry for an embedder's own renderers.
func DefaultRegistry() *Registry {
	return defaultRegistry
}

// Register installs a custom renderer for the given key, which is a tool name
// (e.g. "add") or a "category:<name>" key (e.g. "category:compute"). Registering
// a key that already exists replaces the previous renderer. Registered renderers
// take precedence over the built-in ones.
func (r *Registry) Register(key string, b Builder) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.custom == nil {
		r.custom = map[string]Builder{}
	}
	r.custom[key] = b
}

// RegisterCacheInvalidator adds a theme-change hook for a renderer's shared caches.
func (r *Registry) RegisterCacheInvalidator(invalidate func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.invalidators = append(r.invalidators, invalidate)
}

// InvalidateCaches runs every registered cache invalidator, e.g. on theme change.
func (r *Registry) InvalidateCaches() {
	r.mu.RLock()
	invalidators := slices.Clone(r.invalidators)
	r.mu.RUnlock()
	for _, invalidate := range invalidators {
		invalidate()
	}
}

// resolve returns the renderer for key, preferring a registered custom renderer
// over the built-in one.
func (r *Registry) resolve(key string) (Builder, bool) {
	r.mu.RLock()
	b, ok := r.custom[key]
	r.mu.RUnlock()
	if ok {
		return b, true
	}
	b, ok = builders[key]
	return b, ok
}

// New returns the appropriate tool view for the given message using the shared
// default registry. Embedders that register renderers call [Registry.New].
func New(ar *animation.Runtime, msg *types.Message, sessionState service.SessionStateReader, indexes ...*subagentindex.Index) layout.Model {
	return defaultRegistry.New(ar, msg, sessionState, indexes...)
}

// New returns the appropriate tool view for the given message.
// Lookup order: exact tool name, then "category:<category>", then default.
// At each tier a registered custom renderer wins over the built-in one.
func (r *Registry) customBuilder(key string) (Builder, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	b, ok := r.custom[key]
	return b, ok
}

func (r *Registry) New(ar *animation.Runtime, msg *types.Message, sessionState service.SessionStateReader, indexes ...*subagentindex.Index) layout.Model {
	var lookup subagenttool.NameLookup
	var reference subagenttool.ReferenceLookup
	if len(indexes) > 0 && indexes[0] != nil {
		lookup = indexes[0].Name
		reference = func(id subagentpkg.NodeID) lifecycle.InputReference {
			if id == subagentpkg.ParentAlias {
				return indexes[0].Parent()
			}
			return indexes[0].Resolve("", string(id), "")
		}
	}
	name := msg.ToolCall.Function.Name
	if b, ok := r.customBuilder(name); ok {
		return withInlineImages(b(ar, msg, sessionState), msg.Images, sessionState)
	}
	switch name {
	case subagentpkg.ToolSpawnSubagent:
		return withInlineImages(subagenttool.NewSpawn(ar, msg, sessionState, lookup, reference), msg.Images, sessionState)
	case subagentpkg.ToolSendMessage:
		return withInlineImages(subagenttool.NewSend(ar, msg, sessionState, lookup, reference), msg.Images, sessionState)
	case subagentpkg.ToolReadSubagent:
		return withInlineImages(subagenttool.NewRead(ar, msg, sessionState, lookup, reference), msg.Images, sessionState)
	case subagentpkg.ToolStopSubagent:
		return withInlineImages(subagenttool.NewStop(ar, msg, sessionState, lookup, reference), msg.Images, sessionState)
	case subagentpkg.ToolListAgents:
		return withInlineImages(subagenttool.NewList(ar, msg, sessionState, reference), msg.Images, sessionState)
	}
	var view layout.Model
	if b, ok := r.resolve(msg.ToolCall.Function.Name); ok {
		view = b(ar, msg, sessionState)
	} else if cat := msg.ToolDefinition.Category; cat != "" {
		if b, ok := r.resolve("category:" + cat); ok {
			view = b(ar, msg, sessionState)
		}
	}
	if view == nil {
		view = defaulttool.New(ar, msg, sessionState)
	}
	return withInlineImages(view, msg.Images, sessionState)
}
