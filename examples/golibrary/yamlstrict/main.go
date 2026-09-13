// Example: load an agent from YAML (a local file or an OCI reference) while
// linking only the providers, toolsets and agent sources this binary needs.
//
// The default registries in pkg/teamloader/defaults pull every provider SDK
// and every built-in toolset. Here the registries are hand-picked, and
// teamloader.WithStrict makes the load fail up front — listing every offending
// item — if the YAML asks for anything else.
//
//	go run ./examples/golibrary/yamlstrict ./agent.yaml
//	go run ./examples/golibrary/yamlstrict myorg/agent:latest
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/ocisource"
	"github.com/docker/docker-agent/pkg/host/lifecycle"
	"github.com/docker/docker-agent/pkg/host/turn"
	"github.com/docker/docker-agent/pkg/model/provider"
	"github.com/docker/docker-agent/pkg/model/provider/anthropic"
	"github.com/docker/docker-agent/pkg/model/provider/openai"
	"github.com/docker/docker-agent/pkg/runtime"
	runtimeclient "github.com/docker/docker-agent/pkg/runtime/client"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/teamloader"
	"github.com/docker/docker-agent/pkg/tools/builtin/api"
	"github.com/docker/docker-agent/pkg/tools/builtin/fetch"
	"github.com/docker/docker-agent/pkg/tools/builtin/think"
)

func main() {
	if len(os.Args) != 2 {
		log.Fatal("usage: yamlstrict <agent.yaml | oci-reference>")
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if err := run(ctx, os.Args[1]); err != nil {
		log.Println(err)
	}
}

func run(ctx context.Context, ref string) (retErr error) {
	// Pick the source type explicitly; each lives in its own package so only
	// the one you import is linked. pkg/config/sources resolves any kind of
	// reference (files, directories, URLs, OCI, aliases) at the cost of
	// linking all of them.
	var source config.Source
	if config.IsOCIReference(ref) {
		source = ocisource.New(ref)
	} else {
		source = config.NewFileSource(ref)
	}

	providers := provider.NewRegistry(map[string]provider.Factory{
		"anthropic": provider.Adapt(anthropic.NewClient),
		"openai":    provider.Adapt(openai.NewClient),
	})
	toolsets := teamloader.NewToolsetRegistry(map[string]teamloader.ToolsetCreator{
		"api":   api.Creator,
		"fetch": fetch.Creator,
		"think": teamloader.Creator(think.CreateToolSet),
	})

	runConfig := &config.RuntimeConfig{}
	team, err := teamloader.Load(ctx, source, runConfig,
		teamloader.WithProviderRegistry(providers),
		teamloader.WithToolsetRegistry(toolsets),
		// Reject anything else the YAML relies on: other provider types,
		// other toolset types, and every optional feature (hooks, skills,
		// harness, external sub-agents, code mode, ...). Third-party YAML
		// gets exactly the capabilities listed above, nothing more. Allow a
		// feature explicitly, e.g. WithStrict(config.FeatureSkills), only
		// after weighing what it lets the config reach on the host.
		teamloader.WithStrict(),
	)
	if err != nil {
		var unsupported *config.UnsupportedError
		if errors.As(err, &unsupported) {
			return fmt.Errorf("this binary cannot run %s:\n%w", ref, err)
		}
		return err
	}

	rt, err := runtime.New(ctx, team)
	if err != nil {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		return errors.Join(err, team.StopToolSets(cleanupCtx))
	}
	supervisor := lifecycle.OwnRuntime(runtime.NewSessionRuntimeSupervisor(rt), team)
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		retErr = errors.Join(retErr, supervisor.Shutdown(cleanupCtx))
	}()

	handle, err := supervisor.Runtime().CreateSession(ctx,
		session.New(session.WithNonInteractive(true)), runtime.SessionBinding{})
	if err != nil {
		return err
	}
	ownedTurn, err := turn.Start(ctx, handle, runtime.TurnInput{Content: "Introduce yourself in one sentence."})
	if err != nil {
		return err
	}
	termination := ownedTurn.Consume(ctx, func(_ context.Context, envelope runtime.SessionEvent) (runtimeclient.TurnDecision, error) {
		if event, ok := envelope.Event.(*runtime.ErrorEvent); ok {
			return runtimeclient.TurnTerminate, errors.New(event.Error)
		}
		return runtimeclient.TurnContinue, nil
	})
	if termination.Err != nil {
		return termination.Err
	}
	snapshot, err := handle.Snapshot(ctx)
	if err != nil {
		return err
	}
	fmt.Println(snapshot.GetLastAssistantMessageContent())
	return nil
}
