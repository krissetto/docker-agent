package runtime

import "testing"

func sessionElicitationCountForTest(rt *LocalRuntime) int {
	rt.sessionDrivers.mu.Lock()
	drivers := make([]*sessionDriver, 0, len(rt.sessionDrivers.drivers))
	for _, driver := range rt.sessionDrivers.drivers {
		drivers = append(drivers, driver)
	}
	rt.sessionDrivers.mu.Unlock()
	count := 0
	for _, driver := range drivers {
		driver.mu.Lock()
		for _, interaction := range driver.interactions {
			if interaction.waiter != nil {
				count++
			}
		}
		driver.mu.Unlock()
	}
	return count
}

func elicitationDeclineNotesForTest(t *testing.T, rt *LocalRuntime, id string) []string {
	t.Helper()
	d, ok := rt.sessionDrivers.Lookup(id)
	if !ok {
		return nil
	}
	snapshot, err := d.ownerSnapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var notes []string
	for _, item := range snapshot.Messages {
		if item.Message != nil && item.Message.InputMode == "elicitation_declined" {
			notes = append(notes, item.Message.Message.Content)
		}
	}
	return notes
}
