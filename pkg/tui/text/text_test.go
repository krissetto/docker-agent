package text

import (
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/rivo/uniseg"
)

func TestGraphemeEditing(t *testing.T) {
	for _, value := range []string{"e\u0301", "界", "👩🏽‍💻", "🇫🇷", "a\u0308\u0301"} {
		t.Run(value, func(t *testing.T) {
			s := New(Options{Multiline: true})
			s.SetValue("x" + value + "y")
			s.Apply(Action{Kind: MoveLeft})
			s.Apply(Action{Kind: MoveLeft})
			if s.Column() != 1 {
				t.Fatalf("left split grapheme: %v", s.Cursor())
			}
			s.Delete()
			if s.Value() != "xy" {
				t.Fatalf("delete: %q", s.Value())
			}
			s.Insert(value)
			s.Backspace()
			if s.Value() != "xy" {
				t.Fatalf("backspace: %q", s.Value())
			}
			s.SetValue(value)
			s.SetCursor(Position{Column: 1})
			if utf8.RuneCountInString(value) > 1 && s.Column() != 0 {
				t.Fatal("cursor not snapped")
			}
		})
	}
}
func TestSelectionAndLimits(t *testing.T) {
	s := New(Options{Multiline: true, CharLimit: 8})
	s.SetValue("a界\nbc")
	s.Select(Position{1, 1}, Position{0, 1})
	if s.SelectedText() != "界\nb" {
		t.Fatalf("selection: %q", s.SelectedText())
	}
	s.Insert("👩🏽‍💻")
	if s.Value() != "a👩🏽‍💻c" || s.Column() != 5 {
		t.Fatalf("replace: %q %v", s.Value(), s.Cursor())
	}
	s.SetCharLimit(7)
	s.Insert("e\u0301x")
	if s.Value() != "a👩🏽‍💻c" {
		t.Fatalf("partial grapheme inserted: %q", s.Value())
	}
	s.SelectAll()
	s.Delete()
	if s.Value() != "" {
		t.Fatal("selection deletion")
	}
	s.SetValue("123456e\u0301")
	if s.Value() != "123456" {
		t.Fatalf("limit split cluster: %q", s.Value())
	}
	s.SetValue("a\nb")
	s.SetCursor(Position{1, 0})
	s.Backspace()
	if s.Value() != "ab" || s.Cursor() != (Position{0, 1}) {
		t.Fatal("newline backspace")
	}
	s.Newline()
	if s.Value() != "a\nb" {
		t.Fatal("newline insertion")
	}
}
func TestCopyAndSnapshotIsolation(t *testing.T) {
	s := New(Options{Multiline: true})
	s.SetValue("hello\n界e\u0301")
	copy := s
	clone := s.Clone()
	before := s.Value()
	rev := s.Revision()
	copy.Backspace()
	clone.Insert("!")
	if s.Value() != before || s.Revision() != rev {
		t.Fatal("copy modified source")
	}
	config := Config{Width: 4, Height: 2, Wrap: true}
	first := s.Layout(config)
	want := s.Layout(config)
	first.Rows[0].Text = "broken"
	first.Rows[0].Runs[0].Text = "broken"
	if !reflect.DeepEqual(want, s.Layout(config)) {
		t.Fatal("layout aliases document")
	}
	s.NormalizeViewport(config)
	rev = s.Revision()
	content := s.ContentRevision()
	x, y := s.ScrollXOffset(), s.ScrollYOffset()
	for range 10 {
		_ = s.Layout(config)
		_ = s.LineInfo(config)
		_ = s.PositionAtCell(config, 0, 2)
	}
	if rev != s.Revision() || content != s.ContentRevision() || x != s.ScrollXOffset() || y != s.ScrollYOffset() {
		t.Fatal("read changed state")
	}
	s.SetCursor(Position{})
	if s.ContentRevision() != content || s.Revision() <= rev {
		t.Fatal("revision categories")
	}
}
func TestWrapGeometryAndMotion(t *testing.T) {
	s := New(Options{Multiline: true})
	s.SetValue("ab 界cd\nx\n12345")
	config := Config{Width: 5, Height: 2, Wrap: true}
	layout := s.Layout(config)
	texts := make([]string, len(layout.Rows))
	for i, row := range layout.Rows {
		texts[i] = row.Text
	}
	want := []string{"ab ", "界cd", "x", "12345", ""}
	if !reflect.DeepEqual(texts, want) {
		t.Fatalf("rows %q want %q", texts, want)
	}
	s.SetCursor(Position{0, 5})
	info := s.LineInfo(config)
	if info != (LineInfo{Width: 3, CharWidth: 4, Height: 2, StartColumn: 3, ColumnOffset: 2, CharOffset: 3, RowOffset: 1}) {
		t.Fatalf("line info: %+v", info)
	}
	s.Apply(Action{Kind: MoveDown, Config: config})
	if s.Cursor() != (Position{1, 1}) {
		t.Fatalf("short line: %v", s.Cursor())
	}
	s.Apply(Action{Kind: MoveDown, Config: config})
	if s.Cursor() != (Position{2, 3}) {
		t.Fatalf("preferred cell: %v", s.Cursor())
	}
	s.NormalizeViewport(config)
	if s.ScrollYOffset() != 2 {
		t.Fatalf("scroll %d", s.ScrollYOffset())
	}
	if got := s.PositionAtCell(config, 1, 2); got != (Position{2, 2}) {
		t.Fatalf("local hit: %v", got)
	}
	s.ScrollBy(-100, config)
	if s.ScrollYOffset() != 0 {
		t.Fatal("scroll clamp")
	}
}
func TestHorizontalPasswordAndTabs(t *testing.T) {
	s := New(Options{})
	s.SetValue("a界e\u0301👩🏽‍💻")
	config := Config{Width: 3, Height: 1}
	s.NormalizeViewport(config)
	layout := s.Layout(config)
	if layout.Cursor.Column != 6 || layout.ScrollX != 4 {
		t.Fatalf("horizontal: %+v", layout)
	}
	if got := s.PositionAtCell(config, 0, 0); got.Column != 4 {
		t.Fatalf("scrolled hit: %v", got)
	}
	config.Mask = '*'
	s.NormalizeViewport(config)
	layout = s.Layout(config)
	if layout.Rows[0].Text != "****" || layout.Cursor.Column != 4 || layout.ScrollX != 2 {
		t.Fatalf("mask: %+v", layout)
	}
	for _, run := range layout.Rows[0].Runs {
		if run.Text != "*" {
			t.Fatal("password leak")
		}
	}
	config.EchoNone = true
	s.NormalizeViewport(config)
	layout = s.Layout(config)
	if layout.Rows[0].Text != "" || layout.Cursor.Column != 0 || layout.ScrollX != 0 {
		t.Fatalf("hidden: %+v", layout)
	}
	for _, run := range layout.Rows[0].Runs {
		if run.Text != "" {
			t.Fatal("hidden leak")
		}
	}
	s.SetValue("a\t界")
	layout = s.Layout(Config{Width: 20})
	if layout.Rows[0].Text != "a   界" || layout.Rows[0].Cells != 6 {
		t.Fatal("tab geometry")
	}
}
func TestConversions(t *testing.T) {
	value := "a界e\u0301👩🏽‍💻"
	for column := 0; column <= utf8.RuneCountInString(value); column++ {
		b := RuneToByte(value, column)
		if !utf8.ValidString(value[:b]) || ByteToRune(value, b) != column {
			t.Fatalf("rune roundtrip %d", column)
		}
	}
	if ByteToRune("界", 2) != 0 || RuneToByte("界", 100) != 3 {
		t.Fatal("clamping")
	}
	g := uniseg.NewGraphemes(value)
	column, cell := 0, 0
	for g.Next() {
		if RuneToCell(value, column, Config{}) != cell || CellToRune(value, cell, Config{}) != column {
			t.Fatalf("cell roundtrip column=%d cell=%d", column, cell)
		}
		column += utf8.RuneCountInString(g.Str())
		cell += g.Width()
	}
}
func TestActionsAndSanitation(t *testing.T) {
	s := New(Options{})
	s.Insert("one  two\r\nthree\x1b")
	if s.Value() != "one  two three" {
		t.Fatalf("sanitization %q", s.Value())
	}
	s.Apply(Action{Kind: MoveWordLeft})
	if s.Column() != 9 {
		t.Fatal(s.Cursor())
	}
	s.Apply(Action{Kind: MoveWordLeft, Select: true})
	if s.SelectedText() != "two " {
		t.Fatal(s.SelectedText())
	}
	s.Apply(Action{Kind: DeleteWordBackward})
	if s.Value() != "one  three" {
		t.Fatal(s.Value())
	}
	s.Apply(Action{Kind: MoveDocumentStart})
	s.Apply(Action{Kind: MoveWordRight})
	if s.Column() != 3 {
		t.Fatal(s.Cursor())
	}
	if s.Word() != "one" {
		t.Fatal(s.Word())
	}
	s.Apply(Action{Kind: DeleteToEnd})
	if s.Value() != "one" {
		t.Fatal(s.Value())
	}
	s.Apply(Action{Kind: DeleteToStart})
	if s.Value() != "" {
		t.Fatal(s.Value())
	}
}
func TestConcurrentCopiedLayouts(t *testing.T) {
	s := New(Options{Multiline: true})
	s.SetValue(strings.Repeat("界 abc e\u0301\n", 30))
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Go(func() {
			clone := s.Clone()
			for width := 1; width < 20; width++ {
				_ = clone.Layout(Config{Width: width, Wrap: true})
				clone.Insert("!")
			}
		})
	}
	wg.Wait()
}
func FuzzGraphemeEditing(f *testing.F) {
	for _, value := range []string{"", "a界e\u0301👩🏽‍💻\nz", "🇫🇷\t1", "\xff\x1b\r\n", "\u0301a"} {
		f.Add(value, uint8(4))
	}
	f.Fuzz(func(t *testing.T, value string, width uint8) {
		if len(value) > 4096 {
			t.Skip()
		}
		s := New(Options{Multiline: true})
		s.SetValue(value)
		clean := s.Value()
		config := Config{Width: int(width%40) + 1, Height: 3, Wrap: true}
		original := s.Clone()
		limit := utf8.RuneCountInString(clean) + 1
		for n := 0; s.Value() != ""; n++ {
			if n > limit {
				t.Fatal("backspace made no progress")
			}
			before := s.Value()
			s.Backspace()
			if !utf8.ValidString(s.Value()) || len(s.Value()) >= len(before) {
				t.Fatal("invalid deletion")
			}
		}
		if original.Value() != clean {
			t.Fatal("copy changed")
		}
		rows := original.Layout(config).Rows
		for row, r := range rows {
			for _, run := range r.Runs {
				p := original.PositionAtCell(config, row, run.Cell)
				if p.Line != r.Line || p.Column < r.StartColumn || p.Column > r.EndColumn {
					t.Fatalf("hit outside row: %v %+v", p, r)
				}
			}
		}
	})
}
func BenchmarkLayoutCached(b *testing.B) {
	for _, lines := range []int{1, 100, 1000} {
		b.Run(fmt.Sprint(lines), func(b *testing.B) {
			s := New(Options{Multiline: true})
			s.SetValue(strings.Repeat("Unicode 界 e\u0301 👩🏽‍💻 words and wrapping text\n", lines))
			config := Config{Width: 30, Height: 8, Wrap: true}
			_ = s.Layout(config)
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				_ = s.Layout(config)
			}
		})
	}
}
func BenchmarkEditAndScroll(b *testing.B) {
	s := New(Options{Multiline: true})
	s.SetValue(strings.Repeat("Unicode 界 e\u0301 👩🏽‍💻 words and wrapping text\n", 1000))
	config := Config{Width: 30, Height: 8, Wrap: true}
	s.NormalizeViewport(config)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		s.Insert("界")
		s.NormalizeViewport(config)
		_ = s.Layout(config)
		s.Backspace()
		s.ScrollBy(-1, config)
	}
}

func TestLayoutMetadataParity(t *testing.T) {
	for _, value := range []string{"", "abcde", "a界e\u0301\n👩🏽‍💻\txyz\n", strings.Repeat("long words 界\n", 20)} {
		s := New(Options{Multiline: true})
		s.SetValue(value)
		for _, config := range []Config{{Width: 5, Height: 2, Wrap: true}, {Width: 1, Wrap: true}, {Width: 4}, {Width: 3, Wrap: true, Mask: '*'}, {Width: 2, EchoNone: true}} {
			s.Apply(Action{Kind: MoveDocumentEnd})
			for {
				before := s.Revision()
				layout := s.Layout(config)
				if got := s.VisualLineCount(config); got != len(layout.Rows) {
					t.Fatalf("count %d != %d", got, len(layout.Rows))
				}
				if got := s.CursorCell(config); got != layout.Cursor {
					t.Fatalf("cursor %v != %v, %q at %v", got, layout.Cursor, value, s.Cursor())
				}
				if s.Revision() != before {
					t.Fatal("metadata read changed state")
				}
				if s.Cursor() == (Position{}) {
					break
				}
				s.Apply(Action{Kind: MoveLeft})
			}
			if got := testing.AllocsPerRun(20, func() { _ = s.VisualLineCount(config); _ = s.CursorCell(config) }); got != 0 {
				t.Fatalf("cached metadata allocated %v", got)
			}
		}
	}
}

func TestLayoutRunAppendIsolation(t *testing.T) {
	s := New(Options{Multiline: true})
	s.SetValue("ab\ncd")
	layout := s.Layout(Config{Width: 10})
	next := layout.Rows[1].Runs[0]
	layout.Rows[0].Runs = append(layout.Rows[0].Runs, Run{Text: "overwrite"})
	if layout.Rows[1].Runs[0] != next {
		t.Fatal("append changed neighboring row")
	}
}

func BenchmarkLayoutMetadata(b *testing.B) {
	s := New(Options{Multiline: true})
	s.SetValue(strings.Repeat("Unicode 界 e\u0301 👩🏽‍💻 words and wrapping text\n", 1000))
	config := Config{Width: 30, Height: 8, Wrap: true}
	_ = s.VisualLineCount(config)
	_ = s.CursorCell(config)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		_ = s.VisualLineCount(config)
		_ = s.CursorCell(config)
	}
}

func TestHitEveryOccupiedCellStartsGrapheme(t *testing.T) {
	s := New(Options{})
	s.SetValue("界e\u0301👩‍💻tail")
	config := Config{Width: 20, Height: 1}
	for _, row := range s.Layout(config).Rows {
		for _, run := range row.Runs {
			for cell := run.Cell; cell < run.Cell+run.Width; cell++ {
				if got := s.PositionAtCell(config, 0, cell); got.Column != run.StartColumn {
					t.Fatalf("cell %d: rune %d want %d", cell, got.Column, run.StartColumn)
				}
			}
		}
	}
	if got := s.PositionAtCell(config, 0, 4); got.Column != 3 {
		t.Fatalf("emoji trailing cell: %v", got)
	}
}

func TestNarrowHorizontalViewportBoundaries(t *testing.T) {
	for _, value := range []string{"界界界界界", "界e\u0301👩‍💻tail", "a\t界"} {
		for width := 1; width <= 6; width++ {
			s := New(Options{})
			s.SetValue(value)
			config := Config{Width: width, Height: 1}
			for {
				s.NormalizeViewport(config)
				layout := s.Layout(config)
				x := layout.ScrollX
				if layout.Cursor.Column < x || layout.Cursor.Column >= x+width {
					t.Fatalf("cursor outside width%d: %+v", width, layout)
				}
				for _, run := range layout.Rows[0].Runs {
					if x > run.Cell && x < run.Cell+run.Width {
						t.Fatalf("offset %d splits %+v", x, run)
					}
					for cell := max(x, run.Cell); cell < min(x+width, run.Cell+run.Width); cell++ {
						if got := s.PositionAtCell(config, 0, cell-x); got.Column != run.StartColumn {
							t.Fatalf("viewport cell %d maps %v not %d", cell-x, got, run.StartColumn)
						}
					}
				}
				if s.Column() == 0 {
					break
				}
				s.Apply(Action{Kind: MoveLeft})
			}
		}
	}
}

func TestForwardWordBoundaries(t *testing.T) {
	tests := []struct {
		name, value string
		start, want Position
		deleted     string
	}{
		{"word start", "one  two", Position{}, Position{0, 3}, "  two"},
		{"inside word", "one  two", Position{0, 1}, Position{0, 3}, "o  two"},
		{"leading spaces", "  one  two", Position{}, Position{0, 5}, "  two"},
		{"between words", "one  two", Position{0, 3}, Position{0, 8}, "one"},
		{"trailing spaces", "one  ", Position{0, 3}, Position{0, 5}, "one"},
		{"document end", "one", Position{0, 3}, Position{0, 3}, "one"},
		{"newline", "one\n  two\nlast", Position{0, 3}, Position{1, 5}, "one\nlast"},
		{"word before newline", "one\ntwo", Position{0, 1}, Position{0, 3}, "o\ntwo"},
		{"blank line", "\n\nword", Position{}, Position{2, 4}, ""},
		{"graphemes", " \t界e\u0301👩‍💻 next", Position{}, Position{0, 8}, " next"},
		{"one grapheme", "界 next", Position{}, Position{0, 1}, " next"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := New(Options{Multiline: true})
			s.SetValue(tc.value)
			s.SetCursor(tc.start)
			original := s.Clone()
			s.Apply(Action{Kind: MoveWordRight})
			if s.Cursor() != tc.want || s.Value() != tc.value {
				t.Fatalf("move got %v %q want %v", s.Cursor(), s.Value(), tc.want)
			}
			original.Apply(Action{Kind: DeleteWordForward})
			if original.Value() != tc.deleted || original.Cursor() != tc.start {
				t.Fatalf("delete got %q at %v want %q at %v", original.Value(), original.Cursor(), tc.deleted, tc.start)
			}
		})
	}
}
