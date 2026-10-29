package reviewdraft

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"unicode"
)

const (
	userReferencesOpen  = `<user-references version="1">`
	userReferencesClose = `</user-references>`
	// Presentation parsing is deliberately much smaller than canonical payloads.
	// Oversize bodies remain intact and render as ordinary user text.
	maxReferencePayloadBytes = 1 << 20
	maxReferenceLabelBytes   = 4 << 10
	maxReferenceRangeBytes   = 64
)

type ReferenceKind string

const (
	ReferenceGitChange ReferenceKind = "git-change"
	ReferenceFile      ReferenceKind = "file"
)

type CharacterRange struct {
	Start int `json:"start"`
	End   int `json:"end"`
}

// Reference is the human-readable, non-privileged location and exact selected
// source snippet associated with one comment. Internal draft IDs and source
// fingerprints are deliberately absent.
type Reference struct {
	Kind           ReferenceKind   `json:"kind"`
	Repository     string          `json:"repository"`
	Checkout       string          `json:"checkout"`
	Branch         string          `json:"branch,omitempty"`
	Path           string          `json:"path"`
	OldPath        string          `json:"old_path,omitempty"`
	Comparison     string          `json:"comparison,omitempty"`
	OldLines       string          `json:"old_lines,omitempty"`
	NewLines       string          `json:"new_lines,omitempty"`
	CharacterRange *CharacterRange `json:"character_range,omitempty"`
	Snippet        []string        `json:"snippet"`
	Comment        string          `json:"comment"`
}

// ReferencePayload is the versioned envelope body recognized by the TUI.
// Its outer marker never changes the message's human USER role.
type ReferencePayload struct {
	Comments []Reference `json:"comments,omitempty"`
	General  string      `json:"general,omitempty"`
}

// ParseReferences accepts a complete version-1 envelope plus one optional queued-input newline.
func ParseReferences(body string) (ReferencePayload, bool) {
	var empty ReferencePayload
	if len(body) > maxReferencePayloadBytes || !strings.HasPrefix(body, userReferencesOpen) {
		return empty, false
	}
	trimmed, _ := strings.CutSuffix(body, "\n")
	if !strings.HasSuffix(trimmed, userReferencesClose) {
		return empty, false
	}
	payloadStart := len(userReferencesOpen)
	payloadEnd := len(trimmed) - len(userReferencesClose)
	if payloadEnd-payloadStart < 2 || body[payloadStart:payloadStart+1] != "\n" {
		return empty, false
	}
	jsonBody := body[payloadStart+1 : payloadEnd]
	if !strings.HasSuffix(jsonBody, "\n") {
		return empty, false
	}
	payloadBytes := []byte(strings.TrimSuffix(jsonBody, "\n"))
	dec := json.NewDecoder(bytes.NewReader(payloadBytes))
	dec.DisallowUnknownFields()
	var parsed ReferencePayload
	if err := dec.Decode(&parsed); err != nil {
		return empty, false
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return empty, false
	}
	canonical, err := json.Marshal(parsed)
	if err != nil || !bytes.Equal(canonical, payloadBytes) {
		return empty, false
	}
	if len(parsed.Comments) > MaxComments || len(parsed.Comments) == 0 && parsed.General == "" || len(parsed.General) > MaxBodyBytes {
		return empty, false
	}
	contextLines, contextBytesTotal := 0, 0
	for _, ref := range parsed.Comments {
		if ref.Kind != ReferenceGitChange && ref.Kind != ReferenceFile || ref.Repository == "" || ref.Checkout == "" || filepathIsUnsafe(ref.Path) || ref.Comment == "" || len(ref.Comment) > MaxBodyBytes || len(ref.Snippet) == 0 {
			return empty, false
		}
		if ref.OldPath != "" && filepathIsUnsafe(ref.OldPath) {
			return empty, false
		}
		labelsBytes := len(ref.Repository) + len(ref.Checkout) + len(ref.Branch) + len(ref.Path) + len(ref.OldPath) + len(ref.Comparison)
		if labelsBytes > maxReferenceLabelBytes || len(ref.Repository) > maxReferenceLabelBytes || len(ref.Checkout) > maxReferenceLabelBytes || len(ref.Branch) > maxReferenceLabelBytes || len(ref.Path) > maxReferenceLabelBytes || len(ref.OldPath) > maxReferenceLabelBytes || len(ref.Comparison) > maxReferenceLabelBytes || len(ref.OldLines) > maxReferenceRangeBytes || len(ref.NewLines) > maxReferenceRangeBytes {
			return empty, false
		}
		if ref.Kind == ReferenceFile && ref.Comparison != "" {
			return empty, false
		}
		if ref.CharacterRange != nil && (ref.CharacterRange.Start < 0 || ref.CharacterRange.End < 0 || len(ref.Snippet) == 1 && ref.CharacterRange.End < ref.CharacterRange.Start) {
			return empty, false
		}
		if len(ref.Snippet) > MaxContextLines {
			return empty, false
		}
		commentContextBytes := 0
		for _, line := range ref.Snippet {
			commentContextBytes += len(line)
		}
		if commentContextBytes > MaxContextBytes {
			return empty, false
		}
		contextLines += len(ref.Snippet)
		contextBytesTotal += commentContextBytes
		if contextLines > MaxComments*MaxContextLines || contextBytesTotal > MaxComments*MaxContextBytes {
			return empty, false
		}
	}
	return parsed, true
}

func serializeReferences(payload ReferencePayload) string {
	data, _ := json.Marshal(payload)
	return userReferencesOpen + "\n" + string(data) + "\n" + userReferencesClose
}

// RenderReferences returns a human-readable projection of a complete typed
// reference payload. The source body remains untouched for copy/edit/history.
func RenderReferences(body string) (string, bool) {
	payload, ok := ParseReferences(body)
	if !ok {
		return "", false
	}
	if len(payload.Comments) == 0 {
		return presentationText(payload.General), true
	}
	locations := make(map[string]map[string]struct{})
	for _, ref := range payload.Comments {
		for _, path := range []string{ref.Path, ref.OldPath} {
			if path == "" {
				continue
			}
			if locations[path] == nil {
				locations[path] = make(map[string]struct{})
			}
			locations[path][ref.Repository+"\x00"+ref.Checkout] = struct{}{}
		}
	}
	var b strings.Builder
	for i, ref := range payload.Comments {
		if i > 0 {
			b.WriteString("\n\n")
		}
		if ref.Kind == ReferenceGitChange {
			b.WriteString("Git · ")
		}
		path := ref.Path
		if ref.OldPath != "" {
			path = ref.OldPath + "→" + ref.Path
		}
		b.WriteString(presentationLabel(path))
		if ref.OldLines != "" || ref.NewLines != "" {
			b.WriteByte(':')
			if ref.OldLines != "" {
				b.WriteString(presentationLabel(ref.OldLines))
			}
			if ref.NewLines != "" {
				if ref.OldLines != "" {
					b.WriteString("→")
				}
				b.WriteString(presentationLabel(ref.NewLines))
			} else if ref.OldLines != "" {
				b.WriteString("→∅")
			}
		}
		if ref.Kind == ReferenceGitChange && ref.Comparison != "" {
			b.WriteString(" · ")
			b.WriteString(presentationLabel(ref.Comparison))
		}
		if ref.CharacterRange != nil {
			fmt.Fprintf(&b, " · chars %d–%d", ref.CharacterRange.Start, ref.CharacterRange.End)
		}
		for _, path := range []string{ref.Path, ref.OldPath} {
			if len(locations[path]) <= 1 {
				continue
			}
			b.WriteString(" · ")
			b.WriteString(presentationLabel(ref.Repository))
			b.WriteByte('@')
			checkout := filepath.Base(ref.Checkout)
			for _, other := range payload.Comments {
				if (other.Repository != ref.Repository || other.Checkout != ref.Checkout) && (other.Path == path || other.OldPath == path) && filepath.Base(other.Checkout) == filepath.Base(ref.Checkout) {
					checkout = ref.Checkout
					break
				}
			}
			b.WriteString(presentationLabel(checkout))
			break
		}
		b.WriteByte('\n')
		for lineIndex, line := range ref.Snippet {
			if lineIndex > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(presentationText(line))
		}
		b.WriteString("\n\n")
		b.WriteString(presentationText(ref.Comment))
	}
	if payload.General != "" {
		b.WriteString("\n\n")
		b.WriteString(presentationText(payload.General))
	}
	return b.String(), true
}

func presentationLabel(value string) string {
	value = strings.ReplaceAll(value, "\n", "␊")
	value = strings.ReplaceAll(value, "\t", "␉")
	return presentationText(value)
}

func presentationText(value string) string {
	var b strings.Builder
	for _, r := range value {
		switch {
		case r == '\x1b':
			b.WriteRune('␛')
		case r == '\r':
			b.WriteRune('␍')
		case r == '\x7f':
			b.WriteRune('␡')
		case unicode.IsControl(r) && r != '\n' && r != '\t':
			b.WriteRune(0x2400 + r)
		case unicode.Is(unicode.Cf, r) || unicode.Is(unicode.Zl, r) || unicode.Is(unicode.Zp, r):
			fmt.Fprintf(&b, "⟦U+%04X⟧", r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
