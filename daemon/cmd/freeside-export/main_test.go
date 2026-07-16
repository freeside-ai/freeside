package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRunSmoke drives the command end-to-end against a real workspace and
// output directory; the export semantics themselves are covered in
// internal/export.
func TestRunSmoke(t *testing.T) {
	ws, out := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "file.txt"), []byte("content\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var stderr strings.Builder
	err := run([]string{"--workspace", ws, "--out", out, "--max-blob-bytes", "1024"}, &stderr)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if _, err := os.Stat(filepath.Join(out, "manifest.json")); err != nil {
		t.Errorf("manifest.json missing: %v", err)
	}
	if !strings.Contains(stderr.String(), "exported 1 entries") {
		t.Errorf("stderr = %q, want an exported-entries summary", stderr.String())
	}
}

func TestRunRejectsPositionalArguments(t *testing.T) {
	var stderr strings.Builder
	if err := run([]string{"stray"}, &stderr); err == nil {
		t.Fatal("run accepted positional arguments")
	}
}

func TestRunReportsBadFlag(t *testing.T) {
	var stderr strings.Builder
	if err := run([]string{"--no-such-flag"}, &stderr); err == nil {
		t.Fatal("run accepted an unknown flag")
	}
}
