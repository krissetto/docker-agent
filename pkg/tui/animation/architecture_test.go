package animation

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSingleGlobalTimingSourceArchitecture(t *testing.T) {
	// go test runs in the package directory. runtime.Caller would return an
	// import path under -trimpath, so it cannot locate the sources.
	root, err := os.Getwd()
	if err != nil {
		t.Fatal("locate animation package:", err)
	}
	forbidden := []string{"SubscribeEvery", "NewSubscriptionEvery", "startEvery", "registerEvery", "unregisterEvery", "ScheduledFor"}
	err = filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() || filepath.Ext(path) != ".go" {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if filepath.Base(path) == "architecture_test.go" {
			return nil
		}
		text := string(data)
		for _, symbol := range forbidden {
			if strings.Contains(text, symbol) {
				t.Errorf("%s contains forbidden alternate timing API %q", path, symbol)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if TickRate <= 0 {
		t.Fatal("global TickRate must be positive")
	}
}
