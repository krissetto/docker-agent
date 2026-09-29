package retained

import "testing"

func TestSlotExactInputAndFresh(t *testing.T) {
	type input struct {
		Text  string
		Width int
	}
	var slot Slot[input]
	draw := func(in input) string { return in.Text }
	first := input{Text: "λ界", Width: 12}
	if got := slot.Render(first, draw); got != first.Text {
		t.Fatal(got)
	}
	slot.Render(first, draw)
	if slot.Renders() != 1 {
		t.Fatal("equal input redrawn")
	}
	second := input{Text: "new", Width: 13}
	if got := slot.Fresh(second, draw); got != second.Text {
		t.Fatal(got)
	}
	slot.Render(first, draw)
	if slot.Renders() != 2 {
		t.Fatal("fresh replaced retained entry")
	}
	slot.Render(second, draw)
	slot.Render(first, draw)
	if slot.Renders() != 4 {
		t.Fatal("slot must retain only last input")
	}
}

func TestSlotZeroInputStillDraws(t *testing.T) {
	var slot Slot[string]
	if got := slot.Render("", func(string) string { return "empty" }); got != "empty" || slot.Renders() != 1 {
		t.Fatal(got)
	}
}
