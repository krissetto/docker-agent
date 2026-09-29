package key

import (
	"slices"
	"testing"
)

type event string

func (e event) String() string { return string(e) }

func TestMatches(t *testing.T) {
	t.Parallel()
	binding := NewBinding(WithKeys("ctrl+c", "q", "界"), WithHelp("Ctrl+C", "quit"))
	for _, input := range []string{"ctrl+c", "q", "界"} {
		if !Matches(event(input), Binding{}, binding) {
			t.Errorf("expected %q to match", input)
		}
	}
	for _, input := range []string{"Ctrl+C", "Q", "", "ctrl+c "} {
		if Matches(event(input), binding) {
			t.Errorf("unexpected match for %q", input)
		}
	}
	if Matches(event("q")) {
		t.Fatal("event matched without bindings")
	}
	binding.SetEnabled(false)
	if Matches(event("q"), binding) {
		t.Fatal("disabled binding matched")
	}
	other := NewBinding(WithKeys("q"))
	if !Matches(event("q"), binding, other) {
		t.Fatal("disabled binding hid a later enabled binding")
	}
	binding.SetEnabled(true)
	if !Matches(event("q"), binding) {
		t.Fatal("re-enabled binding did not match")
	}
}

func TestEnabled(t *testing.T) {
	t.Parallel()
	var binding Binding
	if binding.Enabled() {
		t.Fatal("zero binding is enabled")
	}
	binding.SetEnabled(true)
	if binding.Enabled() {
		t.Fatal("unbound binding is enabled")
	}
	binding.SetKeys([]string{}...)
	if !binding.Enabled() || Matches(event(""), binding) {
		t.Fatal("explicitly empty keys must be enabled but not match")
	}
	binding.SetKeys("")
	if !Matches(event(""), binding) {
		t.Fatal("explicit empty string key did not match")
	}
	binding.SetKeys()
	if binding.Enabled() || binding.Keys() != nil {
		t.Fatal("clearing keys did not unbind")
	}
}

func TestDetachedKeys(t *testing.T) {
	t.Parallel()
	input := []string{"a", "b"}
	option := WithKeys(input...)
	input[0] = "changed before construction"
	first := NewBinding(option)
	second := NewBinding(option)
	got := first.Keys()
	got[0] = "changed returned keys"
	if !slices.Equal(first.Keys(), []string{"a", "b"}) || !slices.Equal(second.Keys(), first.Keys()) {
		t.Fatal("WithKeys or Keys retained a caller's slice")
	}
	first.SetKeys(input...)
	input[1] = "changed after setter"
	if first.Keys()[1] != "b" || second.Keys()[0] != "a" {
		t.Fatal("SetKeys retained a caller's slice or changed another binding")
	}
}

func TestBindingCopyAndOptions(t *testing.T) {
	t.Parallel()
	original := NewBinding(WithKeys("a"), WithKeys("b"), WithHelp("B", "before"))
	copy := original
	copy.SetKeys("c")
	copy.SetHelp("C", "after")
	copy.SetEnabled(false)
	if !Matches(event("b"), original) || original.Help() != (Help{Key: "B", Desc: "before"}) {
		t.Fatal("changing a copy changed the original")
	}
	if copy.Enabled() || !slices.Equal(copy.Keys(), []string{"c"}) || copy.Help() != (Help{Key: "C", Desc: "after"}) {
		t.Fatal("copy did not retain its own state")
	}
	help := original.Help()
	help.Key = "changed"
	if original.Help().Key != "B" {
		t.Fatal("Help did not return a value")
	}
}
