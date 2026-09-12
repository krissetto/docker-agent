package main

import (
	"context"
	"fmt"
	"log"
	"os/signal"
	"strings"
	"syscall"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/model/provider/openai"
	"github.com/docker/docker-agent/pkg/runtime"
	runtimeclient "github.com/docker/docker-agent/pkg/runtime/client"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if err := run(ctx); err != nil {
		log.Println(err)
	}
}

func run(ctx context.Context) error {
	llm, err := openai.NewClient(
		ctx,
		&latest.ModelConfig{
			Provider: "openai",
			Model:    "gpt-4o",
		},
		environment.NewDefaultProvider(),
	)
	if err != nil {
		return err
	}

	human := agent.New(
		"root",
		"You are a human, with feelings and emotions.",
		agent.WithModel(llm),
		agent.WithDescription("A human."),
	)

	humanTeam := team.New(team.WithAgents(human))
	defer func() { _ = humanTeam.StopToolSets(context.WithoutCancel(ctx)) }()

	rt, err := runtime.New(ctx, humanTeam)
	if err != nil {
		return err
	}
	defer rt.Close()

	sess := session.New(session.WithAgentName("root"))
	handle, err := rt.CreateSession(ctx, sess, runtime.SessionBinding{AgentName: "root"})
	if err != nil {
		return err
	}
	var response strings.Builder
	_, err = runtimeclient.RunTurnFunc(ctx, handle, runtime.TurnInput{Content: "How are you doing?"}, func(_ context.Context, envelope runtime.SessionEvent) error {
		switch event := envelope.Event.(type) {
		case *runtime.AgentChoiceEvent:
			response.WriteString(event.Content)
		case *runtime.StreamStoppedEvent:
			fmt.Println(response.String())
		}
		return nil
	})
	return err
}
