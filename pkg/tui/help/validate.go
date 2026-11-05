package help

import (
	"errors"
	"fmt"
	"strings"
)

var (
	errEmptyEntryID     = errors.New("empty ID")
	errEmptyDescription = errors.New("empty description")
	errBlankKey         = errors.New("blank key label")
)

// Validate reports the first structural problem in the document. It checks
// shape only: every section and entry needs a non-empty ID, every entry needs a
// non-blank description, and key labels must not be blank. Section IDs are
// unique within each of Current and Reference, and entry IDs are unique within
// their section. Validate does not check whether a key is bound or reachable.
func (d Document) Validate() error {
	if err := validateSections("Current", d.Current); err != nil {
		return err
	}
	return validateSections("Reference", d.Reference)
}

func validateSections(group string, sections []Section) error {
	seenSections := make(map[string]struct{}, len(sections))
	for i, section := range sections {
		if section.ID == "" {
			return fmt.Errorf("%s section %d: empty ID", group, i)
		}
		if _, dup := seenSections[section.ID]; dup {
			return fmt.Errorf("%s: duplicate section ID %q", group, section.ID)
		}
		seenSections[section.ID] = struct{}{}

		seenEntries := make(map[string]struct{}, len(section.Entries))
		for j, entry := range section.Entries {
			if err := validateEntry(entry); err != nil {
				return fmt.Errorf("%s section %q entry %d: %w", group, section.ID, j, err)
			}
			if _, dup := seenEntries[entry.ID]; dup {
				return fmt.Errorf("%s section %q: duplicate entry ID %q", group, section.ID, entry.ID)
			}
			seenEntries[entry.ID] = struct{}{}
		}
	}
	return nil
}

func validateEntry(entry Entry) error {
	if entry.ID == "" {
		return errEmptyEntryID
	}
	if strings.TrimSpace(entry.Description) == "" {
		return errEmptyDescription
	}
	for _, key := range entry.Keys {
		if strings.TrimSpace(key) == "" {
			return errBlankKey
		}
	}
	return nil
}
