package store_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

func modelAdjudication(
	t *testing.T, runID domain.RunID, round int, findingID domain.FindingID, at time.Time,
) domain.FindingAdjudication {
	t.Helper()
	entry, err := domain.NewModelAdjudicationEntry(
		findingID, domain.GoalContradictory, nil, domain.RouteDecline,
		domain.ConfidenceHigh, "the finding contradicts the approved work unit",
		nil, []string{"AGENTS.md"}, []string{"the contract is current"}, nil, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := domain.NewFindingAdjudication(
		runID, round, adjSpecDigest, adjInstructionDigest, adjPolicyDigest,
		[]domain.FindingAdjudicationEntry{entry}, at,
	)
	if err != nil {
		t.Fatal(err)
	}
	return artifact
}

func adjudicationItem(
	t *testing.T, id domain.ItemID, binding domain.FindingAdjudicationBinding,
) domain.AttentionItem {
	t.Helper()
	runID := binding.RunID
	requestedDecision := []domain.Action{
		domain.ActionAcceptRecommendedRoute, domain.ActionDiscuss, domain.ActionStop,
	}
	if slices.ContainsFunc(binding.Proposals, func(proposal domain.FindingAdjudicationProposal) bool {
		return len(proposal.OfferedAlternatives) > 0
	}) {
		requestedDecision = append(requestedDecision, domain.ActionChooseAlternativeRoute)
	}
	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: id, ProjectID: "project-1",
		Subject: domain.Subject{Type: domain.SubjectRun, ID: domain.SubjectID(runID), RunID: &runID},
		Type:    domain.AttentionFindingAdjudication, Priority: domain.PriorityHigh,
		Reason:              "choose the finding route",
		RequestedDecision:   requestedDecision,
		PRHeadSHA:           fmt.Sprintf("head-%d", binding.Round),
		FindingAdjudication: &binding, ItemVersion: 1,
		InterruptionClass: domain.InterruptionPlannedGate, Status: domain.StatusOpen,
	}, nil)
	if err != nil {
		t.Fatalf("new attention item: %v", err)
	}
	return item
}

// bindingFromAdjudication builds the valid projected binding for artifact,
// mirroring the engine producer: the daemon-authenticated message and location
// come from the matching stored Finding (passed alongside so the re-gate's
// GetFinding join is satisfied) and the evidence from the artifact entry.
func bindingFromAdjudication(
	artifact domain.FindingAdjudication, findings ...domain.Finding,
) domain.FindingAdjudicationBinding {
	byID := make(map[domain.FindingID]domain.Finding, len(findings))
	for _, finding := range findings {
		byID[finding.ID] = finding
	}
	proposals := make([]domain.FindingAdjudicationProposal, 0, len(artifact.Entries))
	for _, entry := range artifact.Entries {
		finding := byID[entry.FindingID]
		proposals = append(proposals, domain.FindingAdjudicationProposal{
			FindingID:        entry.FindingID,
			FindingMessage:   domain.NormalizeFindingMessage(finding.Message),
			FindingLocation:  finding.Location,
			Producer:         entry.Producer,
			GoalRelationship: entry.GoalRelationship, Compatibility: entry.Compatibility,
			Route: entry.Route, Rationale: entry.Rationale, Evidence: slices.Clone(entry.Evidence),
			CitedRules: slices.Clone(entry.CitedRules), Assumptions: slices.Clone(entry.Assumptions),
			OpenQuestions: slices.Clone(entry.OpenQuestions), Confidence: entry.Confidence,
			OfferedAlternatives: slices.Clone(entry.OfferedAlternatives),
		})
	}
	return domain.FindingAdjudicationBinding{
		RunID: artifact.RunID, Round: artifact.Round, AdjudicationDigest: artifact.Digest,
		Proposals: proposals,
	}
}

func TestPutAttentionItemRegatesFindingAdjudicationBinding(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	runID := domain.RunID("run-item-adjudication")
	at := time.Date(2026, 8, 21, 20, 0, 0, 0, time.UTC)
	findingID := domain.FindingID("finding-a")
	finding := adjudicationFinding(findingID, runID, "daemon/a.go", at)
	st := seedReviewRound(t, runID, 1, []domain.Finding{finding}, at)
	artifact := modelAdjudication(t, runID, 1, findingID, at)
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutFindingAdjudication(ctx, artifact)
	}); err != nil {
		t.Fatalf("put artifact: %v", err)
	}

	base := bindingFromAdjudication(artifact, finding)
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutAttentionItem(ctx, adjudicationItem(t, "item-valid", base))
	}); err != nil {
		t.Fatalf("put matching item: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*domain.FindingAdjudicationBinding)
	}{
		{"missing digest", func(b *domain.FindingAdjudicationBinding) { b.AdjudicationDigest = adjudicationDigest("f") }},
		{"run", func(b *domain.FindingAdjudicationBinding) { b.RunID = "other-run" }},
		{"round", func(b *domain.FindingAdjudicationBinding) { b.Round++ }},
		{"finding", func(b *domain.FindingAdjudicationBinding) { b.Proposals[0].FindingID = "other-finding" }},
		{"route", func(b *domain.FindingAdjudicationBinding) {
			// The recommendation and its offered set are coupled on the two-route
			// contradictory row; swap both to a self-consistent shape that still
			// diverges from the artifact, so the mismatch is caught by the store
			// re-gate rather than the item's own construction validation.
			b.Proposals[0].Route = domain.RouteDispute
			b.Proposals[0].OfferedAlternatives = []domain.OfferedAlternative{{
				Route: domain.RouteDecline, Consequence: "decline the finding instead",
			}}
		}},
		{"confidence", func(b *domain.FindingAdjudicationBinding) {
			confidence := domain.ConfidenceMedium
			b.Proposals[0].Confidence = &confidence
		}},
		{"axes", func(b *domain.FindingAdjudicationBinding) {
			b.Proposals[0].GoalRelationship = domain.GoalUnclear
			b.Proposals[0].Route = domain.RouteAttentionUnclear
			b.Proposals[0].OfferedAlternatives = nil
		}},
		{"producer", func(b *domain.FindingAdjudicationBinding) {
			compatibility := domain.CompatibilityAllowed
			b.Proposals[0].Producer = domain.AdjudicationProducerEngine
			b.Proposals[0].GoalRelationship = domain.GoalRequired
			b.Proposals[0].Compatibility = &compatibility
			b.Proposals[0].Route = domain.RouteRemediate
			b.Proposals[0].Confidence = nil
			b.Proposals[0].OfferedAlternatives = nil
		}},
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			binding := bindingFromAdjudication(artifact, finding)
			test.mutate(&binding)
			item := adjudicationItem(t, domain.ItemID(fmt.Sprintf("item-mismatch-%d", index)), binding)
			err := st.Write(ctx, func(tx *store.WriteTx) error {
				return tx.PutAttentionItem(ctx, item)
			})
			if !errors.Is(err, domain.ErrParentKeyMismatch) {
				t.Fatalf("put = %v, want ErrParentKeyMismatch", err)
			}
		})
	}
	mismatchedHead := adjudicationItem(t, "item-head-mismatch", base)
	mismatchedHead.PRHeadSHA = "other-head"
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutAttentionItem(ctx, mismatchedHead)
	}); !errors.Is(err, domain.ErrParentKeyMismatch) {
		t.Fatalf("put mismatched review head = %v, want ErrParentKeyMismatch", err)
	}
}

func TestFindingAdjudicationDecisionAuthenticatesCausalCommandHistory(t *testing.T) {
	t.Parallel()
	for _, action := range []domain.Action{
		domain.ActionAcceptRecommendedRoute, domain.ActionStop,
	} {
		t.Run(string(action), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			runID := domain.RunID("run-command-history-" + string(action))
			at := time.Date(2026, 8, 24, 3, 0, 0, 0, time.UTC)
			findingID := domain.FindingID("finding-command-history")
			finding := adjudicationFinding(findingID, runID, "daemon/a.go", at)
			st := seedReviewRound(t, runID, 1, []domain.Finding{finding}, at)
			artifact := modelAdjudication(t, runID, 1, findingID, at)
			item := adjudicationItem(t, "item-command-history", bindingFromAdjudication(artifact, finding))
			command, err := domain.NewCommand(domain.CommandInput{
				CommandID: "command-" + string(action), DeviceID: "device-1",
				ItemID: item.ID, ItemVersion: item.ItemVersion, PRHeadSHA: item.PRHeadSHA,
				ArtifactDigests: item.ArtifactDigests, Action: action,
			})
			if err != nil {
				t.Fatal(err)
			}
			var concluded domain.AttentionItem
			if err := st.Write(ctx, func(tx *store.WriteTx) error {
				if err := tx.PutFindingAdjudication(ctx, artifact); err != nil {
					return err
				}
				if err := tx.PutAttentionItem(ctx, item); err != nil {
					return err
				}
				if err := tx.PutCommand(ctx, command); err != nil {
					return err
				}
				concluded, err = item.WithDecidedAt(at.Add(time.Minute))
				if err != nil {
					return err
				}
				concluded.Status = domain.StatusResolved
				concluded.ItemVersion++
				return tx.PutAttentionItem(ctx, concluded)
			}); err != nil {
				t.Fatal(err)
			}
			// A legal later terminal-item version advance must not break the
			// causal link, which comes from immutable command history rather than
			// current_item_version - 1 arithmetic.
			advanced := concluded
			advanced.ItemVersion++
			if err := st.Write(ctx, func(tx *store.WriteTx) error {
				return tx.PutAttentionItem(ctx, advanced)
			}); err != nil {
				t.Fatal(err)
			}
			if err := st.Read(ctx, func(tx *store.ReadTx) error {
				gotItem, gotCommand, err := tx.FindingAdjudicationDecision(ctx, item.ID)
				if err != nil {
					return err
				}
				if gotItem.ItemVersion != advanced.ItemVersion || gotCommand == nil ||
					gotCommand.CommandID != command.CommandID || gotCommand.Action != action {
					t.Fatalf("decision = item v%d, %#v", gotItem.ItemVersion, gotCommand)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestPutAttentionItemRejectsIncompleteOrTamperedAdjudicationProjection(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	runID := domain.RunID("run-item-adjudication-projection")
	at := time.Date(2026, 8, 21, 20, 0, 0, 0, time.UTC)
	findings := []domain.Finding{
		adjudicationFinding("finding-a", runID, "daemon/a.go", at),
		adjudicationFinding("finding-b", runID, "daemon/b.go", at),
	}
	st := seedReviewRound(t, runID, 1, findings, at)
	entries := make([]domain.FindingAdjudicationEntry, 0, len(findings))
	for _, finding := range findings {
		entry, err := domain.NewModelAdjudicationEntry(
			finding.ID, domain.GoalContradictory, nil, domain.RouteDecline,
			domain.ConfidenceHigh, "the finding contradicts the approved work unit",
			[]string{"the reported change lies outside the declared paths", "no contract update accompanies it"},
			[]string{"AGENTS.md"}, []string{"the contract is current"}, nil,
			[]string{"is the contract current?"},
		)
		if err != nil {
			t.Fatal(err)
		}
		entries = append(entries, entry)
	}
	artifact, err := domain.NewFindingAdjudication(
		runID, 1, adjSpecDigest, adjInstructionDigest, adjPolicyDigest, entries, at,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutFindingAdjudication(ctx, artifact)
	}); err != nil {
		t.Fatalf("put artifact: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*domain.FindingAdjudicationBinding)
	}{
		{"missing proposal", func(b *domain.FindingAdjudicationBinding) { b.Proposals = b.Proposals[:1] }},
		{"rationale", func(b *domain.FindingAdjudicationBinding) { b.Proposals[0].Rationale = "different rationale" }},
		{"cited rules", func(b *domain.FindingAdjudicationBinding) { b.Proposals[0].CitedRules = []string{"other rule"} }},
		{"assumptions", func(b *domain.FindingAdjudicationBinding) { b.Proposals[0].Assumptions = []string{"other assumption"} }},
		{"open questions", func(b *domain.FindingAdjudicationBinding) { b.Proposals[0].OpenQuestions = []string{"other question"} }},
		// The offered set is authenticated element-wise against the artifact
		// (#893): dropping the alternative, or keeping its route while rewriting
		// the consequence, must both fail closed. finding-a is contradictory with a
		// decline recommendation, so its digest-bound offer is the dispute route.
		{"offered removed", func(b *domain.FindingAdjudicationBinding) { b.Proposals[0].OfferedAlternatives = nil }},
		{"offered consequence", func(b *domain.FindingAdjudicationBinding) {
			b.Proposals[0].OfferedAlternatives = []domain.OfferedAlternative{{
				Route: domain.RouteDispute, Consequence: "forged operator-facing consequence",
			}}
		}},
		// The daemon-authenticated coordinates are re-gated against the stored
		// Finding (#892): a message or location that diverges from it, and evidence
		// that diverges from the artifact entry (altered, reordered, or dropped),
		// must all fail closed.
		{"finding message", func(b *domain.FindingAdjudicationBinding) {
			b.Proposals[0].FindingMessage = "forged operator-facing finding message"
		}},
		{"finding location", func(b *domain.FindingAdjudicationBinding) {
			b.Proposals[0].FindingLocation = &domain.FindingLocation{Path: "daemon/forged.go", StartLine: 1, EndLine: 1}
		}},
		{"evidence altered", func(b *domain.FindingAdjudicationBinding) {
			b.Proposals[0].Evidence = []string{"the reported change lies outside the declared paths", "forged second line"}
		}},
		{"evidence reordered", func(b *domain.FindingAdjudicationBinding) {
			b.Proposals[0].Evidence = []string{"no contract update accompanies it", "the reported change lies outside the declared paths"}
		}},
		{"evidence removed", func(b *domain.FindingAdjudicationBinding) { b.Proposals[0].Evidence = nil }},
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			binding := bindingFromAdjudication(artifact, findings...)
			test.mutate(&binding)
			err := st.Write(ctx, func(tx *store.WriteTx) error {
				return tx.PutAttentionItem(ctx, adjudicationItem(
					t, domain.ItemID(fmt.Sprintf("item-projection-mismatch-%d", index)), binding,
				))
			})
			if !errors.Is(err, domain.ErrParentKeyMismatch) {
				t.Fatalf("put = %v, want ErrParentKeyMismatch", err)
			}
		})
	}
}

// TestPutAttentionItemRegatesNilFindingLocation proves the daemon-authenticated
// location re-gate handles the review-level finding that carries no path (#892):
// a nil-location proposal round-trips against a nil-location finding, while a
// location minted into the proposal fails closed against the stored finding.
func TestPutAttentionItemRegatesNilFindingLocation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	runID := domain.RunID("run-item-adjudication-nil-location")
	at := time.Date(2026, 8, 21, 20, 0, 0, 0, time.UTC)
	findingID := domain.FindingID("finding-review-level")
	finding := domain.Finding{
		ID: findingID, RunID: runID, Source: "codex_local",
		Message: "a review-level observation with no path",
		RawText: "a review-level observation with no path", CreatedAt: at,
	}
	st := seedReviewRound(t, runID, 1, []domain.Finding{finding}, at)
	artifact := modelAdjudication(t, runID, 1, findingID, at)
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutFindingAdjudication(ctx, artifact)
	}); err != nil {
		t.Fatalf("put artifact: %v", err)
	}
	base := bindingFromAdjudication(artifact, finding)
	if base.Proposals[0].FindingLocation != nil {
		t.Fatalf("nil-location finding projected location %v", base.Proposals[0].FindingLocation)
	}
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutAttentionItem(ctx, adjudicationItem(t, "item-nil-location", base))
	}); err != nil {
		t.Fatalf("put nil-location item: %v", err)
	}
	minted := bindingFromAdjudication(artifact, finding)
	minted.Proposals[0].FindingLocation = &domain.FindingLocation{Path: "daemon/a.go", StartLine: 1, EndLine: 1}
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutAttentionItem(ctx, adjudicationItem(t, "item-nil-location-minted", minted))
	}); !errors.Is(err, domain.ErrParentKeyMismatch) {
		t.Fatalf("put minted location = %v, want ErrParentKeyMismatch", err)
	}
}

// TestPutAttentionItemRegatesOfferedAlternativeNilEmpty proves the offered-set
// re-gate distinguishes a nil offered set from an empty one (#893): a
// non-contradictory entry carries no offered alternative, so an item claiming an
// empty (but non-nil) slice diverges from the digest-bound artifact and fails
// closed, matching the nil-versus-empty parity the other list fields enforce.
func TestPutAttentionItemRegatesOfferedAlternativeNilEmpty(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	runID := domain.RunID("run-item-adjudication-nil-empty")
	at := time.Date(2026, 8, 21, 20, 0, 0, 0, time.UTC)
	findingID := domain.FindingID("finding-adjacent")
	st := seedReviewRound(t, runID, 1, []domain.Finding{
		adjudicationFinding(findingID, runID, "daemon/a.go", at),
	}, at)
	entry, err := domain.NewModelAdjudicationEntry(
		findingID, domain.GoalAdjacent, nil, domain.RouteDefer,
		domain.ConfidenceHigh, "the finding is adjacent to the approved work unit",
		nil, []string{"AGENTS.md"}, []string{"the contract is current"}, nil, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if entry.OfferedAlternatives != nil {
		t.Fatalf("adjacent entry offered = %v, want nil", entry.OfferedAlternatives)
	}
	artifact, err := domain.NewFindingAdjudication(
		runID, 1, adjSpecDigest, adjInstructionDigest, adjPolicyDigest,
		[]domain.FindingAdjudicationEntry{entry}, at,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutFindingAdjudication(ctx, artifact)
	}); err != nil {
		t.Fatalf("put artifact: %v", err)
	}
	binding := bindingFromAdjudication(artifact, adjudicationFinding(findingID, runID, "daemon/a.go", at))
	binding.Proposals[0].OfferedAlternatives = []domain.OfferedAlternative{}
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutAttentionItem(ctx, adjudicationItem(t, "item-nil-empty", binding))
	}); !errors.Is(err, domain.ErrParentKeyMismatch) {
		t.Fatalf("put empty-vs-nil offered = %v, want ErrParentKeyMismatch", err)
	}
}

// TestAttentionItemReadsRegateOfferedAlternatives proves a raw-row rewrite of an
// item's offered consequence fails closed on every snapshot-backed read (#893),
// so restart and direct database tampering cannot detach the offered set from
// its digest-bound artifact.
func TestAttentionItemReadsRegateOfferedAlternatives(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	runID := domain.RunID("run-item-adjudication-offered-reconstruction")
	at := time.Date(2026, 8, 21, 20, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "store.db")
	findingID := domain.FindingID("finding-a")
	st := seedReviewRoundAt(t, path, runID, 1, []domain.Finding{
		adjudicationFinding(findingID, runID, "daemon/a.go", at),
	}, at)
	artifact := modelAdjudication(t, runID, 1, findingID, at)
	item := adjudicationItem(t, "item-offered-reconstruction",
		bindingFromAdjudication(artifact, adjudicationFinding(findingID, runID, "daemon/a.go", at)))
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutFindingAdjudication(ctx, artifact); err != nil {
			return err
		}
		return tx.PutAttentionItem(ctx, item)
	}); err != nil {
		t.Fatalf("put fixture: %v", err)
	}

	item.FindingAdjudication.Proposals[0].OfferedAlternatives[0].Consequence = "forged operator-facing consequence"
	body, err := json.Marshal(item)
	if err != nil {
		t.Fatalf("marshal forged item: %v", err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open raw database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.ExecContext(ctx, `UPDATE attention_items SET body = ? WHERE id = ?`, string(body), item.ID); err != nil {
		t.Fatalf("forge item body: %v", err)
	}

	reads := []struct {
		name string
		read func(*store.ReadTx) error
	}{
		{"get snapshot", func(tx *store.ReadTx) error {
			_, _, err := tx.GetAttentionItemSnapshot(ctx, item.ID)
			return err
		}},
		{"list all", func(tx *store.ReadTx) error {
			_, err := tx.ListAttentionItems(ctx)
			return err
		}},
		{"list open type", func(tx *store.ReadTx) error {
			_, err := tx.ListOpenAttentionItems(ctx, domain.AttentionFindingAdjudication)
			return err
		}},
		{"list open run", func(tx *store.ReadTx) error {
			_, err := tx.ListOpenAttentionItemsForRun(ctx, runID)
			return err
		}},
	}
	for _, test := range reads {
		t.Run(test.name, func(t *testing.T) {
			if err := st.Read(ctx, test.read); !errors.Is(err, domain.ErrParentKeyMismatch) {
				t.Fatalf("read = %v, want ErrParentKeyMismatch", err)
			}
		})
	}
}

func TestAttentionItemReadsRegateFindingAdjudicationBinding(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	runID := domain.RunID("run-item-adjudication-reconstruction")
	at := time.Date(2026, 8, 21, 20, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "store.db")
	findingID := domain.FindingID("finding-a")
	st := seedReviewRoundAt(t, path, runID, 1, []domain.Finding{
		adjudicationFinding(findingID, runID, "daemon/a.go", at),
	}, at)
	artifact := modelAdjudication(t, runID, 1, findingID, at)
	item := adjudicationItem(t, "item-reconstruction",
		bindingFromAdjudication(artifact, adjudicationFinding(findingID, runID, "daemon/a.go", at)))
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutFindingAdjudication(ctx, artifact); err != nil {
			return err
		}
		return tx.PutAttentionItem(ctx, item)
	}); err != nil {
		t.Fatalf("put fixture: %v", err)
	}

	item.FindingAdjudication.Proposals[0].Rationale = "forged operator-facing rationale"
	body, err := json.Marshal(item)
	if err != nil {
		t.Fatalf("marshal forged item: %v", err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open raw database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.ExecContext(ctx, `UPDATE attention_items SET body = ? WHERE id = ?`, string(body), item.ID); err != nil {
		t.Fatalf("forge item body: %v", err)
	}

	reads := []struct {
		name string
		read func(*store.ReadTx) error
	}{
		{"get snapshot", func(tx *store.ReadTx) error {
			_, _, err := tx.GetAttentionItemSnapshot(ctx, item.ID)
			return err
		}},
		{"list all", func(tx *store.ReadTx) error {
			_, err := tx.ListAttentionItems(ctx)
			return err
		}},
		{"list open type", func(tx *store.ReadTx) error {
			_, err := tx.ListOpenAttentionItems(ctx, domain.AttentionFindingAdjudication)
			return err
		}},
		{"list open run", func(tx *store.ReadTx) error {
			_, err := tx.ListOpenAttentionItemsForRun(ctx, runID)
			return err
		}},
	}
	for _, test := range reads {
		t.Run(test.name, func(t *testing.T) {
			err := st.Read(ctx, test.read)
			if !errors.Is(err, domain.ErrParentKeyMismatch) {
				t.Fatalf("read = %v, want ErrParentKeyMismatch", err)
			}
		})
	}
}

func TestAttentionItemReadsRejectFindingAdjudicationReviewHeadMismatch(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	runID := domain.RunID("run-item-adjudication-head-reconstruction")
	at := time.Date(2026, 8, 21, 20, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "store.db")
	findingID := domain.FindingID("finding-a")
	st := seedReviewRoundAt(t, path, runID, 1, []domain.Finding{
		adjudicationFinding(findingID, runID, "daemon/a.go", at),
	}, at)
	artifact := modelAdjudication(t, runID, 1, findingID, at)
	item := adjudicationItem(t, "item-head-reconstruction",
		bindingFromAdjudication(artifact, adjudicationFinding(findingID, runID, "daemon/a.go", at)))
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutFindingAdjudication(ctx, artifact); err != nil {
			return err
		}
		return tx.PutAttentionItem(ctx, item)
	}); err != nil {
		t.Fatalf("put fixture: %v", err)
	}

	item.PRHeadSHA = "other-head"
	body, err := json.Marshal(item)
	if err != nil {
		t.Fatalf("marshal forged item: %v", err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open raw database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.ExecContext(ctx, `UPDATE attention_items SET body = ? WHERE id = ?`, string(body), item.ID); err != nil {
		t.Fatalf("forge item body: %v", err)
	}

	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		_, _, err := tx.GetAttentionItemSnapshot(ctx, item.ID)
		return err
	}); !errors.Is(err, domain.ErrParentKeyMismatch) {
		t.Fatalf("read mismatched review head = %v, want ErrParentKeyMismatch", err)
	}
}
