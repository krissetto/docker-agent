package styles

import (
	"io/fs"
	"path"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/goccy/go-yaml"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/userconfig"
)

const (
	dockerNextDarkName  = "Docker Next - Dark"
	dockerNextLightName = "Docker Next - Light"
)

// Each Docker Next palette must set every key itself; an omitted key would
// silently inherit the Old School base.
func TestDockerNextPalettesDefineEveryKey(t *testing.T) {
	t.Parallel()

	for ref, wantName := range map[string]string{
		DefaultThemeRef:         dockerNextDarkName,
		DockerNextLightThemeRef: dockerNextLightName,
	} {
		data, ok := readThemeData(builtinThemes, ref)
		require.True(t, ok, ref)

		var raw Theme
		require.NoError(t, yaml.Unmarshal(data, &raw))
		assert.Equal(t, wantName, raw.Name, ref)
		assertAllFieldsSet(t, ref, raw.Colors)
		// Light derives its word-emphasis fill from diff_add_bg; only dark authors the explicit override.
		if ref == DockerNextLightThemeRef {
			require.Empty(t, raw.Colors.DiffAddEmphBg, "light keeps the derived emphasis fill")
		}
		assertAllFieldsSet(t, ref, raw.Chroma)
		assertAllFieldsSet(t, ref, raw.Markdown)
	}
}

func assertAllFieldsSet(t *testing.T, ref string, section any) {
	t.Helper()

	v := reflect.ValueOf(section)
	for i := range v.NumField() {
		field := v.Type().Field(i)
		value := v.Field(i)
		if ref == DockerNextLightThemeRef && field.Name == "DiffAddEmphBg" {
			continue // optional override; light uses the derived fill
		}
		if value.Kind() == reflect.Slice {
			assert.NotZero(t, value.Len(), "%s: %s not set", ref, field.Name)
			continue
		}
		assert.NotEmpty(t, value.String(), "%s: %s not set", ref, field.Name)
	}
}

// Every bundled and user theme merges onto the Old School base, so a palette
// that omits a key must keep the historical value instead of the Docker Next one.
func TestPartialBundledThemesInheritOldSchoolBase(t *testing.T) {
	t.Parallel()

	legacy := legacyBaseTheme()
	err := fs.WalkDir(builtinThemes, "themes", func(file string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		ref := strings.TrimSuffix(path.Base(file), path.Ext(file))
		if ref == DefaultThemeRef || ref == DockerNextLightThemeRef || ref == LegacyDefaultThemeRef {
			return nil
		}
		t.Run(ref, func(t *testing.T) {
			data, ok := readThemeData(builtinThemes, ref)
			require.True(t, ok)
			var raw Theme
			require.NoError(t, yaml.Unmarshal(data, &raw))

			merged, err := newThemeRegistry().LoadTheme(ref)
			require.NoError(t, err)
			assertInheritedFrom(t, raw.Colors, merged.Colors, legacy.Colors)
			assertInheritedFrom(t, raw.Chroma, merged.Chroma, legacy.Chroma)
			assertInheritedFrom(t, raw.Markdown, merged.Markdown, legacy.Markdown)
		})
		return nil
	})
	require.NoError(t, err)
}

// assertInheritedFrom checks that each field omitted in raw equals the base value.
// Derived fields (editor and scrollbar surfaces) are computed from other keys and skipped.
func assertInheritedFrom(t *testing.T, raw, merged, base any) {
	t.Helper()

	rv, mv, bv := reflect.ValueOf(raw), reflect.ValueOf(merged), reflect.ValueOf(base)
	for i := range rv.NumField() {
		name := rv.Type().Field(i).Name
		if name == "EditorBg" || name == "ScrollbarTrack" {
			continue
		}
		if rv.Field(i).IsZero() {
			assert.Equal(t, bv.Field(i).Interface(), mv.Field(i).Interface(), "%s should inherit Old School", name)
		}
	}
}

func TestLegacyDefaultRefLoadsOldSchool(t *testing.T) {
	t.Parallel()

	theme, err := LoadTheme(LegacyDefaultThemeRef)
	require.NoError(t, err)
	assert.Equal(t, "Old School", theme.Name)
	assert.Equal(t, LegacyDefaultThemeRef, theme.Ref)
	assert.Equal(t, "#7AA2F7", theme.Colors.Accent)
	assert.Equal(t, "#1C1C22", theme.Colors.Background)
	assert.Equal(t, legacyBaseTheme().Colors, theme.Colors)
	assert.Equal(t, legacyBaseTheme().Chroma, theme.Chroma)
	assert.Equal(t, legacyBaseTheme().Markdown, theme.Markdown)
}

// A nameless partial theme keeps the historical inherited "Default" display name.
func TestNamelessPartialThemeInheritsHistoricalName(t *testing.T) {
	t.Parallel()

	registry := newThemeRegistry()
	require.NoError(t, registry.RegisterBuiltinThemes(fstest.MapFS{
		"themes/nameless.yaml": &fstest.MapFile{Data: []byte("colors:\n  accent: \"#123456\"\n")},
	}))
	theme, err := registry.LoadTheme("nameless")
	require.NoError(t, err)
	assert.Equal(t, "Default", theme.Name)
	assert.Equal(t, "#123456", theme.Colors.Accent)
}

func TestLegacyDefaultLightStillLoads(t *testing.T) {
	t.Parallel()

	theme, err := LoadTheme("default-light")
	require.NoError(t, err)
	assert.Equal(t, "Default Light", theme.Name)
}

func TestThemeListingStartsWithDockerNextPair(t *testing.T) {
	t.Parallel()

	refs, err := ListThemeRefs()
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(refs), 2)
	assert.Equal(t, []string{DefaultThemeRef, DockerNextLightThemeRef}, refs[:2])
	assert.Equal(t, 1, countOf(refs, LegacyDefaultThemeRef))
	assert.Equal(t, 1, countOf(refs, DefaultThemeRef))
}

// Choosing Docker Next clears the saved value (unset means Dark); choosing
// Old School persists "default" so it stays a distinct explicit choice.
func TestSaveThemeKeepsUnsetMeaningDockerNextAndExplicitOldSchool(t *testing.T) {
	isolateUserConfig(t)

	require.NoError(t, SaveThemeToUserConfig(LegacyDefaultThemeRef))
	cfg, err := userconfig.Load()
	require.NoError(t, err)
	assert.Equal(t, LegacyDefaultThemeRef, cfg.Settings.Theme)
	assert.Equal(t, LegacyDefaultThemeRef, GetPersistedThemeRef())

	require.NoError(t, SaveThemeToUserConfig(DefaultThemeRef))
	cfg, err = userconfig.Load()
	require.NoError(t, err)
	assert.Empty(t, cfg.Settings.Theme)
	assert.Equal(t, DefaultThemeRef, GetPersistedThemeRef())
}

func countOf(values []string, want string) int {
	n := 0
	for _, v := range values {
		if v == want {
			n++
		}
	}
	return n
}
