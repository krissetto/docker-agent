package skills

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/skills"
)

func TestReadSkillFileSymlinks(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	base := filepath.Join(dir, "skill")
	references := filepath.Join(base, "references")
	outside := filepath.Join(dir, "skill-other")
	require.NoError(t, os.MkdirAll(references, 0o755))
	require.NoError(t, os.Mkdir(outside, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(references, "FORMS.md"), []byte("internal text"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(outside, "FORMS.md"), []byte("external text"), 0o644))

	for _, tt := range []struct {
		name   string
		target string
		file   string
		allow  bool
	}{
		{name: "relative-internal-file", target: "references/FORMS.md", allow: true},
		{name: "relative-internal-directory", target: "references", file: "FORMS.md", allow: true},
		{name: "relative-external-file", target: "../skill-other/FORMS.md"},
		{name: "relative-external-directory", target: "../skill-other", file: "FORMS.md"},
		{name: "absolute-internal-file", target: filepath.Join(references, "FORMS.md")},
		{name: "absolute-internal-directory", target: references, file: "FORMS.md"},
		{name: "absolute-external-file", target: filepath.Join(outside, "FORMS.md")},
		{name: "absolute-external-directory", target: outside, file: "FORMS.md"},
		{name: "dangling", target: "missing.md"},
		{name: "loop", target: "loop"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if err := os.Symlink(filepath.FromSlash(tt.target), filepath.Join(base, tt.name)); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}
			path := filepath.Join(tt.name, tt.file)
			st := New([]skills.Skill{{Name: "test", BaseDir: base, Local: true}}, dir)
			content, err := st.ReadSkillFile("test", filepath.ToSlash(path))
			if tt.allow {
				require.NoError(t, err)
				assert.Equal(t, "internal text", content)
			} else {
				require.ErrorContains(t, err, "reading file:")
				assert.Empty(t, content)
			}
		})
	}
}

func TestReadSkillFileReadErrors(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	file := filepath.Join(dir, "resource.md")
	require.NoError(t, os.WriteFile(file, []byte("text"), 0o644))
	for _, tt := range []struct {
		name     string
		base     string
		path     string
		notExist bool
	}{
		{name: "missing-root", base: filepath.Join(dir, "missing"), path: "resource.md", notExist: true},
		{name: "root-is-file", base: file, path: "resource.md"},
		{name: "missing-file", base: dir, path: "missing.md", notExist: true},
		{name: "file-is-directory", base: dir, path: "."},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			st := New([]skills.Skill{{Name: "test", BaseDir: tt.base}}, dir)
			content, err := st.ReadSkillFile("test", tt.path)
			require.ErrorContains(t, err, "reading file:")
			if tt.notExist {
				require.ErrorIs(t, err, os.ErrNotExist)
			}
			assert.Empty(t, content)
		})
	}
}

func TestReadRootedFileContent_KeepsOpenedRoot(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not allow renaming an open directory")
	}
	parent := t.TempDir()
	base := filepath.Join(parent, "skill")
	require.NoError(t, os.Mkdir(base, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(base, "resource.md"), []byte("original"), 0o600))
	root, err := os.OpenRoot(base)
	require.NoError(t, err)
	defer root.Close()
	require.NoError(t, os.Rename(base, filepath.Join(parent, "moved")))
	outside := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(outside, "resource.md"), []byte("outside"), 0o600))
	require.NoError(t, os.Symlink(outside, base))
	content, err := readRootedFileContent(root, "resource.md")
	require.NoError(t, err)
	assert.Equal(t, "original", content)
}

func TestReadRootedFileContent_RejectsReplacedPath(t *testing.T) {
	t.Parallel()
	for _, ancestor := range []bool{false, true} {
		name := "file"
		if ancestor {
			name = "ancestor"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			base := t.TempDir()
			require.NoError(t, os.Mkdir(filepath.Join(base, "references"), 0o700))
			path := filepath.Join(base, "references", "resource.md")
			require.NoError(t, os.WriteFile(path, []byte("original"), 0o600))
			outside := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(outside, "resource.md"), []byte("outside"), 0o600))
			root, err := os.OpenRoot(base)
			require.NoError(t, err)
			defer root.Close()
			if ancestor {
				require.NoError(t, os.RemoveAll(filepath.Dir(path)))
				require.NoError(t, os.Symlink(outside, filepath.Dir(path)))
			} else {
				require.NoError(t, os.Remove(path))
				require.NoError(t, os.Symlink(filepath.Join(outside, "resource.md"), path))
			}
			content, err := readRootedFileContent(root, filepath.Join("references", "resource.md"))
			require.ErrorContains(t, err, "reading file:")
			assert.Empty(t, content)
		})
	}
}

func TestReadSkillFileSupportingCommandsRemainLiteral(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	body := "support !`echo must-not-run`"
	require.NoError(t, os.WriteFile(filepath.Join(base, "resource.md"), []byte(body), 0o600))
	st := New([]skills.Skill{{Name: "test", BaseDir: base, Local: true}}, base)
	content, err := st.ReadSkillFile("test", "resource.md")
	require.NoError(t, err)
	assert.Equal(t, body, content)
}
