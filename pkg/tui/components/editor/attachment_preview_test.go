package editor

import (
	"bytes"
	"image"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/styles"
)

func TestAttachmentPreviewTypedImagesAndSafeText(t *testing.T) {
	var pngData, jpegData bytes.Buffer
	img := image.NewRGBA(image.Rect(0, 0, 8, 4))
	require.NoError(t, png.Encode(&pngData, img))
	require.NoError(t, jpeg.Encode(&jpegData, img, nil))
	for _, tc := range []struct {
		name  string
		data  []byte
		image bool
		text  string
	}{
		{"picture.png", pngData.Bytes(), true, ""},
		{"photo.jpg", jpegData.Bytes(), true, ""},
		{"sniffed.data", pngData.Bytes(), true, ""},
		{"corrupt.png", []byte("not a valid PNG"), true, ""},
		{"note.txt", []byte("  UTF-8 界\r\n\ttext"), false, "  UTF-8 界\n\ttext"},
		{"binary.bin", []byte{0, 255, 1}, false, "Attachment preview unavailable:"},
		{"escape.txt", []byte("\x1b_Ga=T;secret\x1b\\"), false, "Attachment preview unavailable:"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), tc.name)
			require.NoError(t, os.WriteFile(path, tc.data, 0o600))
			e := New(nil).(*editor)
			e.SetSize(80, 5)
			require.NoError(t, e.AttachFile(path))
			e.ToggleContextBar()
			e.BannerView(80)
			draft := e.Value()
			preview, ok := e.AttachmentAtPosition(styles.AppPadding, 2)
			require.True(t, ok)
			assert.Equal(t, tc.image, preview.IsImage)
			if tc.image {
				assert.Equal(t, tc.data, preview.ImageData)
				assert.Empty(t, preview.Content, "encoded images never become text")
			} else {
				assert.Nil(t, preview.ImageData)
				assert.Contains(t, preview.Content, tc.text)
				assert.NotContains(t, preview.Content, "\x1b")
			}
			assert.Equal(t, draft, e.Value())
		})
	}
}

func TestAttachmentPreviewReadAndSizeErrorsAreVisible(t *testing.T) {
	missing := loadAttachmentPreview("missing", filepath.Join(t.TempDir(), "gone"))
	assert.Contains(t, missing.Content, "Unable to read attachment")
	path := filepath.Join(t.TempDir(), "large.txt")
	file, err := os.Create(path)
	require.NoError(t, err)
	require.NoError(t, file.Truncate(maxAttachmentPreviewBytes+1))
	require.NoError(t, file.Close())
	large := loadAttachmentPreview("large", path)
	assert.Contains(t, large.Content, "20 MiB")
	assert.Empty(t, large.ImageData)
	assert.Equal(t, "bad�name", safePreviewLabel("bad\x1bname"))
}

func TestOverflowPreviewKeepsDuplicateBasenameIdentity(t *testing.T) {
	e := New(nil).(*editor)
	e.SetSize(40, 3)
	e.SetBannerWidth(40)
	for _, content := range []string{"first exact bytes", "second exact bytes", "third exact bytes"} {
		path := filepath.Join(t.TempDir(), "same-界.txt")
		require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
		require.NoError(t, e.AttachFile(path))
	}
	e.ToggleContextBar()
	require.True(t, e.banner.IsExpanded())
	require.Len(t, e.banner.regions, 3)
	for i, region := range e.banner.regions {
		preview, ok := e.AttachmentAtPosition(styles.AppPadding+region.start, region.y)
		require.True(t, ok)
		expected, err := os.ReadFile(e.attachments[i].path)
		require.NoError(t, err)
		require.Equal(t, string(expected), preview.Content)
	}
}
