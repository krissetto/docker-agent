package image

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/charmbracelet/x/ansi"
)

type overlay struct {
	explicit                     bool
	cellHeight                   int
	placementCols, placementRows int
	displayHeight                int
	firstRow, totalRows          int
	id                           uint32
	png                          []byte
	managed                      bool
	x, y                         int
	cols, rows                   int
	pixelW, pixelH               int
	sourceY, sourceH             int
}

// Writer adds kitty graphics after Bubble Tea has rendered its text cell buffer.
// Bubble Tea intentionally consumes APC sequences while parsing view content, so
// images must be overlaid on the completed frame instead.
type Writer struct {
	out io.Writer

	mu           sync.Mutex
	overlays     []overlay
	uploaded     map[uint32]bool
	managed      map[uint32]bool
	active       bool
	dirty        bool
	flushPending bool
	enabled      bool
	supported    bool
}

func NewWriter(out io.Writer) *Writer {
	return &Writer{out: out, uploaded: make(map[uint32]bool), managed: make(map[uint32]bool), enabled: true, supported: true}
}

// Fd, Read, and Close preserve the terminal file interface when Writer wraps
// stdout. Bubble Tea uses that interface to detect the output TTY.
func (w *Writer) Fd() uintptr {
	if file, ok := w.out.(interface{ Fd() uintptr }); ok {
		return file.Fd()
	}
	return ^uintptr(0)
}

func (w *Writer) Read(p []byte) (int, error) {
	if reader, ok := w.out.(io.Reader); ok {
		return reader.Read(p)
	}
	return 0, io.EOF
}

func (w *Writer) Close() error {
	return nil
}

// SetSupported records whether the terminal answered the Kitty graphics probe.
func (w *Writer) SetSupported(supported bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.supported != supported {
		w.supported = supported
		w.dirty = true
	}
}

// Supported reports terminal capability without applying the automatic-display preference.
func (w *Writer) Supported() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.supported
}

// RenderingEnabled reports whether both the user setting and terminal support allow images.
func (w *Writer) RenderingEnabled() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.enabled && w.supported
}

// SetEnabled controls whether image markers become terminal overlays.
func (w *Writer) SetEnabled(enabled bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.enabled != enabled {
		w.enabled = enabled
		w.dirty = true
	}
}

// Invalidate rebuilds placements after a resize. Uploaded source data remains
// reusable; actual screen-clear escape sequences invalidate it in Write.
func (w *Writer) Invalidate() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.dirty = true
}

func (w *Writer) SetContent(content string) string {
	clean, overlays := extractOverlays(content)
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.supported {
		overlays = nil
	} else if !w.enabled {
		overlays = slices.DeleteFunc(overlays, func(image overlay) bool { return !image.explicit })
	}
	if !sameOverlays(w.overlays, overlays) {
		w.overlays = overlays
		w.dirty = true
	}
	return clean
}

// RequestFlush coalesces a graphics-only repaint until the output writer runs.
// Text renderers may suppress identical frames even when image markers changed.
func (w *Writer) RequestFlush() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.flushPending {
		return false
	}
	needed := w.dirty && (len(w.overlays) > 0 || w.active)
	if !needed {
		for id := range w.managed {
			if !previewImageRetained(id) {
				needed = true
				break
			}
		}
	}
	if !needed {
		return false
	}
	w.flushPending = true
	return true
}

func (w *Writer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	defer func() { w.flushPending = false }()
	cleared := bytes.Contains(p, []byte("\x1b[2J")) || bytes.Contains(p, []byte("\x1b[?1049h"))
	n, err := w.out.Write(p)
	if err != nil {
		return n, err
	}

	if cleared {
		clear(w.uploaded)
		w.dirty = true
		w.active = false
	}
	visible := make(map[uint32]bool, len(w.overlays))
	for _, image := range w.overlays {
		visible[image.id] = true
	}
	var retired []uint32
	for id := range w.managed {
		if !visible[id] && !previewImageRetained(id) {
			retired = append(retired, id)
		}
	}
	if len(w.overlays) == 0 && !w.active && len(retired) == 0 {
		return n, nil
	}

	wasDirty := w.dirty
	newUploads := make([]uint32, 0, len(w.overlays))
	var b strings.Builder
	capacity := 64 + len(w.overlays)*128 + len(retired)*64
	for _, image := range w.overlays {
		if !w.uploaded[image.id] {
			encoded := base64.StdEncoding.EncodedLen(len(image.png))
			capacity += encoded + (encoded/kittyMaxChunkSize+1)*80
		}
	}
	b.Grow(capacity)
	b.WriteString("\x1b7")
	if wasDirty {
		b.WriteString("\x1b_Ga=d,d=a,q=2\x1b\\")
	}
	for index, image := range w.overlays {
		if image.managed {
			w.managed[image.id] = true
		}
		if !w.uploaded[image.id] {
			writeImageTransmission(&b, image)
			newUploads = append(newUploads, image.id)
		}
		fmt.Fprintf(&b, "\x1b[%d;%dH", image.y+1, image.x+1)
		fmt.Fprintf(&b, "\x1b_Ga=p,i=%d,p=%d,q=2,C=1", image.id, index+1)
		if image.placementCols > 0 {
			fmt.Fprintf(&b, ",c=%d", image.placementCols)
		} else if image.placementRows > 0 {
			fmt.Fprintf(&b, ",r=%d", image.placementRows)
		} else if image.cellHeight == 0 {
			fmt.Fprintf(&b, ",c=%d,r=%d", image.cols, image.rows)
		}
		if image.sourceY > 0 || image.sourceH < image.pixelH {
			fmt.Fprintf(&b, ",x=0,y=%d,w=%d,h=%d", image.sourceY, image.pixelW, image.sourceH)
		}
		b.WriteString("\x1b\\")
	}
	for _, id := range retired {
		fmt.Fprintf(&b, "\x1b_Ga=d,d=I,i=%d,q=2\x1b\\", id)
	}
	b.WriteString("\x1b8")
	overlay := b.String()
	written, writeErr := io.WriteString(w.out, overlay)
	if writeErr == nil && written != len(overlay) {
		writeErr = io.ErrShortWrite
	}
	if writeErr != nil {
		return n, writeErr
	}

	for _, id := range retired {
		delete(w.managed, id)
		delete(w.uploaded, id)
	}
	w.dirty = false
	w.active = len(w.overlays) > 0
	for _, id := range newUploads {
		w.uploaded[id] = true
	}
	return n, nil
}

func writeImageTransmission(b *strings.Builder, image overlay) {
	var encoded [kittyMaxChunkSize]byte
	const rawChunk = kittyMaxChunkSize / 4 * 3
	for offset := 0; offset < len(image.png); offset += rawChunk {
		end := min(offset+rawChunk, len(image.png))
		n := base64.StdEncoding.EncodedLen(end - offset)
		base64.StdEncoding.Encode(encoded[:n], image.png[offset:end])
		more := 0
		if end < len(image.png) {
			more = 1
		}
		if offset == 0 {
			fmt.Fprintf(b, "\x1b_Ga=t,t=d,f=100,i=%d,q=2,m=%d;", image.id, more)
		} else {
			fmt.Fprintf(b, "\x1b_Gm=%d;", more)
		}
		b.Write(encoded[:n])
		b.WriteString("\x1b\\")
	}
}

func sameOverlays(a, b []overlay) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].placementCols != b[i].placementCols || a[i].placementRows != b[i].placementRows || a[i].displayHeight != b[i].displayHeight || a[i].cellHeight != b[i].cellHeight || a[i].explicit != b[i].explicit || a[i].id != b[i].id || a[i].x != b[i].x || a[i].y != b[i].y ||
			a[i].cols != b[i].cols || a[i].rows != b[i].rows ||
			a[i].pixelW != b[i].pixelW || a[i].pixelH != b[i].pixelH ||
			a[i].sourceY != b[i].sourceY || a[i].sourceH != b[i].sourceH {
			return false
		}
	}
	return true
}

func extractOverlays(content string) (string, []overlay) {
	lines := strings.Split(content, "\n")
	overlays := extractMarkerOverlays(lines)
	for y, line := range lines {
		for {
			start := strings.Index(line, "\x1b_G")
			if start < 0 {
				break
			}
			x := ansi.StringWidth(line[:start])
			var encoded strings.Builder
			cols, rows := 0, 0
			end := start
			for strings.HasPrefix(line[end:], "\x1b_G") {
				stop := strings.Index(line[end+3:], "\x1b\\")
				if stop < 0 {
					end += 3
					break
				}
				stop += end + 3
				body := line[end+3 : stop]
				params, payload, _ := strings.Cut(body, ";")
				for param := range strings.SplitSeq(params, ",") {
					key, value, ok := strings.Cut(param, "=")
					if !ok {
						continue
					}
					switch key {
					case "c":
						cols, _ = strconv.Atoi(value)
					case "r":
						rows, _ = strconv.Atoi(value)
					}
				}
				encoded.WriteString(payload)
				end = stop + 2
			}
			data, err := base64.StdEncoding.DecodeString(encoded.String())
			if err == nil && len(data) > 0 && cols > 0 && rows > 0 {
				id := imageID(data)
				overlays = append(overlays, overlay{id: id, png: data, x: x, y: y, cols: cols, rows: rows})
			}
			line = line[:start] + line[end:]
		}
		lines[y] = line
	}
	return strings.Join(lines, "\n"), overlays
}

func extractMarkerOverlays(lines []string) []overlay {
	var overlays []overlay
	for y, line := range lines {
		for {
			start := strings.Index(line, markerPrefix)
			if start < 0 {
				break
			}
			stopRel := strings.Index(line[start+len(markerPrefix):], "\x1b\\")
			if stopRel < 0 {
				line = line[:start] + line[start+len(markerPrefix):]
				continue
			}
			stop := start + len(markerPrefix) + stopRel
			fields := strings.Split(line[start+len(markerPrefix):stop], ";")
			if len(fields) == 4 || (len(fields) == 5 && fields[4] == "preview") || (len(fields) == 7 && fields[4] == "native") || (len(fields) == 11 && fields[4] == "scaled") {
				explicit := len(fields) > 4
				cellHeight := 0
				if len(fields) >= 7 {
					cellHeight, _ = strconv.Atoi(fields[6])
					if cellHeight <= 0 || cellHeight > 512 {
						line = line[:start] + line[stop+2:]
						continue
					}
				}
				id64, idErr := strconv.ParseUint(fields[0], 10, 32)
				cols, colsErr := strconv.Atoi(fields[1])
				totalRows, rowsErr := strconv.Atoi(fields[2])
				row, rowErr := strconv.Atoi(fields[3])
				img, ok := registeredImage(uint32(id64))
				if idErr == nil && colsErr == nil && rowsErr == nil && rowErr == nil && ok && totalRows > 0 && cols > 0 && row >= 0 && row < totalRows {
					x := ansi.StringWidth(line[:start])
					placementCols, placementRows, displayHeight := 0, 0, 0
					if len(fields) == 11 {
						placementCols, _ = strconv.Atoi(fields[7])
						placementRows, _ = strconv.Atoi(fields[8])
						displayHeight, _ = strconv.Atoi(fields[10])
						if placementCols < 0 || placementRows < 0 || (placementCols == 0) == (placementRows == 0) || displayHeight <= 0 {
							line = line[:start] + line[stop+2:]
							continue
						}
					}
					next := markerOverlay(uint32(id64), img, x, y, cols, totalRows, row, explicit, cellHeight)
					next.placementCols, next.placementRows, next.displayHeight = placementCols, placementRows, displayHeight
					if displayHeight > 0 {
						next.sourceY, next.sourceH = 0, img.Height
					}
					if len(overlays) > 0 {
						last := &overlays[len(overlays)-1]
						lastEndRow := last.sourceY + last.sourceH
						expectedSourceY := img.Height * row / totalRows
						if cellHeight > 0 {
							expectedSourceY = min(img.Height, row*cellHeight)
						}
						if last.placementCols == placementCols && last.placementRows == placementRows && last.displayHeight == displayHeight && last.cellHeight == cellHeight && last.explicit == explicit && uint64(last.id) == id64 && last.x == x && last.y+last.rows == y && last.firstRow+last.rows == row && (displayHeight > 0 || lastEndRow == expectedSourceY) {
							last.rows++
							last.sourceH = img.Height*(row+1)/totalRows - last.sourceY
							if displayHeight > 0 {
								last.sourceH = img.Height
							}
							if cellHeight > 0 && displayHeight == 0 {
								last.sourceH = min(img.Height, (row+1)*cellHeight) - last.sourceY
							}
						} else {
							overlays = append(overlays, next)
						}
					} else {
						overlays = append(overlays, next)
					}
				}
			}
			line = line[:start] + line[stop+2:]
		}
		lines[y] = line
	}
	// Scaled source crops can round to a different destination size. Suppress a
	// partially occluded scaled placement rather than paint over another layer.
	return slices.DeleteFunc(overlays, func(image overlay) bool {
		return image.displayHeight > 0 && (image.firstRow != 0 || image.rows != image.totalRows)
	})
}

func markerOverlay(id uint32, img Inline, x, y, cols, totalRows, row int, explicit bool, cellHeight int) overlay {
	sourceY := img.Height * row / totalRows
	sourceEnd := img.Height * (row + 1) / totalRows
	if cellHeight > 0 {
		sourceY, sourceEnd = min(img.Height, row*cellHeight), min(img.Height, (row+1)*cellHeight)
	}
	return overlay{
		explicit: explicit, cellHeight: cellHeight, firstRow: row, totalRows: totalRows,
		id: id, png: img.PNGData, managed: img.registryID != 0, x: x, y: y, cols: cols, rows: 1,
		pixelW: img.Width, pixelH: img.Height,
		sourceY: sourceY, sourceH: sourceEnd - sourceY,
	}
}
