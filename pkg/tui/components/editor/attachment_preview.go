package editor

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"
)

const maxAttachmentPreviewBytes = 20 << 20

// AttachmentPreview keeps encoded image bytes out of the terminal text path.
type AttachmentPreview struct {
	Title     string
	Content   string
	MIME      string
	ImageData []byte
	IsImage   bool
}

func loadAttachmentPreview(title, path string) AttachmentPreview {
	preview := AttachmentPreview{Title: safePreviewLabel(title)}
	file, err := os.Open(path)
	if err != nil {
		preview.Content = fmt.Sprintf("Unable to read attachment: %v", err)
		preview.Content = safePreviewLabel(preview.Content)
		return preview
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxAttachmentPreviewBytes+1))
	if err != nil {
		preview.Content = "Unable to read attachment: " + safePreviewLabel(err.Error())
		return preview
	}
	if len(data) > maxAttachmentPreviewBytes {
		preview.Content = "Attachment preview unavailable: file exceeds the 20 MiB preview limit."
		return preview
	}
	preview.MIME = http.DetectContentType(data)
	preview.IsImage = strings.HasPrefix(preview.MIME, "image/")
	switch strings.ToLower(filepath.Ext(path)) {
	case ".png", ".jpg", ".jpeg", ".gif", ".webp", ".bmp", ".tiff", ".tif", ".svg", ".avif", ".heic", ".ico":
		preview.IsImage = true
	}
	if preview.IsImage {
		preview.ImageData = data
		return preview
	}
	if !utf8.Valid(data) || strings.ContainsFunc(string(data), func(r rune) bool {
		return unicode.IsControl(r) && r != '\n' && r != '\r' && r != '\t'
	}) {
		preview.Content = "Attachment preview unavailable: binary or terminal-control data (" + preview.MIME + ")."
		return preview
	}
	preview.Content = strings.ReplaceAll(strings.ReplaceAll(string(data), "\r\n", "\n"), "\r", "\n")
	return preview
}

func safePreviewLabel(text string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return '�'
		}
		return r
	}, text)
}
