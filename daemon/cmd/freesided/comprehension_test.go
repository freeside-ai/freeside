package main

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/observe/comprehension"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

func TestComprehensionCommandRecordDefectAndMeasures(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "freeside.db")
	st, err := store.Open(ctx, dbPath, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	runID := domain.RunID("run-1")
	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: "item-1", ProjectID: "proj-1",
		Subject: domain.Subject{Type: domain.SubjectRun, ID: "run-1", RunID: &runID},
		Type:    domain.AttentionReadyForFinalReview, Priority: domain.PriorityNormal,
		Reason:            "checks are green and the diff is ready",
		RequestedDecision: []domain.Action{domain.ActionOpenPR, domain.ActionStop, domain.ActionDismiss},
		PRHeadSHA:         "cafebabe", PRReference: &domain.PRReference{Repo: "owner/repo", Number: 123},
		ItemVersion: 1, InterruptionClass: domain.InterruptionPlannedGate, Status: domain.StatusOpen,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Write(ctx, func(tx *store.WriteTx) error { return tx.PutAttentionItem(ctx, item) }); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	claim := "sha256:" + strings.Repeat("c", 64)
	var stdout, stderr bytes.Buffer
	if err := runComprehensionCommand(ctx,
		[]string{"record-defect", "-db", dbPath, "-item", "item-1", "-claim", claim, "-reason", "misread readiness"},
		&stdout, &stderr); err != nil {
		t.Fatalf("record-defect: %v; stderr=%s", err, stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	if err := runComprehensionCommand(ctx, []string{"-db", dbPath}, &stdout, &stderr); err != nil {
		t.Fatalf("measures: %v; stderr=%s", err, stderr.String())
	}
	var measures comprehension.Measures
	if err := json.Unmarshal(stdout.Bytes(), &measures); err != nil {
		t.Fatalf("decode measures: %v; out=%s", err, stdout.String())
	}
	if measures.DefectCount != 1 {
		t.Fatalf("defect count = %d, want 1", measures.DefectCount)
	}
}
