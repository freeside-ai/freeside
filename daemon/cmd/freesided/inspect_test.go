package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/golden"
	"github.com/freeside-ai/freeside/daemon/internal/observe"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/strictjson"
)

// TestInspectIndexGolden pins the index's bytes. A change to the index's
// fields changes inspectIndexVersion; a read added to the list is content and
// changes only the bytes.
func TestInspectIndexGolden(t *testing.T) {
	latest := domain.RunID("run-freeside-656")
	got, err := json.MarshalIndent(inspectIndex{
		Version: inspectIndexVersion, AsOfRevision: 22, Reads: inspectReads(),
		Tasks: []inspectTask{
			{
				TaskID: "task-with-runs", ProjectID: "freeside",
				CreatedAt: time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC), LatestRunID: &latest,
			},
			{TaskID: "task-without-runs", ProjectID: "freeside", CreatedAt: time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)},
		},
	}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	golden.Assert(t, "inspect_index", append(got, '\n'))
}

// TestInspectIndexNamesItsReads holds the index to the versions the reads
// print, so the list cannot name a shape the command no longer writes.
func TestInspectIndexNamesItsReads(t *testing.T) {
	versions := map[string]string{}
	for _, read := range inspectReads() {
		if read.Name == "" || read.Command == "" || read.IDs == nil {
			t.Errorf("read %+v is incomplete", read)
		}
		if read.Version != nil {
			versions[read.Name] = *read.Version
		}
	}
	if versions["inspect"] != inspectIndexVersion || versions["inspect task"] != taskRecordVersion ||
		versions["inspect health"] != systemHealthVersion || versions["inspect proposals"] != proposalCardsVersion {
		t.Fatalf("versions = %v", versions)
	}
}

func startSeededDaemon(t *testing.T) (*daemon, string) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "freeside.db")
	h := startDaemon(t, config{
		DBPath: dbPath, ListenAddr: "127.0.0.1:0", SeedFixture: "representative", Environment: environmentEphemeral,
	})
	return h, dbPath
}

func inspect[T any](t *testing.T, ctx context.Context, args ...string) T {
	t.Helper()
	var out bytes.Buffer
	if err := runInspectCommand(ctx, args, &out, io.Discard); err != nil {
		t.Fatalf("inspect %v: %v", args, err)
	}
	if bytes.Count(out.Bytes(), []byte("\n")) != 1 {
		t.Fatalf("inspect %v printed more than one line", args)
	}
	var value T
	if err := strictjson.Decode(out.Bytes(), &value, strictjson.RejectInvalidUTF8, strictjson.NoLimit); err != nil {
		t.Fatalf("inspect %v: %v", args, err)
	}
	return value
}

// TestInspectReadsThroughRunningDaemon fails any attempt to open the database
// while a daemon holds it: every read must come over the control socket.
func TestInspectReadsThroughRunningDaemon(t *testing.T) {
	t.Parallel()
	h, dbPath := startSeededDaemon(t)
	t.Cleanup(func() {
		if err := h.Close(); err != nil {
			t.Error(err)
		}
	})
	ctx := context.WithValue(t.Context(), directStoreOpenerKey{}, directStoreOpener(
		func(context.Context, string, store.Options, storeOpenMode) (*store.Store, error) {
			return nil, errors.New("inspect opened the database a daemon holds")
		}))
	index := inspect[inspectIndex](t, ctx, "-db", dbPath)
	if index.Version != inspectIndexVersion || index.AsOfRevision == 0 || len(index.Tasks) != 6 ||
		!slices.EqualFunc(index.Reads, inspectReads(), equalInspectReads) {
		t.Fatalf("index = %+v", index)
	}
	for _, task := range index.Tasks {
		record := inspect[taskRecord](t, ctx, "task", "-db", dbPath, "-task", string(task.TaskID))
		if record.Version != taskRecordVersion || record.TaskID != task.TaskID || record.AsOfRevision < index.AsOfRevision {
			t.Errorf("task %s: version %q, id %q, revision %d", task.TaskID, record.Version, record.TaskID, record.AsOfRevision)
		}
		if record.Timeline == nil || record.Items == nil || len(record.Runs) == 0 ||
			!equalPointers(task.LatestRunID, &record.Runs[len(record.Runs)-1].RunID) {
			t.Errorf("task %s: timeline error %+v, items error %+v, %d runs, latest run %v",
				task.TaskID, record.TimelineError, record.ItemsError, len(record.Runs), task.LatestRunID)
		}
	}
	health := inspect[systemHealth](t, ctx, "health", "-db", dbPath)
	if health.Version != systemHealthVersion || health.AsOfRevision < index.AsOfRevision || len(health.Items) == 0 {
		t.Errorf("health = %+v", health)
	}
	proposals := inspect[proposalCards](t, ctx, "proposals", "-db", dbPath)
	if proposals.Version != proposalCardsVersion || proposals.AsOfRevision < index.AsOfRevision ||
		proposals.Cards == nil || proposals.RefusedRows == nil {
		t.Errorf("proposals = %+v", proposals)
	}
	err := runInspectCommand(ctx, []string{"task", "-db", dbPath, "-task", "task-unknown"}, io.Discard, io.Discard)
	if !errors.Is(err, store.ErrNotFound) || errors.Is(err, observe.ErrUsage) {
		t.Fatalf("unknown task over the socket: %v", err)
	}
}

func equalInspectReads(a, b inspectRead) bool {
	return a.Name == b.Name && a.Command == b.Command && slices.Equal(a.IDs, b.IDs) && equalPointers(a.Version, b.Version)
}

// TestInspectReadsStoreWithNoDaemon reads each record through a daemon,
// stops it, and expects the command to read the same record from the file.
func TestInspectReadsStoreWithNoDaemon(t *testing.T) {
	t.Parallel()
	h, dbPath := startSeededDaemon(t)
	ctx := t.Context()
	index := inspect[inspectIndex](t, ctx, "-db", dbPath)
	served := map[domain.TaskID]taskRecord{}
	for _, task := range index.Tasks {
		served[task.TaskID] = inspect[taskRecord](t, ctx, "task", "-db", dbPath, "-task", string(task.TaskID))
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	direct := inspect[inspectIndex](t, ctx, "-db", dbPath)
	if !slices.EqualFunc(direct.Tasks, index.Tasks, func(a, b inspectTask) bool {
		return a.TaskID == b.TaskID && a.CreatedAt.Equal(b.CreatedAt) && equalPointers(a.LatestRunID, b.LatestRunID)
	}) {
		t.Fatalf("direct index tasks = %+v, served = %+v", direct.Tasks, index.Tasks)
	}
	for id, want := range served {
		got := inspect[taskRecord](t, ctx, "task", "-db", dbPath, "-task", string(id))
		if !bytes.Equal(comparableRecord(t, got), comparableRecord(t, want)) {
			t.Errorf("task %s: direct record differs from the served one\ndirect: %s\nserved: %s",
				id, comparableRecord(t, got), comparableRecord(t, want))
		}
	}
	err := runInspectCommand(ctx, []string{"task", "-db", dbPath, "-task", "task-unknown"}, io.Discard, io.Discard)
	if !errors.Is(err, store.ErrNotFound) || errors.Is(err, observe.ErrUsage) {
		t.Fatalf("unknown task with no daemon: %v", err)
	}
	// The reads above moved nothing.
	if after := inspect[inspectIndex](t, ctx, "-db", dbPath); after.AsOfRevision != direct.AsOfRevision {
		t.Fatalf("revision moved from %d to %d across reads", direct.AsOfRevision, after.AsOfRevision)
	}
}

// TestInspectReadsStoppedRootWithRecipeEvidence is the exit-run case: a
// stopped root holding items whose evidence names the daemon's recipe, this
// task's and another task's. With no -approved-recipe the direct read still
// lists the task's items, shows the refused one as its id and error, leaves
// the other task's refused item out, and says on stderr what the read
// needs. With the daemon's digest the record equals the served one.
func TestInspectReadsStoppedRootWithRecipeEvidence(t *testing.T) {
	recipe := domain.Digest("sha256:" + strings.Repeat("e", 64))
	dbPath := filepath.Join(t.TempDir(), "freeside.db")
	h := startDaemon(t, config{
		DBPath: dbPath, ListenAddr: "127.0.0.1:0", SeedFixture: "representative",
		Environment: environmentEphemeral, ApprovedRecipes: map[domain.Digest]bool{recipe: true},
	})
	ctx := t.Context()
	const own, foreign = domain.ItemID("execution-failure-evidence-own"), domain.ItemID("execution-failure-evidence-foreign")
	putRecipeEvidenceItem(t, h.store, recipe, fixtureActiveRunID, own)
	putRecipeEvidenceItem(t, h.store, recipe, "run-evidence-foreign", foreign)
	id := taskOfRun(t, h.store, fixtureActiveRunID)
	args := []string{"task", "-db", dbPath, "-task", string(id)}
	served := inspect[taskRecord](t, ctx, args...)
	if item := recordItem(served, own); item == nil || item.Error != nil || recordItem(served, foreign) != nil {
		t.Fatalf("served own item = %+v, foreign present = %t", item, recordItem(served, foreign) != nil)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	if err := runInspectCommand(ctx, args, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	var refused taskRecord
	if err := strictjson.Decode(stdout.Bytes(), &refused, strictjson.RejectInvalidUTF8, strictjson.NoLimit); err != nil {
		t.Fatal(err)
	}
	item := recordItem(refused, own)
	if item == nil || item.Error == nil || item.Error.Kind != taskRecordErrorUnapprovedRecipe ||
		!strings.Contains(item.Error.Message, string(recipe)) || item.Type != nil || item.Commands != nil {
		t.Fatalf("refused own item = %+v", item)
	}
	if recordItem(refused, foreign) != nil || refused.ItemsError != nil || len(refused.Items) != len(served.Items) {
		t.Fatalf("items = %d (served %d), foreign present = %t, items_error = %+v",
			len(refused.Items), len(served.Items), recordItem(refused, foreign) != nil, refused.ItemsError)
	}
	// The other task's refused row is counted, not shown.
	if !equalPointers(refused.Counts.UnattributedRefusedItems, new(1)) ||
		!equalPointers(served.Counts.UnattributedRefusedItems, new(0)) {
		t.Fatalf("unattributed refused items: direct %v, served %v",
			refused.Counts.UnattributedRefusedItems, served.Counts.UnattributedRefusedItems)
	}
	// The timeline is the client's, unchanged, and still fails whole.
	if refused.TimelineError == nil || refused.TimelineError.Kind != taskRecordErrorUnapprovedRecipe {
		t.Fatalf("timeline error = %+v", refused.TimelineError)
	}
	if note := stderr.String(); !strings.Contains(note, "-approved-recipe") || !strings.Contains(note, "approved 0 ") {
		t.Fatalf("stderr = %q, want the approved-recipe note", note)
	}

	stdout.Reset()
	stderr.Reset()
	if err := runInspectCommand(ctx, append(args, "-approved-recipe", string(recipe)), &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	var approved taskRecord
	if err := strictjson.Decode(stdout.Bytes(), &approved, strictjson.RejectInvalidUTF8, strictjson.NoLimit); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(comparableRecord(t, approved), comparableRecord(t, served)) || stderr.Len() != 0 {
		t.Fatalf("approved direct record differs from the served one, or stderr = %q\ndirect: %s\nserved: %s",
			stderr.String(), comparableRecord(t, approved), comparableRecord(t, served))
	}
}

// putRecipeEvidenceItem writes an execution-failure item on runID, creating
// the run when it is new, whose evidence names recipe. The store must
// approve recipe.
func putRecipeEvidenceItem(t *testing.T, st *store.Store, recipe domain.Digest, runID domain.RunID, id domain.ItemID) {
	t.Helper()
	approved := map[domain.Digest]bool{recipe: true}
	artifact := recipeEvidenceArtifact(t, recipe, id)
	if err := st.Write(t.Context(), func(tx *store.WriteTx) error {
		run, err := tx.GetRun(t.Context(), runID)
		if errors.Is(err, store.ErrNotFound) {
			run = domain.Run{ID: runID, ProjectID: "proj-evidence", SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy"}
			if err := tx.PutRun(t.Context(), run); err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
		item, err := domain.NewAttentionItem(domain.AttentionItemInput{
			ID: id, ProjectID: run.ProjectID,
			Subject: domain.Subject{Type: domain.SubjectRun, ID: domain.SubjectID(runID), RunID: &runID},
			Type:    domain.AttentionExecutionFailure, Priority: domain.PriorityHigh, Reason: "Recovery needed.",
			RequestedDecision: []domain.Action{domain.ActionStop}, ItemVersion: 1, Status: domain.StatusOpen,
			InterruptionClass: domain.InterruptionExceptional, EvidenceSnapshot: []domain.Artifact{artifact},
		}, approved)
		if err != nil {
			return err
		}
		return tx.PutAttentionItem(t.Context(), item)
	}); err != nil {
		t.Fatal(err)
	}
}

// recipeEvidenceArtifact is a verifier artifact for item id whose
// provenance names recipe.
func recipeEvidenceArtifact(t *testing.T, recipe domain.Digest, id domain.ItemID) domain.Artifact {
	t.Helper()
	at := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	artifact, err := domain.NewArtifact(domain.ArtifactInput{
		ID: domain.ArtifactID(id) + "-evidence", Type: domain.ArtifactKindVerifyLog, Digest: "sha256:" + domain.Digest(id),
		Provenance: domain.Provenance{
			ProducerClass: domain.ProducerVerifier, ProducerInvocationID: domain.InvocationID("invocation-" + id),
			HeadBinding: domain.HeadBound, SourceHeadSHA: "cafebabe",
			VerificationRecipeDigest: &recipe, SensitivityClass: domain.SensitivityNormal,
		},
		Metadata: domain.EvidenceMetadata{
			MediaType: domain.EvidenceMediaApplicationJSON, SizeBytes: 1, CreatedAt: at,
			Source: domain.EvidenceSourceRun, Availability: domain.EvidenceAvailable,
		},
	}, map[domain.Digest]bool{recipe: true})
	if err != nil {
		t.Fatal(err)
	}
	return artifact
}

// comparableRecord is the record without the read's wall-clock stamps and
// revision, which differ between two reads of unchanged task state.
func comparableRecord(t *testing.T, record taskRecord) []byte {
	t.Helper()
	record.AsOf, record.AsOfRevision = time.Time{}, 0
	if record.Timeline != nil {
		timeline := *record.Timeline
		timeline.AsOf, timeline.AsOfRevision = time.Time{}, 0
		record.Timeline = &timeline
	}
	out, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestInspectUsageErrors(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "freeside.db")
	for name, args := range map[string][]string{
		"no database":            {},
		"task with no id":        {"task", "-db", dbPath},
		"task flag on index":     {"-db", dbPath, "-task", "task-1"},
		"unknown verb":           {"run", "-db", dbPath},
		"positional":             {"-db", dbPath, "task"},
		"unknown flag":           {"-db", dbPath, "-follow"},
		"health with no db":      {"health"},
		"health positional":      {"health", "-db", dbPath, "extra"},
		"task flag on health":    {"health", "-db", dbPath, "-task", "task-1"},
		"proposals with no db":   {"proposals"},
		"proposals positional":   {"proposals", "-db", dbPath, "extra"},
		"task flag on proposals": {"proposals", "-db", dbPath, "-task", "task-1"},
	} {
		t.Run(name, func(t *testing.T) {
			var out bytes.Buffer
			err := runInspectCommand(t.Context(), args, &out, io.Discard)
			if !errors.Is(err, observe.ErrUsage) || out.Len() != 0 {
				t.Fatalf("err = %v, stdout = %q", err, out.String())
			}
		})
	}
	// A read never creates the database it is pointed at.
	err := runInspectCommand(t.Context(), []string{"-db", dbPath}, io.Discard, io.Discard)
	if err == nil || errors.Is(err, observe.ErrUsage) {
		t.Fatalf("missing database: %v", err)
	}
	if _, statErr := os.Stat(dbPath); !errors.Is(statErr, fs.ErrNotExist) {
		t.Fatalf("inspect created the database: %v", statErr)
	}
}
