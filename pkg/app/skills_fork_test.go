package app

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/skills"
	skillstool "github.com/docker/docker-agent/pkg/tools/builtin/skills"
)

// skillFakeRuntime extends mockRuntime with a real *skillstool.ToolSet so
// the App can detect and expand slash-skill commands.
type skillFakeRuntime struct {
	*mockRuntime

	skillset *skillstool.ToolSet
}

func (f *skillFakeRuntime) CurrentAgentSkillsToolset() *skillstool.ToolSet {
	return f.skillset
}

// writeSkill creates a SKILL.md in a temp dir and returns a Local skill
// pointing at it so ReadSkillContent expands `!`shell“ placeholders.
func writeSkill(t *testing.T, name string, fork bool, body string) skills.Skill {
	t.Helper()

	dir := t.TempDir()
	skillFile := filepath.Join(dir, "SKILL.md")
	require.NoError(t, os.WriteFile(skillFile, []byte(body), 0o644))

	skill := skills.Skill{
		Name:        name,
		Description: "Test skill " + name,
		FilePath:    skillFile,
		BaseDir:     dir,
		Local:       true,
	}
	if fork {
		skill.Context = "fork"
	}
	return skill
}

// TestApp_SlashSkill_InlineContext_StillInlines covers the
// backwards-compatible path: skills without `context: fork` keep being
// inlined into the parent transcript via the <skill> envelope.
func TestApp_SlashSkill_InlineContext_StillInlines(t *testing.T) {
	t.Parallel()

	skill := writeSkill(t, "review", false /* not fork */, "# Review\nPlease review.\n")
	st := skillstool.New([]skills.Skill{skill}, filepath.Dir(skill.FilePath))

	rt := &skillFakeRuntime{
		mockRuntime: &mockRuntime{},
		skillset:    st,
	}

	ctx := t.Context()
	a := New(t.Context(), nil, session.New(), runtime.SessionBinding{}, WithRuntimeServices(rt))

	_, _, ok, err := a.SkillCommandForkResult(ctx, "/review the diff")
	require.NoError(t, err)
	assert.False(t, ok)

	resolved, err := a.ResolveInputOnce(ctx, "/review the diff")
	require.NoError(t, err)
	assert.Contains(t, resolved.Content, "<skill name=\"review\">")
	assert.Contains(t, resolved.Content, "Please review.")
	assert.Contains(t, resolved.Content, "the diff")
}
