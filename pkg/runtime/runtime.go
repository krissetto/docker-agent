package runtime

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.opentelemetry.io/otel/trace"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/config/types"
	"github.com/docker/docker-agent/pkg/harness"
	"github.com/docker/docker-agent/pkg/hooks"
	"github.com/docker/docker-agent/pkg/hooks/builtins"
	"github.com/docker/docker-agent/pkg/httpclient"
	"github.com/docker/docker-agent/pkg/model/provider"
	"github.com/docker/docker-agent/pkg/model/provider/dmr/dmrmodels"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/sessiontitle"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/telemetry/genai"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/builtin/skills"
	todotool "github.com/docker/docker-agent/pkg/tools/builtin/todo"
	"github.com/docker/docker-agent/pkg/tools/lifecycle"
)

// ToolHandlerFunc handles a runtime-managed tool call. rt is the dispatcher's
// per-call handle, used by handlers that need to talk back to the in-flight
// call (streaming output, asking the user to approve an action).
type ToolHandlerFunc func(ctx context.Context, sess *session.Session, toolCall tools.ToolCall, events EventSink, rt tools.Runtime) (*tools.ToolCallResult, error)

// PermissionsInfo contains the allow, ask, and deny patterns for tool permissions.
type PermissionsInfo struct {
	Allow []string
	Ask   []string
	Deny  []string
}

type CurrentAgentInfo struct {
	Name        string
	Description string
	Commands    types.Commands
}

type ModelStore interface {
	GetModel(ctx context.Context, id modelsdev.ID) (*modelsdev.Model, error)
	GetDatabase(ctx context.Context) (*modelsdev.Database, error)
}

const maxStartupToolSubscribers = 64
const maxStartupToolEvents = 128

type startupToolSeed struct {
	events      []Event
	subscribers map[chan Event]struct{}
}

// LocalRuntime manages the execution of agents
type LocalRuntime struct {
	sessionService   *SessionService
	harnessFactory   *harness.Factory
	commandEvaluator *CommandEvaluatorFactory

	ctx                       func() context.Context
	lifecycleCancel           context.CancelFunc
	shutdownMu                sync.Mutex
	shutdownGate              chan struct{}
	shutdownFence             sync.Once
	drainMu                   sync.Mutex
	drainCtx                  context.Context //nolint:containedctx // supervisor shutdown caller owns bounded durability drain
	lifecycleCtx              context.Context //nolint:containedctx // lifecycle root intentionally owned and cancelled by this long-lived component
	toolMap                   map[string]ToolHandlerFunc
	todoToolsets              map[*todotool.ToolSet]*todotool.ToolSet
	toolDeferrals             tools.DeferralTracker
	team                      *team.Team
	agents                    *agentRouter
	tracer                    trace.Tracer
	modelsStore               ModelStore
	sessionCompaction         bool
	managedOAuth              bool
	unmanagedOAuthRedirectURI string
	nonInteractive            bool
	startupToolsMu            sync.Mutex
	startupTools              map[string]*startupToolSeed
	startupToolsCtx           context.Context //nolint:containedctx // lifecycle root intentionally owned and cancelled by this long-lived component
	startupToolsCancel        context.CancelFunc
	startupToolsWG            *driverWorkGroup
	startupToolsClosed        bool

	// elicitationSinkMu guards the embedder-wide fallback sink and the
	// session-scoped App subscribers. Requests prefer their exact session,
	// then the nearest subscribed ancestor, before falling back to the
	// embedder sink.
	elicitationSinkMu          sync.RWMutex
	onElicitationRequest       func(Event)
	elicitationSessionSinks    map[string]*sessionElicitationSink
	nextElicitationSessionSink uint64
	sessionStore               session.Store
	workingDir                 string   // Working directory for hooks execution
	env                        []string // Environment variables for hooks execution
	modelSwitcherCfg           *ModelSwitcherConfig
	providerRegistry           *provider.Registry
	gatewayModels              gatewayModelsCache
	dmrModels                  dmrModelsCache
	// Workspace roots and manifests for generated-media resolution.
	generatedFiles generatedFileCache

	// hooksRegistry is the runtime-private hooks.Registry used to build
	// every Executor. It carries the runtime-owned builtin hooks
	// (add_date, add_environment_info) registered once during
	// NewLocalRuntime, so they're available to every agent without
	// touching any process-wide state.
	hooksRegistry *hooks.Registry

	// autoInjectors run on every per-agent hook config during
	// [buildHooksExecutors] so embedders can plug in builtins (today
	// snapshot via [builtins.SnapshotController]) without the runtime
	// hard-coding their wiring. Set via [WithAutoInjector].
	autoInjectors []builtins.AutoInjector

	// hooksExecByAgent holds the per-agent [hooks.Executor], keyed by
	// agent name. Built once in [NewLocalRuntime.buildHooksExecutors]
	// after team and runtime config are finalized; agents with no hooks
	// have no entry, so [hooksExec] returns nil for them. Read-only after
	// construction, so no locking is needed.
	hooksExecByAgent map[string]*hooks.Executor

	// transforms is the runtime's [MessageTransform] chain, applied to
	// every LLM call in registration order. Populated by
	// [NewLocalRuntime] (for the runtime-shipped strip transform) and by
	// [WithMessageTransform] (for embedder-supplied transforms).
	// Read-only after construction.
	transforms []registeredTransform

	fallback *fallbackExecutor

	// observers receive every event the runtime produces, in
	// registration order. Built up via [WithEventObserver] during
	// construction; read-only afterwards. Always contains at least one
	// entry: the auto-registered [PersistenceObserver] for the
	// configured session store. See [EventObserver] for the contract.
	observers []EventObserver

	// fallback owns the model-fallback chain (primary + configured
	// fallbacks), per-attempt retry/backoff for transient errors, and
	// the per-agent "sticky" cooldown after a fallback succeeds. It
	// holds the cooldownManager and rate-limit retry flag so that state
	// stays out of LocalRuntime. See [fallbackExecutor].

	// budgetCfg is the run-wide budget (nil when unset), budgetsCfg the
	// named budget definitions, and agentBudgets the names each agent
	// declared. All three are immutable templates; budget below is the
	// live accumulator built from them.
	budgetCfg    *latest.BudgetConfig
	budgetsCfg   map[string]latest.BudgetConfig
	agentBudgets map[string][]string

	// rootBudgets keeps independent wallets for canonical root session trees.
	budgetMu    sync.Mutex
	rootBudgets map[string]*budgetSet

	// onToolsChanged is called when an MCP toolset reports a tool list
	// change. Protected by toolsChangedMu because MCP change-notification
	// goroutines call emitToolsChanged concurrently.
	toolsChangedMu       sync.RWMutex
	onToolsChanged       func(Event)
	toolsChangedReleases []func()

	subagents *subagentManager

	// Admission is serialized with policy changes, not with child execution.
	subagentAdmissionMu sync.RWMutex
	subagentsDisabled   atomic.Bool // inverted so the zero value is enabled

	policy                   SessionResourcePolicy
	maxExecutions            int
	maxTools                 int
	executionAdmissionMu     sync.Mutex
	executionAdmission       *executionAdmission
	toolPermits              chan struct{}
	maxActiveDescendants     int
	maxActiveDescendantsRoot int
	maxSubagentDepth         int
	maxPendingMailbox        int
	maxSessions              int
	maxReplayEvents          int
	maxReplayBytes           int
	idleRetention            time.Duration

	// sessionDrivers owns per-session run/delivery/wake/attach state for
	// session-managed local sessions.
	sessionDrivers *sessionDriverRegistry

	// sessionEvents broadcasts each session's run events to attached viewers
	// (e.g. a TUI tab following a subagent's sub-session live).
	sessionEvents *sessionEventHub

	// subagentStore persists subagent swarm snapshots in their own
	// table/backend. Defaults to the session store when it implements
	// [subagent.Store] (the built-in SQLite store does), else in-memory.
	// Embedders override it with [WithSubagentStore].
	subagentStore subagent.Store

	// dmrModelLister lists the models pulled locally in Docker Model Runner,
	// used to populate DMR entries in the model picker. Defaults to
	// dmrmodels.ListModels in NewLocalRuntime; left nil by runtimes built directly
	// (e.g. tests) so DMR discovery stays opt-in. Tests inject a stub here.
	dmrModelLister func(ctx context.Context) ([]string, error)

	// now is the runtime's clock. Defaults to time.Now and can be replaced
	// in tests via WithClock to make timestamps and cooldown windows
	// deterministic. Every time-dependent call inside the runtime (message
	// CreatedAt, fallback cooldown windows, tool-call latency) goes through
	// this hook so a single fake clock controls them all.
	now func() time.Time

	// telemetry receives the runtime's observability events (session
	// start/end, tool calls, token usage, errors). Defaults to
	// defaultTelemetry which forwards to pkg/telemetry. Tests can inject
	// a recorder via WithTelemetry to assert the lifecycle without
	// standing up an OTel pipeline.
	telemetry Telemetry

	// maxOverflowCompactions caps the number of consecutive context-
	// overflow auto-compactions the run loop attempts before surfacing the
	// error. Defaults to defaultMaxOverflowCompactions; tests use
	// WithMaxOverflowCompactions to exercise both the "compaction
	// succeeded" and "compaction exhausted" branches.
	maxOverflowCompactions int

	// toolListTimeout bounds how long EmitStartupInfo waits for a single
	// toolset to enumerate its tools before skipping it, so one hung toolset
	// cannot stall the sidebar's "Loading tools…" state forever. Defaults to
	// defaultToolListTimeout; overridden via WithToolListTimeout.
	toolListTimeout time.Duration

	// toolStartTimeout bounds how long EmitStartupInfo waits for a single
	// toolset to start before skipping it for the startup sidebar pass.
	// Defaults to defaultToolStartTimeout; overridden via WithToolStartTimeout.
	toolStartTimeout time.Duration

	// streamStoppedDeliveryTimeout bounds how long finalizeEventChannel
	// blocks trying to deliver StreamStopped when the event buffer is full.
	// Defaults to defaultStreamStoppedDeliveryTimeout; tests shrink it to
	// exercise the drop-when-abandoned path without a real-time wait. Zero
	// falls back to the default (see streamStoppedTimeout), so runtimes built
	// directly via a struct literal in tests still get bounded delivery.
	streamStoppedDeliveryTimeout time.Duration
}

type Opt func(*LocalRuntime)

func WithCurrentAgent(agentName string) Opt {
	return func(r *LocalRuntime) {
		r.agents = newAgentRouter(r.team, agentName)
	}
}

func WithManagedOAuth(managed bool) Opt {
	return func(r *LocalRuntime) {
		r.managedOAuth = managed
	}
}

// WithUnmanagedOAuthRedirectURI configures the redirect_uri the runtime
// advertises when running MCP server OAuth flows in unmanaged mode (i.e.
// when WithManagedOAuth(false) is set). When set, docker-agent generates
// state + PKCE + DCR in-process and emits an elicitation carrying the
// `authorize_url` + `state`; the client returns `{code, state}` via
// ResumeElicitation and docker-agent does the token exchange itself.
// When empty, the runtime falls back to the legacy unmanaged contract
// where the client performs the OAuth flow and returns an access token.
func WithUnmanagedOAuthRedirectURI(uri string) Opt {
	return func(r *LocalRuntime) {
		r.unmanagedOAuthRedirectURI = uri
	}
}

// WithNonInteractive marks the runtime as headless (e.g., MCP serve mode).
// When set, blocking operations like elicitation requests are automatically
// declined instead of waiting for user interaction that will never come.
//
// Note: this complements session.WithNonInteractive, which controls per-session
// loop behavior (e.g., auto-stop on max iterations). Both should be set for
// fully headless operation - the runtime flag prevents elicitation hangs, while
// the session flag adjusts iteration behavior. In MCP serve mode, both are set
// by the server code in pkg/mcp/server.go.
func WithNonInteractive(nonInteractive bool) Opt {
	return func(r *LocalRuntime) {
		r.nonInteractive = nonInteractive
	}
}

// WithTracer sets a custom OpenTelemetry tracer; if not provided, tracing is disabled (no-op).
func WithTracer(t trace.Tracer) Opt {
	return func(r *LocalRuntime) {
		r.tracer = t
	}
}

// WithSteerQueue is retained for source compatibility and does nothing.
//
// Deprecated: Custom queues are unsupported. Use SessionHandle.Steer for session-owned delivery.
func WithSteerQueue(q MessageQueue) Opt {
	return func(r *LocalRuntime) {
		_ = q
	}
}

// WithFollowUpQueue is retained for source compatibility and does nothing.
//
// Deprecated: Custom queues are unsupported. Use SessionHandle.Submit for session-owned queued delivery.
func WithFollowUpQueue(q MessageQueue) Opt {
	return func(r *LocalRuntime) {
		_ = q
	}
}

func WithSessionCompaction(sessionCompaction bool) Opt {
	return func(r *LocalRuntime) {
		r.sessionCompaction = sessionCompaction
	}
}

func WithProviderRegistry(registry *provider.Registry) Opt {
	return func(r *LocalRuntime) {
		if registry != nil {
			r.providerRegistry = registry
		}
	}
}

// UnlimitedSessionResources explicitly disables a supported resource bound.
// It is currently supported for sessions and pending mailboxes.
const UnlimitedSessionResources = -1

// SessionResourcePolicy bounds session/topology resources. Zero means the resource
// is disabled (no admission/retention), never "use an implicit default".
type SessionResourcePolicy struct {
	// Positive execution/tool limits wait for capacity; zero disables admission.
	MaxExecutions        int
	MaxTools             int
	MaxSessions          int
	MaxActiveDescendants int
	MaxActivePerRoot     int
	MaxDepth             int
	MailboxMessages      int
	ReplayEvents         int
	ReplayBytes          int
	IdleRetention        time.Duration
}

func DefaultSessionResourcePolicy() SessionResourcePolicy {
	return SessionResourcePolicy{
		MaxExecutions: 32, MaxTools: 32,
		MaxSessions: 1024, MaxActiveDescendants: defaultMaxActiveDescendants,
		MaxActivePerRoot: defaultMaxActiveDescendantsRoot, MaxDepth: defaultMaxSubagentDepth,
		MailboxMessages: defaultMaxSubagentMailbox,
		ReplayEvents:    defaultSessionEventReplayCapacity, ReplayBytes: 8 << 20,
		IdleRetention: 5 * time.Minute,
	}
}

// WithSessionResourcePolicy injects the complete resource policy atomically.
func WithSessionResourcePolicy(policy SessionResourcePolicy) Opt {
	return func(r *LocalRuntime) {
		r.policy = policy
	}
}

// applyResourcePolicy derives the per-limit fields from r.policy. It runs once
// in NewLocalRuntime after all options compose; hand-built test runtimes must
// call it too, so a zero policy can never masquerade as "default".
func (r *LocalRuntime) applyResourcePolicy() {
	r.maxExecutions = r.policy.MaxExecutions
	r.maxTools = r.policy.MaxTools
	r.executionAdmission = newExecutionAdmission(r.maxExecutions)
	if r.maxTools > 0 {
		r.toolPermits = make(chan struct{}, r.maxTools)
	}
	r.maxSessions = r.policy.MaxSessions
	r.maxActiveDescendants = r.policy.MaxActiveDescendants
	r.maxActiveDescendantsRoot = r.policy.MaxActivePerRoot
	r.maxSubagentDepth = r.policy.MaxDepth
	r.maxPendingMailbox = r.policy.MailboxMessages
	r.maxReplayEvents = r.policy.ReplayEvents
	r.maxReplayBytes = r.policy.ReplayBytes
	r.idleRetention = r.policy.IdleRetention
}

func WithModelStore(store ModelStore) Opt {
	return func(r *LocalRuntime) {
		r.modelsStore = store
	}
}

func WithSessionStore(store session.Store) Opt {
	return func(r *LocalRuntime) {
		r.sessionStore = store
	}
}

// WithSubagentStore sets the backend for subagent swarm snapshots. Without it
// the runtime uses the session store when it implements [subagent.Store] (the
// built-in SQLite store does, with a dedicated table), or an in-memory store.
func WithSubagentStore(store subagent.Store) Opt {
	return func(r *LocalRuntime) {
		r.subagentStore = store
	}
}

func WithMaxActiveDescendants(n int) Opt {
	return func(r *LocalRuntime) {
		if n > 0 {
			r.policy.MaxActiveDescendants = n
		}
	}
}

func WithMaxActiveDescendantsPerRoot(n int) Opt {
	return func(r *LocalRuntime) {
		if n > 0 {
			r.policy.MaxActivePerRoot = n
		}
	}
}

func WithMaxSubagentDepth(n int) Opt {
	return func(r *LocalRuntime) {
		if n > 0 {
			r.policy.MaxDepth = n
		}
	}
}

func WithMaxSubagentMailbox(n int) Opt {
	return func(r *LocalRuntime) {
		if n > 0 || n == UnlimitedSessionResources {
			r.policy.MailboxMessages = n
		}
	}
}

// WithWorkingDir sets the working directory for hooks execution
func WithWorkingDir(dir string) Opt {
	return func(r *LocalRuntime) {
		r.workingDir = dir
	}
}

// WithEnv sets the environment variables for hooks execution
func WithEnv(env []string) Opt {
	return func(r *LocalRuntime) {
		r.env = env
	}
}

// WithClock replaces the runtime's clock. Defaults to time.Now. Tests that
// need deterministic timestamps (assistant message CreatedAt, fallback
// cooldown windows, tool-call latency) can pass a fake clock so assertions
// don't depend on wall-clock advancement.
func WithClock(now func() time.Time) Opt {
	return func(r *LocalRuntime) {
		if now != nil {
			r.now = now
		}
	}
}

// WithTelemetry replaces the runtime's Telemetry sink. Defaults to a
// pass-through to the package-level pkg/telemetry helpers. Tests pass a
// recorder to assert that the runtime emitted the expected lifecycle
// events without setting up an OTel client.
func WithTelemetry(t Telemetry) Opt {
	return func(r *LocalRuntime) {
		if t != nil {
			r.telemetry = t
		}
	}
}

// WithMaxOverflowCompactions overrides how many consecutive context-overflow
// auto-compactions the run loop is allowed to attempt before surfacing the
// error. Defaults to defaultMaxOverflowCompactions (1).
//
// Tests use this to exercise both branches of the overflow-recovery code
// path: pass 0 to verify the failure surface immediately; pass a higher
// number to verify the loop bounds compaction attempts. Negative values
// are clamped to 0.
func WithMaxOverflowCompactions(n int) Opt {
	return func(r *LocalRuntime) {
		if n < 0 {
			n = 0
		}
		r.maxOverflowCompactions = n
	}
}

// WithBudget sets the run-wide budget: the cost, token and wall-clock
// ceilings that stop a run once crossed, regardless of which agent spends.
// A nil or all-zero config leaves runs unbudgeted, which is the default.
func WithBudget(cfg *latest.BudgetConfig) Opt {
	return func(r *LocalRuntime) {
		if cfg.IsZero() {
			return
		}
		clone := *cfg
		r.budgetCfg = &clone
	}
}

// WithNamedBudgets registers the manifest's named budget definitions and
// the budget names each agent declared. A named budget referenced by
// several agents is one shared pot, not a copy per agent.
func WithNamedBudgets(defs map[string]latest.BudgetConfig, agentBudgets map[string][]string) Opt {
	return func(r *LocalRuntime) {
		if len(defs) == 0 || len(agentBudgets) == 0 {
			return
		}
		r.budgetsCfg = maps.Clone(defs)
		r.agentBudgets = make(map[string][]string, len(agentBudgets))
		for name, budgets := range agentBudgets {
			r.agentBudgets[name] = slices.Clone(budgets)
		}
	}
}

// WithToolListTimeout overrides how long EmitStartupInfo waits for a single
// toolset to enumerate its tools before skipping it. Defaults to
// defaultToolListTimeout. A non-positive value is ignored so the default
// stands. Tests pass a short timeout to exercise the skip path (a toolset
// whose Tools() blocks) without a real-time wait.
func WithToolListTimeout(d time.Duration) Opt {
	return func(r *LocalRuntime) {
		if d > 0 {
			r.toolListTimeout = d
		}
	}
}

// WithToolStartTimeout overrides how long EmitStartupInfo waits for a single
// toolset to start before skipping it. Defaults to defaultToolStartTimeout.
// A non-positive value is ignored so the default stands. Tests pass a short
// timeout to exercise the skip path (a toolset whose Start() blocks) without
// a real-time wait.
func WithToolStartTimeout(d time.Duration) Opt {
	return func(r *LocalRuntime) {
		if d > 0 {
			r.toolStartTimeout = d
		}
	}
}

// WithRetryOnRateLimit enables automatic retry with backoff for HTTP 429 (rate limit)
// errors when no fallback models are available. When enabled, the runtime will honor
// the Retry-After header from the provider's response to determine wait time before
// retrying, falling back to exponential backoff if the header is absent.
//
// This is off by default. It is intended for library consumers that run agents
// programmatically and prefer to wait for rate limits to clear rather than fail
// immediately.
//
// When fallback models are configured, 429 errors always skip to the next model
// regardless of this setting.
func WithRetryOnRateLimit() Opt {
	return func(r *LocalRuntime) {
		r.fallback.retryOnRateLimit = true
	}
}

// WithAutoInjector adds an [builtins.AutoInjector] that augments every
// per-agent hook configuration during executor build. The canonical
// use case is the snapshot controller returned by
// [builtins.RegisterSnapshot]: pass the same controller to the App via
// app.WithSnapshotController so /undo and friends drive the same
// instance that captures the checkpoints.
//
// Multiple calls accumulate; injectors run in registration order.
func WithAutoInjector(inj builtins.AutoInjector) Opt {
	return func(r *LocalRuntime) {
		if inj != nil {
			r.autoInjectors = append(r.autoInjectors, inj)
		}
	}
}

// WithHooksRegistry snapshots embedder registrations into a private runtime
// registry. Later caller changes cannot alter this runtime. Stock builtin names
// retain caller overrides; cache_response is reserved for the runtime, and the
// model hook factory is always bound to the runtime's provider registry.
func WithHooksRegistry(reg *hooks.Registry) Opt {
	return func(r *LocalRuntime) {
		if reg != nil {
			r.hooksRegistry = reg.Clone()
		}
	}
}

// New creates a runtime ready to drive an agent loop. It is a thin
// alias for [NewLocalRuntime] returning the [Runtime] interface, kept
// for source compatibility with callers written before persistence
// became an [EventObserver]. Persistence is auto-registered against
// the configured (or default in-memory) session store; pass
// [WithSessionStore] to override and [WithEventObserver] to layer
// additional observers (telemetry, audit, ...).
func New(ctx context.Context, agents *team.Team, opts ...Opt) (*LocalRuntime, error) {
	return NewLocalRuntime(ctx, agents, opts...)
}

func validateSessionNamespaces(agents *team.Team) error {
	for _, agentName := range agents.AgentNames() {
		a, err := agents.Agent(agentName)
		if err != nil {
			return err
		}
		seen := map[string]struct{}{subagent.ParentAlias: {}}
		reservedTools := map[string]struct{}{subagent.ToolSpawnSubagent: {}, subagent.ToolSendMessage: {}, subagent.ToolReadSubagent: {}, subagent.ToolStopSubagent: {}}
		for _, ref := range a.AsyncSubagents() {
			alias := ref.Name
			if alias == "" {
				alias = ref.Agent
			}
			if _, reserved := reservedTools[alias]; reserved {
				return fmt.Errorf("agent %q subagent alias %q collides with session tool namespace", agentName, alias)
			}
			if _, exists := seen[alias]; exists {
				return fmt.Errorf("agent %q has duplicate or reserved subagent alias %q", agentName, alias)
			}
			seen[alias] = struct{}{}
			if _, err := agents.Agent(ref.Agent); err != nil {
				return fmt.Errorf("agent %q subagent alias %q: %w", agentName, alias, err)
			}
		}
	}
	return nil
}

// NewLocalRuntime creates a new LocalRuntime without the persistence wrapper.
// This is useful for testing or when persistence is handled externally.
func NewLocalRuntime(ctx context.Context, agents *team.Team, opts ...Opt) (*LocalRuntime, error) {
	defaultAgent, err := agents.DefaultAgent()
	if err != nil {
		return nil, err
	}
	if err := validateSessionNamespaces(agents); err != nil {
		return nil, err
	}

	lifecycleCtx, lifecycleCancel := context.WithCancel(ctx)
	r := &LocalRuntime{
		lifecycleCancel:              lifecycleCancel,
		ctx:                          func() context.Context { return context.WithoutCancel(ctx) },
		lifecycleCtx:                 lifecycleCtx,
		toolMap:                      make(map[string]ToolHandlerFunc),
		team:                         agents,
		agents:                       newAgentRouter(agents, defaultAgent.Name()),
		sessionCompaction:            true,
		managedOAuth:                 true,
		sessionStore:                 session.NewInMemorySessionStore(),
		fallback:                     newFallbackExecutor(),
		now:                          time.Now,
		telemetry:                    defaultTelemetry{},
		providerRegistry:             provider.DefaultRegistry(),
		maxOverflowCompactions:       defaultMaxOverflowCompactions,
		toolListTimeout:              defaultToolListTimeout,
		toolStartTimeout:             defaultToolStartTimeout,
		streamStoppedDeliveryTimeout: defaultStreamStoppedDeliveryTimeout,
		policy:                       DefaultSessionResourcePolicy(),
		dmrModelLister:               dmrmodels.ListModels,
	}
	r.startupToolsCtx, r.startupToolsCancel = context.WithCancel(context.WithoutCancel(ctx))
	r.startupToolsWG = newDriverWorkGroup()
	r.fallback.prepareMessages = r.prepareMessagesForModel
	r.fallback.prepareTools = r.filterSessionDelegationTools

	// stripUnsupportedModalitiesTransform captures the runtime closure to
	// resolve the agent from Input.AgentName, so it lives here rather
	// than as a stateless builtin in pkg/hooks/builtins. It drops image
	// content for text-only models on every model call.
	//
	// redact_secrets used to live here as a sibling [MessageTransform];
	// it now ships entirely as a [hooks.BuiltinFunc] in
	// pkg/hooks/builtins/redact_secrets.go and is wired into all three
	// of tool_input_transform, before_llm_call, and tool_response_transform via
	// [builtins.ApplyAgentDefaults] (or a user's hooks YAML directly),
	// so the rewrite path is the same for every leak vector and there
	// is no flag-only code path to keep in sync.
	//
	// Ordering matters: strip_generated_media MUST run before
	// strip_unsupported_modalities. The latter strips any image/audio/
	// video-kind document part the resolved model can't accept,
	// regardless of whether that part is a runtime-materialized generated
	// artifact or a user attachment — it has no placeholder logic. If it
	// ran first, a capability-less or unknown model would have a
	// media-only generated-media assistant message stripped down to
	// nothing right there, and strip_generated_media would then see no
	// generated-media part left to react to: its placeholder would never
	// fire, and the turn would silently vanish from outgoing history.
	// Running strip_generated_media first guarantees its placeholder text
	// is already in place — as ordinary text, not a media part — by the
	// time strip_unsupported_modalities runs, so there is nothing left for
	// it to strip from that message.
	r.transforms = append(r.transforms,
		// strip_generated_media has no runtime state to capture (the policy
		// is unconditional), so it registers the free function directly
		// rather than a method value like the transform below.
		registeredTransform{
			name: BuiltinStripGeneratedMedia,
			fn:   stripGeneratedMediaTransform,
		},
		registeredTransform{
			name: BuiltinStripUnsupportedModalities,
			fn:   r.stripUnsupportedModalitiesTransform,
		},
	)

	for _, opt := range opts {
		opt(r)
	}
	// Derive the per-limit fields once, after all policy options compose.
	r.applyResourcePolicy()

	if r.sessionService == nil {
		r.sessionService = NewSessionService(SessionServiceOptions{MaxSessions: UnlimitedSessionResources})
	}
	r.sessionEvents = newSessionEventHubWithLimits(r.maxReplayEvents, r.maxReplayBytes)
	r.sessionDrivers = newSessionDriverRegistry(r)
	r.subagents = newSubagentManager(r)

	// Default the subagent snapshot backend: piggyback on the session store
	// when it implements [subagent.Store] (the built-in SQLite store persists
	// snapshots in a dedicated table), else keep them in memory.
	if r.subagentStore == nil {
		if s, ok := r.sessionStore.(subagent.Store); ok {
			r.subagentStore = s
		} else {
			r.subagentStore = subagent.NewInMemoryStore()
		}
	}

	// Bind runtime-owned todo instances without mutating the team definitions.
	r.todoToolsets = make(map[*todotool.ToolSet]*todotool.ToolSet)
	store, ok := r.sessionStore.(session.TodoStore)
	if !ok {
		store = session.NewInMemorySessionStore().(session.TodoStore)
	}
	var sharedTodoStorage *todotool.SessionStorage
	for _, name := range r.team.AgentNames() {
		if a, err := r.team.Agent(name); err == nil {
			for _, toolset := range a.ToolSets() {
				if todoSet, ok := tools.As[*todotool.ToolSet](toolset); ok {
					adapter := runtimeTodoStore{store: store, changed: r.publishTodosChanged}
					if todoSet.Shared() {
						if sharedTodoStorage == nil {
							adapter.scope = r.todoRootSessionID
							sharedTodoStorage = todotool.NewSessionStorage(adapter, httpclient.SessionIDFromContext)
						}
						r.todoToolsets[todoSet] = todoSet.BindStorage(sharedTodoStorage)
					} else {
						r.todoToolsets[todoSet] = todoSet.BindStorage(todotool.NewSessionStorage(adapter, httpclient.SessionIDFromContext))
					}
				}
			}
		}
	}

	// Defaults compose beneath embedder hooks; runtime-bound names are reserved.
	defaults := hooks.NewRegistry()
	if err := builtins.Register(defaults); err != nil {
		return nil, fmt.Errorf("register builtin hooks: %w", err)
	}
	if r.hooksRegistry == nil {
		r.hooksRegistry = defaults
	} else {
		if _, exists := r.hooksRegistry.LookupBuiltin(BuiltinCacheResponse); exists {
			return nil, fmt.Errorf("builtin hook %q is reserved for the runtime", BuiltinCacheResponse)
		}
		r.hooksRegistry.RegisterDefaults(defaults)
	}
	registerModelHook(r.hooksRegistry, r.providerRegistry)

	// cache_response is registered here (not in pkg/hooks/builtins)
	// because it needs to capture the runtime to resolve the agent
	// referenced by Input.AgentName. The other builtins are stateless
	// and can stay as package-level functions registered via
	// [builtins.Register] above.
	if err := r.hooksRegistry.RegisterBuiltin(BuiltinCacheResponse, r.cacheResponseBuiltin); err != nil {
		return nil, fmt.Errorf("register %q builtin: %w", BuiltinCacheResponse, err)
	}

	// Build the cooldown manager and wire the fallback executor's
	// runtime-bound dependencies after opts so they pick up the final
	// clock and telemetry sink ([WithClock] / [WithTelemetry]).
	r.fallback.cooldowns = newCooldownManager(r.now)
	r.fallback.telemetry = r.telemetry

	// Default the runtime's working directory to the process CWD when no
	// caller supplied one. This matches the session's default and ensures
	// builtin hooks that look up files (add_prompt_files) can find them
	// without the embedder having to remember to call WithWorkingDir.
	if r.workingDir == "" {
		if cwd, err := os.Getwd(); err == nil {
			r.workingDir = cwd
		}
	}

	if r.modelsStore == nil {
		// Precedence: an explicit WithModelStore (already set above) wins; then a
		// store carried on the ModelSwitcherConfig (the team loader shares the
		// one it warmed so the first /model open skips the cold catalog parse);
		// otherwise a lazy store constructed on first use.
		if r.modelSwitcherCfg != nil && r.modelSwitcherCfg.ModelsStore != nil {
			r.modelsStore = r.modelSwitcherCfg.ModelsStore
		} else {
			r.modelsStore = &lazyModelStore{}
		}
	}

	// Validate that the current agent exists and has a model
	// (the router's current name might have been changed by WithCurrentAgent)
	defaultAgent, err = r.team.Agent(r.agents.Name())
	if err != nil {
		return nil, err
	}

	if defaultAgent.Model(ctx) == nil && !defaultAgent.HasHarness() {
		return nil, fmt.Errorf("agent %s has no valid model", defaultAgent.Name())
	}

	// Register runtime-managed tool handlers once during construction.
	// This avoids concurrent map writes when multiple goroutines call
	// RunStream on the same runtime (e.g. background agent sessions).
	r.registerDefaultTools()

	// Pre-build per-agent hook executors now that workingDir, env and
	// the team are finalized. Read-only afterwards.
	r.buildHooksExecutors()

	// Auto-register the stock persistence observer against the
	// (possibly user-supplied) session store. It runs first in the
	// observer chain so any user-supplied observers see the same view
	// of the session that future RunStream calls and store reads will.
	if obs := newPersistenceObserver(r.sessionStore); obs != nil {
		obs.owner = func(id string) *sessionDriver { d, _ := r.sessionDrivers.Lookup(id); return d }
		obs.lifetime = r.lifetime()
		r.observers = append([]EventObserver{obs}, r.observers...)
	}

	if err := r.sessionService.register(r); err != nil {
		lifecycleCancel()
		r.startupToolsCancel()
		return nil, err
	}

	slog.DebugContext(ctx, "Creating new runtime", "agent", r.agents.Name(), "available_agents", agents.Size())

	return r, nil
}

func (r *LocalRuntime) CurrentAgentName(context.Context) string {
	return r.currentAgentName()
}

// currentAgentName is the context-free internal accessor. LocalRuntime
// resolves the active agent purely from in-memory state, so its internal
// callers (event callbacks, logging) use this directly; the exported
// context-taking method exists to satisfy the Runtime interface, whose
// remote implementation needs a context for its one-time lookup.
func (r *LocalRuntime) currentAgentName() string {
	return r.agents.Name()
}

func (r *LocalRuntime) CurrentAgentInfo(context.Context) CurrentAgentInfo {
	currentAgent := r.currentAgent()

	return CurrentAgentInfo{
		Name:        currentAgent.Name(),
		Description: currentAgent.Description(),
		Commands:    currentAgent.Commands(),
	}
}

// AgentCommands returns a copy of the configured commands for agentName
// without consulting or mutating the runtime's current-agent pointer.
func (r *LocalRuntime) AgentCommands(_ context.Context, agentName string) (types.Commands, error) {
	a, err := r.team.Agent(agentName)
	if err != nil {
		return nil, err
	}
	return maps.Clone(a.Commands()), nil
}

// AgentTools returns the tools bound to agentName, starting only that agent's
// toolsets as required for command preparation.
func (r *LocalRuntime) AgentTools(ctx context.Context, agentName string) ([]tools.Tool, error) {
	a, err := r.team.Agent(agentName)
	if err != nil {
		return nil, err
	}
	return a.Tools(todotool.WithBindings(ctx, r.todoToolsets))
}

func (r *LocalRuntime) CurrentAgentCommands(context.Context) types.Commands {
	return r.currentAgent().Commands()
}

// CurrentAgentTools returns the tools available to the current agent.
// This starts the toolsets if needed and returns all available tools.
func (r *LocalRuntime) CurrentAgentTools(ctx context.Context) ([]tools.Tool, error) {
	a := r.currentAgent()
	return a.Tools(todotool.WithBindings(ctx, r.todoToolsets))
}

// ToolsetState is the coarse lifecycle bucket the agent inspector renders as a
// status glyph. It collapses the full lifecycle.State machine into the three
// distinctions a reader cares about: serving (started), not running (stopped),
// or broken (error).
type ToolsetState string

const (
	ToolsetStarted ToolsetState = "started"
	ToolsetStopped ToolsetState = "stopped"
	ToolsetError   ToolsetState = "error"
)

// ToolsetDetail describes one configured toolset for the agent inspector: its
// display name, kind, lifecycle bucket and the tools it exposes. Tools holds
// the live tool names when the toolset is started, otherwise the declared
// `tools:` allow-list from the retained config, and is empty when neither is
// available (a not-yet-started toolset with no explicit allow-list).
type ToolsetDetail struct {
	Name  string
	Kind  string
	State ToolsetState
	Tools []string
}

// AgentConfigInfo is the static-plus-live dataset behind the read-only agent
// inspector modal. The static parts (sub-agents, handoffs, fallbacks, skills,
// limits, option flags, declared toolset allow-lists) are derived from the
// resolved *agent.Agent and the retained config without starting any toolset;
// the live parts (per-toolset lifecycle state, started tool names, IsCurrent)
// reflect the running team. Remote runtimes (which hold no local team) return
// the zero value, so the modal omits every config-derived section.
type AgentConfigInfo struct {
	SubAgents []string // sub-agent names, sorted
	Handoffs  []string // handoff target names, sorted
	Fallbacks []string // fallback model ids ("provider/model"), in priority order
	Skills    []string // configured skill names (inline + included + used), sorted

	MaxIterations           int // 0 when unset
	NumHistoryItems         int // 0 when unset
	MaxConsecutiveToolCalls int // 0 when unset

	Options  []string        // enabled option flags (add-date, redact-secrets, ...)
	Toolsets []ToolsetDetail // per-toolset live state + tools, in declaration order

	IsCurrent bool // true when this is the live current agent
}

// AgentConfigInfo returns the named agent's inspector dataset. It inspects the
// resolved agent and the retained config, reading live tool names only from
// already-started toolsets (never starting one), so it is safe to call for any
// agent whether or not it has run. Unknown agents yield the zero value, so the
// modal omits the corresponding sections.
func (r *LocalRuntime) AgentConfigInfo(ctx context.Context, agentName string) AgentConfigInfo {
	a, err := r.team.Agent(agentName)
	if err != nil || a == nil {
		return AgentConfigInfo{}
	}

	cfg, hasCfg := r.team.AgentConfig(agentName)

	var subAgents []string
	for _, sub := range a.SubAgents() {
		if sub != nil {
			subAgents = append(subAgents, sub.Name())
		}
	}

	var handoffs []string
	for _, h := range a.Handoffs() {
		if h != nil {
			handoffs = append(handoffs, h.Name())
		}
	}

	var fallbacks []string
	for _, p := range a.FallbackModels() {
		if p != nil {
			fallbacks = append(fallbacks, p.ID().String())
		}
	}

	info := AgentConfigInfo{
		SubAgents:               uniqueNames(subAgents, true),
		Handoffs:                uniqueNames(handoffs, true),
		Fallbacks:               uniqueNames(fallbacks, false),
		MaxIterations:           a.MaxIterations(),
		NumHistoryItems:         a.NumHistoryItems(),
		MaxConsecutiveToolCalls: a.MaxConsecutiveToolCalls(),
		Options:                 agentOptionFlags(a, cfg, hasCfg),
		Toolsets:                toolsetDetails(ctx, a, cfg, hasCfg),
		IsCurrent:               r.agents != nil && r.agents.Name() == agentName,
	}
	if hasCfg {
		info.Skills = configSkillNames(cfg)
	}
	return info
}

// agentOptionFlags lists the agent's enabled boolean options as stable,
// hyphenated display names. AddDate/AddEnvironmentInfo/RedactSecrets read the
// agent's effective (resolved) values; CodeModeTools is only knowable from the
// retained config because the runtime folds it into a single wrapper toolset.
func agentOptionFlags(a *agent.Agent, cfg latest.AgentConfig, hasCfg bool) []string {
	var opts []string
	if a.AddDate() {
		opts = append(opts, "add-date")
	}
	if a.AddEnvironmentInfo() {
		opts = append(opts, "add-environment-info")
	}
	if a.RedactSecrets() {
		opts = append(opts, "redact-secrets")
	}
	if hasCfg && cfg.CodeModeTools {
		opts = append(opts, "code-mode-tools")
	}
	return opts
}

// configSkillNames lists the cleanly-resolvable skill names from the agent
// config: inline skill names, the Include allow-list, and referenced skill
// groups (UseSkills). Skills auto-discovered from Sources (e.g. a local
// directory) are not enumerated here because doing so would require loading
// them from disk; the inspector notes this rather than starting that work.
func configSkillNames(cfg latest.AgentConfig) []string {
	var names []string
	for _, s := range cfg.Skills.Inline {
		names = append(names, s.Name)
	}
	names = append(names, cfg.Skills.Include...)
	names = append(names, cfg.UseSkills...)
	return uniqueNames(names, true)
}

// toolsetDetails builds one ToolsetDetail per live toolset of the agent,
// combining the side-effect-free lifecycle status with tool names: live names
// for started toolsets, otherwise the declared `tools:` allow-list keyed by the
// same name the registry assigns (cmp.Or(name, type)).
func toolsetDetails(ctx context.Context, a *agent.Agent, cfg latest.AgentConfig, hasCfg bool) []ToolsetDetail {
	toolSets := a.ToolSets()
	if len(toolSets) == 0 {
		return nil
	}
	var declared map[string][]string
	if hasCfg {
		declared = declaredToolNames(cfg)
	}
	infos := make([]ToolsetDetail, 0, len(toolSets))
	for _, ts := range toolSets {
		status := toolsetStatusFor(ts)
		info := ToolsetDetail{
			Name:  status.Name,
			Kind:  status.Kind,
			State: toolsetStateBucket(status.State),
		}
		if live, ok := startedToolNames(ctx, ts); ok {
			info.Tools = live
		} else if names, ok := declared[status.Name]; ok {
			info.Tools = names
		}
		infos = append(infos, info)
	}
	return infos
}

// toolsetStateBucket collapses the lifecycle state machine into the inspector's
// three buckets. Failed is the only error state; Stopped (including a
// not-yet-started toolset) is stopped; everything else (Ready, Degraded,
// Starting, Restarting) reads as started/serving.
func toolsetStateBucket(s lifecycle.State) ToolsetState {
	switch s {
	case lifecycle.StateFailed:
		return ToolsetError
	case lifecycle.StateStopped:
		return ToolsetStopped
	default:
		return ToolsetStarted
	}
}

// startedToolNames returns the live tool names of ts, but only when it is a
// started toolset; the boolean is false for a not-yet-started toolset, or one
// whose Start is still in flight, so the caller can fall back to the declared
// allow-list without blocking on a long-running Start (e.g. RAG indexing).
// context.TODO is safe here: the toolset is already started, so listing
// returns its cached tools without a cancellable round-trip.
func startedToolNames(ctx context.Context, ts tools.ToolSet) ([]string, bool) {
	s, ok := tools.As[*tools.StartableToolSet](ts)
	if !ok {
		return nil, false
	}
	if started, _ := s.TryState(); !started {
		return nil, false
	}
	tl, err := s.Tools(ctx)
	if err != nil {
		return nil, false
	}
	names := make([]string, 0, len(tl))
	for i := range tl {
		if tl[i].Name != "" {
			names = append(names, tl[i].Name)
		}
	}
	return names, true
}

// declaredToolNames maps each configured toolset's display name to its declared
// `tools:` allow-list. The key matches the registry's naming
// (cmp.Or(name, type)) so it lines up with the live toolset's Name. Toolsets
// with no explicit allow-list (serve all tools) are omitted.
func declaredToolNames(cfg latest.AgentConfig) map[string][]string {
	m := make(map[string][]string, len(cfg.Toolsets))
	for i := range cfg.Toolsets {
		t := cfg.Toolsets[i]
		key := cmp.Or(t.Name, t.Type)
		if key == "" || len(t.Tools) == 0 {
			continue
		}
		m[key] = slices.Clone(t.Tools)
	}
	return m
}

// uniqueNames drops empty entries and de-duplicates names. By default it keeps
// the first-seen order (meaningful for declaration/priority order); when sorted
// is true it orders the result case-insensitively instead.
func uniqueNames(names []string, sorted bool) []string {
	seen := make(map[string]struct{}, len(names))
	out := make([]string, 0, len(names))
	for _, name := range names {
		if name == "" {
			continue
		}
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	if sorted {
		slices.SortFunc(out, func(a, b string) int {
			return strings.Compare(strings.ToLower(a), strings.ToLower(b))
		})
	}
	return out
}

// CurrentAgentToolsetStatuses returns one ToolsetStatus per toolset of the
// active agent. The list is in declaration order. Toolsets that wrap
// another (StartableToolSet, Multiplexer) are unwrapped so the inner
// supervisor's state is visible.
func (r *LocalRuntime) CurrentAgentToolsetStatuses() []tools.ToolsetStatus {
	a := r.currentAgent()
	if a == nil {
		return nil
	}
	toolSets := a.ToolSets()
	statuses := make([]tools.ToolsetStatus, 0, len(toolSets))
	for _, ts := range toolSets {
		statuses = append(statuses, toolsetStatusFor(ts))
	}
	return statuses
}

// AgentToolsetStatuses returns one ToolsetStatus per toolset of the named
// agent, in declaration order. It mirrors CurrentAgentToolsetStatuses but for
// an arbitrary agent and is side-effect-free: it reads each toolset's live
// lifecycle (state, kind, last error, restart count) without starting it.
// Unknown agents yield nil, so callers degrade gracefully.
func (r *LocalRuntime) AgentToolsetStatuses(name string) []tools.ToolsetStatus {
	a, err := r.team.Agent(name)
	if err != nil || a == nil {
		return nil
	}
	toolSets := a.ToolSets()
	statuses := make([]tools.ToolsetStatus, 0, len(toolSets))
	for _, ts := range toolSets {
		statuses = append(statuses, toolsetStatusFor(ts))
	}
	return statuses
}

// RestartToolset locates the named toolset on the active agent and
// asks it to restart in place. The supervisor closes the current
// session and reconnects; this method blocks until the new session
// is Ready, ctx is cancelled, or the underlying supervisor's
// timeout elapses.
//
// Returns an error when:
//   - no toolset matches name (matching uses the same logic as the
//     /tools dialog: the toolset's Name() if any, otherwise its
//     description),
//   - no matching toolset is supervisor-backed (no Restartable capability),
//   - the supervisor itself returned an error (timeout, classified
//     transport failure, etc.).
//
// Display names are not guaranteed unique (a YAML name: can shadow a
// built-in), so non-restartable matches are skipped rather than aborting:
// the first restartable toolset with the given name wins.
func (r *LocalRuntime) RestartToolset(ctx context.Context, name string) error {
	a := r.currentAgent()
	if a == nil {
		return errors.New("no active agent")
	}
	found := false
	for _, ts := range a.ToolSets() {
		if nameFor(ts, tools.DescribeToolSet(ts)) != name {
			continue
		}
		found = true
		if restartable, ok := tools.As[tools.Restartable](ts); ok {
			return restartable.Restart(ctx)
		}
	}
	if found {
		return fmt.Errorf("toolset %q does not support restart", name)
	}
	return fmt.Errorf("toolset %q not found", name)
}

// toolsetStatusFor builds a ToolsetStatus for ts. tools.As walks the
// wrapper chain so Statable/Describer can live anywhere in the stack.
func toolsetStatusFor(ts tools.ToolSet) tools.ToolsetStatus {
	status := tools.ToolsetStatus{
		Description: tools.DescribeToolSet(ts),
	}
	if kinder, ok := tools.As[tools.Kinder](ts); ok {
		status.Kind = kinder.Kind()
	}
	if statable, ok := tools.As[tools.Statable](ts); ok {
		info := statable.State()
		status.State = info.State
		status.LastError = info.LastError
		status.RestartCount = info.RestartCount
	} else {
		// Toolsets without a supervisor are considered ready by default;
		// the StartableToolSet wrapper would have surfaced an error
		// earlier if Start failed.
		status.State = lifecycleStateForUnsupervised(ts)
	}
	_, status.Restartable = tools.As[tools.Restartable](ts)
	status.Name = nameFor(ts, status.Description)
	return status
}

// lifecycleStateForUnsupervised reports the lifecycle state of a toolset with
// no Statable supervisor, using the wrapper's own non-blocking status probe
// so a toolset whose Start is legitimately still running (e.g. RAG indexing
// a large knowledge base) never stalls a status/introspection caller.
func lifecycleStateForUnsupervised(ts tools.ToolSet) lifecycle.State {
	s, ok := ts.(*tools.StartableToolSet)
	if !ok {
		return lifecycle.StateReady
	}
	started, inFlight := s.TryState()
	switch {
	case inFlight:
		return lifecycle.StateStarting
	case !started:
		return lifecycle.StateStopped
	default:
		return lifecycle.StateReady
	}
}

// nameFor picks a stable, user-visible name for a toolset. We look for
// any inner toolset that implements tools.Named (walked via tools.As so
// wrappers like StartableToolSet are transparent). The registry adds a
// WithName wrapper for every built-in toolset so this is reachable for
// almost every toolset; fallback uses the description ("mcp(stdio cmd=...)"),
// which is still better than the Go type name.
func nameFor(ts tools.ToolSet, fallback string) string {
	if name := tools.GetName(ts); name != "" {
		return name
	}
	return fallback
}

// mcpPromptToolset is the subset of the MCP toolset's API the runtime needs
// for prompt discovery and execution. It is an interface (satisfied by
// *mcp.Toolset) so pkg/runtime does not import the MCP toolset package,
// which would link its whole dependency tree into every embedder.
type mcpPromptToolset interface {
	ListPrompts(ctx context.Context) ([]tools.PromptInfo, error)
	GetPrompt(ctx context.Context, name string, arguments map[string]string) (*mcp.GetPromptResult, error)
}

// CurrentMCPPrompts returns the available MCP prompts from all active MCP toolsets
// for the current agent. It discovers prompts by calling ListPrompts on each MCP toolset
// and aggregates the results into a map keyed by prompt name.
func (r *LocalRuntime) CurrentMCPPrompts(ctx context.Context) map[string]tools.PromptInfo {
	prompts := make(map[string]tools.PromptInfo)

	// Get the current agent to access its toolsets
	currentAgent := r.currentAgent()
	if currentAgent == nil {
		slog.WarnContext(ctx, "No current agent available for MCP prompt discovery")
		return prompts
	}

	// Iterate through all toolsets of the current agent
	for _, toolset := range currentAgent.ToolSets() {
		if mcpToolset, ok := tools.As[mcpPromptToolset](toolset); ok {
			slog.DebugContext(ctx, "Found MCP toolset", "toolset", mcpToolset)
			// Discover prompts from this MCP toolset
			mcpPrompts := r.discoverMCPPrompts(ctx, mcpToolset)

			// Merge prompts into the result map
			// If there are name conflicts, the later toolset's prompt will override
			maps.Copy(prompts, mcpPrompts)
		} else {
			slog.DebugContext(ctx, "Toolset is not an MCP toolset", "type", fmt.Sprintf("%T", toolset))
		}
	}

	slog.DebugContext(ctx, "Discovered MCP prompts", "agent", currentAgent.Name(), "prompt_count", len(prompts))
	return prompts
}

// discoverMCPPrompts queries an MCP toolset for available prompts and converts them
// to PromptInfo structures. This method handles the MCP protocol communication
// and gracefully handles any errors during prompt discovery.
func (r *LocalRuntime) discoverMCPPrompts(ctx context.Context, toolset mcpPromptToolset) map[string]tools.PromptInfo {
	mcpPrompts, err := toolset.ListPrompts(ctx)
	if err != nil {
		slog.WarnContext(ctx, "Failed to list MCP prompts from toolset", "error", err)
		return nil
	}

	prompts := make(map[string]tools.PromptInfo, len(mcpPrompts))
	for _, mcpPrompt := range mcpPrompts {
		promptInfo := tools.PromptInfo{
			Name:        mcpPrompt.Name,
			Description: mcpPrompt.Description,
			Arguments:   make([]tools.PromptArgument, 0, len(mcpPrompt.Arguments)),
		}

		for _, arg := range mcpPrompt.Arguments {
			promptInfo.Arguments = append(promptInfo.Arguments, tools.PromptArgument{
				Name:        arg.Name,
				Description: arg.Description,
				Required:    arg.Required,
			})
		}

		prompts[mcpPrompt.Name] = promptInfo
		slog.DebugContext(ctx, "Discovered MCP prompt", "name", mcpPrompt.Name, "args_count", len(promptInfo.Arguments))
	}

	return prompts
}

func (r *LocalRuntime) agentForContext(ctx context.Context) *agent.Agent {
	if sessionID := genai.ConversationIDFromContext(ctx); sessionID != "" {
		if driver, ok := r.sessionDrivers.Lookup(sessionID); ok {
			if resolved := r.resolveSessionAgent(driver.session()); resolved != nil {
				return resolved
			}
		}
	}
	defaultAgent, _ := r.team.DefaultAgent()
	return defaultAgent
}

func (r *LocalRuntime) currentAgent() *agent.Agent {
	return r.agents.Current()
}

// resolveSessionAgent returns the agent for the given session. Delegates to
// agentRouter.ResolveSession; kept on LocalRuntime for the existing callsites
// in loop.go and elsewhere.
func (r *LocalRuntime) resolveSessionAgent(sess *session.Session) *agent.Agent {
	return r.agents.ResolveSession(sess)
}

// CurrentAgentSkillsToolset returns the skills toolset for the current agent, or nil if not enabled.
func (r *LocalRuntime) CurrentAgentSkillsToolset() *skills.ToolSet {
	return agentSkillsToolset(r.currentAgent())
}

// agentSkillsToolset returns the skills toolset configured on a, or nil if
// a is nil or has none. Session-aware callers use it for pinned agents.
func agentSkillsToolset(a *agent.Agent) *skills.ToolSet {
	if a == nil {
		return nil
	}
	for _, ts := range a.ToolSets() {
		if st, ok := tools.As[*skills.ToolSet](ts); ok {
			return st
		}
	}
	return nil
}

// ExecuteMCPPrompt executes an MCP prompt with provided arguments and returns the content.
func (r *LocalRuntime) ExecuteMCPPrompt(ctx context.Context, promptName string, arguments map[string]string) (string, error) {
	currentAgent := r.currentAgent()
	if currentAgent == nil {
		return "", errors.New("no current agent available")
	}

	for _, toolset := range currentAgent.ToolSets() {
		mcpToolset, ok := tools.As[mcpPromptToolset](toolset)
		if !ok {
			continue
		}

		result, err := mcpToolset.GetPrompt(ctx, promptName, arguments)
		if err != nil {
			// If error is "prompt not found", continue to next toolset
			if err.Error() == "prompt not found" {
				continue
			}
			return "", fmt.Errorf("error executing prompt '%s': %w", promptName, err)
		}

		// Convert the MCP result to a string format
		if len(result.Messages) == 0 {
			return "No content returned from MCP prompt", nil
		}

		var content strings.Builder
		for i, message := range result.Messages {
			if i > 0 {
				content.WriteString("\n\n")
			}
			if textContent, ok := message.Content.(*mcp.TextContent); ok {
				content.WriteString(textContent.Text)
			} else {
				fmt.Fprintf(&content, "[Non-text content: %T]", message.Content)
			}
		}
		return content.String(), nil
	}

	return "", fmt.Errorf("MCP prompt '%s' not found in any active toolset", promptName)
}

// TitleGenerator returns a title generator for automatic session title
// generation, or nil when no configured model is a usable title candidate
// (see [sessiontitle.New]).
func (r *LocalRuntime) TitleGenerator(ctx context.Context) *sessiontitle.Generator {
	a := r.currentAgent()
	if a == nil {
		return nil
	}
	return sessiontitle.New(a.TitleModels(ctx)...)
}

// getAgentModelID returns the model ID for an agent. The zero ID is
// returned when no model is configured.
func getAgentModelID(ctx context.Context, a *agent.Agent) modelsdev.ID {
	if a == nil {
		return modelsdev.ID{}
	}
	if model := a.Model(ctx); model != nil {
		return model.ID()
	}
	return modelsdev.ID{}
}

// getEffectiveModelID returns the currently active model ID for an agent, accounting
// for any active fallback cooldown. During a cooldown period, this returns the fallback
// model ID instead of the configured primary model, so the UI reflects the actual model in use.
func (r *LocalRuntime) getEffectiveModelID(ctx context.Context, a *agent.Agent) modelsdev.ID {
	cooldownState := r.fallback.cooldowns.Get(a.Name())
	if cooldownState != nil {
		fallbacks := a.FallbackModels()
		if cooldownState.fallbackIndex >= 0 && cooldownState.fallbackIndex < len(fallbacks) {
			return fallbacks[cooldownState.fallbackIndex].ID()
		}
	}
	return getAgentModelID(ctx, a)
}

// agentDetailsFromTeam converts team agent info to AgentDetails for events.
// It accounts for active fallback cooldowns, returning the effective model
// instead of the configured model when a fallback is in effect.
func (r *LocalRuntime) agentDetailsFromTeam(ctx context.Context) []AgentDetails {
	agentsInfo := r.team.AgentsInfo(ctx)
	details := make([]AgentDetails, len(agentsInfo))
	db := cachedModelDatabase(r.modelsStore)
	for i, info := range agentsInfo {
		providerName := info.Provider
		modelName := info.Model
		var thinking string
		var display AgentDetails
		display.ModelID = modelName
		display.ModelName = modelName
		display.ThinkingMode = "unknown"
		display.ThinkingLevel = "unknown"

		// Get the agent to access fallbacks and the effective thinking level.
		if a, err := r.team.Agent(info.Name); err == nil && a != nil {
			models := a.EffectiveModels(ctx)
			var effective provider.Provider
			usingFallback := false
			if len(models) > 0 {
				effective = models[0]
			}
			// Check if this agent has an active fallback cooldown
			cooldownState := r.fallback.cooldowns.Get(info.Name)
			if cooldownState != nil {
				fallbacks := a.FallbackModels()
				if cooldownState.fallbackIndex >= 0 && cooldownState.fallbackIndex < len(fallbacks) {
					effective = fallbacks[cooldownState.fallbackIndex]
					usingFallback = true
					fb := effective.ID()
					providerName = fb.Provider
					modelName = fb.Model
				}
			}
			if effective != nil {
				cfg := effective.BaseConfig().ModelConfig
				// Some providers expose identity only through ID, leaving their
				// base config empty. Never erase the roster/fallback identity.
				if cfg.Provider != "" {
					providerName = cfg.Provider
				} else {
					cfg.Provider = providerName
				}
				// Cycling changes the primary session binding, not a cooldown
				// fallback. Do not advertise an action on the fallback's tiers.
				projectModelDisplay(&display, cfg, db, r.SupportsModelSwitching() && !usingFallback)
				if display.ModelID == "" {
					display.ModelID = effective.ID().Model
					display.ModelName = display.ModelID
					if m, ok := db.LookupModel(modelsdev.NewID(providerName, display.ModelID)); ok && strings.TrimSpace(m.Name) != "" {
						display.ModelName = m.Name
					}
				}
			}
			if usingFallback {
				primary := AgentDetails{ThinkingMode: "unknown", ThinkingLevel: "unknown"}
				if len(models) > 0 {
					cfg := models[0].BaseConfig().ModelConfig
					primary.Provider = cfg.Provider
					if primary.Provider == "" {
						primary.Provider = models[0].ID().Provider
					}
					projectModelDisplay(&primary, cfg, db, r.SupportsModelSwitching())
					if primary.ModelID == "" {
						primary.ModelID = models[0].ID().Model
					}
				}
				control := primary.ThinkingControl()
				display.PrimaryThinking = &control
			}
			// Preserve the legacy label's primary-model semantics. New display
			// fields above describe the actual effective (including fallback) model.
			thinking = r.agentThinkingLabel(ctx, a)
		}

		details[i] = AgentDetails{
			Name:             info.Name,
			Description:      info.Description,
			Provider:         providerName,
			Model:            modelName,
			Thinking:         thinking,
			ModelID:          display.ModelID,
			ModelName:        display.ModelName,
			ThinkingMode:     display.ThinkingMode,
			ThinkingLevel:    display.ThinkingLevel,
			ThinkingLevels:   display.ThinkingLevels,
			CanCycleThinking: display.CanCycleThinking,
			PrimaryThinking:  display.PrimaryThinking,
			Commands:         info.Commands,
		}
	}
	return details
}

// agentThinkingLabel returns a short, user-facing label for the effective
// thinking-effort level of the agent's current model: the effort level (e.g.
// "high"), "adaptive" for adaptive budgets, the decimal token count for
// token-based budgets, "off" when thinking is disabled on a reasoning-capable
// model, or "" when the model has no selectable thinking configuration to
// display.
func (r *LocalRuntime) agentThinkingLabel(ctx context.Context, a *agent.Agent) string {
	models := a.EffectiveModels(ctx)
	if len(models) == 0 {
		return ""
	}
	cfg := models[0].BaseConfig().ModelConfig
	var display AgentDetails
	projectModelDisplay(&display, cfg, cachedModelDatabase(r.modelsStore), r.SupportsModelSwitching())
	if display.ThinkingMode == "unsupported" || display.ThinkingMode == "unknown" {
		return ""
	}
	budget := cfg.ThinkingBudget
	if budget == nil || budget.IsDisabled() {
		return "off"
	}
	if l, ok := budget.EffortLevel(); ok {
		return l.String()
	}
	if budget.IsAdaptive() {
		return "adaptive"
	}
	return strconv.Itoa(budget.Tokens) // token-based budget
}

// SessionStore returns the session store for browsing/loading past sessions.
func (r *LocalRuntime) SessionStore() session.Store {
	return r.sessionStore
}

func (r *LocalRuntime) shutdownStartupTools(ctx context.Context) error {
	r.startupToolsMu.Lock()
	if !r.startupToolsClosed {
		r.startupToolsClosed = true
		r.startupToolsCancel()
	}
	r.startupToolsMu.Unlock()
	select {
	case <-r.startupToolsWG.DoneChan():
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// CloseSessions releases session resources while leaving the embedder-owned
// session store open.
func (r *LocalRuntime) shutdownSessions(ctx context.Context) error {
	r.shutdownMu.Lock()
	if r.shutdownGate == nil {
		r.shutdownGate = make(chan struct{}, 1)
	}
	gate := r.shutdownGate
	r.shutdownMu.Unlock()
	r.shutdownFence.Do(func() {
		if r.subagents != nil {
			r.subagents.closeAdmission()
		}
		if r.sessionDrivers != nil {
			r.sessionDrivers.closeAdmission()
		}
		if r.lifecycleCancel != nil {
			r.lifecycleCancel()
		}
	})
	select {
	case gate <- struct{}{}:
		defer func() { <-gate }()
	case <-ctx.Done():
		return ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	r.drainMu.Lock()
	r.drainCtx = ctx
	r.drainMu.Unlock()
	for _, observer := range r.observers {
		if p, ok := observer.(*PersistenceObserver); ok {
			p.setDrainContext(ctx)
		}
	}
	if err := r.shutdownStartupTools(ctx); err != nil {
		return err
	}
	if r.sessionDrivers != nil {
		if err := r.sessionDrivers.CloseContext(ctx); err != nil {
			return err
		}
	}
	if r.subagents != nil {
		if err := r.subagents.CloseContext(ctx); err != nil {
			return err
		}
	}
	if r.sessionEvents != nil {
		r.sessionEvents.Close()
	}
	r.sessionService.unregister(r)
	return nil
}

func (r *LocalRuntime) Close() error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.lifetime()), 5*time.Second)
	defer cancel()
	return NewSessionRuntimeSupervisor(r).Shutdown(ctx)
}

// UpdateSessionTitle persists the session title via the session store.
func (r *LocalRuntime) UpdateSessionTitle(ctx context.Context, sess *session.Session, title string) error {
	if sess == nil {
		return &SessionError{Kind: SessionErrorInvalid, Operation: SessionOperationUpdateTitle}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if d, ok := r.sessionDrivers.Lookup(sess.ID); ok {
		return d.UpdateTitle(ctx, title)
	}
	unlockMetadata := sess.LockMetadata()
	defer unlockMetadata()
	if r.sessionStore != nil {
		if err := r.sessionStore.UpdateSessionTitle(ctx, sess.ID, title); err != nil {
			return err
		}
	}
	sess.SetTitle(title)
	r.sessionEvents.Publish(sess.ID, SessionTitle(sess.ID, title))
	return nil
}

// PermissionsInfo returns the team-level permission patterns.
// Returns nil if no permissions are configured.
func (r *LocalRuntime) PermissionsInfo() *PermissionsInfo {
	permChecker := r.team.Permissions()
	if permChecker == nil || permChecker.IsEmpty() {
		return nil
	}
	return &PermissionsInfo{
		Allow: permChecker.AllowPatterns(),
		Ask:   permChecker.AskPatterns(),
		Deny:  permChecker.DenyPatterns(),
	}
}

// ResetStartupInfo is retained for runtime interface compatibility. Startup
// information is emitted per sink, so a newly-created App sharing this runtime
// always receives its pinned session's presentation state.
func (r *LocalRuntime) ResetStartupInfo() {}

// OnToolsChanged registers a handler that is called when an MCP toolset
// reports a tool list change outside of a RunStream. This allows the UI
// to update the tool count immediately.
func (r *LocalRuntime) OnToolsChanged(handler func(Event)) {
	r.toolsChangedMu.Lock()
	defer r.toolsChangedMu.Unlock()
	for _, release := range r.toolsChangedReleases {
		release()
	}
	r.toolsChangedReleases = nil
	r.onToolsChanged = handler
	if handler == nil {
		return
	}
	for _, name := range r.team.AgentNames() {
		a, err := r.team.Agent(name)
		if err != nil {
			continue
		}
		seen := make(map[tools.ChangeSubscriber]bool)
		for _, ts := range a.ToolSets() {
			if n, ok := tools.As[tools.ChangeSubscriber](ts); ok && !seen[n] {
				seen[n] = true
				release := n.SubscribeToolsChanged(func() { r.emitToolsChanged(a) })
				stop := context.AfterFunc(r.lifetime(), release)
				r.toolsChangedReleases = append(r.toolsChangedReleases, func() { stop(); release() })
			}
		}
	}
}

// Unsolicited notifications identify affected agents, never an inferred session.
func (r *LocalRuntime) emitToolsChanged(a *agent.Agent) {
	r.toolsChangedMu.RLock()
	handler := r.onToolsChanged
	r.toolsChangedMu.RUnlock()
	if handler == nil {
		return
	}
	ctx, cancel := context.WithTimeout(r.ctx(), toolsChangedTimeout)
	defer cancel()
	agentTools, err := a.StartedTools(ctx)
	if err != nil {
		return
	}
	handler(ToolsetInfo(len(agentTools), false, a.Name()))
}

// OnBackgroundEvent is retained for source compatibility. Child events are
// observed through their session journals, never a runtime-global callback.
func (r *LocalRuntime) OnBackgroundEvent(func(Event)) {}

// emitAgentAndTeamInfo sends the AgentInfo and TeamInfo events that drive the
// sidebar's agent/model/thinking display. It returns false when sending was
// aborted (e.g. the context was cancelled). Shared by EmitStartupInfo and
// EmitAgentInfo so both render identical model labels.
func (r *LocalRuntime) emitAgentAndTeamInfo(ctx context.Context, a *agent.Agent, send func(Event) bool) bool {
	modelID := r.getEffectiveModelID(ctx, a)
	modelLabel := modelID.String()
	contextLimit := r.contextLimitForAgentModel(ctx, a, modelID)
	var compactionModel string
	var primaryLimit int64
	if a.CompactionModel() != nil {
		var compactionLimit int64
		compactionModel, primaryLimit, compactionLimit = r.compactionCapAttribution(ctx, a, modelID)
		if !compactionCaps(primaryLimit, compactionLimit) {
			compactionModel, primaryLimit = "", 0
		}
	}
	if a.HasHarness() {
		modelLabel = agentModelLabel(ctx, a)
		contextLimit = 0
		compactionModel, primaryLimit = "", 0
	}
	info := AgentInfo(a.Name(), modelLabel, a.Description(), a.WelcomeMessage(), contextLimit).(*AgentInfoEvent)
	info.CompactionModel = compactionModel
	info.PrimaryContextLimit = primaryLimit
	if !send(info) {
		return false
	}
	// The "current" agent is the one these events describe — for a pinned
	// session (e.g. an attached subagent tab) that is the session's agent,
	// not the runtime's global current agent. Callers that describe the
	// global current agent pass it as a, so this is identical for them.
	return send(TeamInfo(r.agentDetailsFromTeam(ctx), a.Name()))
}

func (r *LocalRuntime) contextLimitForAgentModel(ctx context.Context, a *agent.Agent, modelID modelsdev.ID) int64 {
	if a == nil || modelID.IsZero() {
		return 0
	}
	return r.effectiveContextLimit(ctx, a, r.resolveContextLimit(ctx, a.Model(ctx), modelID))
}

// EmitAgentInfo implements [Runtime.EmitAgentInfo]: it refreshes the agent and
// team display without touching toolset discovery.
func (r *LocalRuntime) EmitAgentInfo(ctx context.Context, events EventSink) {
	a := r.currentAgent()
	if a == nil {
		return
	}
	r.emitAgentAndTeamInfo(ctx, a, func(event Event) bool {
		if ctx.Err() != nil {
			return false
		}
		events.Emit(event)
		return true
	})
}

// EmitStartupInfo emits initial agent, team, and toolset information for immediate sidebar display.
// When sess is non-nil and contains token data, a TokenUsageEvent is also emitted so that the
// sidebar can display context usage percentage on session restore.
func (r *LocalRuntime) EmitStartupInfo(ctx context.Context, sess *session.Session, events EventSink) {
	// Honour the session's pinned agent (e.g. an attached subagent
	// sub-session); unpinned sessions resolve to the current agent as before.
	a := r.currentAgent()
	if sess != nil {
		a = r.resolveSessionAgent(sess)
		// A restored session may carry a model override that its session
		// already runs with; scope it so the info below describes the model
		// actually in use rather than the agent's configured default.
		ctx = r.sessionModelContext(ctx, sess)
	}

	// Helper to send events with context check
	send := func(event Event) bool {
		if ctx.Err() != nil {
			return false
		}
		events.Emit(event)
		return true
	}

	// Emit agent and team information immediately for fast sidebar display.
	// Use getEffectiveModelID to account for active fallback cooldowns.
	modelID := r.getEffectiveModelID(ctx, a)
	if !r.emitAgentAndTeamInfo(ctx, a, send) {
		return
	}

	// When restoring a session that already has token data, emit a
	// TokenUsageEvent so the sidebar can show the context usage percentage.
	// The context limit comes from the model definition (models.dev), which
	// is a model property — not persisted in the session.
	//
	// SessionUsage folds embedded sub-session costs into the parent's cost,
	// which is what a restore/branch context needs: those sub-sessions
	// won't emit their own events.
	var restoredInput, restoredOutput int64
	if sess != nil {
		restoredInput, restoredOutput = sess.Usage()
	}
	if restoredInput > 0 || restoredOutput > 0 {
		contextLimit := r.contextLimitForAgentModel(ctx, a, modelID)
		usage := SessionUsage(sess, contextLimit, a.CompactionThreshold())

		// Reconstruct LastMessage from the parent session's last assistant
		// message so that FinishReason (and other per-message fields) are
		// available on session restore.  We intentionally iterate
		// sess.Messages (not GetAllMessages) so the result reflects the
		// parent agent's state: this event carries the parent session_id,
		// and sub-agents emit their own token_usage events with their own
		// session_id during live streaming.
		//
		// MessagesSnapshot takes sess.mu so this cannot race a concurrent
		// AddMessage/ApplyCompaction (e.g. a live HTTP AddMessage arriving
		// while startup info is being emitted for a restored session).
		items := sess.MessagesSnapshot()
		for i := range slices.Backward(items) {
			item := &items[i]
			if !item.IsMessage() || item.Message.Message.Role != chat.MessageRoleAssistant {
				continue
			}
			msg := &item.Message.Message
			lm := &MessageUsage{
				Model:        msg.Model,
				Cost:         msg.Cost,
				FinishReason: msg.FinishReason,
			}
			if msg.Usage != nil {
				lm.Usage = *msg.Usage
			}
			usage.LastMessage = lm
			break
		}

		send(NewTokenUsageEvent(sess.ID, a.Name(), usage))
	}

	// Tool loading can be slow (MCP servers need to start). The coordinated
	// discovery marks its runtime-owned context non-interactive so toolsets
	// requiring user-driven flows fail fast rather than blocking on a dialog.
	r.emitStartupTools(ctx, a, send)
}

// emitToolsProgressively loads tools from each toolset and emits progress updates.
// This allows the UI to show the tool count incrementally as each toolset loads,
// with a spinner indicating that more tools may be coming.
//
// Events are stamped with a's name — the agent whose tools are being counted —
// not the runtime's global current agent: for a pinned session's startup info
// (e.g. an attached subagent tab) the two differ, and the TUI derives the
// selected agent from event agent names.
func (r *LocalRuntime) emitStartupTools(ctx context.Context, a *agent.Agent, send func(Event) bool) {
	r.startupToolsMu.Lock()
	if r.startupToolsClosed {
		r.startupToolsMu.Unlock()
		return
	}
	if r.startupTools == nil {
		r.startupTools = make(map[string]*startupToolSeed)
	}
	seed := r.startupTools[a.Name()]
	if seed == nil {
		seed = &startupToolSeed{subscribers: make(map[chan Event]struct{})}
		r.startupTools[a.Name()] = seed
		r.startupToolsWG.Add(1)
		go func() {
			defer r.startupToolsWG.Done()
			discoveryCtx := tools.WithoutInteractivePrompts(r.startupToolsCtx)
			publish := func(event Event) bool {
				if discoveryCtx.Err() != nil {
					return false
				}
				r.startupToolsMu.Lock()
				if len(seed.events) == maxStartupToolEvents {
					copy(seed.events, seed.events[1:])
					seed.events[len(seed.events)-1] = event
				} else {
					seed.events = append(seed.events, event)
				}
				for subscriber := range seed.subscribers {
					select {
					case subscriber <- event:
					default:
						delete(seed.subscribers, subscriber)
						close(subscriber)
					}
				}
				r.startupToolsMu.Unlock()
				return discoveryCtx.Err() == nil
			}
			r.emitToolsProgressively(discoveryCtx, a, publish)
			if discoveryCtx.Err() == nil {
				r.emitAgentWarnings(a, EventSinkFunc(func(event Event) { publish(event) }))
			}
			func() {
				r.startupToolsMu.Lock()
				defer r.startupToolsMu.Unlock()
				for subscriber := range seed.subscribers {
					delete(seed.subscribers, subscriber)
					close(subscriber)
				}
				if r.startupTools[a.Name()] == seed {
					delete(r.startupTools, a.Name())
				}
			}()
		}()
	}
	if len(seed.subscribers) == maxStartupToolSubscribers {
		r.startupToolsMu.Unlock()
		return
	}
	history := slices.Clone(seed.events)
	subscriber := make(chan Event, min(len(a.ToolSets())+3, maxStartupToolEvents))
	seed.subscribers[subscriber] = struct{}{}
	r.startupToolsMu.Unlock()
	defer func() {
		r.startupToolsMu.Lock()
		defer r.startupToolsMu.Unlock()
		if _, registered := seed.subscribers[subscriber]; registered {
			delete(seed.subscribers, subscriber)
			close(subscriber)
		}
	}()

	for _, event := range history {
		if !send(event) {
			return
		}
	}
	for {
		select {
		case event, ok := <-subscriber:
			if !ok || !send(event) {
				return
			}
		case <-ctx.Done():
			return
		}
	}
}

// emitToolsProgressively loads and counts tools once for an agent. Startup
// consumers share the resulting authoritative event stream via
// emitStartupTools, preventing concurrent probes from treating another
// consumer's in-flight Start as a terminal zero-tool result.
func (r *LocalRuntime) emitToolsProgressively(ctx context.Context, a *agent.Agent, send func(Event) bool) {
	toolsets := a.ToolSets()
	totalToolsets := len(toolsets)

	// If no toolsets, emit final state immediately
	if totalToolsets == 0 {
		send(ToolsetInfo(0, false, a.Name()))
		return
	}

	// Emit initial loading state
	if !send(ToolsetInfo(0, true, a.Name())) {
		return
	}

	// Start every toolset concurrently so one slow start (e.g. an MCP server
	// pulling an image) doesn't delay the others; the context is already
	// non-interactive here so no start can block on a user-driven flow. Each
	// start is non-blocking and bounded (tools.TryStartWithTimeout): a start
	// already in flight is skipped rather than joined, a wedged start is
	// abandoned at the timeout, and outcomes are consumed in configuration
	// order below so an early fast toolset is listed (and counted) without
	// waiting for a slow later one. Peer-dependent toolsets start in a
	// second wave, after the others have been attempted.
	type startOutcome struct {
		started bool
		err     error
	}
	starts := make([]chan startOutcome, totalToolsets)
	var independents sync.WaitGroup
	var dependents []int
	for i, toolset := range toolsets {
		startable, ok := toolset.(*tools.StartableToolSet)
		if !ok {
			continue
		}
		starts[i] = make(chan startOutcome, 1)
		if _, ok := tools.As[tools.PeerDependent](startable); ok {
			dependents = append(dependents, i)
			continue
		}
		independents.Go(func() {
			started, err := startable.TryStartWithTimeout(ctx, r.toolStartTimeout)
			starts[i] <- startOutcome{started: started, err: err}
		})
	}
	for _, i := range dependents {
		startable := toolsets[i].(*tools.StartableToolSet)
		go func() {
			independents.Wait()
			started, err := startable.TryStartWithTimeout(ctx, r.toolStartTimeout)
			starts[i] <- startOutcome{started: started, err: err}
		}()
	}

	// Load tools from each toolset and emit progress
	var totalTools int
	for i, toolset := range toolsets {
		// Check context before potentially slow operations
		if ctx.Err() != nil {
			return
		}

		isLast := i == totalToolsets-1

		// Handle the start outcome, including recovery: a previously-started
		// toolset whose inner connection died (e.g. background invalid_token)
		// had its recovery start attempted above so ShouldReportRecoveryFailure
		// can fire the targeted re-auth notice. The attempt is a no-op when the
		// toolset is already healthy, so starting it unconditionally is safe.
		if startable, ok := toolset.(*tools.StartableToolSet); ok {
			outcome := <-starts[i]
			if err := outcome.err; err != nil {
				desc := tools.DescribeToolSet(startable.ToolSet)
				// A cancellation-family error means the bounded attempt may
				// have abandoned a wedged Start that keeps running in the
				// background holding the toolset's single-flight lock. The
				// failure reporters below share that lock, so consulting them
				// here would block on the very attempt just abandoned — never
				// touch them for these errors.
				if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
					// Outer context done — possibly canceled after the
					// top-of-loop check: the startup pass is being torn down,
					// so return without a warning.
					if ctx.Err() != nil {
						return
					}
					if errors.Is(err, context.DeadlineExceeded) {
						// A start that outlived its per-toolset deadline (e.g.
						// an MCP container stuck behind a wedged Docker daemon)
						// is reported directly: the abandoned Start goroutine
						// has not returned, so the once-per-streak guard below
						// has recorded nothing and would silently swallow the
						// warning.
						slog.WarnContext(ctx, "Toolset start timed out; skipping",
							"agent", a.Name(), "toolset", desc, "timeout", r.toolStartTimeout)
						a.AddToolWarning(fmt.Sprintf("%s is taking too long to start (>%s) — it keeps starting in the background and its tools appear once it is ready", desc, r.toolStartTimeout))
						continue
					}
					// A Canceled error while the outer context is still live can
					// only come from within the toolset's own Start; skip it for
					// this pass like the agent's turn path does.
					slog.DebugContext(ctx, "Toolset start canceled; skipping",
						"agent", a.Name(), "toolset", desc, "cause", err)
					continue
				}
				// IsAuthorizationRequired must be checked BEFORE
				// ShouldReportFailure: this is the first — expected —
				// failure of a deferred-OAuth toolset, and consuming the
				// failure-reported flag here would suppress the *real*
				// failure (e.g. server 4xx on the eventual interactive
				// retry) that the user actually needs to see.
				switch {
				case tools.IsAuthorizationRequired(err):
					// Two cases:
					// 1. Initial startup deferral (toolset never ran): the
					//    OAuth dialog will appear naturally on the first user
					//    message — no need to pre-announce it.
					// 2. Recovery: the toolset was previously working but the
					//    background watcher detected a server-side invalid_token
					//    (fixes #3198). Surface a deduped re-auth notice so the
					//    user knows what is about to prompt on their next message.
					if startable.ShouldReportRecoveryFailure() {
						slog.WarnContext(ctx, "Toolset needs re-authentication after background token rejection",
							"agent", a.Name(), "toolset", desc)
						a.AddToolWarning(desc + " needs re-authentication — it will prompt on your next message, or use /toolset-restart")
					} else {
						slog.DebugContext(ctx, "Toolset deferred until first message", "agent", a.Name(), "toolset", desc, "reason", err)
					}
				// Route real failures through the agent's warning
				// channel so the TUI surfaces a persistent,
				// user-visible notice that includes the actual
				// server-side cause (threaded through by
				// remoteMCPClient.Initialize). Use the same
				// once-per-streak guard as ensureToolSetsAreStarted
				// so a failing toolset doesn't flood the UI with a
				// new warning every time the agent is restarted.
				case startable.ShouldReportFailure():
					slog.WarnContext(ctx, "Toolset start failed; skipping", "agent", a.Name(), "toolset", desc, "error", err)
					a.AddToolWarning(fmt.Sprintf("%s start failed: %v", desc, err))
				default:
					slog.DebugContext(ctx, "Toolset still unavailable; skipping", "agent", a.Name(), "toolset", desc, "error", err)
				}
				// A partial start leaves the composite latched and usable:
				// fall through so its healthy subset (and the composite's
				// own wrapper tool) is still listed and counted. Fully
				// failed toolsets have nothing to list — skip them.
				if !tools.IsPartialStart(err) {
					continue
				}
			} else if !outcome.started {
				// Another lifecycle operation holds the toolset's single-flight
				// lock (e.g. a start abandoned by an earlier bounded attempt):
				// skip the toolset for this startup pass — silently, because
				// the failure reporters share that lock and consulting them
				// would block on the very attempt being skipped. It is picked
				// up once the attempt settles. Checked only on the error-free
				// path: a partial start reports started=false with its error
				// while the wrapper is latched, and must fall through above.
				slog.DebugContext(ctx, "Toolset start already in flight; skipping",
					"agent", a.Name(), "toolset", tools.DescribeToolSet(startable.ToolSet))
				continue
			}
		}

		// Get tools from this toolset under a bounded deadline. A toolset
		// whose Tools() blocks indefinitely would otherwise stall the whole
		// loop: the terminal ToolsetInfo{Loading:false} below is never sent,
		// so the sidebar stays on "Loading tools…" forever and /quit appears
		// to hang. Time it out, skip it, and move on. The skip is only for
		// this startup sidebar pass — the toolset is not torn down, so a
		// slow-but-responsive one is listed (and counted) again on the next
		// turn or tool-change refresh.
		ts, err := listToolsWithTimeout(ctx, toolset, r.toolListTimeout)
		if err != nil {
			slog.WarnContext(ctx, "Failed to list tools from toolset; skipping",
				"agent", a.Name(), "toolset", tools.DescribeToolSet(toolset), "error", err)
			continue
		}

		totalTools += len(ts)

		// Emit progress update - still loading unless this is the last toolset
		if !send(ToolsetInfo(totalTools, !isLast, a.Name())) {
			return
		}
	}

	// Emit final state (not loading)
	send(ToolsetInfo(totalTools, false, a.Name()))
}

// listToolsWithTimeout enumerates a toolset's tools under a bounded deadline.
// The (potentially blocking) Tools call runs in a goroutine and we select on
// either its completion or the timeout, so a toolset whose Tools() ignores
// context cancellation — e.g. a wedged MCP stdio subprocess — cannot block
// startup. On timeout it returns the context error; the orphaned goroutine
// sends into a buffered channel and exits if the call ever returns, so it does
// not leak past the eventual (or never) return of Tools().
func listToolsWithTimeout(ctx context.Context, toolset tools.ToolSet, timeout time.Duration) ([]tools.Tool, error) {
	// Defend against a zero/negative timeout (e.g. a directly-constructed
	// LocalRuntime that bypassed NewLocalRuntime) so we never collapse to an
	// already-expired context that skips every toolset.
	if timeout <= 0 {
		timeout = defaultToolListTimeout
	}
	toolCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	type listResult struct {
		tools []tools.Tool
		err   error
	}
	done := make(chan listResult, 1) // buffered so a late send never blocks
	go func() {
		ts, err := toolset.Tools(toolCtx)
		done <- listResult{tools: ts, err: err}
	}()

	select {
	case <-toolCtx.Done():
		return nil, toolCtx.Err()
	case res := <-done:
		return res.tools, res.err
	}
}

// Steer enqueues a user message for urgent mid-turn injection into the
// running agent loop. The message will be picked up after the current batch
func (r *LocalRuntime) compatibilityInputTarget(ctx context.Context) (*sessionDriver, error) {
	if id := genai.ConversationIDFromContext(ctx); id != "" {
		if driver, ok := r.sessionDrivers.Lookup(id); ok {
			return driver, nil
		}
		return nil, ErrSessionClosed
	}
	r.sessionDrivers.mu.Lock()
	var target *sessionDriver
	for _, driver := range r.sessionDrivers.drivers {
		if driver.identityParent != "" {
			continue
		}
		if target != nil {
			r.sessionDrivers.mu.Unlock()
			return nil, &SessionError{Kind: SessionErrorInvalid, Operation: "input_route", Detail: "multiple root sessions require an explicit session handle"}
		}
		target = driver
	}
	r.sessionDrivers.mu.Unlock()
	if target == nil {
		return nil, ErrSessionClosed
	}
	return target, nil
}

func (r *LocalRuntime) Steer(ctx context.Context, msg QueuedMessage) error {
	d, err := r.compatibilityInputTarget(ctx)
	if err != nil {
		return err
	}
	if msg.RequestID == "" {
		msg.RequestID, err = newSessionRequestID()
		if err != nil {
			return err
		}
	}
	_, err = d.postSteer(ctx, msg)
	return err
}

func (r *LocalRuntime) FollowUp(ctx context.Context, msg QueuedMessage) error {
	d, err := r.compatibilityInputTarget(ctx)
	if err != nil {
		return err
	}
	if msg.RequestID == "" {
		msg.RequestID, err = newSessionRequestID()
		if err != nil {
			return err
		}
	}
	msg.InputMode = "followup"
	_, err = d.post(ctx, msg, true)
	return err
}

func (r *LocalRuntime) CancelSteer(ctx context.Context, id string) bool {
	d, err := r.compatibilityInputTarget(ctx)
	if err != nil {
		return false
	}
	withdrawn, err := d.cancelPendingMessage(ctx, id)
	return err == nil && withdrawn
}

func (r *LocalRuntime) CancelFollowUp(ctx context.Context, id string) bool {
	return r.CancelSteer(ctx, id)
}

func (r *LocalRuntime) SetRecallHandler(_ RecallHandler) {}

func (r *LocalRuntime) recall(ctx context.Context, msg QueuedMessage) error { return r.Steer(ctx, msg) }

func (r *LocalRuntime) QueueStatus() QueueStatus { return QueueStatus{} }

// Run starts the agent's interaction loop

func (r *LocalRuntime) startSpan(ctx context.Context, name string, opts ...trace.SpanStartOption) (context.Context, trace.Span) {
	if r.tracer == nil {
		return ctx, trace.SpanFromContext(ctx)
	}
	return r.tracer.Start(ctx, name, opts...)
}

// Summarize generates a summary for the session based on the conversation history.
// The additionalPrompt parameter allows users to provide additional instructions
// for the summarization (e.g., "focus on code changes" or "include action items").
//
// Summarize is the public entry point used by user-driven /compact actions; it
// reports compactionReasonManual to BeforeCompaction / AfterCompaction hooks
// and "manual" to PreCompact hooks.
// Internal callers (proactive threshold, overflow recovery) use
// [LocalRuntime.compactWithReason] directly to forward a more specific reason.
func (r *LocalRuntime) Summarize(ctx context.Context, sess *session.Session, additionalPrompt string, events EventSink) {
	r.compactWithReason(ctx, sess, additionalPrompt, compactionReasonManual, events)
}

// compactWithReason runs a session compaction with the supplied reason and
// emits a TokenUsageEvent so the UI immediately reflects the new context
// pressure.
//
// reason is reported to BeforeCompaction / AfterCompaction hooks as
// CompactionReason. Use [compactionReasonThreshold] for proactive
// threshold-of-context triggers (90% by default, tunable via
// compaction_threshold), [compactionReasonOverflow] for post-overflow
// auto-recovery, or [compactionReasonManual] for user-invoked compactions.
//
// PreCompact hooks fire first via the legacy [hooks.Input.Source] field
// ("auto" / "tool_overflow" / "overflow" / "manual"); they may cancel the
// compaction or contribute additional steering text. BeforeCompaction
// hooks then fire inside [LocalRuntime.doCompact] with [Input.CompactionReason]
// set to the canonical reason; they may veto or supply a custom summary.
func (r *LocalRuntime) compactWithReason(ctx context.Context, sess *session.Session, additionalPrompt, reason string, events EventSink) {
	// Stamp the session ID on ctx so the compaction LLM call carries
	// `X-Cagent-Session-Id` to the gateway. Manual compaction
	// (via `Summarize` from the App) bypasses `runStreamLoop`'s seed;
	// internal callers (proactive threshold, overflow recovery) already
	// run with a stamped ctx, but re-stamping is idempotent.
	ctx = httpclient.ContextWithSessionID(ctx, sess.ID)
	a := r.resolveSessionAgent(sess)

	source := preCompactSourceFor(reason)
	skip, msg, extraPrompt := r.executePreCompactHooks(ctx, sess, a, source, events)
	if skip {
		slog.WarnContext(ctx, "pre_compact hook signalled skip",
			"agent", a.Name(), "session_id", sess.ID, "source", source, "reason", msg)
		if msg != "" {
			events.Emit(Warning(msg, a.Name()))
		}
		return
	}
	additionalPrompt = joinPrompts(additionalPrompt, extraPrompt)

	r.doCompact(ctx, sess, a, additionalPrompt, reason, events)

	// Emit a TokenUsageEvent so the sidebar immediately reflects the
	// compaction: tokens drop to the summary size, context % drops, and
	// cost increases by the summary generation cost.
	modelID := r.getEffectiveModelID(ctx, a)
	contextLimit := r.effectiveContextLimit(ctx, a, r.resolveContextLimit(ctx, a.Model(ctx), modelID))
	events.Emit(NewTokenUsageEvent(sess.ID, a.Name(), SessionUsage(sess, contextLimit, a.CompactionThreshold())))
}

// preCompactSourceFor maps the canonical compaction reason
// ([compactionReasonThreshold] / [compactionReasonOverflow] /
// [compactionReasonManual]) onto the [hooks.Input.Source] string
// surfaced by the pre_compact hook ("auto" / "overflow" / "manual").
// Unknown reasons fall through unchanged so future, more specific
// reasons (e.g. "tool_overflow") can be forwarded verbatim without
// touching this map.
func preCompactSourceFor(reason string) string {
	switch reason {
	case compactionReasonThreshold:
		return "auto"
	case compactionReasonOverflow:
		return "overflow"
	case compactionReasonManual:
		return "manual"
	default:
		return reason
	}
}

// joinPrompts concatenates two non-empty prompt fragments with a blank
// line, returning whichever is non-empty when the other isn't. Used by
// compactWithReason to splice pre_compact's additional_context into
// the caller's additionalPrompt without having to special-case empty
// strings at the callsite.
func joinPrompts(a, b string) string {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	default:
		return a + "\n\n" + b
	}
}
