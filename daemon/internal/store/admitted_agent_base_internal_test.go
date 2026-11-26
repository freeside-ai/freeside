package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

func testAdmittedAgentBase(
	t *testing.T, harness domain.HarnessClientKind, digest string, admittedAt time.Time,
) domain.AdmittedAgentBase {
	t.Helper()
	base, err := domain.NewAdmittedAgentBase(domain.AdmittedAgentBaseInput{
		Harness:      harness,
		ImageRef:     domain.ImageRef("127.0.0.1:5100/freeside-agent-claude@sha256:" + strings.Repeat(digest, 64)),
		HarnessBuild: "claude-code 2.1.319", SourceRepoID: 1278475858, SourceRepository: "freeside-ai/freeside",
		SourceCommit: strings.Repeat("1", 40), SourceTreeSHA: strings.Repeat("2", 40), AdmittedAt: admittedAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	return base
}

// A base records once: recording the same build again returns the first
// row. The pointer advances only to a recorded base of its own harness, and
// reads back with the base it names.
func TestAdmittedAgentBaseRecordsOnceAndPointsOnlyAtARecordedBase(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openProjectImageStore(t)
	at := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	base := testAdmittedAgentBase(t, domain.HarnessClientClaudeCode, "a", at)
	codex := testAdmittedAgentBase(t, domain.HarnessClientCodexCLI, "b", at)
	pointer := domain.AdmittedAgentBasePointer{
		Harness: domain.HarnessClientClaudeCode, BaseID: base.ID, AdvancedAt: at,
	}
	advance := func(p domain.AdmittedAgentBasePointer) error {
		return st.WriteInternal(ctx, func(tx *InternalTx) error { return tx.AdvanceAdmittedAgentBasePointer(ctx, p) })
	}

	if err := advance(pointer); !errors.Is(err, ErrAdmittedAgentBaseUnrecorded) {
		t.Fatalf("advance to an unrecorded base = %v, want ErrAdmittedAgentBaseUnrecorded", err)
	}
	if err := st.WriteInternal(ctx, func(tx *InternalTx) error {
		if _, err := tx.RecordAdmittedAgentBase(ctx, base); err != nil {
			return err
		}
		_, err := tx.RecordAdmittedAgentBase(ctx, codex)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	foreign := pointer
	foreign.BaseID = codex.ID
	if err := advance(foreign); !errors.Is(err, ErrAdmittedAgentBaseUnrecorded) {
		t.Fatalf("advance to another harness's base = %v, want ErrAdmittedAgentBaseUnrecorded", err)
	}

	again := testAdmittedAgentBase(t, domain.HarnessClientClaudeCode, "a", at.Add(time.Hour))
	if err := st.WriteInternal(ctx, func(tx *InternalTx) error {
		recorded, err := tx.RecordAdmittedAgentBase(ctx, again)
		if err == nil && recorded != base {
			t.Errorf("recording the same base again = %+v, want the first row %+v", recorded, base)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := advance(pointer); err != nil {
		t.Fatal(err)
	}
	if err := st.Read(ctx, func(tx *ReadTx) error {
		got, named, found, err := tx.GetAdmittedAgentBasePointer(ctx, domain.HarnessClientClaudeCode)
		if err != nil || !found || got != pointer || named != base {
			t.Errorf("pointer = %+v %+v %v err=%v, want %+v naming %+v", got, named, found, err, pointer, base)
		}
		if _, _, found, err := tx.GetAdmittedAgentBasePointer(ctx, domain.HarnessClientCodexCLI); err != nil || found {
			t.Errorf("codex pointer found=%v err=%v, want none", found, err)
		}
		byImage, found, err := tx.FindAdmittedAgentBaseByImage(ctx, base.ImageRef)
		if err != nil || !found || byImage != base {
			t.Errorf("find by image = %+v %v err=%v", byImage, found, err)
		}
		if _, found, err := tx.FindAdmittedAgentBaseByImage(ctx, domain.ImageRef(
			"ghcr.io/owner/base@sha256:"+strings.Repeat("c", 64))); err != nil || found {
			t.Errorf("find an operator's base found=%v err=%v, want none", found, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// A source moved only outside what the build reads rebuilds the same
	// image, which records again from its own tree; the image finds the
	// latest record.
	moved, err := domain.NewAdmittedAgentBase(domain.AdmittedAgentBaseInput{
		Harness: base.Harness, ImageRef: base.ImageRef, HarnessBuild: base.HarnessBuild,
		SourceRepoID: base.SourceRepoID, SourceRepository: base.SourceRepository,
		SourceCommit: strings.Repeat("3", 40), SourceTreeSHA: strings.Repeat("4", 40), AdmittedAt: at.Add(2 * time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.WriteInternal(ctx, func(tx *InternalTx) error {
		_, err := tx.RecordAdmittedAgentBase(ctx, moved)
		return err
	}); err != nil {
		t.Fatalf("record the same image from a moved tree: %v", err)
	}
	if err := st.Read(ctx, func(tx *ReadTx) error {
		byImage, found, err := tx.FindAdmittedAgentBaseByImage(ctx, base.ImageRef)
		if err != nil || !found || byImage != moved {
			t.Errorf("find by a twice-admitted image = %+v %v err=%v, want %+v", byImage, found, err, moved)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
