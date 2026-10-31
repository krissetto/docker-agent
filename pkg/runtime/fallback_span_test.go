package runtime

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/model/provider"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/telemetry/genai"
)

// servingTestStream reports the endpoint that produced it, as a routed stream does.
type servingTestStream struct {
	chat.MessageStream

	serving provider.ServingModel
}

func (s *servingTestStream) ServingModel() provider.ServingModel { return s.serving }

// Serial: the recording tracer replaces global OTel state.
func TestFallbackSpanServingModel(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() {
		otel.SetTracerProvider(previous)
		require.NoError(t, tp.Shutdown(context.WithoutCancel(t.Context())))
	})

	for _, tc := range []struct {
		name, servingModel, wantModel string
		plain                         bool
	}{
		{name: "routed leaf", servingModel: "leaf", wantModel: "leaf"},
		{name: "plain stream", plain: true, wantModel: "router"},
		{name: "dispatch identity", servingModel: "router", wantModel: "router"},
		{name: "empty serving model", wantModel: "router"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			exporter.Reset()
			var stream chat.MessageStream = newStreamBuilder().AddContent("ok").AddStopWithUsage(1, 1).Build()
			if !tc.plain {
				stream = &servingTestStream{MessageStream: stream, serving: provider.ServingModel{ID: modelsdev.NewID("test", tc.servingModel)}}
			}
			model := &queueProvider{id: "test/router", streams: []chat.MessageStream{stream}}
			a := agent.New("delegate", "test", agent.WithModel(model), agent.WithFallbackRetries(-1))
			executor := newFallbackExecutor()
			executor.cooldowns = newCooldownManager(time.Now)
			_, used, err := executor.execute(t.Context(), a, model, nil, nil, session.New(), nil, &collectSink{})
			require.NoError(t, err)
			assert.Same(t, model, used)
			assert.Nil(t, executor.cooldowns.Get(a.Name()))

			var fallbackSpans tracetest.SpanStubs
			for _, span := range exporter.GetSpans() {
				if span.Name == "runtime.fallback" {
					fallbackSpans = append(fallbackSpans, span)
				}
			}
			require.Len(t, fallbackSpans, 1)
			attrs := attribute.NewSet(fallbackSpans[0].Attributes...)
			for key, want := range map[string]attribute.Value{
				genai.AttrFallbackPrimaryModel: attribute.StringValue("router"),
				genai.AttrFallbackFinalModel:   attribute.StringValue(tc.wantModel),
				genai.AttrFallbackAttempts:     attribute.IntValue(1),
				genai.AttrFallbackOutcome:      attribute.StringValue(genai.FallbackOutcomeSuccess),
			} {
				got, ok := attrs.Value(attribute.Key(key))
				assert.True(t, ok, "missing span attribute %s", key)
				assert.Equal(t, want, got, "span attribute %s", key)
			}
		})
	}
}
