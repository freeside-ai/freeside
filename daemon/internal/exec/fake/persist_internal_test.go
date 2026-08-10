package fake

import (
	"path/filepath"
	"slices"
	"testing"
)

func TestMakeStateDirectorySyncsEveryPublishedEntryAndRetry(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "one", "two")
	var synced []string
	if err := makeStateDirectoryWithSync(target, func(path string) error {
		synced = append(synced, path)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	want := []string{filepath.Dir(root), root, filepath.Join(root, "one")}
	if !slices.Equal(synced, want) {
		t.Fatalf("synced directories = %v, want %v", synced, want)
	}

	synced = nil
	if err := makeStateDirectoryWithSync(target, func(path string) error {
		synced = append(synced, path)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	want = []string{filepath.Dir(target)}
	if !slices.Equal(synced, want) {
		t.Fatalf("retry synced directories = %v, want %v", synced, want)
	}
}
