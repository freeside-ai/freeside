package exportscan

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCredentialScannerReadsPastTheChunkBoundary is the regression for a
// truncating scanner: the export's largest member is the agent transcript,
// which is both the most likely to be large and the most likely to echo auth
// material, so material beyond the first chunk must still be caught.
func TestCredentialScannerReadsPastTheChunkBoundary(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	secret := "sk-ant-oat01-" + strings.Repeat("Z", 40)
	tests := []struct {
		name string
		body string
	}{
		{"far past the first chunk", strings.Repeat("x", 3*scanChunkBytes) + secret},
		// Straddling a chunk boundary: caught only because each pass carries
		// the previous chunk's tail forward.
		{"straddling a boundary", strings.Repeat("x", scanChunkBytes-len(secret)/2) + secret},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(dir, strings.ReplaceAll(tc.name, " ", "-")+".jsonl")
			if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
				t.Fatalf("write: %v", err)
			}
			_, matched, err := scanFile(context.Background(), path)
			if err != nil {
				t.Fatalf("scanFile: %v", err)
			}
			if !matched {
				t.Fatal("scanner reported a file carrying credential material as clean")
			}
		})
	}
}
