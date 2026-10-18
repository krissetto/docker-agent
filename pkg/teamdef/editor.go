package teamdef

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/goccy/go-yaml"
	"github.com/goccy/go-yaml/ast"
	"github.com/goccy/go-yaml/parser"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
)

// ActorRef binds every edit to the baseline team/source epoch and stable agent
// identity of the session's pinned caller. SourceRevision is the editor's
// immutable baseline epoch, not the current edited revision; therefore the same
// pinned caller remains eligible after its own successful commit while each
// requested patch still needs the current head as its CAS base. A user reload
// creates a new editor epoch and invalidates these refs.
type ActorRef struct {
	TeamID         string `json:"team_id"`
	AgentKey       string `json:"agent_key"`
	SourceRevision string `json:"source_revision"`
}

// AgentPatch changes only explicitly supported authored fields. Nil means
// unchanged; empty strings/lists clear the selected field.
type AgentPatch struct {
	AgentKey          string
	RenameTo          string
	Instruction       *string
	Description       *string
	WelcomeMessage    *string
	Model             *string
	FallbackModels    *[]string
	ModelPickerModels *[]string
	CloneFrom         string
	NewAgentKey       string
	Remove            bool
}

// AgentView is a safe projection for inspection. It contains only approved
// authored display fields; provider, credential and runtime configuration are
// never included.
type AgentView struct {
	Key            string   `json:"key"`
	ID             string   `json:"id"`
	Description    string   `json:"description,omitempty"`
	WelcomeMessage string   `json:"welcome_message,omitempty"`
	Instruction    string   `json:"instruction,omitempty"`
	Model          string   `json:"model,omitempty"`
	FallbackModels []string `json:"fallback_models,omitempty"`
}

// Inspection exposes a revision token and safe authored agent projection only.
type Inspection struct {
	Revision Revision    `json:"revision"`
	Agents   []AgentView `json:"agents"`
}

// Validation reports whether the source parsed structurally and whether the
// injected full team-build validator succeeded.
type Validation struct {
	Revision     Revision `json:"revision"`
	BuildChecked bool     `json:"build_checked"`
}

// Editor applies narrowly-scoped authored edits against an immutable source
// revision. capability/model boundaries are captured at construction and are
// never refreshed after a patch.
type Editor struct {
	mu              sync.Mutex
	source          *EditableSource
	build           BuildValidator
	modelRefs       map[string]map[string]bool
	teamID          string
	baselineID      string
	baselineKeys    map[string]string          // stable ID -> original authored key
	closure         map[string]map[string]bool // actor stable ID -> reachable stable IDs
	rootAgentKey    string
	rootStableID    string
	baselineGraph   map[string]map[string]bool
	sourceClass     map[string]string // current stable ID -> immutable baseline class
	profiles        map[string]string // baseline class -> immutable capability profile
	cacheEnabled    map[string]bool
	mode            EditAuthorityMode
	allowedActorIDs map[string]bool
	fullAllowlist   bool
	reservedIDs     map[string]bool
	expectedGraph   map[string]map[string]bool
}

// NewEditor captures a direct latest YAML source and snapshots the permitted
// model references for each agent from its baseline. A real build validator is
// required before any revision can be committed.
func NewEditor(ctx context.Context, opts EditableSourceOptions) (*Editor, error) {
	if opts.BuildValidator == nil {
		return nil, errors.New("controlled editor requires an injected build validator")
	}
	mode := opts.Authority
	if mode == "" {
		mode = EditAuthorityRestricted
	}
	if mode != EditAuthorityRestricted && mode != EditAuthorityFull {
		return nil, errors.New("editor authority is disabled or invalid")
	}
	source, err := newEditableSource(ctx, opts.Source, opts.ExportRoot, opts.WritablePath, mode)
	if err != nil {
		return nil, err
	}
	baseline := source.CurrentRevision()
	cfgSource, err := source.SourceForLoad(baseline)
	if err != nil {
		return nil, err
	}
	cfg, err := config.Load(ctx, cfgSource)
	if err != nil {
		return nil, errors.New("editable baseline failed config validation")
	}
	teamID := cfg.ID
	if teamID == "" {
		teamID = SourceIdentity(source.name)
	}
	e := &Editor{source: source, build: opts.BuildValidator, modelRefs: make(map[string]map[string]bool), teamID: teamID, baselineID: baseline.ID, baselineKeys: make(map[string]string), closure: make(map[string]map[string]bool), sourceClass: make(map[string]string), profiles: make(map[string]string), cacheEnabled: make(map[string]bool), mode: mode, fullAllowlist: opts.AllowedAgentIDs != nil, allowedActorIDs: make(map[string]bool), reservedIDs: make(map[string]bool)}
	for _, actorID := range opts.AllowedAgentIDs {
		e.allowedActorIDs[actorID] = true
	}
	if mode == EditAuthorityFull {
		return e, nil
	}
	for _, agent := range cfg.Agents {
		if agent.Name == "root" {
			e.rootAgentKey = agent.Name
		}
		stableID := agent.ID
		if stableID == "" {
			stableID = agent.Name
		}
		e.baselineKeys[stableID] = agent.Name
		e.sourceClass[stableID] = stableID
		e.reservedIDs[stableID] = true
		if agent.Name == "root" {
			e.rootStableID = stableID
		}
	}
	if e.rootAgentKey == "" && len(cfg.Agents) > 0 {
		e.rootAgentKey = cfg.Agents[0].Name
	}
	if e.rootStableID == "" && len(cfg.Agents) > 0 {
		agent := cfg.Agents[0]
		e.rootStableID = agent.ID
		if e.rootStableID == "" {
			e.rootStableID = agent.Name
		}
	}
	baselineBytes, err := cfgSource.Read(ctx)
	if err != nil {
		return nil, err
	}
	root, err := parseRoot(baselineBytes)
	if err != nil {
		return nil, err
	}
	graph, err := localReferenceGraph(root, e.baselineKeys)
	if err != nil {
		return nil, err
	}
	e.modelRefs, err = authoredModelReferences(root, e.baselineKeys)
	if err != nil {
		return nil, err
	}
	e.baselineGraph = graph
	e.expectedGraph = cloneGraph(graph)
	for stable, key := range e.baselineKeys {
		agent, findErr := findAgent(root, key)
		if findErr != nil {
			return nil, findErr
		}
		profile, profileErr := agentCapabilityProfile(agent)
		if profileErr != nil {
			return nil, profileErr
		}
		e.profiles[stable] = profile
		e.cacheEnabled[stable] = agentCacheEnabled(agent)
	}
	for actor := range e.baselineKeys {
		e.closure[actor] = reachable(graph, actor)
	}
	return e, nil
}

func (e *Editor) validateActor(actor ActorRef) error {
	if actor.TeamID == "" || actor.AgentKey == "" || actor.SourceRevision == "" {
		return errors.New("actor identity must include pinned team, stable agent key, and source revision")
	}
	if actor.TeamID != e.teamID || actor.SourceRevision != e.baselineID {
		return errors.New("actor identity does not match this editor's pinned team/source baseline")
	}
	if e.mode == EditAuthorityFull {
		if e.fullAllowlist && !e.allowedActorIDs[actor.AgentKey] {
			return errors.New("actor is outside the trusted full-edit allowlist")
		}
		return nil
	}
	if _, ok := e.baselineKeys[actor.AgentKey]; !ok {
		return errors.New("actor stable agent key is absent from the editor baseline")
	}
	return nil
}

func (e *Editor) actorPresent(root *yamlEditNode, actor ActorRef) error {
	keys, err := stableAgentKeys(root)
	if err != nil {
		return err
	}
	if _, ok := keys[actor.AgentKey]; !ok {
		return errors.New("actor agent is absent from the current edited team")
	}
	return nil
}

// TeamID and BaselineRevisionID are the pinned identity values callers include
// in ActorRef assembled from the session's authoritative identity record.
func (e *Editor) TeamID() string             { return e.teamID }
func (e *Editor) BaselineRevisionID() string { return e.baselineID }

func (e *Editor) rootKey() string { return e.rootAgentKey }

func stableIDForCurrentKey(root *yamlEditNode, key string) string {
	agent, err := findAgent(root, key)
	if err != nil {
		return ""
	}
	id, ok, err := scalarMappingValue(agent, "id")
	if err == nil && ok && id != "" {
		return id
	}
	return key
}

func authoredModelReferences(root *yamlEditNode, baseline map[string]string) (map[string]map[string]bool, error) {
	agents, _ := mappingValue(root, "agents")
	refs := make(map[string]map[string]bool, len(baseline))
	for stable := range baseline {
		refs[stable] = make(map[string]bool)
	}
	for i := 0; i+1 < len(agents.Content); i += 2 {
		key, agent := agents.Content[i].Value, agents.Content[i+1]
		stable := stableIDForCurrentKey(root, key)
		if _, ok := refs[stable]; !ok {
			return nil, errors.New("authored model references do not match baseline identity")
		}
		if model, ok := mappingValue(agent, "model"); ok {
			if model.Kind != yamlScalarNode || model.ShortTag() != "String" {
				return nil, errors.New("agent model must be a string scalar")
			}
			refs[stable][model.Value] = true
		}
		if fallback, ok := mappingValue(agent, "fallback"); ok {
			if models, ok := mappingValue(fallback, "models"); ok {
				if err := addModelSequence(refs[stable], models); err != nil {
					return nil, err
				}
			}
		}
		if toolsets, ok := mappingValue(agent, "toolsets"); ok {
			if toolsets.Kind != yamlSequenceNode {
				return nil, errors.New("agent toolsets must be a sequence")
			}
			for _, toolset := range toolsets.Content {
				typ, ok, err := scalarMappingValue(toolset, "type")
				if err != nil {
					return nil, err
				}
				if ok && typ == "model_picker" {
					if models, ok := mappingValue(toolset, "models"); ok {
						if err := addModelSequence(refs[stable], models); err != nil {
							return nil, err
						}
					}
				}
			}
		}
	}
	return refs, nil
}

func addModelSequence(allowed map[string]bool, node *yamlEditNode) error {
	if node.Kind != yamlSequenceNode {
		return errors.New("model references must be a sequence")
	}
	for _, item := range node.Content {
		if item.Kind != yamlScalarNode || item.ShortTag() != "String" {
			return errors.New("model references must be strings")
		}
		allowed[item.Value] = true
	}
	return nil
}

func localReferenceGraph(root *yamlEditNode, baseline map[string]string) (map[string]map[string]bool, error) {
	agents, _ := mappingValue(root, "agents")
	keys := make(map[string]string, len(baseline))
	graph := make(map[string]map[string]bool, len(baseline))
	seen := make(map[string]bool, len(baseline))
	for stable := range baseline {
		graph[stable] = make(map[string]bool)
	}
	for i := 0; i+1 < len(agents.Content); i += 2 {
		key := agents.Content[i].Value
		stable := stableIDForCurrentKey(root, key)
		if _, exists := graph[stable]; !exists {
			return nil, fmt.Errorf("agent %q has identity outside baseline", key)
		}
		if seen[stable] {
			return nil, fmt.Errorf("duplicate stable agent identity %q", stable)
		}
		seen[stable] = true
		keys[key] = stable
	}
	if len(seen) != len(baseline) {
		return nil, errors.New("agent roster differs from editor baseline")
	}
	for i := 0; i+1 < len(agents.Content); i += 2 {
		key, agent := agents.Content[i].Value, agents.Content[i+1]
		owner := keys[key]
		for _, field := range []string{"sub_agents", "handoffs", "subagents"} {
			node, ok := mappingValue(agent, field)
			if !ok {
				continue
			}
			if field == "subagents" {
				if node.Kind != yamlSequenceNode {
					return nil, fmt.Errorf("agent %q subagents must be a sequence", key)
				}
				for _, item := range node.Content {
					ref := ""
					switch item.Kind {
					case yamlScalarNode:
						ref = item.Value
					case yamlMappingNode:
						ref, _, _ = scalarMappingValue(item, "agent")
					}
					if ref != "" {
						addReference(graph[owner], keys, field, ref)
					}
				}
			} else {
				if node.Kind != yamlSequenceNode {
					return nil, fmt.Errorf("agent %q %s must be a sequence", key, field)
				}
				for _, item := range node.Content {
					if item.Kind != yamlScalarNode {
						return nil, fmt.Errorf("agent %q %s contains non-scalar", key, field)
					}
					addReference(graph[owner], keys, field, item.Value)
				}
			}
		}
		if node, ok := mappingValue(agent, "force_handoff"); ok {
			if node.Kind != yamlScalarNode {
				return nil, fmt.Errorf("agent %q force_handoff must be scalar", key)
			}
			addReference(graph[owner], keys, "force_handoff", node.Value)
		}
	}
	return graph, nil
}

func addReference(edges map[string]bool, keys map[string]string, field, ref string) {
	if stable, ok := keys[ref]; ok {
		edges[field+"\x00"+stable] = true
		return
	}
	edges[field+"\x00external:"+ref] = true
}

func reachable(graph map[string]map[string]bool, start string) map[string]bool {
	seen := map[string]bool{start: true}
	queue := []string{start}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		for edge := range graph[current] {
			_, target, _ := strings.Cut(edge, "\x00")
			if strings.HasPrefix(target, "external:") || seen[target] {
				continue
			}
			seen[target] = true
			queue = append(queue, target)
		}
	}
	return seen
}

func sameGraph(a, b map[string]map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for key, edges := range a {
		other, ok := b[key]
		if !ok || len(edges) != len(other) {
			return false
		}
		for edge := range edges {
			if !other[edge] {
				return false
			}
		}
	}
	return true
}

func cloneGraph(input map[string]map[string]bool) map[string]map[string]bool {
	output := make(map[string]map[string]bool, len(input))
	for key, edges := range input {
		output[key] = cloneBoolMap(edges)
	}
	return output
}

func cloneBoolMap(input map[string]bool) map[string]bool {
	output := make(map[string]bool, len(input))
	maps.Copy(output, input)
	return output
}

func cloneStringMap(input map[string]string) map[string]string {
	output := make(map[string]string, len(input))
	maps.Copy(output, input)
	return output
}

func hasAgentKey(root *yamlEditNode, key string) bool {
	_, err := findAgent(root, key)
	return err == nil
}

func mappingValueMust(root *yamlEditNode, key string) *yamlEditNode {
	node, _ := mappingValue(root, key)
	return node
}

func stableAgentKeys(root *yamlEditNode) (map[string]string, error) {
	agents, ok := mappingValue(root, "agents")
	if !ok || agents.Kind != yamlMappingNode {
		return nil, errors.New("agents must be a mapping")
	}
	keys := make(map[string]string, len(agents.Content)/2)
	for i := 0; i+1 < len(agents.Content); i += 2 {
		key := agents.Content[i].Value
		stable := stableIDForCurrentKey(root, key)
		if stable == "" {
			return nil, errors.New("agent has no stable identity")
		}
		if _, exists := keys[stable]; exists {
			return nil, errors.New("duplicate stable agent identity")
		}
		keys[stable] = key
	}
	return keys, nil
}

func validateAgentRoster(current, classes map[string]string) error {
	if len(current) != len(classes) {
		return errors.New("agent roster differs from editor revision")
	}
	for stable := range current {
		if classes[stable] == "" {
			return errors.New("agent roster contains an unproven capability class")
		}
	}
	return nil
}

func yamlValue(node *yamlEditNode) (any, error) {
	if node == nil {
		return nil, nil
	}
	switch node.Kind {
	case yamlMappingNode:
		values := make(map[string]any, len(node.Content)/2)
		for i := 0; i+1 < len(node.Content); i += 2 {
			key := node.Content[i].Value
			value, err := yamlValue(node.Content[i+1])
			if err != nil {
				return nil, err
			}
			values[key] = value
		}
		return values, nil
	case yamlSequenceNode:
		values := make([]any, 0, len(node.Content))
		for _, child := range node.Content {
			value, err := yamlValue(child)
			if err != nil {
				return nil, err
			}
			values = append(values, value)
		}
		return values, nil
	case yamlScalarNode:
		if scalar, ok := node.node.(ast.ScalarNode); ok {
			return scalar.GetValue(), nil
		}
		return node.Value, nil
	default:
		return nil, errors.New("unsupported node in capability profile")
	}
}

func agentCacheEnabled(agent *yamlEditNode) bool {
	cache, ok := mappingValue(agent, "cache")
	if !ok {
		return false
	}
	enabled, ok := mappingValue(cache, "enabled")
	if !ok || enabled.Kind != yamlScalarNode {
		return false
	}
	value, err := strconv.ParseBool(enabled.Value)
	return err == nil && value
}

func agentCapabilityProfile(agent *yamlEditNode) (string, error) {
	fields := make(map[string]any, len(agent.Content)/2)
	for i := 0; i+1 < len(agent.Content); i += 2 {
		key, value := agent.Content[i].Value, agent.Content[i+1]
		switch key {
		case "id", "instruction", "description", "welcome_message", "model", "sub_agents", "handoffs", "subagents", "force_handoff":
			continue
		case "fallback":
			if value.Kind != yamlMappingNode {
				return "", errors.New("fallback must be a mapping")
			}
			fallback := make(map[string]any, len(value.Content)/2)
			for j := 0; j+1 < len(value.Content); j += 2 {
				subkey, subvalue := value.Content[j].Value, value.Content[j+1]
				if subkey == "models" {
					continue
				}
				converted, err := yamlValue(subvalue)
				if err != nil {
					return "", err
				}
				fallback[subkey] = converted
			}
			fields[key] = fallback
			continue
		case "toolsets":
			if value.Kind != yamlSequenceNode {
				return "", errors.New("toolsets must be a sequence")
			}
			toolsets := make([]any, 0, len(value.Content))
			for _, toolset := range value.Content {
				if toolset.Kind != yamlMappingNode {
					return "", errors.New("toolset must be a mapping")
				}
				kind, hasKind, _ := scalarMappingValue(toolset, "type")
				clone, err := agentCapabilityValue(toolset)
				if err != nil {
					return "", err
				}
				if hasKind && kind == "model_picker" {
					m := clone
					delete(m, "models")
				}
				toolsets = append(toolsets, clone)
			}
			fields[key] = toolsets
			continue
		}
		converted, err := yamlValue(value)
		if err != nil {
			return "", err
		}
		fields[key] = converted
	}
	encoded, err := json.Marshal(fields)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

func agentCapabilityValue(agent *yamlEditNode) (map[string]any, error) {
	values := make(map[string]any, len(agent.Content)/2)
	for i := 0; i+1 < len(agent.Content); i += 2 {
		key := agent.Content[i].Value
		value, err := yamlValue(agent.Content[i+1])
		if err != nil {
			return nil, err
		}
		values[key] = value
	}
	return values, nil
}

func validateCapabilityProfiles(root *yamlEditNode, keys, classes, profiles map[string]string) error {
	for stable, key := range keys {
		class := classes[stable]
		expected, ok := profiles[class]
		if !ok {
			return errors.New("capability class is not in the pinned baseline")
		}
		agent, err := findAgent(root, key)
		if err != nil {
			return err
		}
		actual, err := agentCapabilityProfile(agent)
		if err != nil {
			return err
		}
		if actual != expected {
			return errors.New("agent capability profile differs from its pinned baseline class")
		}
	}
	return nil
}

func validateClassReachability(graph map[string]map[string]bool, classes map[string]string, baselineClosure map[string]map[string]bool, baselineClass map[string]string) error {
	for stable, class := range classes {
		allowedStable, ok := baselineClosure[class]
		if !ok {
			return errors.New("agent capability class has no baseline delegation closure")
		}
		allowedClasses := map[string]bool{class: true}
		for reachableStable := range allowedStable {
			if reachableClass := baselineClass[reachableStable]; reachableClass != "" {
				allowedClasses[reachableClass] = true
			}
		}
		seen := map[string]bool{class: true}
		queue := []string{stable}
		for len(queue) > 0 {
			current := queue[0]
			queue = queue[1:]
			for edge := range graph[current] {
				_, target, _ := strings.Cut(edge, "\x00")
				if strings.HasPrefix(target, "external:") {
					continue
				}
				targetClass := classes[target]
				if targetClass == "" {
					return errors.New("delegation edge targets unknown agent")
				}
				if !allowedClasses[targetClass] {
					return errors.New("agent delegation can reach a class outside its pinned baseline closure")
				}
				if !seen[target] {
					seen[target] = true
					queue = append(queue, target)
				}
			}
		}
	}
	return nil
}

func cloneAgent(root *yamlEditNode, sourceKey, newKey string) error {
	source, err := findAgent(root, sourceKey)
	if err != nil {
		return err
	}
	if newKey == "" || strings.TrimSpace(newKey) != newKey {
		return errors.New("clone target key must be a non-empty trimmed value")
	}
	if _, err := findAgent(root, newKey); err == nil {
		return errors.New("clone target key is already in use")
	}
	if _, err := agentCapabilityProfile(source); err != nil {
		return err
	}
	if _, hasHarness := mappingValue(source, "harness"); hasHarness {
		return errors.New("harness agents cannot be cloned")
	}
	if _, hasCache := mappingValue(source, "cache"); hasCache {
		return errors.New("agents with cache cannot be cloned")
	}
	if toolsets, ok := mappingValue(source, "toolsets"); ok {
		for _, toolset := range toolsets.Content {
			if typ, has, _ := scalarMappingValue(toolset, "type"); has && typ == "team_edit" {
				return errors.New("agents with team_edit cannot be cloned")
			}
		}
	}
	if hasExternalEdges(source) {
		return errors.New("agents with external delegation references cannot be cloned")
	}
	agents, ok := mappingValue(root, "agents")
	if !ok {
		return errors.New("agents mapping is missing")
	}
	astAgents := agents.node.(*ast.MappingNode)
	var sourceEntry *ast.MappingValueNode
	for _, entry := range astAgents.Values {
		if key, ok := entry.Key.(*ast.StringNode); ok && key.Value == sourceKey {
			sourceEntry = entry
			break
		}
	}
	if sourceEntry == nil {
		return errors.New("clone source does not exist")
	}
	valueText := sourceEntry.Value.String()
	prefix := strings.Repeat(" ", sourceEntry.Key.GetToken().Position.Column-1)
	flow := false
	if mapNode, ok := sourceEntry.Value.(*ast.MappingNode); ok {
		flow = mapNode.IsFlowStyle
	}
	snippet := prefix + mustYAMLString(newKey) + ":"
	if flow {
		snippet += " " + valueText + "\n"
	} else {
		lines := strings.Split(indentCloneMapping(valueText), "\n")
		for i := range lines {
			lines[i] = prefix + "  " + lines[i]
		}
		snippet += "\n" + strings.Join(lines, "\n") + "\n"
	}
	parsed, err := parser.ParseBytes([]byte(snippet), parser.ParseComments)
	if err != nil || len(parsed.Docs) != 1 {
		return errors.New("could not clone authored agent mapping")
	}
	entry := parsed.Docs[0].Body.(*ast.MappingNode).Values[0]
	agent := entry.Value.(*ast.MappingNode)
	if _, ok := mappingValue(source, "id"); ok {
		setMappingStringNode(agent, "id", newKey)
	} else if err := appendASTMapping(agent, "id", scalarAST(newKey)); err != nil {
		return err
	}
	astAgents.Values = append(astAgents.Values, entry)
	keyNode, err := wrapYAMLNode(entry.Key, func(next ast.Node) { entry.Key = next.(ast.MapKeyNode) })
	if err != nil {
		return err
	}
	valueNode, err := wrapYAMLNode(entry.Value, func(next ast.Node) { entry.Value = next })
	if err != nil {
		return err
	}
	agents.Content = append(agents.Content, keyNode, valueNode)
	return nil
}

func indentCloneMapping(value string) string {
	lines := strings.Split(value, "\n")
	prefix := ""
	if len(lines) > 0 {
		first := lines[0]
		prefix = first[:len(first)-len(strings.TrimLeft(first, " 	"))]
	}
	for i, line := range lines {
		lines[i] = strings.TrimPrefix(line, prefix)
	}
	return strings.Join(lines, "\n")
}

func setMappingStringNode(mapping *ast.MappingNode, key, value string) {
	for _, entry := range mapping.Values {
		if k, ok := entry.Key.(*ast.StringNode); ok && k.Value == key {
			wrapper, _ := wrapYAMLNode(entry.Value, func(next ast.Node) { entry.Value = next })
			wrapper.SetString(value)
			return
		}
	}
}

func removeAgent(root *yamlEditNode, key, stable string, graph map[string]map[string]bool) (map[string]map[string]bool, error) {
	for _, edges := range graph {
		for edge := range edges {
			field, target, _ := strings.Cut(edge, "\x00")
			if target == stable && field == "force_handoff" {
				return nil, errors.New("cannot remove an agent targeted by force_handoff")
			}
		}
	}
	agents, ok := mappingValue(root, "agents")
	if !ok {
		return nil, errors.New("agents mapping is missing")
	}
	for i := 0; i+1 < len(agents.Content); i += 2 {
		agentKey, agent := agents.Content[i].Value, agents.Content[i+1]
		if agentKey == key {
			continue
		}
		for _, field := range []string{"sub_agents", "handoffs", "subagents"} {
			if list, ok := mappingValue(agent, field); ok {
				if err := removeRefFromList(list, key); err != nil {
					return nil, fmt.Errorf("%s: %w", field, err)
				}
			}
		}
	}
	newGraph := cloneGraph(graph)
	for _, edges := range newGraph {
		for edge := range edges {
			_, target, _ := strings.Cut(edge, "\x00")
			if target == stable {
				delete(edges, edge)
			}
		}
	}
	delete(newGraph, stable)
	removeMappingEntry(agents, key)
	return newGraph, nil
}

func removeRefFromList(list *yamlEditNode, key string) error {
	if list.Kind != yamlSequenceNode {
		return errors.New("agent reference field must be a sequence")
	}
	keep := make([]int, 0, len(list.Content))
	for i, item := range list.Content {
		match := false
		switch item.Kind {
		case yamlScalarNode:
			match = item.Value == key
		case yamlMappingNode:
			ref, ok, _ := scalarMappingValue(item, "agent")
			match = ok && ref == key
		}
		if !match {
			keep = append(keep, i)
		}
	}
	if len(keep) == len(list.Content) {
		return nil
	}
	if len(list.Content) > 0 && len(keep) == 0 {
		return errors.New("removal would empty an agent reference list")
	}
	seq := list.node.(*ast.SequenceNode)
	nodes := make([]ast.Node, 0, len(keep))
	wrappers := make([]*yamlEditNode, 0, len(keep))
	for _, i := range keep {
		nodes = append(nodes, seq.Values[i])
		wrappers = append(wrappers, list.Content[i])
	}
	seq.Values = nodes
	list.Content = wrappers
	return nil
}

func removeMappingEntry(mapping *yamlEditNode, key string) {
	astMap := mapping.node.(*ast.MappingNode)
	for i, entry := range astMap.Values {
		if k, ok := entry.Key.(*ast.StringNode); ok && k.Value == key {
			astMap.Values = append(astMap.Values[:i], astMap.Values[i+1:]...)
			start := i * 2
			mapping.Content = append(mapping.Content[:start], mapping.Content[start+2:]...)
			return
		}
	}
}

func (e *Editor) Source() *EditableSource   { return e.source }
func (e *Editor) CurrentRevision() Revision { return e.source.CurrentRevision() }

// Inspect returns a display-safe view of an immutable source revision.
func (e *Editor) Inspect(ctx context.Context, revision Revision, actor ActorRef) (Inspection, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.validateActor(actor); err != nil {
		return Inspection{}, err
	}
	source, err := e.source.SourceForLoad(revision)
	if err != nil {
		return Inspection{}, err
	}
	data, err := source.Read(ctx)
	if err != nil {
		return Inspection{}, err
	}
	root, err := parseRoot(data)
	if err != nil {
		return Inspection{}, err
	}
	if e.mode != EditAuthorityFull {
		if err := e.actorPresent(root, actor); err != nil {
			return Inspection{}, err
		}
	}
	views, err := inspectAgents(root)
	if err != nil {
		return Inspection{}, err
	}
	visible := views[:0]
	for _, view := range views {
		stable := stableIDForCurrentKey(root, view.Key)
		if e.mode == EditAuthorityFull || e.closure[actor.AgentKey][e.sourceClass[stable]] {
			visible = append(visible, view)
		}
	}
	return Inspection{Revision: revision, Agents: visible}, nil
}

// EditOperation identifies the authored operation committed by Apply.
type EditOperation string

const (
	EditOperationUpdate  EditOperation = "update"
	EditOperationRename  EditOperation = "rename"
	EditOperationClone   EditOperation = "clone"
	EditOperationRemove  EditOperation = "remove"
	EditOperationReplace EditOperation = "replace"
)

// EditResult is an integration receipt for constructing a derived session team.
// CapabilityClassID identifies the immutable baseline security class, not a
// permission grant. BaselineRevisionID never changes across editor commits.
type EditResult struct {
	Operation           EditOperation
	TeamID              string
	BaselineRevisionID  string
	BaseRevision        Revision
	Revision            Revision
	ActorStableID       string
	AgentStableID       string
	AgentKey            string
	CapabilityClassID   string
	SourceAgentStableID string `json:"source_agent_stable_id,omitempty"`
	SourceAgentKey      string `json:"source_agent_key,omitempty"`
	DryRun              bool   `json:"dry_run,omitempty"`
}

func buildEditResult(e *Editor, base, revision Revision, actor ActorRef, patch AgentPatch, targetStableID, targetClass string) EditResult {
	operation := EditOperationUpdate
	switch {
	case patch.RenameTo != "":
		operation = EditOperationRename
	case patch.CloneFrom != "":
		operation = EditOperationClone
	case patch.Remove:
		operation = EditOperationRemove
	}
	result := EditResult{Operation: operation, TeamID: e.teamID, BaselineRevisionID: e.baselineID, BaseRevision: base, Revision: revision, ActorStableID: actor.AgentKey}
	if patch.CloneFrom != "" {
		result.AgentKey = patch.NewAgentKey
		result.AgentStableID = patch.NewAgentKey
		result.SourceAgentKey = patch.CloneFrom
		result.SourceAgentStableID = targetStableID
		result.CapabilityClassID = targetClass
	} else {
		result.AgentKey = patch.AgentKey
		result.AgentStableID = targetStableID
		result.CapabilityClassID = targetClass
		if patch.RenameTo != "" {
			result.AgentKey = patch.RenameTo
		}
	}
	return result
}

// Patch is a compatibility wrapper around Apply returning the new CAS revision.
// Use Apply when a derived-runtime builder needs the immutable operation/class
// receipt.
func (e *Editor) Patch(ctx context.Context, base Revision, actor ActorRef, patch AgentPatch) (Revision, error) {
	result, err := e.Apply(ctx, base, actor, patch)
	return result.Revision, err
}

func (e *Editor) Apply(ctx context.Context, base Revision, actor ActorRef, patch AgentPatch) (EditResult, error) {
	return e.ApplyCoordinated(ctx, base, actor, patch, nil)
}

// PublishCoordinator atomically couples the prepared authored-source head with
// the runtime's prepared environment head. It may call publishSource exactly
// once, and only after all its CAS checks pass; returning nil without calling it
// is rejected. The callback is a non-I/O in-memory CAS commit.
type PublishCoordinator func(EditResult, config.Source, func() error) error

func (e *Editor) ApplyCoordinated(ctx context.Context, base Revision, actor ActorRef, patch AgentPatch, coordinator PublishCoordinator) (EditResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.mode != EditAuthorityRestricted {
		return EditResult{}, errors.New("narrow patch is unavailable outside restricted edit authority")
	}
	if err := e.validateActor(actor); err != nil {
		return EditResult{}, err
	}
	if patch.AgentKey == "" && patch.CloneFrom == "" {
		return EditResult{}, errors.New("agent key is required")
	}
	if patch.AgentKey != "" && patch.CloneFrom != "" {
		return EditResult{}, errors.New("clone source uses CloneFrom; AgentKey must be empty")
	}

	if patch.Remove && patch.CloneFrom != "" {
		return EditResult{}, errors.New("clone and remove cannot be combined")
	}
	if patch.Remove && (patch.RenameTo != "" || patch.Instruction != nil || patch.Description != nil || patch.WelcomeMessage != nil || patch.Model != nil || patch.FallbackModels != nil || patch.ModelPickerModels != nil) {
		return EditResult{}, errors.New("remove cannot be combined with other edits")
	}
	if patch.CloneFrom != "" && (patch.RenameTo != "" || patch.Remove || patch.Instruction != nil || patch.Description != nil || patch.WelcomeMessage != nil || patch.Model != nil || patch.FallbackModels != nil || patch.ModelPickerModels != nil) {
		return EditResult{}, errors.New("clone must be an exact copy operation")
	}
	if patch.CloneFrom == "" && patch.NewAgentKey != "" {
		return EditResult{}, errors.New("new agent key is only valid with clone")
	}
	if e.mode == EditAuthorityRestricted && patch.RenameTo == "root" {
		return EditResult{}, errors.New("the default/root key is reserved")
	}
	if patch.RenameTo != "" && patch.AgentKey == e.rootKey() {
		return EditResult{}, errors.New("the default/root agent cannot be renamed")
	}
	current := e.source.CurrentRevision()
	if current.ID != base.ID || current.Generation != base.Generation {
		return EditResult{}, errors.New("editable source revision conflict")
	}
	baseSource, err := e.source.SourceForLoad(base)
	if err != nil {
		return EditResult{}, err
	}
	data, err := baseSource.Read(ctx)
	if err != nil {
		return EditResult{}, err
	}
	root, err := parseRoot(data)
	if err != nil {
		return EditResult{}, err
	}
	currentKeys, err := stableAgentKeys(root)
	if err != nil {
		return EditResult{}, err
	}
	if err := validateAgentRoster(currentKeys, e.sourceClass); err != nil {
		return EditResult{}, err
	}
	if patch.RenameTo != "" {
		agents, _ := mappingValue(root, "agents")
		for i := 1; i < len(agents.Content); i += 2 {
			if hasExternalEdges(agents.Content[i]) {
				return EditResult{}, errors.New("rename refuses a team with external agent references")
			}
		}
	}
	if err := e.actorPresent(root, actor); err != nil {
		return EditResult{}, err
	}
	targetStableID := stableIDForCurrentKey(root, patch.AgentKey)
	targetClass := e.sourceClass[targetStableID]
	if patch.CloneFrom != "" {
		if patch.NewAgentKey == "" {
			return EditResult{}, errors.New("new agent key is required for clone")
		}
		targetStableID = stableIDForCurrentKey(root, patch.CloneFrom)
		targetClass = e.sourceClass[targetStableID]
	} else if patch.NewAgentKey != "" {
		return EditResult{}, errors.New("new agent key is only valid with clone")
	}
	if targetStableID == "" || targetClass == "" || !e.closure[actor.AgentKey][targetClass] {
		return EditResult{}, errors.New("actor is not authorized to edit the target capability class in its pinned baseline delegation closure")
	}
	if patch.Remove && targetStableID == actor.AgentKey {
		return EditResult{}, errors.New("actor cannot remove itself")
	}
	if patch.Remove && targetStableID == e.rootStableID {
		return EditResult{}, errors.New("default/root agent cannot be removed")
	}
	if patch.RenameTo != "" && targetStableID == e.rootStableID {
		return EditResult{}, errors.New("default/root agent cannot be renamed")
	}
	if patch.CloneFrom != "" && (hasAgentKey(root, patch.NewAgentKey) || e.reservedIDs[patch.NewAgentKey] || (e.mode == EditAuthorityRestricted && patch.NewAgentKey == "root")) {
		return EditResult{}, errors.New("clone target key is already in use or reserved")
	}
	if e.cacheEnabled[targetClass] && (patch.Instruction != nil || patch.Description != nil || patch.WelcomeMessage != nil || patch.Model != nil || patch.FallbackModels != nil || patch.ModelPickerModels != nil) {
		return EditResult{}, errors.New("prompt and model edits are disabled for cache-enabled baseline agents")
	}
	if patch.RenameTo == "" && patch.CloneFrom == "" && !patch.Remove && patch.Instruction == nil && patch.Description == nil && patch.WelcomeMessage == nil && patch.Model == nil && patch.FallbackModels == nil && patch.ModelPickerModels == nil {
		return EditResult{}, errors.New("patch has no changes")
	}
	if patch.Model != nil && !e.modelRefs[targetClass][*patch.Model] {
		return EditResult{}, errors.New("model edit is limited to model references present in the baseline agent model/fallback/model-picker set")
	}
	if patch.FallbackModels != nil {
		for _, ref := range *patch.FallbackModels {
			if !e.modelRefs[targetClass][ref] {
				return EditResult{}, errors.New("fallback model is outside the target agent's baseline model/fallback/model-picker set")
			}
		}
	}
	if patch.ModelPickerModels != nil {
		for _, ref := range *patch.ModelPickerModels {
			if !e.modelRefs[targetClass][ref] {
				return EditResult{}, errors.New("model-picker reference is outside the target agent's baseline model/fallback/model-picker set")
			}
		}
	}
	baselineGraph, err := localReferenceGraph(root, currentKeys)
	if err != nil {
		return EditResult{}, err
	}
	if !sameGraph(baselineGraph, e.expectedGraph) {
		return EditResult{}, errors.New("source delegation graph no longer matches editor baseline")
	}
	if err := validateCapabilityProfiles(root, currentKeys, e.sourceClass, e.profiles); err != nil {
		return EditResult{}, err
	}
	classes := cloneStringMap(e.sourceClass)
	expectedGraph := cloneGraph(e.expectedGraph)
	reserved := cloneBoolMap(e.reservedIDs)
	if patch.CloneFrom != "" {
		for i := 1; i < len(mappingValueMust(root, "agents").Content); i += 2 {
			if hasExternalEdges(mappingValueMust(root, "agents").Content[i]) {
				return EditResult{}, errors.New("teams with external agent references cannot be cloned")
			}
		}
		if err := cloneAgent(root, patch.CloneFrom, patch.NewAgentKey); err != nil {
			return EditResult{}, err
		}
		newStableID := patch.NewAgentKey
		classes[newStableID] = targetClass
		reserved[newStableID] = true
		cloneEdges := cloneBoolMap(expectedGraph[targetStableID])
		expectedGraph[newStableID] = cloneEdges
	} else if patch.Remove {
		var err error
		expectedGraph, err = removeAgent(root, patch.AgentKey, targetStableID, expectedGraph)
		if err != nil {
			return EditResult{}, err
		}
		delete(classes, targetStableID)
	}
	if patch.RenameTo != "" {
		if targetStableID == e.rootStableID {
			return EditResult{}, errors.New("the default/root agent cannot be renamed")
		}
		if err := renameAgent(root, patch.AgentKey, patch.RenameTo); err != nil {
			return EditResult{}, err
		}
	}
	target := patch.AgentKey
	if patch.RenameTo != "" {
		target = patch.RenameTo
	}
	if patch.CloneFrom != "" || patch.Remove {
		target = ""
	}
	var agent *yamlEditNode
	if target != "" {
		agent, err = findAgent(root, target)
		if err != nil {
			return EditResult{}, err
		}
	}
	if patch.Instruction != nil {
		setMappingString(agent, "instruction", *patch.Instruction)
	}
	if patch.Description != nil {
		setMappingString(agent, "description", *patch.Description)
	}
	if patch.WelcomeMessage != nil {
		setMappingString(agent, "welcome_message", *patch.WelcomeMessage)
	}
	if patch.Model != nil {
		setMappingString(agent, "model", *patch.Model)
	}
	if patch.FallbackModels != nil {
		if err := setFallbackModels(agent, *patch.FallbackModels); err != nil {
			return EditResult{}, err
		}
	}
	if patch.ModelPickerModels != nil {
		if err := setPickerModels(agent, *patch.ModelPickerModels); err != nil {
			return EditResult{}, err
		}
	}
	candidate, err := encodeYAMLNode(root)
	if err != nil {
		return EditResult{}, fmt.Errorf("encoding edited YAML: %w", err)
	}
	if err := validateEditableDocument(candidate); err != nil {
		return EditResult{}, err
	}
	candidateSource := inMemorySource{name: baseSource.Name(), parentDir: baseSource.ParentDir(), data: candidate}
	candidateRoot, err := parseRoot(candidate)
	if err != nil {
		return EditResult{}, err
	}
	candidateKeys, err := stableAgentKeys(candidateRoot)
	if err != nil {
		return EditResult{}, err
	}
	if err := validateAgentRoster(candidateKeys, classes); err != nil {
		return EditResult{}, err
	}
	candidateGraph, err := localReferenceGraph(candidateRoot, candidateKeys)
	if err != nil {
		return EditResult{}, err
	}
	if !sameGraph(candidateGraph, expectedGraph) {
		return EditResult{}, errors.New("edit changed the pinned baseline delegation graph")
	}
	if err := validateCapabilityProfiles(candidateRoot, candidateKeys, classes, e.profiles); err != nil {
		return EditResult{}, err
	}
	if err := validateClassReachability(candidateGraph, classes, e.closure, e.sourceClass); err != nil {
		return EditResult{}, err
	}
	if _, err := config.Load(ctx, candidateSource); err != nil {
		return EditResult{}, errors.New("edited config failed validation")
	}
	if err := e.build(ctx, candidateSource); err != nil {
		return EditResult{}, errors.New("edited team build failed validation")
	}
	prepared, err := e.source.prepare(base, candidate)
	if err != nil {
		return EditResult{}, err
	}
	result := buildEditResult(e, base, prepared, actor, patch, targetStableID, targetClass)
	committed := false
	commitSource := func() error {
		if committed {
			return errors.New("prepared source may only be committed once")
		}
		if err := e.source.publishPrepared(base, prepared); err != nil {
			return err
		}
		committed = true
		return nil
	}
	if coordinator != nil {
		if err := coordinator(result, candidateSource, commitSource); err != nil {
			if committed {
				return EditResult{}, errors.New("coordinator failed after publishing the source head")
			}
			e.source.discardPrepared(prepared)
			return EditResult{}, err
		}
		if !committed {
			e.source.discardPrepared(prepared)
			return EditResult{}, errors.New("coordinator returned without publishing the source head")
		}
	} else if err := commitSource(); err != nil {
		e.source.discardPrepared(prepared)
		return EditResult{}, err
	}
	e.sourceClass, e.expectedGraph, e.reservedIDs = classes, expectedGraph, reserved
	return result, nil
}

// Replace replaces the complete authored root YAML only when trusted construction
// selected full-edit authority. It does not inherit or modify that authority.
// DryRun validates the prospective document without committing a revision.
func (e *Editor) Replace(ctx context.Context, base Revision, actor ActorRef, authoredYAML []byte, dryRun bool) (EditResult, error) {
	return e.ReplaceCoordinated(ctx, base, actor, authoredYAML, dryRun, nil)
}

func (e *Editor) ReplaceCoordinated(ctx context.Context, base Revision, actor ActorRef, authoredYAML []byte, dryRun bool, coordinator PublishCoordinator) (EditResult, error) {
	if e.mode != EditAuthorityFull {
		return EditResult{}, errors.New("whole-document replacement requires trusted full-edit authority")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.validateActor(actor); err != nil {
		return EditResult{}, err
	}
	if current := e.source.CurrentRevision(); current.ID != base.ID || current.Generation != base.Generation {
		return EditResult{}, errors.New("editable source revision conflict")
	}
	if err := validateFullEditableDocument(authoredYAML); err != nil {
		return EditResult{}, err
	}
	candidate := bytes.Clone(authoredYAML)
	source, err := e.source.SourceForLoad(base)
	if err != nil {
		return EditResult{}, err
	}
	candidateSource := inMemorySource{name: source.Name(), parentDir: source.ParentDir(), data: candidate, encryptedConfig: e.source.encryptedConfig}
	if _, err := config.Load(ctx, candidateSource); err != nil {
		return EditResult{}, errors.New("replacement config failed validation")
	}
	if err := e.build(ctx, candidateSource); err != nil {
		return EditResult{}, errors.New("replacement team build failed validation")
	}
	if dryRun {
		return EditResult{Operation: EditOperationReplace, TeamID: e.teamID, BaselineRevisionID: e.baselineID, BaseRevision: base, Revision: base, ActorStableID: actor.AgentKey, DryRun: true}, nil
	}
	prepared, err := e.source.prepare(base, candidate)
	if err != nil {
		return EditResult{}, err
	}
	result := EditResult{Operation: EditOperationReplace, TeamID: e.teamID, BaselineRevisionID: e.baselineID, BaseRevision: base, Revision: prepared, ActorStableID: actor.AgentKey}
	committed := false
	commitSource := func() error {
		if committed {
			return errors.New("prepared source may only be committed once")
		}
		if err := e.source.publishPrepared(base, prepared); err != nil {
			return err
		}
		committed = true
		return nil
	}
	if coordinator != nil {
		if err := coordinator(result, candidateSource, commitSource); err != nil {
			if committed {
				return EditResult{}, errors.New("coordinator failed after publishing the source head")
			}
			e.source.discardPrepared(prepared)
			return EditResult{}, err
		}
		if !committed {
			e.source.discardPrepared(prepared)
			return EditResult{}, errors.New("coordinator returned without publishing the source head")
		}
	} else if err := commitSource(); err != nil {
		e.source.discardPrepared(prepared)
		return EditResult{}, err
	}
	return result, nil
}

func validateFullEditableDocument(data []byte) error {
	file, err := parser.ParseBytes(data, parser.ParseComments)
	if err != nil || len(file.Docs) != 1 || file.Docs[0].Body == nil {
		return errors.New("replacement must be one YAML document")
	}
	root, ok := file.Docs[0].Body.(*ast.MappingNode)
	if !ok {
		return errors.New("replacement must be a root YAML mapping")
	}
	for _, entry := range root.Values {
		key, ok := entry.Key.(*ast.StringNode)
		if !ok || key.Value != "version" {
			continue
		}
		version, ok := entry.Value.(*ast.StringNode)
		if !ok || version.Value != latest.Version {
			return errors.New("full replacement requires the latest supported config version")
		}
	}
	return nil
}

// Validate reruns ordinary config validation and the injected full build
// validator for a revision. Actor identity must match the pinned source epoch.
func (e *Editor) Validate(ctx context.Context, revision Revision, actor ActorRef) (Validation, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.validateActor(actor); err != nil {
		return Validation{}, err
	}
	source, err := e.source.SourceForLoad(revision)
	if err != nil {
		return Validation{}, err
	}
	data, err := source.Read(ctx)
	if err != nil {
		return Validation{}, errors.New("editable source revision could not be read")
	}
	root, err := parseRoot(data)
	if err != nil {
		return Validation{}, errors.New("editable source revision is invalid")
	}
	if e.mode != EditAuthorityFull {
		if err := e.actorPresent(root, actor); err != nil {
			return Validation{}, err
		}
	}
	if _, err := config.Load(ctx, source); err != nil {
		return Validation{Revision: revision}, errors.New("editable config failed validation")
	}
	if err := e.build(ctx, source); err != nil {
		return Validation{Revision: revision}, errors.New("editable team build failed validation")
	}
	return Validation{Revision: revision, BuildChecked: true}, nil
}

func parseRoot(data []byte) (*yamlEditNode, error) {
	var doc yamlDocument
	if err := decodeYAMLNode(data, &doc); err != nil {
		return nil, errors.New("editable YAML could not be decoded")
	}
	return doc.Root, nil
}

func inspectAgents(root *yamlEditNode) ([]AgentView, error) {
	agents, _ := mappingValue(root, "agents")
	views := make([]AgentView, 0, len(agents.Content)/2)
	for i := 0; i+1 < len(agents.Content); i += 2 {
		key, node := agents.Content[i].Value, agents.Content[i+1]
		view := AgentView{Key: key, ID: key}
		if id, ok, err := scalarMappingValue(node, "id"); err != nil {
			return nil, err
		} else if ok && id != "" {
			view.ID = id
		}
		for field, dest := range map[string]*string{"description": &view.Description, "welcome_message": &view.WelcomeMessage, "instruction": &view.Instruction, "model": &view.Model} {
			value, present, err := scalarMappingValue(node, field)
			if field == "instruction" && present && err != nil {
				if instruction, ok := mappingValue(node, field); ok && instruction.Kind == yamlSequenceNode {
					parts := make([]string, 0, len(instruction.Content))
					for _, part := range instruction.Content {
						if part.Kind != yamlScalarNode {
							return nil, errors.New("instruction parts must be strings")
						}
						parts = append(parts, part.Value)
					}
					value, err = strings.Join(parts, "\n\n"), nil
				}
			}
			if err != nil {
				return nil, err
			}
			if present {
				*dest = value
			}
		}
		view.Model = safeDisplayModel(view.Model)
		if fallback, ok := mappingValue(node, "fallback"); ok {
			if models, ok := mappingValue(fallback, "models"); ok {
				for _, item := range models.Content {
					if item.Kind != yamlScalarNode {
						return nil, errors.New("fallback models must be strings")
					}
					view.FallbackModels = append(view.FallbackModels, safeDisplayModel(item.Value))
				}
			}
		}
		views = append(views, view)
	}
	return views, nil
}

func safeDisplayModel(raw string) string {
	if !strings.Contains(raw, "://") {
		return raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "[redacted-url]"
	}
	if u.User != nil {
		u.User = url.User("redacted")
	}
	if u.RawQuery != "" {
		u.RawQuery = "redacted"
	}
	if u.Fragment != "" {
		u.Fragment = "redacted"
	}
	return u.String()
}

func findAgent(root *yamlEditNode, key string) (*yamlEditNode, error) {
	agents, _ := mappingValue(root, "agents")
	for i := 0; i+1 < len(agents.Content); i += 2 {
		if agents.Content[i].Value == key {
			return agents.Content[i+1], nil
		}
	}
	return nil, fmt.Errorf("agent %q does not exist", key)
}

func renameAgent(root *yamlEditNode, from, to string) error {
	if strings.TrimSpace(to) == "" || from == to {
		return errors.New("rename target must be a different non-empty agent key")
	}
	agents, _ := mappingValue(root, "agents")
	var keyNode, agent *yamlEditNode
	for i := 0; i+1 < len(agents.Content); i += 2 {
		if agents.Content[i].Value == to {
			return fmt.Errorf("agent key %q already exists", to)
		}
		if agents.Content[i].Value == from {
			keyNode, agent = agents.Content[i], agents.Content[i+1]
		}
	}
	if keyNode == nil {
		return fmt.Errorf("agent %q does not exist", from)
	}
	if id, ok, err := scalarMappingValue(agent, "id"); err != nil {
		return err
	} else if !ok || id == "" {
		setMappingString(agent, "id", from)
	}
	for i := 1; i < len(agents.Content); i += 2 {
		if hasExternalEdges(agents.Content[i]) {
			return errors.New("rename refuses a team with external agent references")
		}
		if err := rewriteReferences(agents.Content[i], from, to); err != nil {
			return err
		}
	}
	keyNode.SetString(to)
	return nil
}

func hasExternalEdges(agent *yamlEditNode) bool {
	for _, field := range []string{"sub_agents", "handoffs"} {
		if seq, ok := mappingValue(agent, field); ok && seq.Kind == yamlSequenceNode {
			for _, item := range seq.Content {
				if item.Kind == yamlScalarNode && config.IsExternalReference(item.Value) {
					return true
				}
			}
		}
	}
	if node, ok := mappingValue(agent, "force_handoff"); ok && node.Kind == yamlScalarNode && config.IsExternalReference(node.Value) {
		return true
	}
	if seq, ok := mappingValue(agent, "subagents"); ok && seq.Kind == yamlSequenceNode {
		for _, item := range seq.Content {
			if item.Kind == yamlScalarNode && config.IsExternalReference(item.Value) {
				return true
			}
			if item.Kind == yamlMappingNode {
				if ref, ok, _ := scalarMappingValue(item, "agent"); ok && config.IsExternalReference(ref) {
					return true
				}
			}
		}
	}
	return false
}

func rewriteReferences(agent *yamlEditNode, from, to string) error {
	for _, field := range []string{"sub_agents", "handoffs"} {
		if node, ok := mappingValue(agent, field); ok {
			if err := rewriteSequence(node, from, to); err != nil {
				return fmt.Errorf("%s: %w", field, err)
			}
		}
	}
	if node, ok := mappingValue(agent, "force_handoff"); ok {
		if node.Kind != yamlScalarNode {
			return errors.New("force_handoff must be a scalar")
		}
		if node.Value == from {
			node.SetString(to)
		}
	}
	if node, ok := mappingValue(agent, "subagents"); ok {
		if node.Kind != yamlSequenceNode {
			return errors.New("subagents must be a sequence")
		}
		for _, item := range node.Content {
			if item.Kind == yamlScalarNode {
				if item.Value == from {
					item.SetString(to)
				}
				continue
			}
			if item.Kind != yamlMappingNode {
				return errors.New("unsupported subagents entry")
			}
			ref, ok, err := scalarMappingValue(item, "agent")
			if err != nil {
				return err
			}
			if !ok {
				return errors.New("subagents object has no agent key")
			}
			if ref == from {
				setMappingString(item, "agent", to)
			}
		}
	}
	return nil
}

func rewriteSequence(node *yamlEditNode, from, to string) error {
	if node.Kind != yamlSequenceNode {
		return errors.New("agent references must be a sequence")
	}
	for _, item := range node.Content {
		if item.Kind != yamlScalarNode {
			return errors.New("agent references must be strings")
		}
		if item.Value == from {
			item.SetString(to)
		}
	}
	return nil
}

func setFallbackModels(agent *yamlEditNode, refs []string) error {
	fallback, ok := mappingValue(agent, "fallback")
	if !ok {
		return errors.New("adding fallback block is unsupported; provide an existing fallback mapping")
	}
	if fallback.Kind != yamlMappingNode {
		return errors.New("fallback must be a mapping")
	}
	seq := stringSequence(refs)
	if existing, ok := mappingValue(fallback, "models"); ok {
		replaceNode(existing, seq)
	} else {
		if err := appendMapping(fallback, "models", seq.node); err != nil {
			return err
		}
	}
	return nil
}

func setPickerModels(agent *yamlEditNode, refs []string) error {
	toolsets, ok := mappingValue(agent, "toolsets")
	if !ok || toolsets.Kind != yamlSequenceNode {
		return errors.New("model-picker edit requires direct agent toolsets sequence")
	}
	found := false
	for _, toolset := range toolsets.Content {
		if typ, ok, _ := scalarMappingValue(toolset, "type"); ok && typ == "model_picker" {
			seq := stringSequence(refs)
			if existing, ok := mappingValue(toolset, "models"); ok {
				replaceNode(existing, seq)
			} else {
				if err := appendMapping(toolset, "models", seq.node); err != nil {
					return err
				}
			}
			found = true
		}
	}
	if !found {
		return errors.New("model-picker edit requires an existing model_picker toolset")
	}
	return nil
}

func stringSequence(values []string) *yamlEditNode {
	encoded, err := yaml.Marshal(map[string][]string{"value": values})
	if err != nil {
		return nil
	}
	doc, err := parser.ParseBytes(encoded, parser.ParseComments)
	if err != nil || len(doc.Docs) != 1 {
		return nil
	}
	root := doc.Docs[0].Body.(*ast.MappingNode)
	node := root.Values[0].Value
	wrapped, err := wrapYAMLNode(node, nil)
	if err != nil {
		return nil
	}
	return wrapped
}

func (s *EditableSource) commit(base Revision, data []byte) (Revision, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.current.ID != base.ID || s.current.Generation != base.Generation {
		return Revision{}, errors.New("editable source revision conflict")
	}
	generation := s.current.Generation + 1
	revision, err := revisionFor(generation)
	if err != nil {
		return Revision{}, err
	}
	s.revisions[revision.ID] = slices.Clone(data)
	s.current = revision
	if len(s.revisions) > maxEditableRevisions {
		evictOldEditableRevisions(s.revisions, revision.ID)
	}
	return revision, nil
}
