//go:build unix

package export_test

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/export"
)

func TestCommitPlanFIFORejectedWithoutOpening(t *testing.T) {
	workspace := t.TempDir()
	if err := syscall.Mkfifo(filepath.Join(workspace, export.CommitPlanFilename), 0o600); err != nil {
		t.Skipf("mkfifo unsupported: %v", err)
	}
	_, err := export.Export(os.DirFS(workspace), filepath.Join(t.TempDir(), "handoff"), export.Options{})
	if !errors.Is(err, export.ErrCommitPlanNotRegular) {
		t.Fatalf("Export = %v, want irregular-plan failure", err)
	}
}
