package image

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type failSecondWrite struct {
	buf    bytes.Buffer
	writes int
	failed bool
}

func (w *failSecondWrite) Write(p []byte) (int, error) {
	w.writes++
	if w.writes == 2 && !w.failed {
		w.failed = true
		return 0, errors.New("terminal write failed")
	}
	return w.buf.Write(p)
}

func (w *failSecondWrite) Reset() {
	w.buf.Reset()
}

func (w *failSecondWrite) String() string {
	return w.buf.String()
}

func TestWriterRetriesImageStateAfterOverlayWriteFailure(t *testing.T) {
	output := &failSecondWrite{}
	writer := NewWriter(output)
	writer.SetContent(KittySequence([]byte("png-data"), 20, 10))

	_, err := writer.Write([]byte("first frame"))
	require.Error(t, err)
	assert.Empty(t, writer.uploaded)
	assert.True(t, writer.dirty)
	assert.False(t, writer.active)

	output.Reset()
	_, err = writer.Write([]byte("second frame"))
	require.NoError(t, err)
	assert.Contains(t, output.String(), "a=t,t=d", "failed image upload must be retried")
	assert.Contains(t, output.String(), "a=d,d=a", "failed placement reset must be retried")
}

func TestWriterExtractsAndOverlaysKittyImage(t *testing.T) {
	png := []byte("png-data")
	content := "header\n    " + KittySequence(png, 20, 10) + "\n"
	var output bytes.Buffer
	writer := NewWriter(&output)

	clean := writer.SetContent(content)
	require.NotContains(t, clean, "\x1b_G")
	assert.Equal(t, "header\n    \n", clean)

	_, err := writer.Write([]byte("frame"))
	require.NoError(t, err)
	rendered := output.String()
	assert.True(t, strings.HasPrefix(rendered, "frame"))
	assert.Contains(t, rendered, "a=t,t=d,f=100")
	assert.Contains(t, rendered, "\x1b[2;5H")
	assert.Contains(t, rendered, "a=p,i=")
	assert.Contains(t, rendered, "c=20,r=10")
}

func TestWriterOnlyUploadsUnchangedImageOnce(t *testing.T) {
	var output bytes.Buffer
	writer := NewWriter(&output)
	writer.SetContent(KittySequence([]byte("png-data"), 20, 10))
	_, err := writer.Write([]byte("first"))
	require.NoError(t, err)

	output.Reset()
	writer.SetContent(KittySequence([]byte("png-data"), 20, 10))
	_, err = writer.Write([]byte("second"))
	require.NoError(t, err)
	assert.NotContains(t, output.String(), "a=t,t=d", "image data should remain cached")
	assert.Contains(t, output.String(), "a=p", "placement is refreshed after terminal text updates")
}

func TestMarkerOverlayCropsScrolledImage(t *testing.T) {
	img := Inline{Name: "test", PNGData: []byte("png-data"), Width: 100, Height: 100}
	lines := RenderMarkers(img, 24)
	require.GreaterOrEqual(t, len(lines), 8)

	clean, overlays := extractOverlays(strings.Join(lines[4:8], "\n"))
	require.Len(t, overlays, 1)
	assert.NotContains(t, clean, markerPrefix)
	assert.Equal(t, 0, overlays[0].y)
	assert.Equal(t, 4, overlays[0].rows)
	assert.Positive(t, overlays[0].sourceY)
	assert.Less(t, overlays[0].sourceH, img.Height)
}

func TestWriterUnsupportedTerminalStripsMarkersWithoutRendering(t *testing.T) {
	img := Inline{PNGData: []byte("png-data"), Width: 100, Height: 100}
	var output bytes.Buffer
	writer := NewWriter(&output)
	writer.SetSupported(false)

	clean := writer.SetContent(strings.Join(RenderMarkers(img, 40), "\n"))
	assert.NotContains(t, clean, markerPrefix)
	_, err := writer.Write([]byte("frame"))
	require.NoError(t, err)
	assert.Equal(t, "frame", output.String())
}

func TestWriterDisabledStripsMarkersWithoutRendering(t *testing.T) {
	img := Inline{PNGData: []byte("png-data"), Width: 100, Height: 100}
	var output bytes.Buffer
	writer := NewWriter(&output)
	writer.SetEnabled(false)

	clean := writer.SetContent(strings.Join(RenderMarkers(img, 40), "\n"))
	assert.NotContains(t, clean, markerPrefix)
	_, err := writer.Write([]byte("frame"))
	require.NoError(t, err)
	assert.Equal(t, "frame", output.String())
}

func TestWriterDeletesPlacementWhenImageLeavesView(t *testing.T) {
	var output bytes.Buffer
	writer := NewWriter(&output)
	writer.SetContent(KittySequence([]byte("png-data"), 20, 10))
	_, err := writer.Write([]byte("first"))
	require.NoError(t, err)

	output.Reset()
	writer.SetContent("no image")
	_, err = writer.Write([]byte("second"))
	require.NoError(t, err)
	assert.Contains(t, output.String(), "a=d,d=a")
}

func TestWriterDeletesMarkerPlacementsOnCloseReplaceAndDisable(t *testing.T) {
	img := Inline{PNGData: []byte("first-png"), Width: 100, Height: 100}
	other := Inline{PNGData: []byte("second-png"), Width: 100, Height: 100}
	for _, operation := range []string{"close", "replace", "disable", "unsupported"} {
		t.Run(operation, func(t *testing.T) {
			var output bytes.Buffer
			writer := NewWriter(&output)
			writer.SetContent(strings.Join(RenderMarkers(img, 24), "\n"))
			_, err := writer.Write([]byte("open"))
			require.NoError(t, err)
			require.True(t, writer.active)
			output.Reset()
			content := "closed"
			switch operation {
			case "replace":
				content = strings.Join(RenderMarkers(other, 24), "\n")
			case "disable":
				writer.SetEnabled(false)
				content = strings.Join(RenderMarkers(img, 24), "\n")
			case "unsupported":
				writer.SetSupported(false)
				content = strings.Join(RenderMarkers(img, 24), "\n")
			}
			clean := writer.SetContent(content)
			assert.NotContains(t, clean, markerPrefix)
			_, err = writer.Write([]byte("next"))
			require.NoError(t, err)
			assert.Contains(t, output.String(), "a=d,d=a", "stale placements are removed after the final frame loses markers")
			assert.Equal(t, operation == "replace", writer.active)
		})
	}
}

func TestExplicitPreviewPolicyIsPerPlacementNotSharedImage(t *testing.T) {
	img := Inline{PNGData: []byte("identical-image-data"), Width: 100, Height: 100}
	automatic := RenderMarkers(img, 24)
	explicit := RenderPreviewMarkers(img, 24)
	require.Greater(t, len(automatic), 2)
	// Adjacent source rows of the same image must not merge across policies.
	content := automatic[0] + "\n" + explicit[1] + "\n" + automatic[2]
	_, overlays := extractOverlays(content)
	require.Len(t, overlays, 3)
	assert.Equal(t, overlays[0].id, overlays[1].id)
	assert.False(t, overlays[0].explicit)
	assert.True(t, overlays[1].explicit)
	assert.False(t, overlays[2].explicit)
	var output bytes.Buffer
	writer := NewWriter(&output)
	writer.SetEnabled(false)
	writer.SetContent(content)
	require.Len(t, writer.overlays, 1)
	assert.Equal(t, 1, writer.overlays[0].y)
	assert.True(t, writer.Supported())
	assert.False(t, writer.RenderingEnabled(), "automatic preference unchanged")
	_, err := writer.Write([]byte("frame"))
	require.NoError(t, err)
	assert.Contains(t, output.String(), "a=p,i=")
	writer.SetSupported(false)
	writer.SetContent(content)
	assert.Empty(t, writer.overlays, "explicit preview never bypasses terminal capability")
}

func TestExplicitMarkersIgnoreAutomaticGlobalWithoutChangingIt(t *testing.T) {
	SetRenderingEnabled(false)
	t.Cleanup(func() { SetRenderingEnabled(true) })
	img := Inline{PNGData: []byte("preview"), Width: 40, Height: 40}
	assert.Empty(t, RenderMarkers(img, 20))
	lines := RenderPreviewMarkers(img, 20)
	require.NotEmpty(t, lines)
	assert.Contains(t, lines[0], ";preview\x1b\\")
	assert.Empty(t, RenderMarkers(img, 20), "manual preview never toggles the global preference")
	for _, line := range lines {
		start := strings.Index(line, markerPrefix)
		cols, marker := MarkerColumns(line[start:])
		assert.True(t, marker)
		assert.Positive(t, cols)
		assert.NotContains(t, StripMarkers(line), markerPrefix)
	}
}

func TestNativePreviewPlacementUsesPixelsAndPartialCellCrop(t *testing.T) {
	img := Inline{PNGData: []byte("native-png"), Width: 17, Height: 35}
	lines := RenderNativePreviewMarkers(img, CellSize{9, 23})
	require.Len(t, lines, 2)
	_, overlays := extractOverlays(strings.Join(lines, "\n"))
	require.Len(t, overlays, 1)
	assert.Equal(t, 2, overlays[0].cols)
	assert.Equal(t, 35, overlays[0].sourceH)
	_, cropped := extractOverlays(lines[1])
	require.Len(t, cropped, 1)
	assert.Equal(t, 23, cropped[0].sourceY)
	assert.Equal(t, 12, cropped[0].sourceH)
	var output bytes.Buffer
	writer := NewWriter(&output)
	writer.SetContent(lines[1])
	_, err := writer.Write([]byte("frame"))
	require.NoError(t, err)
	assert.Contains(t, output.String(), "a=p,i=")
	assert.NotContains(t, output.String(), ",c=", "native previews never scale to rounded terminal cells")
	assert.NotContains(t, output.String(), ",r=")
	assert.Contains(t, output.String(), ",y=23,w=17,h=12")
}

func TestInvalidateThenEmptyFrameDeletesExistingPlacement(t *testing.T) {
	var output bytes.Buffer
	writer := NewWriter(&output)
	writer.SetContent(strings.Join(RenderPreviewMarkers(Inline{PNGData: []byte("png"), Width: 100, Height: 100}, 24), "\n"))
	_, err := writer.Write([]byte("visible"))
	require.NoError(t, err)
	output.Reset()
	writer.Invalidate()
	writer.SetContent("animation frame without markers")
	_, err = writer.Write([]byte("resize"))
	require.NoError(t, err)
	assert.Contains(t, output.String(), "a=d,d=a", "invalidation does not prove terminal placements disappeared")
	assert.NotContains(t, output.String(), "a=p,i=")
}

func TestInvalidateEmptyOverlayFailureRetriesDeletion(t *testing.T) {
	output := &failSecondWrite{}
	writer := NewWriter(output)
	writer.active = true
	writer.Invalidate()
	writer.SetContent("tiny")
	_, err := writer.Write([]byte("frame"))
	require.Error(t, err)
	assert.True(t, writer.active)
	assert.True(t, writer.dirty)
	output.Reset()
	_, err = writer.Write([]byte("retry"))
	require.NoError(t, err)
	assert.Contains(t, output.String(), "a=d,d=a")
	assert.False(t, writer.active)
}

func TestGraphicsFlushRequestsCoalesceAndRetireWithoutIdleWork(t *testing.T) {
	var output bytes.Buffer
	writer := NewWriter(&output)
	assert.False(t, writer.RequestFlush())
	writer.Invalidate()
	assert.False(t, writer.RequestFlush(), "empty graphics state never schedules output")
	writer.SetContent(strings.Join(RenderNativePreviewMarkers(Inline{PNGData: []byte("png"), Width: 8, Height: 16}, CellSize{}), "\n"))
	assert.True(t, writer.RequestFlush())
	assert.False(t, writer.RequestFlush(), "only one wakeup per pending output")
	_, err := writer.Write([]byte("\x1b7\x1b8"))
	require.NoError(t, err)
	assert.False(t, writer.RequestFlush(), "committed placement has no recurring graphics work")
	writer.SetContent("")
	assert.True(t, writer.RequestFlush())
	_, err = writer.Write([]byte("\x1b7\x1b8"))
	require.NoError(t, err)
	assert.False(t, writer.RequestFlush())
	assert.Contains(t, output.String(), "a=d,d=a")
}

func TestGraphicsFlushWriteFailureAllowsRetryWithoutClearingDirtyState(t *testing.T) {
	output := &failSecondWrite{}
	writer := NewWriter(output)
	writer.SetContent(KittySequence([]byte("png"), 2, 1))
	require.True(t, writer.RequestFlush())
	_, err := writer.Write([]byte("first"))
	require.Error(t, err)
	assert.True(t, writer.dirty)
	assert.True(t, writer.RequestFlush(), "failed delivery releases only wakeup ownership, not graphics state")
	_, err = writer.Write([]byte("retry"))
	require.NoError(t, err)
	assert.False(t, writer.RequestFlush())
}

func TestWriterEnabledIsIndependentOfTerminalSupport(t *testing.T) {
	var output bytes.Buffer
	writer := NewWriter(&output)
	writer.SetSupported(false)
	writer.SetEnabled(true)
	require.True(t, writer.Enabled())
	require.False(t, writer.RenderingEnabled())
	writer.SetEnabled(false)
	writer.SetSupported(true)
	require.False(t, writer.Enabled())
	require.False(t, writer.RenderingEnabled())
}
