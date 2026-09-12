package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os/signal"
	"strconv"
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
	"github.com/docker/docker-agent/pkg/tools"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if err := run(ctx); err != nil {
		log.Println(err)
	}
}

type AddNumbersArgs struct {
	A int `json:"a"`
	B int `json:"b"`
}

func addNumbers(_ context.Context, toolCall tools.ToolCall, _ tools.Runtime) (*tools.ToolCallResult, error) {
	var p AddNumbersArgs
	if err := json.Unmarshal([]byte(toolCall.Function.Arguments), &p); err != nil {
		return nil, err
	}

	fmt.Println("Adding numbers", p.A, p.B)

	return tools.ResultSuccess(strconv.Itoa(p.A + p.B)), nil
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

	toolAddNumbers := tools.Tool{
		Name:        "add",
		Category:    "compute",
		Description: "Add two numbers",
		Parameters:  tools.MustSchemaFor[AddNumbersArgs](),
		Handler:     addNumbers,
	}

	calculator := agent.New(
		"root",
		"You are a human, with feelings and emotions.",
		agent.WithModel(llm),
		agent.WithTools(toolAddNumbers),
	)

	calculatorTeam := team.New(team.WithAgents(calculator))
	defer func() { _ = calculatorTeam.StopToolSets(context.WithoutCancel(ctx)) }()

	rt, err := runtime.New(ctx, calculatorTeam)
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
	_, err = runtimeclient.RunTurnFunc(ctx, handle, runtime.TurnInput{Content: "What is 1 + 2?"}, func(_ context.Context, envelope runtime.SessionEvent) error {
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
