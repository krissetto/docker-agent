package teamloader

import (
	"context"
	"io"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/builtin/backgroundjobs"
	builtinshell "github.com/docker/docker-agent/pkg/tools/builtin/shell"
)

type mergeableSet struct {
	name string
}

func (m *mergeableSet) Tools(context.Context) ([]tools.Tool, error) {
	return []tools.Tool{{Name: m.name}}, nil
}
func (m *mergeableSet) MergeKey() string { return "fake" }
func (m *mergeableSet) Merge(siblings []tools.MergeSibling) tools.ToolSet {
	return &mergedSet{siblings: siblings}
}

type mergedSet struct {
	siblings []tools.MergeSibling
}

func (m *mergedSet) Tools(context.Context) ([]tools.Tool, error) {
	return []tools.Tool{{Name: "merged"}}, nil
}

type plainSet struct{}

func (plainSet) Tools(context.Context) ([]tools.Tool, error) {
	return []tools.Tool{{Name: "plain"}}, nil
}

func TestGetToolsForAgent_MergesSiblingMergeableToolsets(t *testing.T) {
	t.Parallel()

	registry := NewToolsetRegistry(map[string]ToolsetCreator{
		"fake": func(_ context.Context, ts latest.Toolset, _ string, _ *config.RuntimeConfig, _ string) (tools.ToolSet, error) {
			return &mergeableSet{name: ts.Command}, nil
		},
		"plain": func(context.Context, latest.Toolset, string, *config.RuntimeConfig, string) (tools.ToolSet, error) {
			return plainSet{}, nil
		},
	})
	runConfig := config.RuntimeConfig{}
	expander := newEnvExpander(runConfig.EnvProvider())

	t.Run("single mergeable toolset is used as-is", func(t *testing.T) {
		t.Parallel()
		a := &latest.AgentConfig{Toolsets: []latest.Toolset{{Type: "fake", Command: "a"}, {Type: "plain"}}}
		got, warnings, err := getToolsForAgent(t.Context(), a, ".", &runConfig, "cfg", &loadOptions{toolsetRegistry: registry}, expander)
		require.NoError(t, err)
		assert.Empty(t, warnings)
		require.Len(t, got, 2)
		names := allToolNames(t, got)
		assert.ElementsMatch(t, []string{"plain", "a"}, names)
	})

	t.Run("several are merged into one", func(t *testing.T) {
		t.Parallel()
		a := &latest.AgentConfig{Toolsets: []latest.Toolset{{Type: "fake", Command: "a"}, {Type: "plain"}, {Type: "fake", Command: "b"}}}
		got, _, err := getToolsForAgent(t.Context(), a, ".", &runConfig, "cfg", &loadOptions{toolsetRegistry: registry}, expander)
		require.NoError(t, err)
		require.Len(t, got, 2)
		assert.Equal(t, []string{"plain", "merged"}, allToolNames(t, got))
		merged, ok := got[1].(*mergedSet)
		require.True(t, ok)
		require.Len(t, merged.siblings, 2)
		assert.Equal(t, "a", merged.siblings[0].Raw.(*mergeableSet).name)
		assert.Equal(t, "b", merged.siblings[1].Raw.(*mergeableSet).name)
	})
}

func allToolNames(t *testing.T, sets []tools.ToolSet) []string {
	t.Helper()
	var names []string
	for _, s := range sets {
		names = append(names, toolNames(t, s)...)
	}
	return names
}

func TestShellGetsSharedBackgroundOwnerAndOnlyOneInspectionSet(t *testing.T) {
	runConfig := config.RuntimeConfig{Config: config.Config{WorkingDir: t.TempDir()}}
	pool, err := NewToolsetLeasePool("shell-source-runtime")
	require.NoError(t, err)
	loadOpts := &loadOptions{toolsetRegistry: testToolsetRegistry(), toolsetPool: pool, toolsetPoolScope: "shell-source-runtime", ownedResources: new([]io.Closer)}
	agentConfig := &latest.AgentConfig{Toolsets: []latest.Toolset{{Type: "shell"}, {Type: "shell"}, {Type: "background_jobs"}}}
	sets, _, err := getToolsForAgent(t.Context(), agentConfig, ".", &runConfig, "team.yaml", loadOpts, newEnvExpander(runConfig.EnvProvider()))
	require.NoError(t, err)
	var shellSets []*builtinshell.ToolSet
	for _, set := range sets {
		if raw, ok := tools.As[*builtinshell.ToolSet](set); ok {
			shellSets = append(shellSets, raw)
		}
	}
	require.Len(t, shellSets, 2)
	first, err := shellSets[0].Tools(t.Context())
	require.NoError(t, err)
	second, err := shellSets[1].Tools(t.Context())
	require.NoError(t, err)
	count := func(list []tools.Tool, name string) int {
		n := 0
		for _, tool := range list {
			if tool.Name == name {
				n++
			}
		}
		return n
	}
	require.Equal(t, 0, count(first, backgroundjobs.ToolNameRunBackgroundJob))
	require.Equal(t, 0, count(first, backgroundjobs.ToolNameViewBackgroundJob))
	require.Equal(t, 0, count(second, backgroundjobs.ToolNameViewBackgroundJob))
	var declaredJobs tools.ToolSet
	for _, set := range sets {
		if _, ok := tools.As[*backgroundjobs.ToolSet](set); ok {
			declaredJobs = set
		}
	}
	require.NotNil(t, declaredJobs)
	declaredTools, err := declaredJobs.Tools(t.Context())
	require.NoError(t, err)
	require.Equal(t, 1, count(declaredTools, backgroundjobs.ToolNameRunBackgroundJob))
	require.Equal(t, 1, count(declaredTools, backgroundjobs.ToolNameViewBackgroundJob))
	for _, resource := range *loadOpts.ownedResources {
		require.NoError(t, resource.Close())
	}
	require.NoError(t, pool.Close(t.Context()))
}

func TestShellOnlyGetsPooledSyntheticInspectionOwner(t *testing.T) {
	runConfig := config.RuntimeConfig{Config: config.Config{WorkingDir: t.TempDir()}}
	pool, err := NewToolsetLeasePool("shell-only-scope")
	require.NoError(t, err)
	load := func() (*builtinshell.ToolSet, []io.Closer) {
		resources := new([]io.Closer)
		opts := &loadOptions{toolsetRegistry: testToolsetRegistry(), toolsetPool: pool, toolsetPoolScope: "shell-only-scope", ownedResources: resources}
		sets, _, err := getToolsForAgent(t.Context(), &latest.AgentConfig{Toolsets: []latest.Toolset{{Type: "shell"}}}, ".", &runConfig, "team.yaml", opts, newEnvExpander(runConfig.EnvProvider()))
		require.NoError(t, err)
		require.Len(t, sets, 1)
		tool, ok := tools.As[*builtinshell.ToolSet](sets[0])
		require.True(t, ok)
		return tool, *resources
	}
	first, firstClosers := load()
	second, secondClosers := load()
	one, err := first.Tools(t.Context())
	require.NoError(t, err)
	two, err := second.Tools(t.Context())
	require.NoError(t, err)
	for _, name := range []string{backgroundjobs.ToolNameListBackgroundJobs, backgroundjobs.ToolNameViewBackgroundJob, backgroundjobs.ToolNameWaitBackgroundJob, backgroundjobs.ToolNameStopBackgroundJob} {
		require.Equal(t, 1, countToolName(one, name))
		require.Equal(t, 1, countToolName(two, name))
	}
	require.Equal(t, 0, countToolName(one, backgroundjobs.ToolNameRunBackgroundJob))
	for _, closer := range firstClosers {
		require.NoError(t, closer.Close())
	}
	for _, closer := range secondClosers {
		require.NoError(t, closer.Close())
	}
	require.NoError(t, pool.Close(t.Context()))
}

func countToolName(list []tools.Tool, name string) int {
	n := 0
	for _, tool := range list {
		if tool.Name == name {
			n++
		}
	}
	return n
}

func TestShellSyntheticManagerSurvivesLeaseRevisionAndEnvironmentEdit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell job test")
	}
	pool, err := NewToolsetLeasePool("edit-source-runtime")
	require.NoError(t, err)
	wd := t.TempDir()
	build := func(marker string) (*builtinshell.ToolSet, io.Closer) {
		runConfig := config.RuntimeConfig{Config: config.Config{WorkingDir: wd}}
		resources := new([]io.Closer)
		opts := &loadOptions{toolsetRegistry: testToolsetRegistry(), toolsetPool: pool, toolsetPoolScope: "edit-source-runtime", ownedResources: resources}
		sets, _, err := getToolsForAgent(t.Context(), &latest.AgentConfig{Toolsets: []latest.Toolset{{Type: "shell", Env: map[string]string{"SHELL_EDIT_MARKER": marker}}}}, ".", &runConfig, "edited.yaml", opts, newEnvExpander(runConfig.EnvProvider()))
		require.NoError(t, err)
		raw, ok := tools.As[*builtinshell.ToolSet](sets[0])
		require.True(t, ok)
		require.Len(t, *resources, 1)
		return raw, (*resources)[0]
	}
	old, oldLease := build("old")
	owner := tools.NewResourceOwner()
	var registered *shellJobWork
	workRuntime := &shellJobWorkRuntime{begin: func() tools.BackgroundWork {
		registered = &shellJobWork{complete: make(chan string, 1), closed: make(chan struct{})}
		return registered
	}}
	oldTools, err := old.Tools(t.Context())
	require.NoError(t, err)
	var shellTool tools.Tool
	for _, candidate := range oldTools {
		if candidate.Name == builtinshell.ToolNameShell {
			shellTool = candidate
		}
	}
	require.NotNil(t, shellTool)
	result, err := shellTool.Handler(tools.WithResourceOwner(t.Context(), owner), tools.ToolCall{Function: tools.FunctionCall{Name: shellTool.Name, Arguments: `{"cmd":"echo old-start; sleep 2.5; echo old-finish","timeout":5}`}}, workRuntime)
	require.NoError(t, err)
	require.Contains(t, result.Output, "Background job started")
	newTool, newLease := build("new")
	definitions, err := newTool.Tools(t.Context())
	require.NoError(t, err)
	var view tools.Tool
	for _, candidate := range definitions {
		if candidate.Name == backgroundjobs.ToolNameViewBackgroundJob {
			view = candidate
		}
	}
	require.NotNil(t, view)
	id := strings.Fields(strings.Split(result.Output, "ID: ")[1])[0]
	viewResult, err := view.Handler(tools.WithResourceOwner(t.Context(), owner), tools.ToolCall{Function: tools.FunctionCall{Name: view.Name, Arguments: `{"job_id":"` + id + `"}`}}, tools.NopRuntime{})
	require.NoError(t, err)
	require.Contains(t, viewResult.Output, "old-start")
	require.NoError(t, oldLease.Close())
	select {
	case completion := <-registered.complete:
		require.Contains(t, completion, "old-finish")
	case <-time.After(4 * time.Second):
		t.Fatal("job failed to complete across revision")
	}
	require.NoError(t, newLease.Close())
	require.NoError(t, pool.Close(t.Context()))
}

type shellJobWorkRuntime struct {
	tools.NopRuntime
	begin func() tools.BackgroundWork
}

func (r *shellJobWorkRuntime) BeginBackgroundWork(context.Context) (tools.BackgroundWork, error) {
	return r.begin(), nil
}
func (*shellJobWorkRuntime) Supports(c tools.Capability) bool {
	return c == tools.CapabilityBackgroundWork
}

type shellJobWork struct {
	complete chan string
	closed   chan struct{}
	once     sync.Once
}

func (w *shellJobWork) Complete(s string) error { w.complete <- s; return nil }
func (*shellJobWork) Cancel()                   {}
func (w *shellJobWork) Close() error            { w.once.Do(func() { close(w.closed) }); return nil }
