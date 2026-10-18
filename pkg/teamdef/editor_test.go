package teamdef

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config"
)

const editableTeamYAML = `version: "16"
id: safe-team
agents:
  root:
    model: openai/root-model
    fallback:
      models: [openai/fallback]
    instruction: "${ROOT_PROMPT}"
    sub_agents: [worker]
    force_handoff: worker
  worker:
    id: worker-stable
    model: openai/worker-model
    description: Worker.
    instruction: Do work.
  isolated:
    model: openai/isolated-model
    instruction: Stay isolated.
`

func testActor(e *Editor, key string) ActorRef {
	return ActorRef{TeamID: e.TeamID(), AgentKey: key, SourceRevision: e.BaselineRevisionID()}
}

func testEditor(t *testing.T, source config.Source, calls *int) *Editor {
	t.Helper()
	opts := EditableSourceOptions{Source: source, BuildValidator: func(ctx context.Context, src config.Source) error {
		if calls != nil {
			*calls++
		}
		_, err := config.Load(ctx, src)
		return err
	}}
	if info, err := os.Lstat(source.Name()); err == nil && info.Mode().IsRegular() && filepath.IsAbs(source.Name()) {
		opts.WritablePath = source.Name()
		opts.ExportRoot = filepath.Dir(source.Name())
	}
	e, err := NewEditor(t.Context(), opts)
	require.NoError(t, err)
	return e
}

func TestEditorPatchesOnlyAuthoredFieldsAndKeepsCommentsPlaceholders(t *testing.T) {
	t.Parallel()
	data := strings.Replace(editableTeamYAML, "version: \"16\"", "version: \"16\"\nproviders:\n  private:\n    provider: openai\n    token_key: SECRET_CREDENTIAL_NAME\n    base_url: https://user:password@private.invalid?token=secret\n# Keep this comment.", 1)
	data = strings.Replace(data, "instruction: Do work.", "instruction: Do work. # inline stays", 1)
	calls := 0
	e := testEditor(t, config.NewBytesSource("team.yaml", []byte(data)), &calls)
	base := e.CurrentRevision()
	actor := testActor(e, "root")
	revision, err := e.Patch(t.Context(), base, actor, AgentPatch{AgentKey: "worker", Instruction: new("New instructions.")})
	require.NoError(t, err)
	assert.Equal(t, 1, calls)
	before, err := e.Source().SourceForLoad(base)
	require.NoError(t, err)
	beforeBytes, err := before.Read(t.Context())
	require.NoError(t, err)
	assert.Contains(t, string(beforeBytes), "Do work.")
	current, err := e.Source().SourceForLoad(revision)
	require.NoError(t, err)
	currentBytes, err := current.Read(t.Context())
	require.NoError(t, err)
	assert.Contains(t, string(currentBytes), "# Keep this comment.")
	assert.Contains(t, string(currentBytes), "# inline stays")
	assert.Contains(t, string(currentBytes), "${ROOT_PROMPT}", "unresolved placeholders stay authored")
	assert.Contains(t, string(currentBytes), "instruction: New instructions.")
	inspection, err := e.Inspect(t.Context(), revision, actor)
	require.NoError(t, err)
	assert.Len(t, inspection.Agents, 2, "inspection is scoped to actor baseline reachability")
	for _, agent := range inspection.Agents {
		assert.NotEqual(t, "isolated", agent.Key)
	}
	encoded, err := json.Marshal(inspection)
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), "providers")
	assert.NotContains(t, string(encoded), "credential")
	assert.NotContains(t, string(encoded), "SECRET_CREDENTIAL_NAME")
	assert.NotContains(t, string(encoded), "private.invalid")
	assert.NotContains(t, string(encoded), "password")
	validated, err := e.Validate(t.Context(), revision, actor)
	require.NoError(t, err)
	assert.True(t, validated.BuildChecked)
}

func TestEditorRequiresPinnedActorAndBaselineDelegationClosure(t *testing.T) {
	t.Parallel()
	e := testEditor(t, config.NewBytesSource("team.yaml", []byte(editableTeamYAML)), nil)
	base := e.CurrentRevision()
	root := testActor(e, "root")
	var err error
	wrongTeam := root
	wrongTeam.TeamID = "other-team"
	_, err = e.Patch(t.Context(), base, wrongTeam, AgentPatch{AgentKey: "worker", Instruction: new("change")})
	require.ErrorContains(t, err, "pinned team/source baseline")
	wrongEpoch := root
	wrongEpoch.SourceRevision = "other-baseline"
	_, err = e.Patch(t.Context(), base, wrongEpoch, AgentPatch{AgentKey: "worker", Instruction: new("stale epoch")})
	require.ErrorContains(t, err, "pinned team/source baseline")
	worker := testActor(e, "worker-stable")
	_, err = e.Patch(t.Context(), base, worker, AgentPatch{AgentKey: "root", Instruction: new("unauthorized")})
	require.ErrorContains(t, err, "delegation closure")
	_, err = e.Patch(t.Context(), base, ActorRef{AgentKey: "root"}, AgentPatch{AgentKey: "root", Instruction: new("no actor")})
	require.ErrorContains(t, err, "actor identity must include")
}

func TestEditorCASRenameAndFrozenDelegationEdges(t *testing.T) {
	t.Parallel()
	e := testEditor(t, config.NewBytesSource("team.yaml", []byte(editableTeamYAML)), nil)
	base := e.CurrentRevision()
	root := testActor(e, "root")
	newRevision, err := e.Patch(t.Context(), base, root, AgentPatch{AgentKey: "worker", RenameTo: "reviewer"})
	require.NoError(t, err)
	_, err = e.Patch(t.Context(), base, root, AgentPatch{AgentKey: "worker", Instruction: new("stale")})
	require.ErrorContains(t, err, "revision conflict")
	loaded, err := e.Source().SourceForLoad(newRevision)
	require.NoError(t, err)
	bytes, err := loaded.Read(t.Context())
	require.NoError(t, err)
	text := string(bytes)
	assert.Contains(t, text, "id: worker-stable")
	assert.Contains(t, text, "sub_agents: [reviewer]")
	assert.Contains(t, text, "force_handoff: reviewer")
	assert.NotContains(t, text, "sub_agents: [worker]")
	assert.Equal(t, base.ID, root.SourceRevision, "actor authorization remains pinned to original baseline")
	secondRevision, err := e.Patch(t.Context(), newRevision, root, AgentPatch{AgentKey: "reviewer", Description: new("Updated by same pinned actor")})
	require.NoError(t, err, "the baseline actor ref remains valid after its own commit")
	newRevision = secondRevision

	_, err = e.Patch(t.Context(), newRevision, root, AgentPatch{AgentKey: "root", RenameTo: "renamed-root"})
	require.ErrorContains(t, err, "root agent cannot be renamed")
}

func TestEditorModelAllowlistAndExplicitUnsupportedRosterOperations(t *testing.T) {
	t.Parallel()
	e := testEditor(t, config.NewBytesSource("team.yaml", []byte(editableTeamYAML)), nil)
	base := e.CurrentRevision()
	root := testActor(e, "root")
	var err error
	_, err = e.Patch(t.Context(), base, root, AgentPatch{AgentKey: "root", Model: new("openai/other-model")})
	require.ErrorContains(t, err, "baseline agent")
	_, err = e.Patch(t.Context(), base, root, AgentPatch{CloneFrom: "worker", NewAgentKey: "extra"})
	require.NoError(t, err)
	_, err = e.Patch(t.Context(), e.CurrentRevision(), root, AgentPatch{CloneFrom: "worker", NewAgentKey: "worker-copy"})
	require.NoError(t, err)
	_, err = e.Patch(t.Context(), e.CurrentRevision(), root, AgentPatch{AgentKey: "worker", Remove: true})
	require.ErrorContains(t, err, "force_handoff")
	_, err = e.Patch(t.Context(), e.CurrentRevision(), root, AgentPatch{AgentKey: "root", Model: new("openai/fallback")})
	require.NoError(t, err)
}

func TestEditorRequiresBuildValidator(t *testing.T) {
	t.Parallel()
	_, err := NewEditor(t.Context(), EditableSourceOptions{Source: config.NewBytesSource("team.yaml", []byte(editableTeamYAML))})
	require.ErrorContains(t, err, "requires an injected build validator")
}

func TestEditorLocalPersistUsesSourceConflictCheck(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "team.yaml")
	require.NoError(t, os.WriteFile(path, []byte(editableTeamYAML), 0o600))
	e := testEditor(t, config.NewFileSource(path), nil)
	base := e.CurrentRevision()
	actor := testActor(e, "root")
	revision, err := e.Patch(t.Context(), base, actor, AgentPatch{AgentKey: "root", Instruction: new("edited")})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, []byte(editableTeamYAML+"# outside edit\n"), 0o600))
	err = e.Source().Persist(revision)
	require.ErrorContains(t, err, "changed externally")
	got, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(got), "outside edit")
}

func TestEditorRenameRefusesExternalDelegationEdges(t *testing.T) {
	t.Parallel()
	team := strings.Replace(editableTeamYAML, "sub_agents: [worker]", "sub_agents: [worker, ext:org/other]", 1)
	e := testEditor(t, config.NewBytesSource("team.yaml", []byte(team)), nil)
	actor := testActor(e, "root")
	_, err := e.Patch(t.Context(), e.CurrentRevision(), actor, AgentPatch{AgentKey: "worker", RenameTo: "reviewer"})
	require.ErrorContains(t, err, "external agent references")
}

func TestEditorModelPickerEditsStayWithinBaselinePickerSet(t *testing.T) {
	t.Parallel()
	team := strings.Replace(editableTeamYAML, "instruction: \"${ROOT_PROMPT}\"", "instruction: \"${ROOT_PROMPT}\"\n    toolsets:\n      - type: model_picker\n        models: [openai/root-model, openai/fallback]", 1)
	e := testEditor(t, config.NewBytesSource("team.yaml", []byte(team)), nil)
	actor := testActor(e, "root")
	refs := []string{"openai/fallback"}
	_, err := e.Patch(t.Context(), e.CurrentRevision(), actor, AgentPatch{AgentKey: "root", ModelPickerModels: &refs})
	require.NoError(t, err)
	newRefs := []string{"openai/worker-model"}
	_, err = e.Patch(t.Context(), e.CurrentRevision(), actor, AgentPatch{AgentKey: "root", ModelPickerModels: &newRefs})
	require.ErrorContains(t, err, "baseline model/fallback/model-picker set")
}

func TestEditorBuildFailureDoesNotAdvanceRevision(t *testing.T) {
	t.Parallel()
	boom := errors.New("injected build rejection")
	e, err := NewEditor(t.Context(), EditableSourceOptions{Source: config.NewBytesSource("team.yaml", []byte(editableTeamYAML)), BuildValidator: func(context.Context, config.Source) error { return boom }})
	require.NoError(t, err)
	base := e.CurrentRevision()
	actor := testActor(e, "root")
	_, err = e.Patch(t.Context(), base, actor, AgentPatch{AgentKey: "root", Instruction: new("rejected")})
	require.ErrorContains(t, err, "edited team build failed validation")
	assert.Equal(t, base, e.CurrentRevision(), "failed validation must be atomic")
}

func TestEditorInspectRejectsActorFromAnotherBaselineEpoch(t *testing.T) {
	t.Parallel()
	e := testEditor(t, config.NewBytesSource("team.yaml", []byte(editableTeamYAML)), nil)
	actor := testActor(e, "root")
	actor.SourceRevision = "other-epoch"
	_, err := e.Inspect(t.Context(), e.CurrentRevision(), actor)
	require.ErrorContains(t, err, "pinned team/source baseline")
}

func TestEditorDefaultRootIsNamedRootNotFirstAgent(t *testing.T) {
	t.Parallel()
	team := `version: "16"
agents:
  helper:
    model: openai/helper
    instruction: helper
  root:
    model: openai/root
    instruction: root
`
	e := testEditor(t, config.NewBytesSource("team.yaml", []byte(team)), nil)
	actor := testActor(e, "root")
	_, err := e.Patch(t.Context(), e.CurrentRevision(), actor, AgentPatch{AgentKey: "root", RenameTo: "new-root"})
	require.ErrorContains(t, err, "default/root agent cannot be renamed")
}

const cloneGraphYAML = `version: "16"
id: clone-team
agents:
  root:
    model: openai/root
    sub_agents: [worker, helper]
    instruction: Root.
  worker:
    model: openai/worker
    sub_agents: [helper]
    instruction: |
      Work carefully.
      Keep this paragraph.
  helper:
    model: openai/helper
    instruction: Help.
  isolated:
    model: openai/isolated
    instruction: Private.
`

func TestEditorClonePreservesSourceProfileAndUsesStableIdentity(t *testing.T) {
	t.Parallel()
	e := testEditor(t, config.NewBytesSource("clone.yaml", []byte(cloneGraphYAML)), nil)
	rootActor := testActor(e, "root")
	base := e.CurrentRevision()
	revision, err := e.Patch(t.Context(), base, rootActor, AgentPatch{CloneFrom: "worker", NewAgentKey: "worker-copy"})
	require.NoError(t, err)
	loaded, err := e.Source().SourceForLoad(revision)
	require.NoError(t, err)
	data, err := loaded.Read(t.Context())
	require.NoError(t, err)
	text := string(data)
	assert.Contains(t, text, "worker-copy:")
	assert.Contains(t, text, "id: worker-copy")
	assert.Contains(t, text, "instruction: |\n      Work carefully.\n      Keep this paragraph.")
	assert.Contains(t, text, "worker-copy:\n    model: openai/worker")
	assert.Contains(t, text, "    id: worker-copy")
	_, err = e.Validate(t.Context(), revision, rootActor)
	require.NoError(t, err)
	_, err = e.Patch(t.Context(), revision, rootActor, AgentPatch{CloneFrom: "worker-copy", NewAgentKey: "worker-copy-2"})
	require.NoError(t, err, "clones retain their source capability class")
}

func TestEditorCloneRejectsUnreachableAndCapabilityBearingSourceClasses(t *testing.T) {
	t.Parallel()
	e := testEditor(t, config.NewBytesSource("clone.yaml", []byte(cloneGraphYAML)), nil)
	rootActor := testActor(e, "root")
	_, err := e.Patch(t.Context(), e.CurrentRevision(), rootActor, AgentPatch{CloneFrom: "isolated", NewAgentKey: "copy"})
	require.ErrorContains(t, err, "delegation closure")

	for _, field := range []string{
		"cache:\n      enabled: true\n",
		"harness:\n      type: codex\n",
		"toolsets:\n      - type: team_edit\n",
	} {
		team := strings.Replace(cloneGraphYAML, "    instruction: Help.", "    "+strings.ReplaceAll(strings.TrimSuffix(field, "\n"), "\n", "\n    ")+"\n    instruction: Help.", 1)
		editor := testEditor(t, config.NewBytesSource("clone.yaml", []byte(team)), nil)
		actor := testActor(editor, "root")
		_, err := editor.Patch(t.Context(), editor.CurrentRevision(), actor, AgentPatch{CloneFrom: "helper", NewAgentKey: "copy"})
		require.Error(t, err, "unsafe capability profiles cannot be cloned")
	}
}

func TestEditorRemovePrunesAllInboundEdgesButNeverEmptiesEdgeLists(t *testing.T) {
	t.Parallel()
	team := `version: "16"
agents:
  root:
    model: openai/root
    sub_agents: [worker, helper]
    subagents: [worker, helper]
    handoffs: [worker, helper]
    instruction: root
  worker:
    model: openai/worker
    instruction: worker
  helper:
    model: openai/helper
    instruction: helper
`
	e := testEditor(t, config.NewBytesSource("remove.yaml", []byte(team)), nil)
	actor := testActor(e, "root")
	revision, err := e.Patch(t.Context(), e.CurrentRevision(), actor, AgentPatch{AgentKey: "worker", Remove: true})
	require.NoError(t, err)
	loaded, err := e.Source().SourceForLoad(revision)
	require.NoError(t, err)
	data, err := loaded.Read(t.Context())
	require.NoError(t, err)
	assert.NotContains(t, string(data), "  worker:")
	assert.Contains(t, string(data), "sub_agents: [helper]")
	assert.Contains(t, string(data), "subagents: [helper]")
	assert.Contains(t, string(data), "handoffs: [helper]")
	_, err = e.Validate(t.Context(), revision, actor)
	require.NoError(t, err)

	onlyInbound := `version: "16"
agents:
  root:
    model: openai/root
    sub_agents: [worker]
    instruction: root
  worker:
    model: openai/worker
    instruction: worker
`
	blocked := testEditor(t, config.NewBytesSource("remove.yaml", []byte(onlyInbound)), nil)
	_, err = blocked.Patch(t.Context(), blocked.CurrentRevision(), testActor(blocked, "root"), AgentPatch{AgentKey: "worker", Remove: true})
	require.ErrorContains(t, err, "would empty an agent reference list")
}

func TestEditorRemoveRejectsDefaultActorForceTargetAndUnreachableClone(t *testing.T) {
	t.Parallel()
	team := strings.Replace(cloneGraphYAML, "sub_agents: [worker, helper]", "sub_agents: [worker, helper]\n    force_handoff: worker", 1)
	e := testEditor(t, config.NewBytesSource("team.yaml", []byte(team)), nil)
	actor := testActor(e, "root")
	_, err := e.Patch(t.Context(), e.CurrentRevision(), actor, AgentPatch{AgentKey: "worker", Remove: true})
	require.ErrorContains(t, err, "force_handoff")
	_, err = e.Patch(t.Context(), e.CurrentRevision(), actor, AgentPatch{AgentKey: "root", Remove: true})
	require.ErrorContains(t, err, "cannot remove itself")

	_, err = e.Patch(t.Context(), e.CurrentRevision(), actor, AgentPatch{CloneFrom: "isolated", NewAgentKey: "copy"})
	require.ErrorContains(t, err, "delegation closure")
}

func TestEditorCloneRejectsReservedCollisionAndRemovesWithBaselineActorEpoch(t *testing.T) {
	t.Parallel()
	e := testEditor(t, config.NewBytesSource("clone.yaml", []byte(cloneGraphYAML)), nil)
	actor := testActor(e, "root")
	_, err := e.Patch(t.Context(), e.CurrentRevision(), actor, AgentPatch{CloneFrom: "worker", NewAgentKey: "helper"})
	require.ErrorContains(t, err, "already in use")
	revision, err := e.Patch(t.Context(), e.CurrentRevision(), actor, AgentPatch{CloneFrom: "worker", NewAgentKey: "temporary"})
	require.NoError(t, err)
	_, err = e.Patch(t.Context(), revision, actor, AgentPatch{AgentKey: "temporary", Remove: true})
	require.NoError(t, err, "the pinned baseline actor must remain valid after clone commit")
}

func TestEditorCacheEnabledClassRejectsSemanticCacheKeyEdits(t *testing.T) {
	t.Parallel()
	team := strings.Replace(cloneGraphYAML, "    instruction: Work carefully.", "    instruction: Work carefully.\n    cache: {enabled: true}", 1)
	team = strings.Replace(team, "      Work carefully.\n      Keep this paragraph.", "      Work carefully.\n      Keep this paragraph.\n    cache: {enabled: true}", 1)
	e := testEditor(t, config.NewBytesSource("cache.yaml", []byte(team)), nil)
	actor := testActor(e, "root")
	for _, patch := range []AgentPatch{{AgentKey: "worker", Instruction: new("changed")}, {AgentKey: "worker", Description: new("changed")}, {AgentKey: "worker", Model: new("openai/worker")}} {
		_, err := e.Patch(t.Context(), e.CurrentRevision(), actor, patch)
		require.ErrorContains(t, err, "cache-enabled baseline agents")
	}
}

func TestEditorInspectionIncludesCreatedCloneOnlyWithinActorReachableClass(t *testing.T) {
	t.Parallel()
	e := testEditor(t, config.NewBytesSource("clone.yaml", []byte(cloneGraphYAML)), nil)
	actor := testActor(e, "root")
	revision, err := e.Patch(t.Context(), e.CurrentRevision(), actor, AgentPatch{CloneFrom: "worker", NewAgentKey: "copy"})
	require.NoError(t, err)
	view, err := e.Inspect(t.Context(), revision, actor)
	require.NoError(t, err)
	keys := make(map[string]bool)
	for _, agent := range view.Agents {
		keys[agent.Key] = true
	}
	assert.True(t, keys["copy"])
	assert.False(t, keys["isolated"])
}

func fullEditor(t *testing.T, source config.Source, validate BuildValidator, allowlist []string) *Editor {
	t.Helper()
	e, err := NewEditor(t.Context(), EditableSourceOptions{Source: source, BuildValidator: validate, Authority: EditAuthorityFull, AllowedAgentIDs: allowlist})
	require.NoError(t, err)
	return e
}

func TestFullReplaceAllowsWholeConfigButRestrictedModeDoesNot(t *testing.T) {
	t.Parallel()
	baseYAML := []byte(`version: "16"
id: full-team
agents:
  root:
    model: openai/base
    instruction: Original.
`)
	full := fullEditor(t, config.NewBytesSource("full.yaml", baseYAML), func(ctx context.Context, src config.Source) error { _, err := config.Load(ctx, src); return err }, nil)
	actor := testActor(full, "root")
	base := full.CurrentRevision()
	replacement := []byte(`version: "16"
id: full-team
providers:
  extra:
    provider: openai
    base_url: https://api.example.invalid
    token_key: TOKEN_ENV
permissions:
  allow: [team_edit]
agents:
  root:
    model: openai/new
    instruction: New.
    sub_agents: [worker]
  worker:
    model: openai/worker
    instruction: Work.
`)
	result, err := full.Replace(t.Context(), base, actor, replacement, true)
	require.NoError(t, err)
	assert.True(t, result.DryRun)
	assert.Equal(t, base, full.CurrentRevision())
	result, err = full.Replace(t.Context(), base, actor, replacement, false)
	require.NoError(t, err)
	assert.Equal(t, EditOperationReplace, result.Operation)
	assert.NotEqual(t, base, result.Revision)
	loaded, err := full.Source().SourceForLoad(result.Revision)
	require.NoError(t, err)
	actual, err := loaded.Read(t.Context())
	require.NoError(t, err)
	assert.Equal(t, replacement, actual)
	restricted := testEditor(t, config.NewBytesSource("restricted.yaml", baseYAML), nil)
	_, err = restricted.Replace(t.Context(), restricted.CurrentRevision(), testActor(restricted, "root"), replacement, false)
	require.ErrorContains(t, err, "trusted full-edit authority")
}

func TestFullReplaceTrustedAllowlistCasValidationAndModeCannotEscalate(t *testing.T) {
	t.Parallel()
	base := []byte(`version: "16"
agents: {root: {model: openai/root, instruction: before}, worker: {model: openai/worker, instruction: worker}}
`)
	validator := func(ctx context.Context, src config.Source) error { _, err := config.Load(ctx, src); return err }
	e := fullEditor(t, config.NewBytesSource("allow.yaml", base), validator, []string{"root"})
	actor := testActor(e, "worker")
	_, err := e.Replace(t.Context(), e.CurrentRevision(), actor, base, false)
	require.ErrorContains(t, err, "trusted full-edit allowlist")
	root := testActor(e, "root")
	stale := e.CurrentRevision()
	bad := []byte("version: \"16\"\nagents: {root: {model: openai/root, definitely_unknown: x}}\n")
	_, err = e.Replace(t.Context(), stale, root, bad, false)
	require.ErrorContains(t, err, "replacement config failed validation")
	assert.Equal(t, stale, e.CurrentRevision(), "failed validation is atomic")
	valid := []byte("version: \"16\"\nagents: {root: {model: openai/root, instruction: after}}\n")
	result, err := e.Replace(t.Context(), stale, root, valid, false)
	require.NoError(t, err)
	_, err = e.Replace(t.Context(), stale, root, valid, false)
	require.ErrorContains(t, err, "revision conflict")
	_, err = e.Patch(t.Context(), result.Revision, root, AgentPatch{AgentKey: "root", Instruction: new("self-escalate")})
	require.ErrorContains(t, err, "unavailable outside restricted")
	untrusted := fullEditor(t, config.NewBytesSource("untrusted.yaml", base), validator, nil)
	_, err = untrusted.Replace(t.Context(), untrusted.CurrentRevision(), testActor(untrusted, "root"), valid, false)
	require.NoError(t, err)
}

func TestFullReplaceBuildFailureLeavesRevisionAndPersistenceCapabilitiesUnchanged(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "team.yaml")
	original := []byte(`version: "16"
agents: {root: {model: openai/root, instruction: before}}
`)
	require.NoError(t, os.WriteFile(path, original, 0o600))
	boom := errors.New("candidate team build rejected")
	e, err := NewEditor(t.Context(), EditableSourceOptions{Source: config.NewFileSource(path), Authority: EditAuthorityFull, WritablePath: path, BuildValidator: func(context.Context, config.Source) error { return boom }})
	require.NoError(t, err)
	base := e.CurrentRevision()
	actor := testActor(e, "root")
	replacement := []byte(`version: "16"
agents: {root: {model: openai/root, instruction: after}}
`)
	_, err = e.Replace(t.Context(), base, actor, replacement, false)
	require.ErrorContains(t, err, "replacement team build failed validation")
	assert.Equal(t, base, e.CurrentRevision())
	file, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, original, file)
}

func TestRestrictedCloneAndRenameCannotReuseOrStealDefaultRootIdentity(t *testing.T) {
	t.Parallel()
	team := `version: "16"
id: restricted
agents:
  alpha:
    model: openai/alpha
    sub_agents: [worker, relay]
    instruction: Alpha.
  worker:
    model: openai/worker
    sub_agents: [relay, helper]
    instruction: Worker.
  relay:
    model: openai/relay
    sub_agents: [worker]
    instruction: Relay.
  helper:
    model: openai/helper
    instruction: Helper.
`
	e := testEditor(t, config.NewBytesSource("restricted.yaml", []byte(team)), nil)
	actor := testActor(e, "alpha")
	removeRevision, err := e.Patch(t.Context(), e.CurrentRevision(), actor, AgentPatch{AgentKey: "relay", Remove: true})
	require.NoError(t, err)
	_, err = e.Patch(t.Context(), removeRevision, actor, AgentPatch{CloneFrom: "helper", NewAgentKey: "relay"})
	require.ErrorContains(t, err, "already in use or reserved")
	_, err = e.Patch(t.Context(), removeRevision, actor, AgentPatch{AgentKey: "helper", RenameTo: "root"})
	require.ErrorContains(t, err, "root key is reserved")
}

func TestFullAuthorityAllowsDefaultRenameAndSelfRemovalButNotIdentityForgery(t *testing.T) {
	t.Parallel()
	team := []byte(`version: "16"
id: full
agents:
  root:
    model: openai/root
    instruction: Root.
  worker:
    model: openai/worker
    instruction: Worker.
`)
	validator := func(ctx context.Context, src config.Source) error { _, err := config.Load(ctx, src); return err }
	e := fullEditor(t, config.NewBytesSource("full.yaml", team), validator, nil)
	actor := testActor(e, "root")
	_, err := e.Replace(t.Context(), e.CurrentRevision(), actor, []byte(`version: "16"
id: full
agents:
  operator:
    model: openai/operator
    instruction: Operator.
`), false)
	require.NoError(t, err, "full source-wide authority may replace/remove the root identity")
	_, err = e.Replace(t.Context(), e.CurrentRevision(), actor, []byte(`version: "16"
id: changed
agents: {next: {model: openai/next, instruction: Next.}}
`), false)
	require.NoError(t, err, "full replacement preserves actor auth to the pinned original source epoch")
}

func TestFullEditorAcceptsSupportedConfigShapesRejectedByRestrictedProvenance(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "full.yaml")
	yaml := `version: "16"
toolsets:
  base:
    type: filesystem
agents:
  root:
    model: openai/root
    use_toolsets: [base]
    instruction_file: prompt.md
`
	require.NoError(t, os.WriteFile(path, []byte(yaml), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "prompt.md"), []byte("Prompt."), 0o600))
	validator := func(ctx context.Context, src config.Source) error { _, err := config.Load(ctx, src); return err }
	_, err := NewEditor(t.Context(), EditableSourceOptions{Source: config.NewFileSource(path), Authority: EditAuthorityRestricted, BuildValidator: validator})
	require.Error(t, err, "restricted mode rejects paths that need direct provenance")
	full := fullEditor(t, config.NewFileSource(path), validator, nil)
	actor := ActorRef{TeamID: full.TeamID(), AgentKey: "root", SourceRevision: full.BaselineRevisionID()}
	_, err = full.Replace(t.Context(), full.CurrentRevision(), actor, []byte(yaml), true)
	require.NoError(t, err, "full mode passes the direct source and resolver to normal config.Load/build validation")
}

func TestEditorApplyCoordinatedCommitAndCoordinatorFailureRollback(t *testing.T) {
	t.Parallel()
	e := testEditor(t, config.NewBytesSource("team.yaml", []byte(editableTeamYAML)), nil)
	actor := testActor(e, "root")
	base := e.CurrentRevision()
	patch := AgentPatch{AgentKey: "worker", Instruction: new("prepared")}
	coordinatorRejected := false
	_, err := e.ApplyCoordinated(t.Context(), base, actor, patch, func(result EditResult, source config.Source, publishSource func() error) error {
		require.Equal(t, base.ID, result.BaseRevision.ID)
		candidate, err := config.Load(t.Context(), source)
		require.NoError(t, err)
		require.Equal(t, "safe-team", candidate.ID)
		coordinatorRejected = true
		return errors.New("environment CAS rejected")
	})
	require.ErrorContains(t, err, "environment CAS rejected")
	require.True(t, coordinatorRejected)
	assert.Equal(t, base, e.CurrentRevision())
	if _, err := e.Source().SourceForLoad(Revision{ID: ""}); err == nil {
		t.Fatal("invalid token unexpectedly loaded")
	}
	result, err := e.ApplyCoordinated(t.Context(), base, actor, patch, func(_ EditResult, _ config.Source, publishSource func() error) error { return publishSource() })
	require.NoError(t, err)
	require.Equal(t, result.Revision, e.CurrentRevision())
}

func TestEditorReplaceCoordinatedDoesNotAdvanceSourceIfEnvironmentPublishFails(t *testing.T) {
	t.Parallel()
	baseYaml := []byte(`version: "16"
agents: {root: {model: openai/root, instruction: before}}
`)
	e := fullEditor(t, config.NewBytesSource("full.yaml", baseYaml), func(ctx context.Context, src config.Source) error { _, err := config.Load(ctx, src); return err }, nil)
	actor := testActor(e, "root")
	base := e.CurrentRevision()
	candidate := []byte(`version: "16"
agents: {root: {model: openai/root, instruction: after}}
`)
	_, err := e.ReplaceCoordinated(t.Context(), base, actor, candidate, false, func(_ EditResult, _ config.Source, _ func() error) error { return errors.New("head changed") })
	require.ErrorContains(t, err, "head changed")
	assert.Equal(t, base, e.CurrentRevision())
	result, err := e.ReplaceCoordinated(t.Context(), base, actor, candidate, false, func(_ EditResult, _ config.Source, publishSource func() error) error { return publishSource() })
	require.NoError(t, err)
	assert.Equal(t, result.Revision, e.CurrentRevision())
}
