package store_test

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/specify"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// The fixture is a sqlite3 .dump of a database the pre-rename daemon wrote:
// freesided submit through the elaboration invocation, its terminal, the
// spec-approval item, one operator discussion with its reply, the operator's
// approval, and the reserved implementation run with its production attempt. Its bytes are frozen; the
// test proves the renamed code opens, migrates, and reconstructs it without
// rewriting a row.
const (
	legacySpecificationRun = domain.RunID(
		"run-elaboration-694193339c5545bbeabec19b6fc46182625db76693811ba435eb18dcb2601160")
	legacyImplementationRun = domain.RunID("implementation-from-submit")
	legacyInvocationKey     = "inv-elaborate-run-elaboration-694193339c5545bbeabec19b6fc46182625db76693811ba435eb18dcb2601160-1"
	legacyStageID           = "elaborate-run-elaboration-694193339c5545bbeabec19b6fc46182625db76693811ba435eb18dcb2601160"
	legacyClaimKey          = "claim-elaboration-implementation-implementation-from-submit"
	legacyDiscussionKey     = "elaboration-discussion-explain-submitted-spec"
	legacyApprovalItem      = domain.ItemID("spec-approval-implementation-from-submit-1")
)

func openPreRenameDatabase(t *testing.T) *store.Store {
	t.Helper()
	dump, err := os.ReadFile(filepath.Join("testdata", "pre_rename_specification_vocabulary.sql"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "pre-rename.db")
	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(context.Background(), string(dump)); err != nil {
		t.Fatalf("load pre-rename dump: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(context.Background(), path, store.Options{
		AdmissionFloors: map[domain.OperatingMode]domain.CapabilitySnapshot{
			domain.ModeAttendedDev: domain.NewCapabilitySnapshot(domain.CapPostExitExport),
		},
	})
	if err != nil {
		t.Fatalf("open pre-rename database through the renamed daemon: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func TestPreRenameDatabaseReconstructs(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openPreRenameDatabase(t)

	var (
		specificationRun domain.Run
		implementation   domain.Run
		approval         domain.AttentionItem
		marker, claim    store.QueueEntry
		terminal         store.QueueEntry
		discussion       store.QueueEntry
		discussionReply  store.QueueEntry
		dispatched       []store.QueueEntry
		attempt          domain.ProductionAttempt
		policy           domain.ResolvedPolicy
	)
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		if specificationRun, err = tx.GetRun(ctx, legacySpecificationRun); err != nil {
			return err
		}
		if implementation, err = tx.GetRun(ctx, legacyImplementationRun); err != nil {
			return err
		}
		if approval, _, err = tx.GetAttentionItemSnapshot(ctx, legacyApprovalItem); err != nil {
			return err
		}
		if marker, err = tx.GetOutbox(ctx, legacyInvocationKey); err != nil {
			return err
		}
		if claim, err = tx.GetOutbox(ctx, legacyClaimKey); err != nil {
			return err
		}
		if terminal, err = tx.GetInbox(ctx, legacyInvocationKey); err != nil {
			return err
		}
		if discussion, err = tx.GetOutbox(ctx, legacyDiscussionKey); err != nil {
			return err
		}
		if discussionReply, err = tx.GetInbox(ctx, legacyDiscussionKey); err != nil {
			return err
		}
		if dispatched, err = tx.ListDispatchedOutbox(ctx, engine.KindSpecificationInvocationRequested); err != nil {
			return err
		}
		if attempt, err = tx.GetProductionAttemptByRun(ctx, legacyImplementationRun); err != nil {
			return err
		}
		policy, err = tx.GetResolvedPolicy(ctx, legacySpecificationRun)
		return err
	}); err != nil {
		t.Fatalf("reconstruct pre-rename rows: %v", err)
	}

	// The run keeps its identifier family; only the stage name canonicalizes.
	if len(specificationRun.Stages) != 1 || specificationRun.Stages[0].ID != legacyStageID ||
		specificationRun.Stages[0].Name != string(domain.StageNameSpecification) {
		t.Fatalf("specification run stages = %+v, want legacy stage id with canonical name", specificationRun.Stages)
	}
	if got := domain.SpecificationStageID(legacySpecificationRun); got != legacyStageID {
		t.Fatalf("derived stage id = %q, want %q", got, legacyStageID)
	}
	if got := domain.SpecificationInvocationID(legacySpecificationRun, 1); string(got) != legacyInvocationKey {
		t.Fatalf("derived invocation id = %q, want %q", got, legacyInvocationKey)
	}
	if runID, ok := domain.SpecificationRunIDFromInvocationID(domain.InvocationID(legacyInvocationKey)); !ok ||
		runID != legacySpecificationRun {
		t.Fatalf("parsed run from legacy invocation = %q, %t", runID, ok)
	}
	if !domain.SpecificationRunIDMatchesImplementation(legacySpecificationRun, legacyImplementationRun) {
		t.Fatal("legacy specification run does not derive from its implementation run")
	}
	if implementation.SpecDigest == "" || len(implementation.Stages) != 1 {
		t.Fatalf("implementation run = %+v, want the approved single-stage run", implementation)
	}

	// Queue rows read with the current kinds and keys, and every engine
	// decoder that re-encodes for a canonical-bytes check accepts them.
	if marker.Kind != engine.KindSpecificationInvocationRequested || claim.Kind != engine.KindSpecificationImplementationClaim ||
		terminal.Kind != "specification_stage_terminal" {
		t.Fatalf("queue kinds = %q, %q, %q", marker.Kind, claim.Kind, terminal.Kind)
	}
	if bytes.Contains(marker.Payload, []byte(`"elaboration_run_id":`)) ||
		bytes.Contains(marker.Payload, []byte("freeside.elaboration-request")) ||
		!bytes.Contains(marker.Payload, []byte(`"specification_run_id":`)) {
		t.Fatalf("marker payload was not canonicalized: %s", marker.Payload)
	}
	if err := engine.AuthenticateSpecificationInvocationMarker(marker, legacySpecificationRun, legacyStageID); err != nil {
		t.Fatalf("authenticate legacy marker: %v", err)
	}
	if _, err := engine.SpecificationInvocationBackupPayloadDigests(marker); err != nil {
		t.Fatalf("legacy marker backup closure: %v", err)
	}
	if _, err := engine.SpecificationImplementationClaimBackupPayloadDigests(claim); err != nil {
		t.Fatalf("legacy claim backup closure: %v", err)
	}
	if len(dispatched) != 1 || dispatched[0].IdempotencyKey != legacyInvocationKey {
		t.Fatalf("dispatched specification markers = %+v, want the legacy key", dispatched)
	}

	// The discussion marker keeps its legacy key; its intent decodes and
	// binds to that key, and its terminal reads under the current kind.
	if discussion.Kind != engine.KindSpecificationDiscussionRequested || !discussion.Dispatched() ||
		discussionReply.Kind != "specification_discussion_terminal" {
		t.Fatalf("discussion rows = %q (%s), %q", discussion.Kind, discussion.Status, discussionReply.Kind)
	}
	intent, err := domain.DecodeSpecificationDiscussionInvocationIntent(discussion.Payload)
	if err != nil {
		t.Fatalf("decode legacy discussion intent: %v", err)
	}
	if string(intent.InvocationID) != legacyDiscussionKey || intent.SpecificationRunID != legacySpecificationRun {
		t.Fatalf("legacy discussion intent = %+v", intent)
	}
	if commandID, ok := domain.SpecificationDiscussionCommandID(discussion.IdempotencyKey); !ok || commandID != "explain-submitted-spec" {
		t.Fatalf("discussion command = %q, %t", commandID, ok)
	}
	if _, err := engine.SpecificationDiscussionBackupPayloadDigests(discussion); err != nil {
		t.Fatalf("legacy discussion backup closure: %v", err)
	}

	// The attempt's column moved and its cross-checks accept the legacy
	// derivation; the approval item reconstructs with its claims intact.
	if attempt.SpecificationRunID != legacySpecificationRun || attempt.ApprovedSpecDigest != implementation.SpecDigest {
		t.Fatalf("production attempt = %+v", attempt)
	}
	if approval.Status != domain.StatusResolved || len(approval.AgentClaims) != 2 {
		t.Fatalf("approval item = status %q claims %d", approval.Status, len(approval.AgentClaims))
	}

	// The digest-addressed policy keeps its legacy keys; the parser reads them.
	hasLegacyKey := false
	for _, key := range policy.Keys {
		if key.Key == "elaboration.max_iterations" {
			hasLegacyKey = true
		}
	}
	if !hasLegacyKey {
		t.Fatalf("resolved policy keys were rewritten: %+v", policy.Keys)
	}
	parsed, err := specify.ParsePolicy(policy)
	if err != nil {
		t.Fatalf("parse legacy policy: %v", err)
	}
	if parsed.MaxIterations < 1 {
		t.Fatalf("parsed legacy policy = %+v", parsed)
	}

	// Re-putting the reconstructed run converges instead of conflicting.
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutRun(ctx, specificationRun)
	}); err != nil {
		t.Fatalf("re-put legacy run: %v", err)
	}

	// A specification run derived today uses the current family, so a fresh
	// intake would not collide with the legacy rows but does see them.
	if !domain.LegacySpecificationRun(legacySpecificationRun) ||
		domain.LegacySpecificationRun(domain.SpecificationRunIDForImplementation(legacyImplementationRun)) {
		t.Fatal("identifier family detection disagrees with the fixture")
	}
	present, err := engine.HasSpecificationIntakeState(ctx, st,
		domain.SpecificationRunIDForImplementation(legacyImplementationRun), legacyImplementationRun)
	if err != nil || !present {
		t.Fatalf("legacy intake state present = %t, %v", present, err)
	}
	resolved, err := engine.ResolveSpecificationRunID(ctx, st, legacyImplementationRun)
	if err != nil || resolved != legacySpecificationRun {
		t.Fatalf("resolved specification run = %q, %v, want the legacy run", resolved, err)
	}
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		_, err := tx.GetOutbox(ctx, "claim-specification-implementation-implementation-from-submit")
		return err
	}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("current-family claim key = %v, want not found", err)
	}
}
