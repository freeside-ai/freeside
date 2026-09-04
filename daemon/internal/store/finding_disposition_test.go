package store_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/golden"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
)

func dispositionReviewRecord(
	t *testing.T, runID domain.RunID, round int, findingIDs []domain.FindingID, completedAt time.Time,
) domain.ReviewRecord {
	t.Helper()
	outcome := domain.ReviewFindings
	if len(findingIDs) == 0 {
		outcome = domain.ReviewClean
	}
	record, err := domain.NewReviewRecord(domain.ReviewRecord{
		InvocationID: domain.InvocationID("review-" + string(runID) + "-" + strconv.Itoa(round)),
		RunID:        runID, Round: round, Provider: "openai", ModelConfiguration: "gpt-codex/high",
		ConfigurationDigest: domain.Digest("sha256:" + strings.Repeat("c", 64)),
		InstructionDigest:   domain.Digest("sha256:" + strings.Repeat("d", 64)),
		CostOwner:           "owner", BaseSHA: "base", HeadSHA: "head-" + strconv.Itoa(round), CompletedAt: completedAt,
		CompletionEvidence: domain.Digest("sha256:" + strings.Repeat("e", 64)),
		Outcome:            outcome, FindingIDs: findingIDs,
	})
	if err != nil {
		t.Fatal(err)
	}
	return record
}

func finalDispositionAdjudication(
	t *testing.T,
	runID domain.RunID,
	round int,
	dispositions map[domain.FindingID]domain.ReviewDisposition,
	createdAt time.Time,
) domain.FindingAdjudication {
	t.Helper()
	entries := make([]domain.FindingAdjudicationEntry, 0, len(dispositions))
	for findingID, disposition := range dispositions {
		var goal domain.GoalRelationship
		var route domain.AdjudicationRoute
		switch disposition {
		case domain.ReviewDispositionDeclined:
			goal, route = domain.GoalContradictory, domain.RouteDecline
		case domain.ReviewDispositionDeferred:
			goal, route = domain.GoalAdjacent, domain.RouteDefer
		case domain.ReviewDispositionFixed:
			t.Fatalf("fixed disposition %q does not use an adjudication binding", findingID)
		}
		entry, err := domain.NewModelAdjudicationEntry(
			findingID, goal, nil, route, domain.ConfidenceHigh,
			"authorizes the final disposition", nil, nil, nil, nil, nil)
		if err != nil {
			t.Fatalf("new adjudication entry %q: %v", findingID, err)
		}
		entries = append(entries, entry)
	}
	artifact, err := domain.NewFindingAdjudication(
		runID, round, adjSpecDigest, adjInstructionDigest, adjPolicyDigest,
		entries, "",

		createdAt)
	if err != nil {
		t.Fatalf("new final-disposition adjudication: %v", err)
	}
	return artifact
}

func TestFindingDispositionsPersistAcrossRestart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "store.db")
	st := storetest.Open(t, path, store.Options{})
	run := domain.Run{
		ID: "run-dispositions", ProjectID: "project-1",
		SpecDigest: adjSpecDigest, PolicyDigest: adjPolicyDigest,
	}
	at := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	findingA := domain.Finding{
		ID: "finding-a", RunID: run.ID, Source: "codex_local", Location: &domain.FindingLocation{Path: "a.go", StartLine: 1, EndLine: 1},
		Message: "first", RawText: "first", CreatedAt: at,
	}
	findingB := domain.Finding{
		ID: "finding-b", RunID: run.ID, Source: "codex_local", Location: &domain.FindingLocation{Path: "b.go", StartLine: 2, EndLine: 2},
		Message: "second", RawText: "second", CreatedAt: at,
	}
	firstReview := dispositionReviewRecord(
		t, run.ID, 1, []domain.FindingID{findingB.ID, findingA.ID}, at)
	secondReview := dispositionReviewRecord(t, run.ID, 2, []domain.FindingID{findingA.ID}, at.Add(time.Minute))
	thirdReview := dispositionReviewRecord(t, run.ID, 3, nil, at.Add(90*time.Second))
	adjudication := finalDispositionAdjudication(t, run.ID, 1, map[domain.FindingID]domain.ReviewDisposition{
		findingA.ID: domain.ReviewDispositionDeferred,
		findingB.ID: domain.ReviewDispositionDeclined,
	}, at.Add(90*time.Second))
	want := []domain.ReviewDispositionRecord{
		{FindingID: findingA.ID, RunID: run.ID, Round: 1, Disposition: domain.ReviewDispositionDeferred, Reason: "tracked in issue 700", AdjudicationDigest: adjudication.Digest, CreatedAt: at.Add(2 * time.Minute)},
		{FindingID: findingB.ID, RunID: run.ID, Round: 1, Disposition: domain.ReviewDispositionDeclined, Reason: "contradicted by the contract test", AdjudicationDigest: adjudication.Digest, CreatedAt: at.Add(3 * time.Minute)},
		{FindingID: findingA.ID, RunID: run.ID, Round: 2, Disposition: domain.ReviewDispositionFixed, Reason: "fixed in the remediation head", RemediationInvocationID: thirdReview.InvocationID, CreatedAt: at.Add(4 * time.Minute)},
	}
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutRun(ctx, run); err != nil {
			return err
		}
		if err := tx.PutReviewRecord(ctx, firstReview, []domain.Finding{findingA, findingB}); err != nil {
			return err
		}
		if err := tx.PutReviewRecord(ctx, secondReview, []domain.Finding{findingA}); err != nil {
			return err
		}
		if err := tx.PutReviewRecord(ctx, thirdReview, nil); err != nil {
			return err
		}
		if err := tx.PutFindingAdjudication(ctx, adjudication); err != nil {
			return err
		}
		for _, disposition := range want {
			if err := tx.PutFindingDisposition(ctx, disposition); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	reopened := storetest.Open(t, path, store.Options{})
	if err := reopened.Read(ctx, func(tx *store.ReadTx) error {
		got, err := tx.ListFindingDispositions(ctx, run.ID)
		if err != nil {
			return err
		}
		if !slices.Equal(got, want) {
			t.Fatalf("recovered dispositions = %#v, want %#v", got, want)
		}
		gotOne, err := tx.GetFindingDisposition(ctx, findingA.ID, 2)
		if err != nil {
			return err
		}
		if gotOne != want[2] {
			t.Fatalf("round-bound disposition = %#v, want %#v", gotOne, want[2])
		}
		empty, err := tx.ListFindingDispositions(ctx, "run-other")
		if err != nil || empty == nil || len(empty) != 0 {
			t.Fatalf("empty list = %#v, %v", empty, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	pretty, err := json.MarshalIndent(want[0], "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	golden.Assert(t, "review_disposition_record", append(pretty, '\n'))

	rewrite := want[0]
	rewrite.Reason = "rewritten"
	if err := reopened.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutFindingDisposition(ctx, rewrite)
	}); !errors.Is(err, store.ErrImmutableConflict) {
		t.Fatalf("disposition rewrite = %v, want immutable conflict", err)
	}
}

func TestFindingDispositionKeepsSupersededAdjudicationBinding(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openStore(t, store.Options{})
	at := time.Date(2026, 8, 25, 15, 0, 0, 0, time.UTC)
	run := domain.Run{
		ID: "run-superseded-disposition", ProjectID: "project-1",
		SpecDigest: adjSpecDigest, PolicyDigest: adjPolicyDigest,
	}
	finding := domain.Finding{
		ID: "finding-superseded-disposition", RunID: run.ID, Source: "codex_local",
		Location: &domain.FindingLocation{Path: "daemon/a.go", StartLine: 1, EndLine: 1},
		Message:  "finding", RawText: "finding", CreatedAt: at,
	}
	record := dispositionReviewRecord(t, run.ID, 1, []domain.FindingID{finding.ID}, at)
	initial := finalDispositionAdjudication(t, run.ID, 1,
		map[domain.FindingID]domain.ReviewDisposition{finding.ID: domain.ReviewDispositionDeclined}, at)
	conversation := adjudicationConversation(t, "conversation-superseded-disposition", []string{"reconsider"}, at)
	invocation, feedback := adjudicationFeedback(t, conversation, "invocation-superseded-disposition", 1)
	entries := slices.Clone(initial.Entries)
	entries[0].Route = domain.RouteDispute
	entries[0].Rationale = "feedback now requires operator judgment"
	// The offered set is coupled to the recommendation on the contradictory
	// row: flipping the route to dispute makes decline the offered alternative.
	entries[0].OfferedAlternatives = []domain.OfferedAlternative{{
		Route: domain.RouteDecline, Consequence: "record the finding as declined instead",
	}}
	successor, err := domain.NewSuccessorFindingAdjudication(
		initial, feedback, entries, "",
		at.Add(time.Minute))
	if err != nil {
		t.Fatalf("successor: %v", err)
	}
	disposition := domain.ReviewDispositionRecord{
		FindingID: finding.ID, RunID: run.ID, Round: 1,
		Disposition: domain.ReviewDispositionDeclined, Reason: "bound to the original decision",
		AdjudicationDigest: initial.Digest, CreatedAt: at.Add(2 * time.Minute),
	}
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutRun(ctx, run); err != nil {
			return err
		}
		if err := tx.PutReviewRecord(ctx, record, []domain.Finding{finding}); err != nil {
			return err
		}
		if err := tx.PutFindingAdjudication(ctx, initial); err != nil {
			return err
		}
		if err := putAdjudicationFeedbackAuthority(
			t, ctx, tx, initial, conversation, invocation, "item-superseded-disposition", 2,
		); err != nil {
			return err
		}
		if err := tx.PutFindingAdjudication(ctx, successor); err != nil {
			return err
		}
		return tx.PutFindingDisposition(ctx, disposition)
	}); err != nil {
		t.Fatalf("put superseded binding: %v", err)
	}
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		head, err := tx.GetFindingAdjudicationForRound(ctx, run.ID, 1)
		if err != nil {
			return err
		}
		if head.Digest != successor.Digest {
			t.Fatalf("head = %q, want successor %q", head.Digest, successor.Digest)
		}
		got, err := tx.GetFindingDisposition(ctx, finding.ID, 1)
		if err != nil {
			return err
		}
		if got.AdjudicationDigest != initial.Digest {
			t.Fatalf("disposition rebound to %q, want original %q", got.AdjudicationDigest, initial.Digest)
		}
		return nil
	}); err != nil {
		t.Fatalf("read superseded binding: %v", err)
	}
}

func TestPutFindingDispositionRejectsInvalidAdjudicationBinding(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		digest      func(domain.FindingAdjudication, domain.FindingAdjudication, domain.FindingAdjudication) domain.Digest
		want        error
		disposition domain.ReviewDisposition
	}{
		{
			name: "missing", disposition: domain.ReviewDispositionDeclined,
			digest: func(domain.FindingAdjudication, domain.FindingAdjudication, domain.FindingAdjudication) domain.Digest {
				return ""
			},
			want: domain.ErrEmptyField,
		},
		{
			name: "dangling", disposition: domain.ReviewDispositionDeclined,
			digest: func(domain.FindingAdjudication, domain.FindingAdjudication, domain.FindingAdjudication) domain.Digest {
				return domain.Digest("sha256:" + strings.Repeat("f", 64))
			},
			want: store.ErrNotFound,
		},
		{
			name: "wrong round", disposition: domain.ReviewDispositionDeferred,
			digest: func(_ domain.FindingAdjudication, roundTwo domain.FindingAdjudication, _ domain.FindingAdjudication) domain.Digest {
				return roundTwo.Digest
			},
			want: domain.ErrParentKeyMismatch,
		},
		{
			name: "wrong run", disposition: domain.ReviewDispositionDeclined,
			digest: func(_ domain.FindingAdjudication, _ domain.FindingAdjudication, otherRun domain.FindingAdjudication) domain.Digest {
				return otherRun.Digest
			},
			want: domain.ErrParentKeyMismatch,
		},
		{
			name: "route not admitted", disposition: domain.ReviewDispositionDeferred,
			digest: func(roundOne domain.FindingAdjudication, _ domain.FindingAdjudication, _ domain.FindingAdjudication) domain.Digest {
				return roundOne.Digest
			},
			want: domain.ErrInvalidDispositionAdjudication,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			st := openStore(t, store.Options{})
			at := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
			run := domain.Run{ID: "run-binding", ProjectID: "project-1", SpecDigest: adjSpecDigest, PolicyDigest: adjPolicyDigest}
			finding := domain.Finding{ID: "finding-binding", RunID: run.ID, Source: "codex", CreatedAt: at}
			roundOneRecord := dispositionReviewRecord(t, run.ID, 1, []domain.FindingID{finding.ID}, at)
			roundTwoRecord := dispositionReviewRecord(t, run.ID, 2, []domain.FindingID{finding.ID}, at.Add(time.Minute))
			otherRun := domain.Run{ID: "run-other-binding", ProjectID: "project-1", SpecDigest: adjSpecDigest, PolicyDigest: adjPolicyDigest}
			otherFinding := domain.Finding{ID: "finding-other-binding", RunID: otherRun.ID, Source: "codex", CreatedAt: at}
			otherRecord := dispositionReviewRecord(t, otherRun.ID, 1, []domain.FindingID{otherFinding.ID}, at)
			roundOne := finalDispositionAdjudication(t, run.ID, 1, map[domain.FindingID]domain.ReviewDisposition{
				finding.ID: domain.ReviewDispositionDeclined,
			}, at)
			roundTwo := finalDispositionAdjudication(t, run.ID, 2, map[domain.FindingID]domain.ReviewDisposition{
				finding.ID: domain.ReviewDispositionDeferred,
			}, at.Add(time.Minute))
			other := finalDispositionAdjudication(t, otherRun.ID, 1, map[domain.FindingID]domain.ReviewDisposition{
				otherFinding.ID: domain.ReviewDispositionDeclined,
			}, at)
			if err := st.Write(ctx, func(tx *store.WriteTx) error {
				if err := tx.PutRun(ctx, run); err != nil {
					return err
				}
				if err := tx.PutRun(ctx, otherRun); err != nil {
					return err
				}
				if err := tx.PutReviewRecord(ctx, roundOneRecord, []domain.Finding{finding}); err != nil {
					return err
				}
				if err := tx.PutReviewRecord(ctx, roundTwoRecord, []domain.Finding{finding}); err != nil {
					return err
				}
				if err := tx.PutReviewRecord(ctx, otherRecord, []domain.Finding{otherFinding}); err != nil {
					return err
				}
				for _, artifact := range []domain.FindingAdjudication{roundOne, roundTwo, other} {
					if err := tx.PutFindingAdjudication(ctx, artifact); err != nil {
						return err
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			disposition := domain.ReviewDispositionRecord{
				FindingID: finding.ID, RunID: run.ID, Round: 1,
				Disposition: tc.disposition, Reason: "final disposition",
				AdjudicationDigest: tc.digest(roundOne, roundTwo, other), CreatedAt: at,
			}
			err := st.Write(ctx, func(tx *store.WriteTx) error {
				return tx.PutFindingDisposition(ctx, disposition)
			})
			if !errors.Is(err, tc.want) {
				t.Fatalf("put = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestPutFindingDispositionRejectsForeignReviewRound(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openStore(t, store.Options{})
	run := domain.Run{ID: "run-binding", ProjectID: "project-1", SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy"}
	at := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	listed := domain.Finding{ID: "finding-listed", RunID: run.ID, Source: "codex", CreatedAt: at}
	foreign := domain.Finding{ID: "finding-foreign", RunID: run.ID, Source: "codex", CreatedAt: at}
	review := dispositionReviewRecord(t, run.ID, 1, []domain.FindingID{listed.ID}, at)
	remediation := dispositionReviewRecord(t, run.ID, 2, nil, at.Add(time.Minute))
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutRun(ctx, run); err != nil {
			return err
		}
		if err := tx.PutReviewRecord(ctx, review, []domain.Finding{listed}); err != nil {
			return err
		}
		if err := tx.PutReviewRecord(ctx, remediation, nil); err != nil {
			return err
		}
		if err := tx.PutFinding(ctx, foreign); err != nil {
			return err
		}
		return tx.PutFindingDisposition(ctx, domain.ReviewDispositionRecord{
			FindingID: foreign.ID, RunID: run.ID, Round: 1,
			Disposition: domain.ReviewDispositionFixed, Reason: "not in this pass",
			RemediationInvocationID: remediation.InvocationID, CreatedAt: at,
		})
	}); !errors.Is(err, domain.ErrParentKeyMismatch) {
		t.Fatalf("foreign-round disposition = %v, want parent mismatch", err)
	}
}

func TestPutFindingDispositionRejectsUnboundRemediationReview(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openStore(t, store.Options{})
	run := domain.Run{ID: "run-remediation", ProjectID: "project-1", SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy"}
	at := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	finding := domain.Finding{ID: "finding-remediation", RunID: run.ID, Source: "codex", CreatedAt: at}
	review := dispositionReviewRecord(t, run.ID, 1, []domain.FindingID{finding.ID}, at)
	remediation := dispositionReviewRecord(t, run.ID, 2, nil, at.Add(time.Minute))
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutRun(ctx, run); err != nil {
			return err
		}
		if err := tx.PutReviewRecord(ctx, review, []domain.Finding{finding}); err != nil {
			return err
		}
		return tx.PutReviewRecord(ctx, remediation, nil)
	}); err != nil {
		t.Fatal(err)
	}
	disposition := domain.ReviewDispositionRecord{
		FindingID: finding.ID, RunID: run.ID, Round: 1,
		Disposition: domain.ReviewDispositionFixed, Reason: "fixed in a mismatched head",
		RemediationInvocationID: review.InvocationID, CreatedAt: at,
	}
	err := st.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutFindingDisposition(ctx, disposition)
	})
	if !errors.Is(err, domain.ErrParentKeyMismatch) {
		t.Fatalf("unbound remediation review = %v, want parent mismatch", err)
	}
}

func TestFindingDispositionSurvivesProcessKill(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "store.db")
	// A reexecuted helper cannot share the parent's cached template. Prepare
	// its file here so its readiness budget covers the write being tested.
	prepared := storetest.Open(t, path, store.Options{})
	if err := prepared.Close(); err != nil {
		t.Fatal(err)
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	cmd := exec.Command(binary, "-test.run=^TestFindingDispositionKillWriter$") //nolint:gosec // reexecutes this test binary with fixed arguments.
	cmd.Env = append(os.Environ(),
		"FREESIDE_FINDING_DISPOSITION_KILL_DB="+path,
	)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = &output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
		}
	})
	ready := make(chan error, 1)
	go func() {
		line, err := bufio.NewReader(stdout).ReadString('\n')
		if err == nil && line != "ready\n" {
			err = fmt.Errorf("unexpected readiness marker %q", line)
		}
		ready <- err
	}()
	select {
	case err := <-ready:
		if err != nil {
			t.Fatalf("kill writer readiness: %v: %s", err, output.String())
		}
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatalf("kill writer did not become ready: %s", output.String())
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err == nil {
		t.Fatal("kill writer exited cleanly, want forced termination")
	}

	ctx := context.Background()
	reopened := storetest.Open(t, path, store.Options{})
	err = reopened.Read(ctx, func(tx *store.ReadTx) error {
		got, err := tx.GetFindingDisposition(ctx, "finding-kill", 1)
		if err != nil {
			return err
		}
		if got.Disposition != domain.ReviewDispositionFixed || got.Reason != "committed before kill" {
			t.Fatalf("recovered disposition = %#v", got)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestFindingDispositionKillWriter(t *testing.T) {
	path := os.Getenv("FREESIDE_FINDING_DISPOSITION_KILL_DB")
	if path == "" {
		t.Skip("helper process")
	}
	ctx := context.Background()
	st := storetest.Open(t, path, store.Options{})
	at := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	run := domain.Run{ID: "run-kill", ProjectID: "project-1", SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy"}
	finding := domain.Finding{ID: "finding-kill", RunID: run.ID, Source: "codex", CreatedAt: at}
	review := dispositionReviewRecord(t, run.ID, 1, []domain.FindingID{finding.ID}, at)
	remediation := dispositionReviewRecord(t, run.ID, 2, nil, at.Add(time.Minute))
	disposition := domain.ReviewDispositionRecord{
		FindingID: finding.ID, RunID: run.ID, Round: 1,
		Disposition: domain.ReviewDispositionFixed, Reason: "committed before kill",
		RemediationInvocationID: remediation.InvocationID, CreatedAt: at,
	}
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutRun(ctx, run); err != nil {
			return err
		}
		if err := tx.PutReviewRecord(ctx, review, []domain.Finding{finding}); err != nil {
			return err
		}
		if err := tx.PutReviewRecord(ctx, remediation, nil); err != nil {
			return err
		}
		return tx.PutFindingDisposition(ctx, disposition)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintln(os.Stdout, "ready"); err != nil {
		t.Fatal(err)
	}
	select {}
}
