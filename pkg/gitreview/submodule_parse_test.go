package gitreview

import (
	"bytes"
	"slices"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5/config"
	"github.com/stretchr/testify/require"
)

// legacySubmodulePaths is the previous config.ReadConfig-based parser, kept as the oracle.
func legacySubmodulePaths(data []byte) ([]string, error) {
	cfg, err := config.ReadConfig(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	paths := make([]string, 0, len(cfg.Submodules))
	for _, submodule := range cfg.Submodules {
		if submodule != nil && submodule.Path != "" {
			paths = append(paths, submodule.Path)
		}
	}
	slices.Sort(paths)
	return paths, nil
}

func TestSubmodulePathParsingMatchesGoGitConfig(t *testing.T) {
	fixtures := map[string]string{
		"empty":                         "",
		"basic":                         "[submodule \"a\"]\n\tpath = a\n\turl = https://x/a\n[submodule \"b\"]\n\tpath = b\n\turl = https://x/b\n",
		"quoted name and value":         "[submodule \"my lib\"]\n\tpath = \"vendor dir/my lib\"\n\turl = https://x\n",
		"escaped quote in value":        "[submodule \"e\"]\n\tpath = \"quote\\\"d\"\n\turl = https://x\n",
		"malformed syntax":              "[submodule \"a\"\n\tpath = a\n",
		"duplicate name keeps last":     "[submodule \"a\"]\n\tpath = first\n\turl = u\n[submodule \"a\"]\n\tpath = second\n\turl = u\n",
		"duplicate path, two names":     "[submodule \"a\"]\n\tpath = same\n\turl = u\n[submodule \"b\"]\n\tpath = same\n\turl = u\n",
		"empty path replaces same name": "[submodule \"a\"]\n\tpath = keep\n\turl = u\n[submodule \"a\"]\n\turl = u\n",
		"missing url keeps path":        "[submodule \"a\"]\n\tpath = a\n",
		"dot-dot path without url":      "[submodule \"a\"]\n\tpath = ../escape\n",
		"dot-dot path with url":         "[submodule \"a\"]\n\tpath = ../escape\n\turl = u\n",
		"dot-dot name":                  "[submodule \"..\"]\n\tpath = x\n\turl = u\n",
		"hfs ignorable dot-dot name":    "[submodule \".\u200c.\"]\n\tpath = x\n\turl = u\n",
		"ntfs trailing space name":      "[submodule \".. \"]\n\tpath = x\n\turl = u\n",
		"ntfs ads name":                 "[submodule \"..:stream\"]\n\tpath = x\n\turl = u\n",
		"separator dot-dot name":        "[submodule \"a/../b\"]\n\tpath = x\n\turl = u\n",
		"leading separator name":        "[submodule \"/a\"]\n\tpath = x\n\turl = u\n",
		"drive prefix name":             "[submodule \"c:a\"]\n\tpath = x\n\turl = u\n",
		"three dots name is allowed":    "[submodule \"...x\"]\n\tpath = y\n\turl = u\n",
		"mixed-case section":            "[Submodule \"a\"]\n\tpath = a\n\turl = u\n",
	}
	for name, data := range fixtures {
		t.Run(name, func(t *testing.T) {
			want, wantErr := legacySubmodulePaths([]byte(data))
			got, gotErr := parseSubmodulePaths([]byte(data))
			require.Equal(t, wantErr != nil, gotErr != nil)
			require.Equal(t, want, got)
		})
	}
}

func TestSubmodulePathParsingDropsDotDotNames(t *testing.T) {
	for _, name := range []string{"..", ".\u200c.", ".. ", "..:stream", "a/../b", "..\\b", "/a", "c:a", "."} {
		escaped := strings.ReplaceAll(name, "\\", "\\\\")
		paths, err := parseSubmodulePaths([]byte("[submodule \"" + escaped + "\"]\n\tpath = x\n\turl = u\n"))
		require.NoError(t, err, name)
		require.Empty(t, paths, "name %q must be skipped", name)
	}
}

func TestSubmodulePathParsingReturnsSortedPaths(t *testing.T) {
	paths, err := parseSubmodulePaths([]byte("[submodule \"b\"]\n\tpath = b\n\turl = u\n[submodule \"a\"]\n\tpath = a\n\turl = u\n"))
	require.NoError(t, err)
	require.Equal(t, []string{"a", "b"}, paths)
}

func TestSubmodulePathParsingRejectsMalformedSyntax(t *testing.T) {
	for _, data := range []string{
		"[submodule \"a\"\n\tpath = a\n",
		"[submodule \"a\"]\n\tpath a\n",
	} {
		_, err := parseSubmodulePaths([]byte(data))
		require.Error(t, err, "malformed .gitmodules must fail discovery: %q", data)
	}
}

// Semantic validation of non-submodule sections is intentionally not applied:
// it cannot change which .gitmodules paths are extracted.
func TestSubmodulePathParsingIgnoresIrrelevantSections(t *testing.T) {
	data := "[pack]\n\tthreads = not-a-number\n[core]\n\trepositoryformatversion = bogus\n[submodule \"a\"]\n\tpath = a\n\turl = u\n"
	paths, err := parseSubmodulePaths([]byte(data))
	require.NoError(t, err)
	require.Equal(t, []string{"a"}, paths)
}
