package image

import (
	"bytes"
	"fmt"
	stdimage "image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func previewFixture(t testing.TB, width, height int, noisy bool) []byte {
	t.Helper()
	img := stdimage.NewNRGBA(stdimage.Rect(0, 0, width, height))
	state := uint32(17)
	for y := range height {
		for x := range width {
			if noisy {
				state = 1664525*state + 1013904223
				img.SetNRGBA(x, y, color.NRGBA{uint8(state), uint8(state >> 8), uint8(state >> 16), 255})
			} else {
				img.SetNRGBA(x, y, color.NRGBA{uint8((x / 32) % 4 * 50), uint8((y / 24) % 4 * 50), 80, 255})
			}
		}
	}
	var encoded bytes.Buffer
	require.NoError(t, png.Encode(&encoded, img))
	return encoded.Bytes()
}

func TestPreview49FitsRetainOnlyOneSourceAndReleaseOnClose(t *testing.T) {
	data := previewFixture(t, 1200, 800, true)
	p, err := DecodePreview("noisy", data)
	require.NoError(t, err)
	id := p.source.registryID
	assert.Equal(t, data, p.source.PNGData, "valid PNG is passed through byte-for-byte")
	var output bytes.Buffer
	writer := NewWriter(&output)
	for i := range 49 {
		fit, err := p.Fit(24+i, 15+i%10, CellSize{9, 23})
		require.NoError(t, err)
		require.Equal(t, id, fit.registryID)
		require.Same(t, &p.source.PNGData[0], &fit.PNGData[0])
		writer.Invalidate()
		writer.SetContent(strings.Join(RenderNativePreviewMarkers(fit, CellSize{9, 23}), "\n"))
		_, err = writer.Write([]byte("frame"))
		require.NoError(t, err)
	}
	runtime.GC()
	runtime.GC()
	inlineRegistry.Lock()
	entry := inlineRegistry.entries[id].Value.(*registryEntry)
	assert.Equal(t, 1, entry.owners)
	assert.Equal(t, len(data), len(entry.image.PNGData))
	assert.LessOrEqual(t, cap(entry.image.PNGData), len(data)+8192)
	t.Logf("after49fits+2GC registrySourceBytes=%d backingCapacity=%d owners=%d", len(entry.image.PNGData), cap(entry.image.PNGData), entry.owners)
	inlineRegistry.Unlock()
	assert.Equal(t, 1, strings.Count(output.String(), "a=t,t=d"), "49 resize fits upload one source, including placement invalidations")
	assert.NotContains(t, output.String(), ",c=0")
	p.Close()
	p.Close()
	_, ok := registeredImage(id)
	assert.False(t, ok)
	output.Reset()
	writer.SetContent("closed")
	_, err = writer.Write([]byte("frame"))
	require.NoError(t, err)
	assert.Contains(t, output.String(), fmt.Sprintf("a=d,d=I,i=%d", id))
	assert.Empty(t, writer.managed)
	assert.NotContains(t, writer.uploaded, id)
	assert.Empty(t, p.source.PNGData)
	runtime.GC()
	runtime.GC()
	t.Logf("afterClose+2GC ownedRegistryEntryPresent=%v writerManaged=%d", previewImageRetained(id), len(writer.managed))
}

func TestPreviewSharingWithOtherPreviewAndAutomaticImage(t *testing.T) {
	data := previewFixture(t, 31, 19, false)
	a, err := DecodePreview("a", data)
	require.NoError(t, err)
	b, err := DecodePreview("b", data)
	require.NoError(t, err)
	require.Equal(t, a.source.registryID, b.source.registryID)
	require.Same(t, &a.source.PNGData[0], &b.source.PNGData[0])
	id := a.source.registryID
	a.Close()
	require.True(t, previewImageRetained(id))
	auto := Inline{PNGData: data, Width: 31, Height: 19}
	RenderMarkers(auto, 20)
	b.Close()
	require.True(t, previewImageRetained(id), "automatic image ownership cannot be evicted by preview close")
	inlineRegistry.Lock()
	entry := inlineRegistry.entries[id].Value.(*registryEntry)
	assert.True(t, entry.cached)
	assert.Zero(t, entry.owners)
	inlineRegistry.Unlock()
}

func TestPreviewClosedWhilePublishedFrameStillOwnsBytes(t *testing.T) {
	p, err := DecodePreview("image", previewFixture(t, 40, 40, false))
	require.NoError(t, err)
	fit, err := p.Fit(20, 10, CellSize{})
	require.NoError(t, err)
	var output bytes.Buffer
	writer := NewWriter(&output)
	writer.SetContent(strings.Join(RenderNativePreviewMarkers(fit, CellSize{}), "\n"))
	id := fit.registryID
	p.Close()
	_, err = writer.Write([]byte("published"))
	require.NoError(t, err)
	assert.Contains(t, output.String(), "a=t,t=d")
	assert.NotContains(t, output.String(), fmt.Sprintf("a=d,d=I,i=%d", id))
	writer.SetContent("")
	output.Reset()
	_, err = writer.Write([]byte("closed"))
	require.NoError(t, err)
	assert.Contains(t, output.String(), fmt.Sprintf("a=d,d=I,i=%d", id))
}

func TestScaledPreviewProtocolAndOcclusion(t *testing.T) {
	p, err := DecodePreview("image", previewFixture(t, 120, 80, false))
	require.NoError(t, err)
	defer p.Close()
	fit, err := p.Fit(6, 4, CellSize{9, 23})
	require.NoError(t, err)
	assert.Equal(t, 54, fit.Width)
	assert.Equal(t, 36, fit.Height)
	lines := RenderNativePreviewMarkers(fit, CellSize{9, 23})
	_, partial := extractOverlays(strings.Join(lines[1:], "\n"))
	assert.Empty(t, partial, "partially occluded scaled image never bleeds across modal")
	var output bytes.Buffer
	writer := NewWriter(&output)
	writer.SetContent(strings.Join(lines, "\n"))
	_, err = writer.Write([]byte("frame"))
	require.NoError(t, err)
	assert.Contains(t, output.String(), ",c=6")
	assert.NotContains(t, output.String(), ",r=")
	assert.NotContains(t, output.String(), ",h=", "complete original source is scaled by the terminal")
	portrait, err := DecodePreview("portrait", previewFixture(t, 3, 100, false))
	require.NoError(t, err)
	defer portrait.Close()
	fit, err = portrait.Fit(1, 1, CellSize{9, 23})
	require.NoError(t, err)
	assert.Equal(t, 1, fit.placementRows)
	assert.Zero(t, fit.placementCols)
}

func TestPreviewNonPNGConvertsOnceAndRetainsStraightAlphaSemantics(t *testing.T) {
	img := stdimage.NewNRGBA(stdimage.Rect(0, 0, 10, 10))
	img.SetNRGBA(0, 0, color.NRGBA{R: 200, G: 100, B: 50, A: 80})
	var data bytes.Buffer
	require.NoError(t, png.Encode(&data, img))
	p, err := DecodePreview("alpha", data.Bytes())
	require.NoError(t, err)
	defer p.Close()
	assert.Equal(t, data.Bytes(), p.source.PNGData)
	decoded, err := png.Decode(bytes.NewReader(p.source.PNGData))
	require.NoError(t, err)
	assert.Equal(t, color.NRGBA{R: 200, G: 100, B: 50, A: 80}, color.NRGBAModel.Convert(decoded.At(0, 0)))
	data.Reset()
	require.NoError(t, jpeg.Encode(&data, img, nil))
	jpegPreview, err := DecodePreview("jpeg", data.Bytes())
	require.NoError(t, err)
	defer jpegPreview.Close()
	first, err := jpegPreview.Fit(1, 1, CellSize{8, 16})
	require.NoError(t, err)
	second, err := jpegPreview.Fit(20, 20, CellSize{8, 16})
	require.NoError(t, err)
	assert.Same(t, &first.PNGData[0], &second.PNGData[0])
}

func BenchmarkPreviewPlacementOnlyResize(b *testing.B) {
	p, err := DecodePreview("image", previewFixture(b, 1200, 800, true))
	require.NoError(b, err)
	defer p.Close()
	writer := NewWriter(io.Discard)
	b.ReportAllocs()
	b.ResetTimer()
	for i := range b.N {
		fit, err := p.Fit(24+i%49, 15+i%10, CellSize{9, 23})
		if err != nil {
			b.Fatal(err)
		}
		writer.Invalidate()
		writer.SetContent(strings.Join(RenderNativePreviewMarkers(fit, CellSize{9, 23}), "\n"))
		if _, err := writer.Write([]byte("frame")); err != nil {
			b.Fatal(err)
		}
	}
}

func TestManagedTerminalRetirementRetriesAfterWriteFailureAndReset(t *testing.T) {
	p, err := DecodePreview("image", previewFixture(t, 12, 12, false))
	require.NoError(t, err)
	fit, err := p.Fit(10, 10, CellSize{})
	require.NoError(t, err)
	output := &failSecondWrite{}
	writer := NewWriter(output)
	writer.SetContent(strings.Join(RenderNativePreviewMarkers(fit, CellSize{}), "\n"))
	_, err = writer.Write([]byte("first"))
	require.Error(t, err)
	assert.Contains(t, writer.managed, fit.registryID, "partial upload is retained for later cleanup even when upload fails")
	p.Close()
	writer.SetContent("")
	output.Reset()
	output.writes = 0
	output.failed = false
	_, err = writer.Write([]byte("close"))
	require.Error(t, err)
	assert.Contains(t, writer.managed, fit.registryID)
	output.Reset()
	_, err = writer.Write([]byte("retry"))
	require.NoError(t, err)
	assert.Contains(t, output.String(), "d=I")
	assert.Empty(t, writer.managed)
}

func TestPreviewFitDoesNotAllocateOrReencode(t *testing.T) {
	p, err := DecodePreview("source", previewFixture(t, 1200, 800, false))
	require.NoError(t, err)
	defer p.Close()
	allocs := testing.AllocsPerRun(100, func() {
		for i := range 49 {
			_, err := p.Fit(24+i, 15+i%10, CellSize{9, 23})
			if err != nil {
				panic(err)
			}
		}
	})
	assert.Zero(t, allocs, "changed placement dimensions never decode, resample, encode, hash, or allocate a fit")
}

func TestManagedRegistrySharingRetirementCannotDeleteOtherPlacements(t *testing.T) {
	data := previewFixture(t, 32, 32, false)
	first, err := DecodePreview("first", data)
	require.NoError(t, err)
	second, err := DecodePreview("second", data)
	require.NoError(t, err)
	fit, err := first.Fit(10, 10, CellSize{})
	require.NoError(t, err)
	var output bytes.Buffer
	writer := NewWriter(&output)
	show := func() {
		writer.SetContent(strings.Join(RenderNativePreviewMarkers(fit, CellSize{}), "\n"))
		_, err := writer.Write([]byte("frame"))
		require.NoError(t, err)
	}
	show()
	output.Reset()
	first.Close()
	writer.SetContent("covered")
	_, err = writer.Write([]byte("frame"))
	require.NoError(t, err)
	assert.NotContains(t, output.String(), "d=I", "covered but live second preview keeps source data")
	output.Reset()
	show()
	assert.NotContains(t, output.String(), "a=t,t=d", "uncovering shared live image does not reupload")
	output.Reset()
	second.Close()
	writer.SetContent("")
	_, err = writer.Write([]byte("frame"))
	require.NoError(t, err)
	assert.Contains(t, output.String(), "d=I")
	third, err := DecodePreview("third", data)
	require.NoError(t, err)
	defer third.Close()
	assert.NotEqual(t, fit.registryID, third.source.registryID, "retired source IDs are not reused")
}

func TestAutomaticCacheCannotEvictOwnedPreview(t *testing.T) {
	p, err := DecodePreview("pinned", previewFixture(t, 13, 11, false))
	require.NoError(t, err)
	id := p.source.registryID
	for i := range maxInlineRegistryEntries + 10 {
		registerImage(uint32(i+100), Inline{PNGData: []byte(fmt.Sprintf("cached-%d", i))})
	}
	require.True(t, previewImageRetained(id))
	p.Close()
	assert.False(t, previewImageRetained(id))
}
