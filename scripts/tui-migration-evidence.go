//go:build ignore

// Run with go run scripts/tui-migration-evidence.go. This is an evidence-only
// decoder, not a terminal emulator or production output normalization.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"unicode/utf8"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/rivo/uniseg"
)

type surface struct {
	Rows     [][]uv.Cell
	EndStyle uv.Style
	EndLink  uv.Link
}

// validateSGR deliberately supports only an audited subset. Unknown attributes,
// malformed colors, private parameters and subparameters fail closed instead of
// being silently ignored by the underlying styling decoder.
func validateSGR(seq string) error {
	if !strings.HasPrefix(seq, "\x1b[") || !strings.HasSuffix(seq, "m") {
		return fmt.Errorf("unsupported SGR %q", seq)
	}
	body := seq[2 : len(seq)-1]
	if body == "" {
		return nil
	}
	parts := strings.Split(body, ";")
	values := make([]int, len(parts))
	for i, part := range parts {
		if part == "" {
			return fmt.Errorf("empty SGR parameter %q", seq)
		}
		for _, r := range part {
			if r < '0' || r > '9' {
				return fmt.Errorf("unsupported SGR parameter %q", seq)
			}
		}
		v, err := strconv.Atoi(part)
		if err != nil {
			return err
		}
		values[i] = v
	}
	for i := 0; i < len(values); i++ {
		v := values[i]
		switch {
		case v == 38 || v == 48 || v == 58:
			if i+1 >= len(values) {
				return fmt.Errorf("incomplete color %q", seq)
			}
			n := 0
			switch values[i+1] {
			case 2:
				n = 3
			case 5:
				n = 1
			default:
				return fmt.Errorf("unsupported color %q", seq)
			}
			if i+1+n >= len(values) {
				return fmt.Errorf("incomplete color %q", seq)
			}
			for _, c := range values[i+2 : i+2+n] {
				if c > 255 {
					return fmt.Errorf("invalid color %q", seq)
				}
			}
			i += n + 1
		case v >= 0 && v <= 9, v >= 22 && v <= 25, v >= 27 && v <= 29, v >= 30 && v <= 37, v == 39, v >= 40 && v <= 47, v == 49, v == 59, v >= 90 && v <= 97, v >= 100 && v <= 107:
		default:
			return fmt.Errorf("unsupported SGR %q", seq)
		}
	}
	return nil
}

func decode(text string) (surface, error) {
	result := surface{Rows: [][]uv.Cell{{}}}
	if !utf8.ValidString(text) {
		return result, fmt.Errorf("invalid UTF-8")
	}
	p := ansi.NewParser()
	p.SetParamsSize(256)
	var state byte
	for len(text) > 0 {
		// DecodeSequence's ASCII fast path may split a following combining
		// mark. Segment each uninterrupted printable run into full graphemes.
		if text[0] != '\x1b' && text[0] != '\n' {
			end := strings.IndexAny(text, "\x1b\n")
			if end < 0 {
				end = len(text)
			}
			run := text[:end]
			for _, r := range run {
				if r < 32 || r == 127 || r >= 128 && r < 160 {
					return result, fmt.Errorf("unsupported control in printable run %q", run)
				}
			}
			graphemes := uniseg.NewGraphemes(run)
			for graphemes.Next() {
				cluster := graphemes.Str()
				width := ansi.StringWidth(cluster)
				if width <= 0 {
					return result, fmt.Errorf("standalone zero-width grapheme %q", cluster)
				}
				cell := uv.Cell{Content: cluster, Width: width, Style: result.EndStyle, Link: result.EndLink}
				row := len(result.Rows) - 1
				result.Rows[row] = append(result.Rows[row], cell)
				for j := 1; j < width; j++ {
					result.Rows[row] = append(result.Rows[row], uv.Cell{Style: cell.Style, Link: cell.Link})
				}
			}
			text = text[end:]
			continue
		}
		seq, width, n, next := ansi.DecodeSequence(text, state, p)
		if n <= 0 {
			return result, fmt.Errorf("decoder made no progress")
		}
		text, state = text[n:], next
		switch {
		case width > 0:
			for _, r := range seq {
				if r < 32 || r == 127 || r >= 128 && r < 160 {
					return result, fmt.Errorf("unsupported printable control %q", seq)
				}
			}
			cell := uv.Cell{Content: seq, Width: width, Style: result.EndStyle, Link: result.EndLink}
			row := len(result.Rows) - 1
			result.Rows[row] = append(result.Rows[row], cell)
			// Preserve wide-cell occupied columns and their attributes explicitly.
			for j := 1; j < width; j++ {
				result.Rows[row] = append(result.Rows[row], uv.Cell{Style: cell.Style, Link: cell.Link})
			}
		case seq == "\n":
			result.Rows = append(result.Rows, []uv.Cell{})
		case strings.HasPrefix(seq, "\x1b["):
			if err := validateSGR(seq); err != nil {
				return result, err
			}
			uv.ReadStyle(p.Params(), &result.EndStyle)
		case strings.HasPrefix(seq, "\x1b]8;"):
			body := strings.TrimPrefix(seq, "\x1b]8;")
			if strings.HasSuffix(body, "\x1b\\") {
				body = strings.TrimSuffix(body, "\x1b\\")
			} else if strings.HasSuffix(body, "\a") {
				body = strings.TrimSuffix(body, "\a")
			} else {
				return result, fmt.Errorf("unterminated OSC8 %q", seq)
			}
			params, url, ok := strings.Cut(body, ";")
			if !ok {
				return result, fmt.Errorf("invalid OSC8 %q", seq)
			}
			for _, r := range body {
				if r < 32 || r == 127 {
					return result, fmt.Errorf("control in OSC8 %q", seq)
				}
			}
			result.EndLink = uv.Link{Params: params, URL: url}
		default:
			return result, fmt.Errorf("unsupported control/escape %q", seq)
		}
	}
	if state != 0 {
		return result, fmt.Errorf("incomplete escape, state %d", state)
	}
	return result, nil
}

func selftest() error {
	for _, tc := range []struct {
		name, a, b string
		equal      bool
	}{
		{"segmentation", "\x1b[31mab\x1b[m", "\x1b[31ma\x1b[31mb\x1b[0m", true},
		{"combining-grapheme", "\x1b[31máb\x1b[m", "\x1b[31má\x1b[31mb\x1b[0m", true},
		{"emoji-zwj", "\x1b[31m👩‍💻x\x1b[m", "\x1b[31m👩‍💻\x1b[31mx\x1b[0m", true},
		{"background", "\x1b[41mx\x1b[m", "\x1b[42mx\x1b[m", false},
		{"attribute", "\x1b[1mx\x1b[m", "x", false},
		{"default-vs-rgb", "x", "\x1b[38;2;192;192;192mx\x1b[m", false},
		{"indexed-vs-rgb", "\x1b[38;5;196mx\x1b[m", "\x1b[38;2;255;0;0mx\x1b[m", false},
		{"hyperlink", "\x1b]8;;https://a\x1b\\x\x1b]8;;\x1b\\", "\x1b]8;;https://b\x1b\\x\x1b]8;;\x1b\\", false},
		{"hyperlink-closure", "\x1b]8;;https://a\x1b\\x\x1b]8;;\x1b\\", "\x1b]8;;https://a\x1b\\x", false},
		{"trailing-reset", "\x1b[31mx\x1b[m", "\x1b[31mx", false},
		{"wide-cell", "界", "界 ", false},
	} {
		a, err := decode(tc.a)
		if err != nil {
			return fmt.Errorf("%s: %w", tc.name, err)
		}
		b, err := decode(tc.b)
		if err != nil {
			return fmt.Errorf("%s: %w", tc.name, err)
		}
		if reflect.DeepEqual(a, b) != tc.equal {
			return fmt.Errorf("selftest %s failed", tc.name)
		}
	}
	wide, _ := decode("\x1b[41m界\x1b[m")
	changed, _ := decode("\x1b[41m界\x1b[m")
	if len(wide.Rows[0]) != 2 || wide.Rows[0][1].Width != 0 {
		return fmt.Errorf("wide continuation missing")
	}
	changed.Rows[0][1].Style = uv.Style{}
	if reflect.DeepEqual(wide, changed) {
		return fmt.Errorf("wide continuation attributes ignored")
	}
	for _, bad := range []string{"\x1b[2J", "\x1b[999m", "\x1b[38;2;999;0;0m", "\x1b[", "\x1b]0;title\a", "\r", "\t", "\x1b[4:3m", "\x1b]8;;unterminated"} {
		if _, err := decode(bad); err == nil {
			return fmt.Errorf("accepted unsupported escape %q", bad)
		}
	}
	return nil
}

func compare(dir, label string) error {
	files, err := filepath.Glob(filepath.Join(dir, "baseline-frames", "*.jsonl"))
	if err != nil {
		return err
	}
	other, err := filepath.Glob(filepath.Join(dir, label+"-frames", "*.jsonl"))
	if err != nil {
		return err
	}
	if len(files) == 0 || len(files) != len(other) {
		return fmt.Errorf("stream sets differ or empty")
	}
	frames, rawDifferent, semanticDifferent, bytesA, bytesB := 0, 0, 0, 0, 0
	differences := []any{}
	for _, file := range files {
		a, err := os.ReadFile(file)
		if err != nil {
			return err
		}
		b, err := os.ReadFile(filepath.Join(dir, label+"-frames", filepath.Base(file)))
		if err != nil {
			return err
		}
		aa, bb := strings.Split(strings.TrimSuffix(string(a), "\n"), "\n"), strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
		if len(aa) != len(bb) {
			return fmt.Errorf("frame counts differ %s", file)
		}
		for i := range aa {
			var av, bv map[string]json.RawMessage
			if err = json.Unmarshal([]byte(aa[i]), &av); err != nil {
				return err
			}
			if err = json.Unmarshal([]byte(bb[i]), &bv); err != nil {
				return err
			}
			var ac, bc string
			if err = json.Unmarshal(av["Content"], &ac); err != nil {
				return err
			}
			if err = json.Unmarshal(bv["Content"], &bc); err != nil {
				return err
			}
			delete(av, "Content")
			delete(bv, "Content")
			as, err := decode(ac)
			if err != nil {
				return fmt.Errorf("baseline %s frame%d: %w", file, i, err)
			}
			bs, err := decode(bc)
			if err != nil {
				return fmt.Errorf("candidate %s frame%d: %w", file, i, err)
			}
			frames++
			bytesA += len(ac)
			bytesB += len(bc)
			if aa[i] != bb[i] {
				rawDifferent++
			}
			metadataEqual := reflect.DeepEqual(av, bv)
			if !reflect.DeepEqual(as, bs) || !metadataEqual {
				semanticDifferent++
				if len(differences) < 32 {
					d := map[string]any{"stream": filepath.Base(file), "frame": i, "metadata_equal": metadataEqual, "ending_style_equal": reflect.DeepEqual(as.EndStyle, bs.EndStyle), "ending_link_equal": as.EndLink == bs.EndLink, "baseline_height": len(as.Rows), "candidate_height": len(bs.Rows)}
					found := false
					for y := 0; y < min(len(as.Rows), len(bs.Rows)) && !found; y++ {
						if len(as.Rows[y]) != len(bs.Rows[y]) {
							d["different_row_width"] = y
							found = true
							break
						}
						for x := range as.Rows[y] {
							if !reflect.DeepEqual(as.Rows[y][x], bs.Rows[y][x]) {
								d["cell"] = map[string]any{"x": x, "y": y, "baseline": fmt.Sprintf("%#v", as.Rows[y][x]), "candidate": fmt.Sprintf("%#v", bs.Rows[y][x])}
								found = true
								break
							}
						}
					}
					differences = append(differences, d)
				}
			}
		}
	}
	result := map[string]any{"streams": len(files), "frames": frames, "raw_different_frames": rawDifferent, "semantic_different_frames": semanticDifferent, "baseline_content_bytes": bytesA, "candidate_content_bytes": bytesB, "first_differences": differences}
	encoded, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return err
	}
	if err = os.WriteFile(filepath.Join(dir, "baseline-vs-"+label+"-semantic.json"), append(encoded, '\n'), 0600); err != nil {
		return err
	}
	fmt.Printf("Complete-View comparison: streams=%d frames=%d rawDifferent=%d semanticDifferent=%d ContentBytes=%d→%d\n", len(files), frames, rawDifferent, semanticDifferent, bytesA, bytesB)
	if semanticDifferent > 0 {
		return fmt.Errorf("styled cells, complete metadata, or ending state differ")
	}
	return nil
}

func main() {
	if err := selftest(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if len(os.Args) == 2 && os.Args[1] == "-selftest" {
		fmt.Println("Strict semantic comparator selftests PASS")
		return
	}
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: go run scripts/tui-migration-evidence.go <artifacts> <candidate-label>")
		os.Exit(2)
	}
	if err := compare(os.Args[1], os.Args[2]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
