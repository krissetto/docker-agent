package webhook

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker-agent/pkg/backoff"
	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/httpclient"
	"github.com/docker/docker-agent/pkg/js"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/useragent"
)

const (
	ToolNameSendWebhook = "send_webhook"
	category            = "webhook"
	maxRespRead         = 64 << 10
	defaultMaxAttempts  = 4
	defaultDedupeWindow = 30 * time.Second
	defaultMinInterval  = time.Second
	maxRecentDeliveries = 1024
)

type httpDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

var supportedProviders = []string{
	"slack", "discord", "ifttt", "telegram",
	"mattermost", "rocketchat", "googlechat", "teams", "generic",
}

func textPayload(msg, _, _, _ string) map[string]string {
	return map[string]string{"text": msg}
}

var providerPayloads = map[string]func(msg, value2, value3, chatID string) map[string]string{
	"generic":    textPayload,
	"slack":      textPayload,
	"mattermost": textPayload,
	"rocketchat": textPayload,
	"googlechat": textPayload,
	"teams":      textPayload,
	"discord": func(msg, _, _, _ string) map[string]string {
		return map[string]string{"content": msg}
	},
	"ifttt": func(msg, value2, value3, _ string) map[string]string {
		p := map[string]string{"value1": msg}
		if value2 != "" {
			p["value2"] = value2
		}
		if value3 != "" {
			p["value3"] = value3
		}
		return p
	},
	"telegram": func(msg, _, _, chatID string) map[string]string {
		return map[string]string{"chat_id": chatID, "text": msg}
	},
}

func normalizeProvider(p string) string {
	switch p = strings.ToLower(strings.TrimSpace(p)); p {
	case "":
		return "generic"
	case "google_chat", "gchat":
		return "googlechat"
	case "msteams", "microsoftteams", "microsoft_teams":
		return "teams"
	case "rocket.chat", "rocket_chat":
		return "rocketchat"
	default:
		return p
	}
}

func buildPayload(provider, message, value2, value3, chatID string) ([]byte, error) {
	build, ok := providerPayloads[provider]
	if !ok {
		return nil, fmt.Errorf("unknown provider %q (supported: %s)", provider, strings.Join(supportedProviders, ", "))
	}
	return json.Marshal(build(message, value2, value3, chatID))
}

type verdict int

const (
	delivered verdict = iota
	transient
	permanent
)

type deliveryState struct {
	at      time.Time
	pending bool
}

type ToolSet struct {
	cfg      latest.WebhookToolConfig
	expander *js.Expander
	client   httpDoer

	maxAttempts  int
	dedupeWindow time.Duration
	minInterval  time.Duration

	now   func() time.Time
	sleep func(ctx context.Context, d time.Duration) bool

	mu       sync.Mutex
	recent   map[string]deliveryState
	lastSent time.Time

	// Delivery lifetime is owned by the team toolset; each job retains its invoking runtime.
	lifecycle sync.Mutex
	cancels   map[string]context.CancelFunc
	stopped   bool
	wg        sync.WaitGroup
}

var (
	_ tools.ToolSet      = (*ToolSet)(nil)
	_ tools.Startable    = (*ToolSet)(nil)
	_ tools.Instructable = (*ToolSet)(nil)
)

func New(cfg latest.WebhookToolConfig, expander *js.Expander, timeout time.Duration) *ToolSet {
	return &ToolSet{
		cfg:          cfg,
		expander:     expander,
		client:       httpclient.NewSafeClient(timeout, false),
		maxAttempts:  defaultMaxAttempts,
		dedupeWindow: defaultDedupeWindow,
		minInterval:  defaultMinInterval,
		now:          time.Now,
		sleep:        backoff.SleepWithContext,
		recent:       make(map[string]deliveryState),
		cancels:      make(map[string]context.CancelFunc),
	}
}

func CreateToolSet(toolset latest.Toolset, runConfig *config.RuntimeConfig) (tools.ToolSet, error) {
	cfg := toolset.WebhookConfig
	if strings.TrimSpace(cfg.URL) == "" {
		return nil, errors.New("webhook tool requires a url in webhook_config")
	}
	if _, ok := providerPayloads[normalizeProvider(cfg.Provider)]; !ok {
		return nil, fmt.Errorf("webhook tool: unknown provider %q (supported: %s)",
			cfg.Provider, strings.Join(supportedProviders, ", "))
	}
	if normalizeProvider(cfg.Provider) == "telegram" && strings.TrimSpace(cfg.ChatID) == "" {
		return nil, errors.New("webhook tool: provider telegram requires chat_id in webhook_config")
	}

	timeout := httpclient.DefaultToolHTTPTimeout
	if toolset.Timeout > 0 {
		timeout = time.Duration(toolset.Timeout) * time.Second
	}
	return New(cfg, js.NewJsExpander(runConfig.EnvProvider()), timeout), nil
}

type SendArgs struct {
	Message string `json:"message" jsonschema:"The message text to deliver"`
	Value2  string `json:"value2,omitempty" jsonschema:"IFTTT value2 field (provider=ifttt only)"`
	Value3  string `json:"value3,omitempty" jsonschema:"IFTTT value3 field (provider=ifttt only)"`
}

func (t *ToolSet) send(ctx context.Context, args SendArgs, rt tools.Runtime) (*tools.ToolCallResult, error) {
	if strings.TrimSpace(args.Message) == "" {
		return tools.ResultError("Error: message is required."), nil
	}

	async := rt != nil && rt.Supports(tools.CapabilityRecall)
	jobCtx, finish, rejected := t.admit(ctx, args, async)
	if rejected != nil {
		return rejected, nil
	}
	if async {
		go func() {
			msg, failed := t.deliver(jobCtx, args)
			defer finish(!failed)
			if failed {
				t.recall(jobCtx, msg, rt)
			}
		}()
		return tools.ResultSuccess(fmt.Sprintf(
			"Queued delivery to the %s webhook. You will only be notified if it ultimately fails.",
			normalizeProvider(t.cfg.Provider),
		)), nil
	}

	msg, failed := t.deliver(jobCtx, args)
	finish(!failed)
	if failed {
		return tools.ResultError(msg), nil
	}
	return tools.ResultSuccess(msg), nil
}

func (t *ToolSet) deliver(ctx context.Context, args SendArgs) (string, bool) {
	provider := normalizeProvider(t.cfg.Provider)
	var (
		lastErr    string
		retryAfter time.Duration
	)

	for attempt := range t.maxAttempts {
		if attempt > 0 {
			if !t.sleep(ctx, t.retryDelay(attempt, retryAfter)) {
				return fmt.Sprintf("Delivery to the %s webhook was cancelled after %d attempt(s): %s",
					provider, attempt, lastErr), true
			}
		}

		v, ra, detail := t.attempt(ctx, args)
		retryAfter = ra
		switch v {
		case delivered:
			return fmt.Sprintf("Delivered to the %s webhook (attempt %d/%d).",
				provider, attempt+1, t.maxAttempts), false
		case permanent:
			return fmt.Sprintf("Delivery to the %s webhook failed permanently: %s", provider, detail), true
		case transient:
			lastErr = detail
		}
	}
	return fmt.Sprintf("Delivery to the %s webhook failed after %d attempt(s): %s",
		provider, t.maxAttempts, lastErr), true
}

func (t *ToolSet) retryDelay(attempt int, retryAfter time.Duration) time.Duration {
	if retryAfter > 0 {
		if retryAfter > backoff.MaxRetryAfterWait {
			return backoff.MaxRetryAfterWait
		}
		return retryAfter
	}
	return backoff.Calculate(attempt)
}

func (t *ToolSet) attempt(ctx context.Context, args SendArgs) (verdict, time.Duration, string) {
	endpoint := t.expander.Expand(ctx, t.cfg.URL, nil)
	if u, err := url.Parse(endpoint); err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return permanent, 0, "configured url is not a valid http(s) URL"
	}

	body, err := buildPayload(normalizeProvider(t.cfg.Provider), args.Message, args.Value2, args.Value3, t.cfg.ChatID)
	if err != nil {
		return permanent, 0, err.Error()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		// http.NewRequestWithContext may return a *url.Error; its Error() string embeds
		// the full request URL which may contain secrets.
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			return permanent, 0, urlErr.Err.Error()
		}
		return permanent, 0, err.Error()
	}
	req.Header.Set("Content-Type", "application/json")
	useragent.SetIdentity(req)
	for k, v := range t.expander.ExpandMap(ctx, t.cfg.Headers) {
		req.Header.Set(k, v)
	}

	resp, err := t.client.Do(req)
	if err != nil {
		// http.Client.Do returns *url.Error on network failures; its Error() string
		// embeds the full request URL which may carry embedded secrets (e.g. Slack/Discord tokens).
		// Unwrap to expose only the underlying cause and never the URL.
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			return transient, 0, "request failed: " + urlErr.Err.Error()
		}
		return transient, 0, "request failed: " + err.Error()
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxRespRead))

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return delivered, 0, ""
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
		return transient, parseRetryAfter(resp.Header.Get("Retry-After")),
			fmt.Sprintf("HTTP %d: %s", resp.StatusCode, snippet(respBody))
	default:
		return permanent, 0, fmt.Sprintf("HTTP %d: %s", resp.StatusCode, snippet(respBody))
	}
}

func parseRetryAfter(v string) time.Duration {
	secs, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || secs <= 0 {
		return 0
	}
	return time.Duration(secs) * time.Second
}

func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	if s == "" {
		return "(no body)"
	}
	return s
}

func dedupeKey(args SendArgs) string {
	sum := sha256.Sum256([]byte(args.Message + "\x00" + args.Value2 + "\x00" + args.Value3))
	return hex.EncodeToString(sum[:8])
}

// Admission consumes the rate budget even if delivery fails; only successful
// delivery consumes the dedupe window. Pending duplicates never start a second job.
func (t *ToolSet) admit(callCtx context.Context, args SendArgs, async bool) (context.Context, func(bool), *tools.ToolCallResult) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.stopped {
		return nil, nil, tools.ResultError("Error: webhook toolset is stopped.")
	}
	now := t.now()
	for key, state := range t.recent {
		if !state.pending && now.Sub(state.at) > t.dedupeWindow {
			delete(t.recent, key)
		}
	}
	key := dedupeKey(args)
	if _, ok := t.recent[key]; ok {
		return nil, nil, tools.ResultSuccess("Suppressed: an identical message is pending or was already delivered within the last " + t.dedupeWindow.String() + ".")
	}
	if !t.lastSent.IsZero() && now.Sub(t.lastSent) < t.minInterval {
		wait := t.minInterval - now.Sub(t.lastSent)
		return nil, nil, tools.ResultError(fmt.Sprintf("Error: rate limited; wait %s before sending another notification.", wait.Round(time.Millisecond)))
	}
	if len(t.recent) >= maxRecentDeliveries {
		return nil, nil, tools.ResultError("Error: webhook delivery capacity reached; try again later.")
	}
	base := callCtx
	if async {
		base = context.WithoutCancel(callCtx)
	}
	ctx, cancel := context.WithCancel(base)
	t.recent[key] = deliveryState{pending: true}
	t.lastSent = now
	t.cancels[key] = cancel
	t.wg.Add(1)
	return ctx, func(success bool) {
		cancel()
		t.mu.Lock()
		delete(t.cancels, key)
		if success {
			t.recent[key] = deliveryState{at: t.now()}
		} else {
			delete(t.recent, key)
		}
		t.mu.Unlock()
		t.wg.Done()
	}, nil
}

func (t *ToolSet) recall(ctx context.Context, message string, rt tools.Runtime) {
	if err := rt.Recall(ctx, "⚠️ "+message); err != nil {
		slog.WarnContext(ctx, "Failed to enqueue webhook delivery-failure recall", "error", err)
	}
}

func (t *ToolSet) Start(context.Context) error {
	t.lifecycle.Lock()
	defer t.lifecycle.Unlock()
	t.mu.Lock()
	defer t.mu.Unlock()
	t.stopped = false
	return nil
}

func (t *ToolSet) Stop(context.Context) error {
	t.lifecycle.Lock()
	defer t.lifecycle.Unlock()
	t.mu.Lock()
	t.stopped = true
	for _, cancel := range t.cancels {
		cancel()
	}
	t.mu.Unlock()
	t.wg.Wait()
	return nil
}

func (t *ToolSet) Tools(context.Context) ([]tools.Tool, error) {
	return []tools.Tool{
		{
			Name:     ToolNameSendWebhook,
			Category: category,
			Description: "Send a notification to the configured webhook (Slack, Discord, IFTTT, Telegram, Mattermost, " +
				"Rocket.Chat, Google Chat, Teams, or a generic endpoint). The destination is set in the agent's " +
				"configuration, so you only supply the message. Delivery is retried automatically; you are notified " +
				"only if it ultimately fails.",
			Parameters:              tools.MustSchemaFor[SendArgs](),
			OutputSchema:            tools.MustSchemaFor[string](),
			Handler:                 tools.NewRuntimeHandler(t.send),
			Annotations:             tools.ToolAnnotations{Title: "Send Webhook"},
			AddDescriptionParameter: true,
		},
	}, nil
}

func (t *ToolSet) Instructions() string {
	return `## Webhook Tool

send_webhook(message) delivers a notification to the destination configured for
this agent. You do not choose the destination or its credentials.

- Delivery is reliable: transient failures (rate limits, 5xx, network errors) are
  retried with backoff, honouring the server's Retry-After.
- It is fire-and-forget: the call returns immediately and you are messaged only
  if delivery ultimately fails.
- Identical messages sent again within a short window are suppressed, and
  notifications are rate limited, so avoid resending on your own.`
}
