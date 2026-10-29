package types

import (
	"path/filepath"
	"strconv"
	"strings"

	"github.com/docker/docker-agent/pkg/chat"
)

const (
	maxProjectedAttachments   = 128
	maxAttachedFileTagSize    = 16 * 1024 * 1024
	maxPreviewAttachmentBytes = 20 * 1024 * 1024
)

// UserAttachment is presentation metadata for an attachment on a sent user
// message. Data is copied from the session payload; rendering never opens paths.
type UserAttachment struct {
	Name              string
	MediaType         string
	SourcePath        string
	Text              string
	Data              []byte
	Available         bool
	UnavailableReason string
}

// ProjectUserAttachments projects structured user payload parts into compact
// transcript metadata. Content remains canonical and unchanged for edit/copy.
func ProjectUserAttachments(content string, parts []chat.MessagePart) (displayContent string, attachments []UserAttachment) {
	displayContent = content
	if len(parts) == 0 {
		return displayContent, nil
	}

	attachments = make([]UserAttachment, 0, min(len(parts), maxProjectedAttachments))
	for _, part := range parts {
		if part.Type != chat.MessagePartTypeText || !strings.HasPrefix(part.Text, content) {
			continue
		}
		for _, block := range parseAttachedFileBlocks(part.Text[len(content):]) {
			if len(attachments) >= maxProjectedAttachments {
				break
			}
			name := filepath.Base(block.path)
			if name == "" || name == "." || name == string(filepath.Separator) {
				name = block.path
			}
			attachments = append(attachments, UserAttachment{
				Name: name, MediaType: "text/plain", SourcePath: block.path,
				Text: block.text, Available: true,
			})
		}
		break
	}

	for _, part := range parts {
		if len(attachments) >= maxProjectedAttachments {
			break
		}
		switch {
		case part.Document != nil:
			doc := part.Document
			attachment := UserAttachment{Name: doc.Name, MediaType: doc.MimeType}
			attachment.Text = doc.Source.InlineText
			if len(doc.Source.InlineData) > maxPreviewAttachmentBytes {
				attachment.Available = false
				attachment.UnavailableReason = "attachment too large to preview"
			} else {
				attachment.Data = append([]byte(nil), doc.Source.InlineData...)
				attachment.Available = doc.Source.ArtifactPath == "" &&
					(doc.Source.InlineText != "" || strings.HasPrefix(doc.MimeType, "text/") || len(doc.Source.InlineData) > 0)
			}
			if doc.Source.ArtifactPath != "" {
				attachment.Available = false
				attachment.UnavailableReason = "stored content unavailable"
			} else if !attachment.Available && attachment.UnavailableReason == "" {
				attachment.UnavailableReason = "stored content unavailable"
			}
			if attachment.Name == "" {
				attachment.Name = "Attachment"
			}
			attachments = append(attachments, attachment)
		case part.File != nil:
			path := part.File.Path
			name := filepath.Base(path)
			if name == "" || name == "." {
				name = "Attachment"
			}
			attachments = append(attachments, UserAttachment{
				Name: name, MediaType: part.File.MimeType, SourcePath: path,
				UnavailableReason: "legacy attachment content unavailable",
			})
		case part.ImageURL != nil:
			attachments = append(attachments, UserAttachment{
				Name: "Image", MediaType: "image", UnavailableReason: "legacy image unavailable",
			})
		}
	}
	assignDocumentSourcePaths(content, attachments)
	return redactAttachmentReferences(content, attachments), attachments
}

type attachedFileBlock struct {
	path string
	text string
}

func parseAttachedFileBlocks(suffix string) []attachedFileBlock {
	const opener = "<attached_file path="
	const closer = "</attached_file>"
	var blocks []attachedFileBlock
	for len(blocks) < maxProjectedAttachments {
		i := strings.Index(suffix, opener)
		if i < 0 {
			if strings.Contains(suffix, closer) {
				return nil
			}
			return blocks
		}
		// The app-generated tags start on their own separated line. Text before
		// the first tag can be notes, but tag-like text after a closing tag must
		// be a clean frame separator or the whole suffix is ambiguous.
		if len(blocks) > 0 && strings.Trim(suffix[:i], "\n") != "" {
			return nil
		}
		if i != 0 && !strings.HasSuffix(suffix[:i], "\n\n") && !strings.HasSuffix(suffix[:i], "\n") {
			if len(blocks) > 0 {
				return nil
			}
			suffix = suffix[i+len(opener):]
			continue
		}
		rest := suffix[i+len(opener):]
		if len(rest) > maxAttachedFileTagSize {
			rest = rest[:maxAttachedFileTagSize]
		}
		path, consumed, ok := parseQuotedString(rest)
		if !ok || consumed >= len(rest) || rest[consumed] != '>' {
			if len(blocks) > 0 {
				return nil
			}
			suffix = suffix[i+len(opener):]
			continue
		}
		bodyStart := consumed + 1
		end := strings.Index(rest[bodyStart:], closer)
		if end < 0 {
			return nil
		}
		body := rest[bodyStart : bodyStart+end]
		if strings.Contains(body, opener) {
			return nil
		}
		body = strings.TrimPrefix(body, "\n")
		body = strings.TrimSuffix(body, "\n")
		blocks = append(blocks, attachedFileBlock{path: path, text: body})
		suffix = rest[bodyStart+end+len(closer):]
		if next := strings.Index(suffix, opener); next >= 0 {
			if strings.Trim(suffix[:next], "\n") != "" {
				return nil
			}
			continue
		}
		if strings.Contains(suffix, closer) {
			return nil
		}
		return blocks
	}
	if strings.Contains(suffix, opener) || strings.Contains(suffix, closer) {
		return nil
	}
	return blocks
}

// parseQuotedString accepts only a complete Go-quoted string and returns the
// byte offset immediately after its closing quote.
func parseQuotedString(s string) (string, int, bool) {
	if s == "" || s[0] != '"' {
		return "", 0, false
	}
	for i := 1; i < len(s); i++ {
		if s[i] == '\\' {
			i++
			continue
		}
		if s[i] == '"' {
			quoted := s[:i+1]
			value, err := strconv.Unquote(quoted)
			return value, i + 1, err == nil
		}
	}
	return "", 0, false
}

func assignDocumentSourcePaths(content string, attachments []UserAttachment) {
	counts := make(map[string]int)
	for _, attachment := range attachments {
		name := filepath.Base(attachment.Name)
		if name != "" && name != "." {
			counts[name]++
		}
	}
	for i := range attachments {
		attachment := &attachments[i]
		if attachment.SourcePath != "" {
			continue
		}
		name := filepath.Base(attachment.Name)
		if name == "" || name == "." || counts[name] != 1 {
			continue
		}
		paths := absoluteReferencesEndingIn(content, name)
		if len(paths) == 1 {
			attachment.SourcePath = paths[0]
		}
	}
}

// AttachmentNumberMarker is the 1-based presentation number shared by the
// inline reference marker and the attachment footer row.
func AttachmentNumberMarker(index int) string {
	return "📎" + strconv.Itoa(index+1)
}

func redactAttachmentReferences(content string, attachments []UserAttachment) string {
	if content == "" || len(attachments) == 0 {
		return content
	}
	// Exact structured source paths are unambiguous. Document names from the
	// modern binary shape are matched only when unique, absolute-path-shaped,
	// and bounded as a standalone @reference; duplicate names are preserved.
	known := make(map[string]struct{})
	markers := make(map[string][]string)
	counts := make(map[string]int)
	for i, attachment := range attachments {
		if attachment.SourcePath != "" && filepath.IsAbs(attachment.SourcePath) {
			known[attachment.SourcePath] = struct{}{}
			markers[attachment.SourcePath] = append(markers[attachment.SourcePath], AttachmentNumberMarker(i))
		}
		name := filepath.Base(attachment.Name)
		if name != "" && name != "." {
			counts[name]++
		}
	}
	for name, count := range counts {
		if count != 1 {
			continue
		}
		paths := absoluteReferencesEndingIn(content, name)
		if len(paths) == 1 {
			known[paths[0]] = struct{}{}
		}
	}
	// Only paths with a presentation number can be replaced; a same-basename
	// reference to another path stays literal.
	for path := range known {
		if len(markers[path]) == 0 {
			delete(known, path)
		}
	}
	if len(known) == 0 {
		return content
	}
	var b strings.Builder
	b.Grow(len(content))
	for i := 0; i < len(content); {
		if content[i] != '@' || !referenceBoundaryBefore(content, i) {
			b.WriteByte(content[i])
			i++
			continue
		}
		matched := ""
		for path := range known {
			if strings.HasPrefix(content[i+1:], path) && referenceBoundaryAfter(content, i+1+len(path)) && len(path) > len(matched) {
				matched = path
			}
		}
		if matched == "" {
			b.WriteByte(content[i])
			i++
			continue
		}
		// Repeated references consume the path's markers in order, then reuse the last
		// one so no occurrence of the user's reference silently disappears.
		queue := markers[matched]
		b.WriteString(queue[0])
		if len(queue) > 1 {
			markers[matched] = queue[1:]
		}
		i += len(matched) + 1
	}
	return b.String()
}

func absoluteReferencesEndingIn(content, name string) []string {
	var matches []string
	seen := make(map[string]struct{})
	for from := 0; from < len(content); {
		rel := strings.Index(content[from:], name)
		if rel < 0 {
			break
		}
		nameStart := from + rel
		from = nameStart + len(name)
		if !referenceBoundaryAfter(content, from) {
			continue
		}
		start := strings.LastIndex(content[:nameStart], "@/")
		if start < 0 || !referenceBoundaryBefore(content, start) {
			continue
		}
		path := content[start+1 : from]
		if strings.ContainsAny(path, "\r\n") || !filepath.IsAbs(path) || filepath.Base(path) != name {
			continue
		}
		if _, ok := seen[path]; !ok {
			seen[path] = struct{}{}
			matches = append(matches, path)
		}
	}
	return matches
}

func referenceBoundaryBefore(s string, i int) bool {
	if i == 0 {
		return true
	}
	c := s[i-1]
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || strings.ContainsRune("([{<,;:", rune(c))
}

func referenceBoundaryAfter(s string, i int) bool {
	if i >= len(s) {
		return true
	}
	c := s[i]
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || strings.ContainsRune(")]}>,;:!?", rune(c))
}
