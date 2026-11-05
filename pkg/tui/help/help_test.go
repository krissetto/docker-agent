package help

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func wellFormed() Document {
	return Document{
		Context: "Composer",
		Current: []Section{{ID: "composer", Title: "Composer", Entries: []Entry{
			{ID: "composer.send", Keys: []string{"enter"}, Description: "Send message"},
			{ID: "composer.queue", Keys: []string{"alt+enter"}, Description: "Queue for the next turn", Condition: "While working"},
		}}},
		Reference: []Section{{ID: "global", Title: "Global", Entries: []Entry{
			{ID: "app.help", Keys: []string{"f1"}, Description: "Show help"},
		}}},
	}
}

func TestZeroDocumentIsValid(t *testing.T) {
	require.NoError(t, Document{}.Validate())
}

func TestWellFormedDocumentIsValid(t *testing.T) {
	require.NoError(t, wellFormed().Validate())
}

func TestEntryWithoutKeysIsValid(t *testing.T) {
	d := wellFormed()
	d.Current[0].Entries[0].Keys = nil
	require.NoError(t, d.Validate())
}

func TestSameIDsAcrossSectionsOrGroupsAreValid(t *testing.T) {
	d := wellFormed()
	d.Current = append(d.Current, Section{ID: "other", Title: "Other", Entries: []Entry{
		{ID: "composer.send", Keys: []string{"enter"}, Description: "Send again"},
	}})
	d.Reference = append(d.Reference, Section{ID: "composer", Title: "Composer reference", Entries: []Entry{
		{ID: "app.help", Keys: []string{"f1"}, Description: "Show help"},
	}})
	require.NoError(t, d.Validate())
}

func TestValidateRejectsStructuralProblems(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Document)
		want   string
	}{
		{"empty section ID", func(d *Document) { d.Current[0].ID = "" }, "Current section 0: empty ID"},
		{"duplicate section ID in current", func(d *Document) {
			d.Current = append(d.Current, Section{ID: "composer", Title: "Again"})
		}, `Current: duplicate section ID "composer"`},
		{"duplicate section ID in reference", func(d *Document) {
			d.Reference = append(d.Reference, Section{ID: "global", Title: "Again"})
		}, `Reference: duplicate section ID "global"`},
		{"empty entry ID", func(d *Document) { d.Current[0].Entries[0].ID = "" }, "empty ID"},
		{"blank description", func(d *Document) { d.Current[0].Entries[0].Description = "  " }, "empty description"},
		{"blank key label", func(d *Document) { d.Current[0].Entries[0].Keys = []string{"enter", " "} }, "blank key label"},
		{"duplicate entry ID in section", func(d *Document) {
			d.Current[0].Entries = append(d.Current[0].Entries, Entry{ID: "composer.send", Keys: []string{"ctrl+s"}, Description: "Save"})
		}, `Current section "composer": duplicate entry ID "composer.send"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := wellFormed()
			tt.mutate(&d)
			require.ErrorContains(t, d.Validate(), tt.want)
		})
	}
}
