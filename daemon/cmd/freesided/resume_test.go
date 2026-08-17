package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

func TestResumeTargetsExactLiveRunAndRefusesTerminal(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "state.db")
	st, _, err := openStoreWithTopicKey(ctx, dbPath, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	liveID := domain.RunID("run-live")
	terminalID := domain.RunID("run-terminal")
	terminalInvocation := domain.InvocationID("inv-terminal")
	terminalStatus := domain.ObservedStatusFailed
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		for _, id := range []domain.RunID{liveID, terminalID} {
			if err := tx.PutRun(ctx, domain.Run{
				ID: id, ProjectID: "project-1", SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy",
			}); err != nil {
				return err
			}
		}
		liveInvocation := domain.InvocationID("inv-live")
		if err := tx.AppendRunMilestone(ctx, domain.RunMilestone{
			RunID: liveID, Kind: domain.MilestoneRunSubmitted,
			InvocationID: &liveInvocation, RecordedAt: now,
		}); err != nil {
			return err
		}
		return tx.AppendRunMilestone(ctx, domain.RunMilestone{
			RunID: terminalID, Kind: domain.MilestoneTerminalRecorded,
			InvocationID: &terminalInvocation, Terminal: &terminalStatus,
			RecordedAt: now,
		})
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	err = runResumeCommand(ctx, []string{"-db", dbPath, "-run", string(terminalID), "-once"}, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), `terminal in state "failed"`) ||
		!strings.Contains(err.Error(), "freesided reattempt") {
		t.Fatalf("terminal resume = %v, want state and reattempt refusal", err)
	}
	stdout.Reset()
	err = runResumeCommand(ctx, []string{"-db", dbPath, "-run", string(liveID), "-once"}, &stdout, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), string(liveID)) || !strings.Contains(stdout.String(), "outcome  pending") {
		t.Fatalf("live resume output = %q, want exact run pending snapshot", stdout.String())
	}

	st, _, err = openStoreWithTopicKey(ctx, dbPath, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		runs, err := tx.ListRuns(ctx)
		if err != nil {
			return err
		}
		if len(runs) != 2 {
			t.Fatalf("resume changed run count to %d, want 2", len(runs))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
