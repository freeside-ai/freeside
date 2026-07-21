package export_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/export"
)

func TestCommitPlanOpaqueLiftAndNamespace(t *testing.T) {
	workspace := t.TempDir()
	plan := []byte(`{"not":"parsed by the helper"`)
	if err := os.WriteFile(filepath.Join(workspace, export.CommitPlanFilename), plan, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, export.CommitPlanFilename+".bak"), []byte("ordinary"), 0o600); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "handoff")
	m, err := export.Export(os.DirFS(workspace), out, export.Options{})
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	// #nosec G304 -- out is a test-owned temporary directory and the leaf is fixed.
	got, err := os.ReadFile(filepath.Join(out, export.CommitPlanFilename))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(plan) {
		t.Fatalf("lifted plan changed: %q", got)
	}
	if len(m.Entries) != 1 || m.Entries[0].Path != export.CommitPlanFilename+".bak" {
		t.Fatalf("manifest entries = %+v, want only the near-prefix file", m.Entries)
	}
}

func TestCommitPlanLiftFailsClosed(t *testing.T) {
	cases := []struct {
		name    string
		prepare func(*testing.T, string)
		opts    export.Options
		want    error
	}{
		{"directory", func(t *testing.T, dir string) {
			t.Helper()
			if err := os.Mkdir(filepath.Join(dir, export.CommitPlanFilename), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, export.CommitPlanFilename, "child"), []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
		}, export.Options{}, export.ErrCommitPlanNotRegular},
		{"symlink", func(t *testing.T, dir string) {
			t.Helper()
			if err := os.WriteFile(filepath.Join(dir, "target"), []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("target", filepath.Join(dir, export.CommitPlanFilename)); err != nil {
				t.Fatal(err)
			}
		}, export.Options{}, export.ErrCommitPlanNotRegular},
		{"over cap", func(t *testing.T, dir string) {
			t.Helper()
			if err := os.WriteFile(filepath.Join(dir, export.CommitPlanFilename), []byte("12345"), 0o600); err != nil {
				t.Fatal(err)
			}
		}, export.Options{MaxCommitPlanBytes: 4}, export.ErrCommitPlanTooLarge},
		{"case alias", func(t *testing.T, dir string) {
			t.Helper()
			if err := os.WriteFile(filepath.Join(dir, ".FREESIDE-COMMIT-PLAN.JSON"), []byte("{}"), 0o600); err != nil {
				t.Fatal(err)
			}
		}, export.Options{}, export.ErrCommitPlanPathAlias},
		{"unicode fold alias", func(t *testing.T, dir string) {
			t.Helper()
			if err := os.WriteFile(filepath.Join(dir, ".freeſide-commit-plan.json"), []byte("{}"), 0o600); err != nil {
				t.Fatal(err)
			}
		}, export.Options{}, export.ErrCommitPlanPathAlias},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			workspace := t.TempDir()
			tc.prepare(t, workspace)
			out := filepath.Join(t.TempDir(), "handoff")
			_, err := export.Export(os.DirFS(workspace), out, tc.opts)
			if !errors.Is(err, tc.want) {
				t.Fatalf("Export = %v, want %v", err, tc.want)
			}
			if _, statErr := os.Stat(filepath.Join(out, export.ManifestFilename)); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("manifest exists after failed plan lift: %v", statErr)
			}
		})
	}
}

func TestCommitPlanAbsent(t *testing.T) {
	out := filepath.Join(t.TempDir(), "handoff")
	if _, err := export.Export(os.DirFS(t.TempDir()), out, export.Options{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(out, export.CommitPlanFilename)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("absent workspace plan produced a handoff member: %v", err)
	}
}
