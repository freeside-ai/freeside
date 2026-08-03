package engine

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

func claudeInstructions(path string) VendorInstructionConfig {
	return VendorInstructionConfig{
		Vendor:   domain.AgentVendorClaude,
		Delivery: domain.VendorInstructionDeliveryAppendFile,
		HostPath: path,
	}
}

func TestSnapshotVendorInstructionsRecordsExactBytesOrExplicitAbsence(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing-CLAUDE.md")
	absent, body, err := snapshotVendorInstructions(
		t.Context(), claudeInstructions(missing),
	)
	if err != nil {
		t.Fatal(err)
	}
	if absent.Vendor != domain.AgentVendorClaude || absent.Digest != nil || body != nil {
		t.Fatalf("absent snapshot = %+v, body %q", absent, body)
	}

	path := filepath.Join(t.TempDir(), "CLAUDE.md")
	want := []byte("# Exact host instructions\n")
	if err := os.WriteFile(path, want, 0o600); err != nil {
		t.Fatal(err)
	}
	present, body, err := snapshotVendorInstructions(
		t.Context(), claudeInstructions(path),
	)
	if err != nil {
		t.Fatal(err)
	}
	if present.Digest == nil || !bytes.Equal(body, want) {
		t.Fatalf("present snapshot = %+v, body %q", present, body)
	}
	body[0] = '!'
	replayed, _, err := snapshotVendorInstructions(
		t.Context(), claudeInstructions(path),
	)
	if err != nil {
		t.Fatal(err)
	}
	if replayed.Digest == nil || *replayed.Digest != *present.Digest {
		t.Fatal("caller mutation changed the source or its content identity")
	}
}

func TestSnapshotVendorInstructionsCarriesCodexAppendFileBinding(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing-AGENTS.md")
	snapshot, body, err := snapshotVendorInstructions(t.Context(), VendorInstructionConfig{
		Vendor:   domain.AgentVendorCodex,
		Delivery: domain.VendorInstructionDeliveryAppendFile,
		HostPath: path,
	})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Vendor != domain.AgentVendorCodex ||
		snapshot.Delivery != domain.VendorInstructionDeliveryAppendFile ||
		snapshot.Digest != nil || body != nil {
		t.Fatalf("Codex absent snapshot = %+v, body %q", snapshot, body)
	}
}

func TestSnapshotVendorInstructionsDereferencesOnce(t *testing.T) {
	dir := t.TempDir()
	firstPath := filepath.Join(dir, "first.md")
	secondPath := filepath.Join(dir, "second.md")
	link := filepath.Join(dir, "CLAUDE.md")
	first := []byte("first target\n")
	second := []byte("second target\n")
	if err := os.WriteFile(firstPath, first, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(secondPath, second, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(firstPath, link); err != nil {
		t.Fatal(err)
	}

	snapshot, body, err := snapshotVendorInstructions(
		t.Context(), claudeInstructions(link),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secondPath, link); err != nil {
		t.Fatal(err)
	}
	if snapshot.Digest == nil || !bytes.Equal(body, first) {
		t.Fatal("retargeting the configured symlink changed the admitted snapshot")
	}
}

func TestSnapshotVendorInstructionsRejectsInvalidSourceStates(t *testing.T) {
	dir := t.TempDir()
	dangling := filepath.Join(dir, "dangling")
	if err := os.Symlink(filepath.Join(dir, "absent"), dangling); err != nil {
		t.Fatal(err)
	}
	oversized := filepath.Join(dir, "oversized")
	if err := os.WriteFile(
		oversized, bytes.Repeat([]byte{'x'}, int(domain.MaxVendorInstructionBytes+1)), 0o600,
	); err != nil {
		t.Fatal(err)
	}
	unreadable := filepath.Join(dir, "unreadable")
	if err := os.WriteFile(unreadable, []byte("private\n"), 0o000); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		path string
	}{
		{"dangling symlink", dangling},
		{"directory", dir},
		{"oversized", oversized},
	}
	if os.Geteuid() != 0 {
		cases = append(cases, struct {
			name string
			path string
		}{"unreadable", unreadable})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := snapshotVendorInstructions(
				t.Context(), claudeInstructions(tc.path),
			); err == nil {
				t.Fatal("snapshot accepted an invalid source")
			}
		})
	}
}

func TestSnapshotVendorInstructionsHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, _, err := snapshotVendorInstructions(
		ctx, claudeInstructions(filepath.Join(t.TempDir(), "missing")),
	)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("snapshot = %v, want %v", err, context.Canceled)
	}
}
