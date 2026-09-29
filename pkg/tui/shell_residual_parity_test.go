package tui

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"hash"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/tui/components/notification"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

// residualViewBytes encodes every exported tea.View field without stripping ANSI
// or normalizing metadata. Callback fields must be nil and are explicitly encoded
// as null. This fails closed if a future View field cannot be serialized.
func residualViewBytes(t testing.TB, view tea.View) []byte {
	t.Helper()
	value := reflect.ValueOf(view)
	typ := value.Type()
	fields := make(map[string]any, value.NumField())
	for i := range value.NumField() {
		field := value.Field(i)
		if !field.CanInterface() {
			t.Fatalf("unexported tea.View field %s needs explicit parity coverage", typ.Field(i).Name)
		}
		if field.Kind() == reflect.Func {
			if !field.IsNil() {
				t.Fatalf("non-nil tea.View callback %s needs explicit parity coverage", typ.Field(i).Name)
			}
			fields[typ.Field(i).Name] = nil
			continue
		}
		fields[typ.Field(i).Name] = field.Interface()
	}
	encoded, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

type residualViewStream struct {
	t       *testing.T
	hash    hash.Hash
	file    *os.File
	count   int
	changed int
	last    string
}

func newResidualViewStream(t *testing.T) *residualViewStream {
	t.Helper()
	stream := &residualViewStream{t: t, hash: sha256.New()}
	// Optional capture keeps bounded raw frames for field-by-field diagnosis.
	// Default test runs retain only a hash and the immediately previous frame.
	if dir := os.Getenv("TUI_RESIDUAL_PARITY_DIR"); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		name := strings.ReplaceAll(t.Name(), "/", "__") + ".jsonl"
		file, err := os.Create(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		stream.file = file
		t.Cleanup(func() {
			if err := file.Close(); err != nil {
				t.Error(err)
			}
		})
	}
	t.Cleanup(func() {
		t.Logf("frames=%d changed=%d fingerprint=%x", stream.count, stream.changed, stream.hash.Sum(nil))
	})
	return stream
}

func (s *residualViewStream) capture(view tea.View) {
	s.t.Helper()
	encoded := residualViewBytes(s.t, view)
	current := string(encoded)
	if s.count > 0 && s.last != current {
		s.changed++
	}
	s.last = current
	s.count++
	encoded = append(encoded, '\n')
	_, _ = s.hash.Write(encoded)
	if s.file != nil {
		if _, err := s.file.Write(encoded); err != nil {
			s.t.Fatal(err)
		}
	}
}

// TestResidualViewStreams supplies byte-identical bounded sequences to baseline
// and candidate builds. Cross-process comparison requires the same test-only
// spinner-label input overlay; the test never normalizes production output.
// TUI_RESIDUAL_PARITY_DIR optionally writes full JSONL tea.View streams.
func TestResidualViewStreams(t *testing.T) {
	for _, panes := range []int{1, 3} {
		for _, draft := range []string{"empty", "long"} {
			for _, cadence := range []string{"clean", "changed", "real"} {
				for _, toast := range []bool{false, true} {
					t.Run(fmt.Sprintf("panes=%d/draft=%s/%s/toast=%t", panes, draft, cadence, toast), func(t *testing.T) {
						root, scheduler := residualMatrixFixture(t, panes, draft, cadence, toast)
						stream := newResidualViewStream(t)
						stream.capture(root.View())
						for range 24 {
							residualTick(t, root, scheduler)
							stream.capture(root.View())
						}
					})
				}
			}
		}
		for _, kind := range []string{"typing", "scroll", "stream"} {
			t.Run(fmt.Sprintf("panes=%d/%s", panes, kind), func(t *testing.T) {
				root := residualInputFixture(t, panes, kind)
				stream := newResidualViewStream(t)
				stream.capture(root.View())
				for step := range 32 {
					residualInput(root, kind, step)
					stream.capture(residualViewSink)
				}
			})
		}
		t.Run(fmt.Sprintf("panes=%d/transitions", panes), func(t *testing.T) {
			original := styles.CurrentTheme()
			t.Cleanup(func() { styles.ApplyTheme(original) })
			root, scheduler := residualPaneFixture(t, "notification", 3, panes, "long")
			stream := newResidualViewStream(t)
			stream.capture(root.View())
			for _, transition := range []func(){
				func() { root.Update(tea.PasteMsg{Content: " pasted 界"}) },
				func() { root.Update(tea.WindowSizeMsg{Width: 130, Height: 42}) },
				func() {
					theme := *original
					theme.Colors.TextMuted = "#123456"
					styles.ApplyTheme(&theme)
					root.Update(messages.ThemeChangedMsg{})
				},
				func() { root.editor.Blur(); root.focusedPanel = PanelContent; root.Update(struct{}{}) },
				func() { root.editor.Focus(); root.focusedPanel = PanelEditor; root.Update(struct{}{}) },
				func() { root.editor.SetValue(""); root.Update(struct{}{}) },
				func() { root.Update(notification.HideMsg{}) },
				func() { root.Update(notification.ShowMsg{Text: "Reopened parity notification"}) },
				func() { root.Update(tea.WindowSizeMsg{Width: 156, Height: 48}) },
			} {
				transition()
				stream.capture(root.View())
				for range 4 {
					residualTick(t, root, scheduler)
					stream.capture(root.View())
				}
			}
		})
	}
}
