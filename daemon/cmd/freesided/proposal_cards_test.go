package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/golden"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/strictjson"
)

// TestProposalCardsGolden pins the read's shape. A change to these bytes is a
// change to proposalCardsVersion, and to taskRecordVersion, which embeds the
// card.
func TestProposalCardsGolden(t *testing.T) {
	at := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	effect, task := domain.AttentionEffectProposal, domain.AttentionTaskProposal
	project, open := domain.ProjectID("gh-imgup"), domain.StatusOpen
	closure, taskProposal := domain.EffectSourceIssueClosure, domain.EffectTaskProposal
	runID, taskID := domain.RunID("run-117"), domain.TaskID("task-117")
	handle := domain.OpaqueSubjectHandle(domain.WorkUnitIDForRun(runID))
	closureInstance := domain.ProposalInstanceID("proposal-" + strings.Repeat("1", 64))
	taskInstance := domain.ProposalInstanceID("proposal-" + strings.Repeat("2", 64))
	brokenInstance := domain.ProposalInstanceID("proposal-" + strings.Repeat("3", 64))
	got, err := json.MarshalIndent(proposalCards{
		Version: proposalCardsVersion, AsOfRevision: 57, AsOf: at,
		Cards: []proposalCard{
			{
				ItemID: domain.ItemID(closureInstance) + "/effect/0a1b2c3d", Type: &effect, ProjectID: &project,
				Status: &open, CreatedAt: new(at.Add(-time.Hour)),
				RequestedDecision: []domain.Action{
					domain.ActionApprove, domain.ActionApproveWithChanges, domain.ActionDecline, domain.ActionSnooze,
				},
				InstanceID: &closureInstance, Kind: &closure, SubjectHandle: &handle, RunID: &runID, TaskID: &taskID,
				Merge: &domain.ProspectiveMerge{
					PublicationIdentity: domain.Digest("sha256:" + strings.Repeat("9", 64)),
					CandidateHeadSHA:    strings.Repeat("c", 40), BaseRef: "main", BaseSHA: strings.Repeat("b", 40),
				},
			},
			{
				ItemID: domain.ItemID(taskInstance), Type: &task, ProjectID: &project, Status: &open,
				CreatedAt:         new(at.Add(-50 * time.Minute)),
				RequestedDecision: []domain.Action{domain.ActionStart, domain.ActionStartWithChanges, domain.ActionDecline, domain.ActionSnooze},
				InstanceID:        &taskInstance, Kind: &taskProposal, SubjectHandle: &handle, RunID: &runID, TaskID: &taskID,
				SnoozedUntil: new(at.Add(-10 * time.Minute)),
			},
			{
				ItemID: domain.ItemID(brokenInstance) + "/effect/4e5f6a7b", Type: &effect, ProjectID: &project,
				Status: &open, CreatedAt: new(at.Add(-40 * time.Minute)),
				RequestedDecision: []domain.Action{domain.ActionApprove, domain.ActionDecline, domain.ActionSnooze},
				Error:             &taskRecordError{Kind: taskRecordErrorIntegrity, Message: "stored row inconsistent"},
			},
		},
		RefusedRows: []systemHealthRefusedRow{{
			ID:    "execution-failure-evidence",
			Error: &taskRecordError{Kind: taskRecordErrorUnapprovedRecipe, Message: "recipe not approved"},
		}},
	}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	golden.Assert(t, "proposal_cards", append(got, '\n'))

	// A decided card is closed, so only a task record's closure_cards show
	// one; this pins the decision's shape there.
	resolved, digest := domain.StatusResolved, domain.Digest("sha256:"+strings.Repeat("d", 64))
	got, err = json.MarshalIndent(proposalCard{
		ItemID: domain.ItemID(closureInstance) + "/effect/0a1b2c3d", Type: &effect, ProjectID: &project,
		Status: &resolved, CreatedAt: new(at.Add(-time.Hour)),
		RequestedDecision: []domain.Action{
			domain.ActionApprove, domain.ActionApproveWithChanges, domain.ActionDecline, domain.ActionSnooze,
		},
		InstanceID: &closureInstance, Kind: &closure, SubjectHandle: &handle, RunID: &runID, TaskID: &taskID,
		Merge: &domain.ProspectiveMerge{
			PublicationIdentity: domain.Digest("sha256:" + strings.Repeat("9", 64)),
			CandidateHeadSHA:    strings.Repeat("c", 40), BaseRef: "main", BaseSHA: strings.Repeat("b", 40),
		},
		Decision: &proposalCardDecision{
			CommandID: "command-approve", Action: domain.ActionApprove, SelectedDigest: &digest,
			DecidedAt: at.Add(-5 * time.Minute),
		},
	}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	golden.Assert(t, "proposal_card_decided", append(got, '\n'))
}

// TestInspectProposalsServedEqualsDirect reads the root's cards through a
// daemon launched with an approved recipe, stops it, and reads the file. With
// the daemon's digest the read equals the served one. Without it the cards
// are the same, because the store always approves the proposal carrier's
// built-in recipe; only another item whose evidence names the daemon's
// recipe is refused, listed under refused_rows with the stderr note. A
// decided card is closed and not listed.
func TestInspectProposalsServedEqualsDirect(t *testing.T) {
	recipe := domain.Digest("sha256:" + strings.Repeat("e", 64))
	dbPath := filepath.Join(t.TempDir(), "freeside.db")
	h := startDaemon(t, config{
		DBPath: dbPath, ListenAddr: "127.0.0.1:0", SeedFixture: "representative",
		Environment: environmentEphemeral, ApprovedRecipes: map[domain.Digest]bool{recipe: true},
	})
	ctx := t.Context()
	open := seedProposalCards(t, h.store, "open")
	decided := seedProposalCards(t, h.store, "decided")
	decided.decide(t, h.store, decided.closureCard, domain.ActionDecline)
	const evidence = domain.ItemID("execution-failure-evidence")
	putRecipeEvidenceItem(t, h.store, recipe, fixtureActiveRunID, evidence)
	served := inspect[proposalCards](t, ctx, "proposals", "-db", dbPath)
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	direct := func(args ...string) (proposalCards, string) {
		t.Helper()
		var stdout, stderr bytes.Buffer
		if err := runInspectCommand(ctx, append([]string{"proposals", "-db", dbPath}, args...), &stdout, &stderr); err != nil {
			t.Fatal(err)
		}
		var cards proposalCards
		if err := strictjson.Decode(stdout.Bytes(), &cards, strictjson.RejectInvalidUTF8, strictjson.NoLimit); err != nil {
			t.Fatal(err)
		}
		return cards, stderr.String()
	}
	approved, note := direct("-approved-recipe", string(recipe))
	if !bytes.Equal(comparableCards(t, approved), comparableCards(t, served)) || note != "" {
		t.Fatalf("approved direct read differs from the served one, or stderr = %q\ndirect: %s\nserved: %s",
			note, comparableCards(t, approved), comparableCards(t, served))
	}
	unflagged, note := direct()
	if len(unflagged.RefusedRows) != 1 || unflagged.RefusedRows[0].ID != evidence ||
		unflagged.RefusedRows[0].Error.Kind != taskRecordErrorUnapprovedRecipe {
		t.Fatalf("refused rows = %+v", unflagged.RefusedRows)
	}
	if !strings.Contains(note, "-approved-recipe") || !strings.Contains(note, "approved 0 ") {
		t.Fatalf("stderr = %q, want the approved-recipe note", note)
	}
	unflagged.RefusedRows = served.RefusedRows
	if !bytes.Equal(comparableCards(t, unflagged), comparableCards(t, served)) {
		t.Fatalf("cards read with no -approved-recipe differ from the served ones\ndirect: %s\nserved: %s",
			comparableCards(t, unflagged), comparableCards(t, served))
	}
	if served.Version != proposalCardsVersion || len(served.RefusedRows) != 0 {
		t.Fatalf("cards = %s", comparableCards(t, served))
	}
	closure := listedCard(served, open.closureCard.ID)
	if closure == nil || closure.Error != nil || !equalPointers(closure.Kind, new(domain.EffectSourceIssueClosure)) ||
		!equalPointers(closure.Status, new(domain.StatusOpen)) || !equalPointers(closure.RunID, &open.runID) ||
		!equalPointers(closure.TaskID, &open.taskID) || !equalPointers(closure.InstanceID, &open.closure.ID) ||
		closure.Merge == nil || *closure.Merge != open.merge || closure.Decision != nil {
		t.Fatalf("closure card = %+v", closure)
	}
	task := listedCard(served, open.taskCard.ID)
	if task == nil || task.Error != nil || !equalPointers(task.Kind, new(domain.EffectTaskProposal)) ||
		!equalPointers(task.RunID, &open.runID) || task.Merge != nil {
		t.Fatalf("task-proposal card = %+v", task)
	}
	if listedCard(served, decided.closureCard.ID) != nil || listedCard(served, decided.taskCard.ID) == nil {
		t.Fatalf("decided seed: closure listed %t, task proposal listed %t",
			listedCard(served, decided.closureCard.ID) != nil, listedCard(served, decided.taskCard.ID) != nil)
	}
}

// TestTaskRecordListsRunClosureCards: a run's closure cards read beside its
// publication, open or closed, with the decision that closed one. The run's
// task-proposal card is not a closure card and stays out.
func TestTaskRecordListsRunClosureCards(t *testing.T) {
	st, _ := openSeededStore(t)
	seed := seedProposalCards(t, st, "record")
	closureCards := func() []proposalCard {
		t.Helper()
		record := readRecord(t, st, seed.taskID)
		if len(record.Runs) != 1 || record.Runs[0].RunID != seed.runID || len(record.Runs[0].Errors) != 0 {
			t.Fatalf("runs = %+v", record.Runs)
		}
		return record.Runs[0].ClosureCards
	}
	cards := closureCards()
	if len(cards) != 1 || cards[0].ItemID != seed.closureCard.ID || cards[0].Error != nil ||
		!equalPointers(cards[0].Status, new(domain.StatusOpen)) || cards[0].Merge == nil ||
		*cards[0].Merge != seed.merge || cards[0].Decision != nil {
		t.Fatalf("closure cards = %+v", cards)
	}

	seed.decide(t, st, seed.closureCard, domain.ActionApprove)
	cards = closureCards()
	if len(cards) != 1 || !equalPointers(cards[0].Status, new(domain.StatusResolved)) || cards[0].Decision == nil {
		t.Fatalf("closure cards = %+v", cards)
	}
	decision := cards[0].Decision
	if decision.Action != domain.ActionApprove || decision.CommandID != "decide-"+string(seed.closureCard.ID) ||
		!equalPointers(decision.SelectedDigest, &seed.closure.Proposal.Digest) {
		t.Fatalf("decision = %+v", decision)
	}
	// The root-wide read lists open cards only.
	var cardsRead proposalCards
	if err := st.Read(t.Context(), func(tx *store.ReadTx) error {
		var err error
		cardsRead, err = readProposalCards(t.Context(), tx, taskRecordAsOf)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if listedCard(cardsRead, seed.closureCard.ID) != nil || listedCard(cardsRead, seed.taskCard.ID) == nil {
		t.Fatalf("root read lists the decided card, or not the open task proposal: %+v", cardsRead.Cards)
	}
}

// TestProposalCardReportsUnreadBinding: a card whose binding the store
// refuses keeps its place in both reads, with the item's fields, its error,
// and null for everything the binding would have said.
func TestProposalCardReportsUnreadBinding(t *testing.T) {
	st, dbPath := openSeededStore(t)
	ctx := t.Context()
	seed := seedProposalCards(t, st, "unread")
	raw, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	_, updateErr := raw.ExecContext(ctx, `UPDATE effect_proposal_items SET content_digest = ? WHERE item_id = ?`,
		"sha256:"+strings.Repeat("f", 64), seed.closureCard.ID)
	if closeErr := raw.Close(); updateErr != nil || closeErr != nil {
		t.Fatal(errors.Join(updateErr, closeErr))
	}
	unread := func(card *proposalCard) bool {
		return card != nil && card.Error != nil && equalPointers(card.Status, new(domain.StatusOpen)) &&
			card.InstanceID == nil && card.Kind == nil && card.RunID == nil && card.Merge == nil
	}
	var cards proposalCards
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		cards, err = readProposalCards(ctx, tx, taskRecordAsOf)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if card := listedCard(cards, seed.closureCard.ID); !unread(card) || len(cards.RefusedRows) != 0 {
		t.Fatalf("card = %+v, refused rows = %+v", card, cards.RefusedRows)
	}
	if card := listedCard(cards, seed.taskCard.ID); card == nil || card.Error != nil {
		t.Fatalf("task-proposal card = %+v", card)
	}
	record := readRecord(t, st, seed.taskID)
	if len(record.Runs) != 1 || len(record.Runs[0].ClosureCards) != 1 || !unread(&record.Runs[0].ClosureCards[0]) {
		t.Fatalf("runs = %+v", record.Runs)
	}

	// A refused item row: the root read lists it under refused_rows, and the
	// task record lists it as the run's closure card without counting it as
	// unattributed.
	execStoreSQL(t, dbPath, `UPDATE attention_items SET project_id = 'proj-elsewhere' WHERE id = '`+
		string(seed.closureCard.ID)+`'`)
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		cards, err = readProposalCards(ctx, tx, taskRecordAsOf)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(cards.RefusedRows) != 1 || cards.RefusedRows[0].ID != seed.closureCard.ID ||
		listedCard(cards, seed.closureCard.ID) != nil {
		t.Fatalf("refused rows = %+v, cards = %+v", cards.RefusedRows, cards.Cards)
	}
	record = readRecord(t, st, seed.taskID)
	if len(record.Runs) != 1 || len(record.Runs[0].ClosureCards) != 1 {
		t.Fatalf("runs = %+v", record.Runs)
	}
	if card := record.Runs[0].ClosureCards[0]; card.ItemID != seed.closureCard.ID || card.Error == nil || card.Type != nil ||
		!equalPointers(record.Counts.UnattributedRefusedItems, new(0)) {
		t.Fatalf("closure card = %+v, unattributed = %v", card, record.Counts.UnattributedRefusedItems)
	}
}

// proposalSeed is one run's work unit holding an open closure card and an
// open task-proposal card, written through the store and the attention
// service the way the daemon writes them.
type proposalSeed struct {
	service     *signet.Service
	runID       domain.RunID
	taskID      domain.TaskID
	closure     domain.ProposalInstance
	closureCard domain.AttentionItem
	taskCard    domain.AttentionItem
	merge       domain.ProspectiveMerge
}

var proposalSeedAt = time.Date(2026, 10, 24, 11, 0, 0, 0, time.UTC)

// seedProposalCards seeds a project, a task sourced from one of its issues,
// and a run whose work unit admits both cards. name keeps several seeds in
// one store apart.
func seedProposalCards(t *testing.T, st *store.Store, name string) proposalSeed {
	t.Helper()
	ctx := t.Context()
	projectID, repo := domain.ProjectID("proj-proposals-"+name), "owner/proposals-"+name
	repositoryID := int64(4000 + len(name))
	policy, err := domain.NewResolvedPolicy(domain.RunID("run-proposals-"+name), []domain.PolicyKey{{
		Key: "paths", Value: "daemon/", Provenance: domain.KeyProvenance{
			Source: domain.ProvenanceOverride, Digest: domain.Digest("sha256:" + strings.Repeat("a", 64)),
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	handle := domain.OpaqueSubjectHandle(domain.WorkUnitIDForRun(policy.RunID))
	issue := domain.IssueSubjectRef{Repo: repo, RepositoryID: repositoryID, IssueNumber: 117}
	closureProposal, err := domain.NewEffectProposal(domain.EffectSourceIssueClosure, domain.SourceIssueClosureInput{
		SubjectHandle: handle,
		Source: domain.ClosableSource{
			Present: true, Provenance: domain.ClosureProvenanceVerified,
			Repo: repo, RepositoryID: repositoryID, IssueNumber: issue.IssueNumber,
		},
		ProposedTarget: issue, Origin: domain.ClosureFlagOriginProposeSite, Resolves: true,
	}, policy)
	if err != nil {
		t.Fatal(err)
	}
	taskProposal, err := domain.NewEffectProposal(domain.EffectTaskProposal, domain.TaskProposalParameters{
		SubjectHandle: handle, Intent: domain.TaskProposalIntentImplement, ExpectedCostUnits: 10,
		Scope: domain.TaskProposalScope{ComponentCount: 1, DeclaredPathCount: 1},
	}, policy)
	if err != nil {
		t.Fatal(err)
	}
	seed := proposalSeed{runID: policy.RunID}
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.RegisterProject(ctx, domain.Project{ID: projectID, Repo: repo, RepositoryID: repositoryID}); err != nil {
			return err
		}
		task, err := tx.GetOrCreateTask(ctx, projectID, domain.SpecificationSource{
			Kind: domain.SpecificationSourceIssueSubject, IssueSubject: &issue,
		})
		if err != nil {
			return err
		}
		seed.taskID = task.ID
		if err := tx.PutRun(ctx, domain.Run{
			ID: policy.RunID, ProjectID: projectID, TaskID: task.ID,
			SpecDigest: "sha256:spec", PolicyDigest: policy.Digest, Stages: []domain.Stage{},
		}); err != nil {
			return err
		}
		if err := tx.PutResolvedPolicy(ctx, policy); err != nil {
			return err
		}
		declaration, err := domain.NewWorkUnitDeclaration(domain.WorkUnitDeclarationInput{
			CompletionCriterion: domain.CompletionBoundPRMerged, DeclaredPaths: domain.CanonicalDeclaredPaths(policy),
		}, policy.RunID, projectID, proposalSeedAt.Add(-time.Hour))
		if err != nil {
			return err
		}
		if err := tx.RecordWorkUnitDeclaration(ctx, declaration); err != nil {
			return err
		}
		if seed.closure, _, err = tx.AllocateProposalInstance(ctx,
			domain.ProposalAdmissionKey{Source: domain.ProposalSourceUpstreamEvent, UpstreamEventID: "closure-" + name},
			domain.ProposalBatchID("batch-closure-"+name), closureProposal, proposalSeedAt); err != nil {
			return err
		}
		seed.taskCard, err = putTaskProposalCard(ctx, tx, projectID, taskProposal, name)
		return err
	}); err != nil {
		t.Fatalf("seed proposal cards: %v", err)
	}
	blobs, err := signet.NewBlobStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	seed.service = signet.NewService(st, signet.WithBlobStore(blobs),
		signet.WithClock(func() time.Time { return proposalSeedAt }))
	seed.merge = domain.ProspectiveMerge{
		PublicationIdentity: domain.Digest("sha256:" + strings.Repeat("9", 64)),
		CandidateHeadSHA:    strings.Repeat("c", 40), BaseRef: "main", BaseSHA: strings.Repeat("b", 40),
	}
	if seed.closureCard, err = seed.service.OpenEffectProposalItem(ctx, seed.closure.ID, seed.merge); err != nil {
		t.Fatalf("open closure card: %v", err)
	}
	return seed
}

// putTaskProposalCard admits a task proposal and opens its card, as proposal
// admission does: the card's id is its instance's.
func putTaskProposalCard(
	ctx context.Context, tx *store.WriteTx, projectID domain.ProjectID, proposal domain.EffectProposal, name string,
) (domain.AttentionItem, error) {
	batch := domain.ProposalBatchID("batch-task-" + name)
	instance, _, err := tx.AllocateProposalInstance(ctx,
		domain.ProposalAdmissionKey{Source: domain.ProposalSourceClientCommand, SubmissionCommandID: "admit-" + name},
		batch, proposal, proposalSeedAt)
	if err != nil {
		return domain.AttentionItem{}, err
	}
	artifact, err := instance.EvidenceArtifact()
	if err != nil {
		return domain.AttentionItem{}, err
	}
	if err := tx.PutArtifact(ctx, artifact); err != nil {
		return domain.AttentionItem{}, err
	}
	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: domain.ItemID(instance.ID), ProjectID: projectID,
		Subject: domain.Subject{Type: domain.SubjectProposalBatch, ID: domain.SubjectID(batch)},
		Type:    domain.AttentionTaskProposal, Priority: domain.PriorityNormal, Reason: "Start this work.",
		RequestedDecision: []domain.Action{
			domain.ActionStart, domain.ActionStartWithChanges, domain.ActionDecline, domain.ActionSnooze,
		},
		EvidenceSnapshot: []domain.Artifact{artifact}, ItemVersion: 1, CreatedAt: &proposalSeedAt,
		InterruptionClass: domain.InterruptionPlannedGate, Status: domain.StatusOpen,
	}, map[domain.Digest]bool{domain.EffectProposalRecipeDigest: true})
	if err != nil {
		return domain.AttentionItem{}, err
	}
	if err := tx.PutAttentionItem(ctx, item); err != nil {
		return domain.AttentionItem{}, err
	}
	return item, tx.BindProposalItem(ctx, item.ID, instance.ID, proposal.Digest, nil)
}

// decide submits action on card from a paired device, as the phone does.
func (s proposalSeed) decide(t *testing.T, st *store.Store, card domain.AttentionItem, action domain.Action) {
	t.Helper()
	ctx := t.Context()
	device := domain.DeviceID("device-proposals")
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		if _, err := tx.GetDevice(ctx, device); err == nil {
			return nil
		}
		return tx.PutDevice(ctx, domain.Device{ID: device, DisplayName: "Phone", Status: domain.DeviceActive, PairedAt: proposalSeedAt})
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.service.Submit(ctx, signet.ClientCommand{
		CommandID: "decide-" + string(card.ID), DeviceID: device, ExpectedEntityVersion: int64(card.ItemVersion),
		Payload: signet.DecisionPayload{
			ItemID: card.ID, Action: action, ItemVersion: card.ItemVersion,
			PRHeadSHA: card.PRHeadSHA, ArtifactDigests: card.ArtifactDigests,
		},
	}); err != nil {
		t.Fatalf("decide %s: %v", card.ID, err)
	}
}

func listedCard(cards proposalCards, id domain.ItemID) *proposalCard {
	for index := range cards.Cards {
		if cards.Cards[index].ItemID == id {
			return &cards.Cards[index]
		}
	}
	return nil
}

// comparableCards is the read without its wall-clock stamp and revision,
// which differ between two reads of unchanged state.
func comparableCards(t *testing.T, cards proposalCards) []byte {
	t.Helper()
	cards.AsOf, cards.AsOfRevision = time.Time{}, 0
	out, err := json.Marshal(cards)
	if err != nil {
		t.Fatal(err)
	}
	return out
}
