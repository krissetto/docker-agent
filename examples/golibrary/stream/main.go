package main

import (
	"context"
	"log"
	"os/signal"
	"syscall"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/model/provider/openai"
	"github.com/docker/docker-agent/pkg/runtime"
	runtimeclient "github.com/docker/docker-agent/pkg/runtime/client"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
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
	_, err = runtimeclient.RunTurnFuncOwnedInteractions(ctx, handle, runtime.TurnInput{Content: "How are you doing?"}, func(ctx context.Context, envelope runtime.SessionEvent) error {
		switch e := envelope.Event.(type) {
		case *runtime.AgentChoiceEvent:
			log.Printf("Agent %s: %s\n", e.AgentName, e.Content)
		case *runtime.StreamStartedEvent:
			log.Println("Stream started for session")
		case *runtime.StreamStoppedEvent:
			log.Println("Stream stopped for session")
		case *runtime.ToolCallConfirmationEvent:
			if err := handle.Respond(ctx, runtime.InteractionResponse{
				InteractionID: envelope.InteractionID,
				Kind:          runtime.InteractionConfirmation,
				Resume:        runtime.ResumeApproveAutonomous(),
			}); err != nil {
				return err
			}
		case *runtime.MaxIterationsReachedEvent:
			if err := handle.Respond(ctx, runtime.InteractionResponse{
				InteractionID: envelope.InteractionID,
				Kind:          runtime.InteractionMaxIterations,
				Resume:        runtime.ResumeReject("Maximum iterations require an interactive handler."),
			}); err != nil {
				return err
			}
		case *runtime.ElicitationRequestEvent:
			if err := handle.Respond(ctx, runtime.InteractionResponse{
				InteractionID: envelope.InteractionID,
				Kind:          runtime.InteractionElicitation,
				ElicitationID: e.ElicitationID,
				Elicitation:   runtime.ElicitationResult{Action: tools.ElicitationActionDecline},
			}); err != nil {
				return err
			}
		case *runtime.ToolCallEvent:
			log.Printf("Tool call: %s\n", e.ToolCall.Function.Name)
		case *runtime.ToolCallResponseEvent:
			log.Printf("Tool call response: %s\n", e.Response)
			// etc...
		}
		return nil
	})
	return err
}
