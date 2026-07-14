package domain_test

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// validItemInput is a minimal, valid AttentionItemInput of the given type,
// used as the base a test mutates one field of.
func validItemInput(typ domain.AttentionType) domain.AttentionItemInput {
	return domain.AttentionItemInput{
		ID:        "item-1",
		ProjectID: "proj-1",
		Subject: domain.Subject{
			Type: domain.SubjectRun,
			ID:   "run-1",
		},
		Type:              typ,
		Priority:          domain.PriorityNormal,
		Reason:            "needs a decision",
		RequestedDecision: []domain.Action{domain.ActionStop},
		ItemVersion:       1,
		InterruptionClass: domain.InterruptionPlannedGate,
		Status:            domain.StatusOpen,
	}
}

// TestNewAttentionItemTypes is acceptance criterion 1: each of the ten Phase 1
// attention types constructs a valid item; an unknown type and an invalid
// subject type are rejected.
func TestNewAttentionItemTypes(t *testing.T) {
	if len(domain.AllAttentionTypes) != 10 {
		t.Fatalf("expected ten Phase 1 attention types, got %d", len(domain.AllAttentionTypes))
	}
	for _, typ := range domain.AllAttentionTypes {
		t.Run(string(typ), func(t *testing.T) {
			item, err := domain.NewAttentionItem(validItemInput(typ), nil)
			if err != nil {
				t.Fatalf("NewAttentionItem(%s): %v", typ, err)
			}
			if item.Type != typ {
				t.Errorf("Type = %q, want %q", item.Type, typ)
			}
		})
	}
}

// TestNewAttentionItemDetachesInput checks that a constructed item does not
// alias caller-owned input: mutating the input's slices or the recipe pointer
// inside an evidence artifact after construction cannot slip an agent artifact
// or invalid action past the gate that already validated the item.
func TestNewAttentionItemDetachesInput(t *testing.T) {
	recipe := approvedRecipe
	verifierArt := domain.Artifact{
		ID: "art-good", Type: "log", Digest: "sha256:g",
		Provenance: provenance(domain.ProducerVerifier, &recipe),
	}
	in := validItemInput(domain.AttentionReadyForFinalReview)
	in.EvidenceSnapshot = []domain.Artifact{verifierArt}
	item, err := domain.NewAttentionItem(in, approvedRecipes())
	if err != nil {
		t.Fatal(err)
	}

	// Swapping the caller's slice element must not change the validated item.
	in.EvidenceSnapshot[0] = domain.Artifact{ID: "art-bad", Type: "img", Digest: "sha256:b", Provenance: provenance(domain.ProducerAgent, nil)}
	if item.EvidenceSnapshot[0].Provenance.ProducerClass == domain.ProducerAgent {
		t.Error("mutating the input evidence slice changed the validated item")
	}
	// Mutating the caller's recipe pointer inside an evidence artifact must not
	// change the item's stored digest.
	recipe = "sha256:tampered"
	if got := item.EvidenceSnapshot[0].Provenance.VerificationRecipeDigest; got == nil || *got != approvedRecipe {
		t.Errorf("mutating the input recipe pointer changed the item's evidence: %v", got)
	}
	// The action slice is likewise detached: mutating the input leaves the
	// item's own element unchanged (and therefore still valid).
	in.RequestedDecision[0] = "bogus_action"
	if item.RequestedDecision[0] != domain.ActionStop {
		t.Errorf("mutating the input action slice changed the item: %q", item.RequestedDecision[0])
	}
	if err := item.Validate(); err != nil {
		t.Errorf("item invalidated after input mutation: %v", err)
	}
}

// TestNewAttentionItemRecomputesEvidenceEligibility checks that publish_eligible
// on an evidence artifact is computed by trusted policy, never trusted from the
// supplied artifact: a verifier artifact under an approved recipe supplied with
// PublishEligible=false is corrected to true.
func TestNewAttentionItemRecomputesEvidenceEligibility(t *testing.T) {
	recipe := approvedRecipe
	understated := domain.Artifact{
		ID: "art-1", Type: "log", Digest: "sha256:g",
		Provenance:      provenance(domain.ProducerVerifier, &recipe),
		PublishEligible: false, // caller lie: it is actually eligible
	}
	in := validItemInput(domain.AttentionReadyForFinalReview)
	in.EvidenceSnapshot = []domain.Artifact{understated}
	item, err := domain.NewAttentionItem(in, approvedRecipes())
	if err != nil {
		t.Fatal(err)
	}
	if !item.EvidenceSnapshot[0].PublishEligible {
		t.Error("evidence publish_eligible was trusted from input, not recomputed by policy")
	}
}

// TestNewAttentionItemDerivesBindingSet is acceptance 1: the binding set is
// derived from the rendered inputs, so it is exactly the canonical (sorted,
// deduplicated) union of the evidence and claim digests, and a caller cannot
// supply a divergent one (the field is not on the input).
func TestNewAttentionItemDerivesBindingSet(t *testing.T) {
	recipe := approvedRecipe
	// Two evidence artifacts sharing a digest and one claim; the union
	// deduplicates and sorts. "sha256:aaa" < "sha256:zzz" gives the order.
	ev1 := domain.Artifact{ID: "e1", Type: "log", Digest: "sha256:zzz", Provenance: provenance(domain.ProducerVerifier, &recipe)}
	ev2 := domain.Artifact{ID: "e2", Type: "log", Digest: "sha256:zzz", Provenance: provenance(domain.ProducerVerifier, &recipe)}
	claim := domain.AgentClaim{Label: "shot", Artifact: "c1", Digest: "sha256:aaa"}
	in := validItemInput(domain.AttentionReadyForFinalReview)
	in.PRHeadSHA = "abc123" // matches provenance() so evidence head-binding passes
	in.EvidenceSnapshot = []domain.Artifact{ev1, ev2}
	in.AgentClaims = []domain.AgentClaim{claim}

	item, err := domain.NewAttentionItem(in, approvedRecipes())
	if err != nil {
		t.Fatal(err)
	}
	want := []domain.Digest{"sha256:aaa", "sha256:zzz"}
	if !slices.Equal(item.ArtifactDigests, want) {
		t.Errorf("ArtifactDigests = %v, want %v", item.ArtifactDigests, want)
	}
}

// TestNewAttentionItemEmptyBindingIsArray checks an item that renders no
// artifacts serializes artifact_digests as "[]" rather than null, matching the
// required, non-null array the wire contract declares.
func TestNewAttentionItemEmptyBindingIsArray(t *testing.T) {
	item, err := domain.NewAttentionItem(validItemInput(domain.AttentionSystemHealth), nil)
	if err != nil {
		t.Fatal(err)
	}
	if item.ArtifactDigests == nil || len(item.ArtifactDigests) != 0 {
		t.Fatalf("ArtifactDigests = %v, want non-nil empty slice", item.ArtifactDigests)
	}
	body, err := json.Marshal(item)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"artifact_digests":[]`) {
		t.Errorf("serialized item missing artifact_digests:[] — got %s", body)
	}
}

// TestValidateRejectsBindingMismatch is the reconstruction backstop for
// acceptance 1: an item decoded from the store whose artifact_digests does not
// equal its rendered evidence/claim digests must not validate — this is the
// "display A, bind B" defect the derived field closes. NewAttentionItem cannot
// produce such a value, so the mismatch is injected past it.
func TestValidateRejectsBindingMismatch(t *testing.T) {
	base := func() domain.AttentionItem {
		recipe := approvedRecipe
		in := validItemInput(domain.AttentionReadyForFinalReview)
		in.PRHeadSHA = "abc123"
		in.EvidenceSnapshot = []domain.Artifact{{ID: "e1", Type: "log", Digest: "sha256:log", Provenance: provenance(domain.ProducerVerifier, &recipe)}}
		item, err := domain.NewAttentionItem(in, approvedRecipes())
		if err != nil {
			t.Fatal(err)
		}
		return item
	}
	for _, tc := range []struct {
		name    string
		digests []domain.Digest
	}{
		{"extra unrendered digest", []domain.Digest{"sha256:log", "sha256:phantom"}},
		{"omitted rendered digest", nil},
		{"substituted digest", []domain.Digest{"sha256:other"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			item := base()
			item.ArtifactDigests = tc.digests
			if err := item.Validate(); !errors.Is(err, domain.ErrBindingMismatch) {
				t.Fatalf("Validate() = %v, want ErrBindingMismatch", err)
			}
		})
	}
}

// TestValidateRejectsCorruptTiming is the reconstruction backstop: an item
// decoded from the store with an impossible Timing shape must not validate.
func TestValidateRejectsCorruptTiming(t *testing.T) {
	item, err := domain.NewAttentionItem(validItemInput(domain.AttentionSpecApproval), nil)
	if err != nil {
		t.Fatal(err)
	}
	item.Timing = domain.TimingSummary{DeliveryCount: -1}
	if err := item.Validate(); !errors.Is(err, domain.ErrInconsistentTiming) {
		t.Fatalf("Validate() = %v, want ErrInconsistentTiming", err)
	}
}

func TestNewAttentionItemRejects(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*domain.AttentionItemInput)
		wantErr error
	}{
		{
			name:    "unknown type",
			mutate:  func(in *domain.AttentionItemInput) { in.Type = "not_a_type" },
			wantErr: domain.ErrUnknownAttentionType,
		},
		{
			name:    "invalid subject type",
			mutate:  func(in *domain.AttentionItemInput) { in.Subject.Type = "planet" },
			wantErr: domain.ErrInvalidSubjectType,
		},
		{
			name:    "missing project_id",
			mutate:  func(in *domain.AttentionItemInput) { in.ProjectID = "" },
			wantErr: domain.ErrEmptyID,
		},
		{
			name:    "agent claim without label",
			mutate:  func(in *domain.AttentionItemInput) { in.AgentClaims = []domain.AgentClaim{{Artifact: "art-1"}} },
			wantErr: domain.ErrEmptyField,
		},
		{
			name:    "agent claim without artifact",
			mutate:  func(in *domain.AttentionItemInput) { in.AgentClaims = []domain.AgentClaim{{Label: "note"}} },
			wantErr: domain.ErrEmptyID,
		},
		{
			name: "agent claim without digest",
			mutate: func(in *domain.AttentionItemInput) {
				in.AgentClaims = []domain.AgentClaim{{Label: "shot", Artifact: "art-1"}}
			},
			wantErr: domain.ErrEmptyField,
		},
		{
			name:    "non-positive item_version",
			mutate:  func(in *domain.AttentionItemInput) { in.ItemVersion = 0 },
			wantErr: domain.ErrNonPositive,
		},
		{
			name: "run_id on a system subject",
			mutate: func(in *domain.AttentionItemInput) {
				runID := domain.RunID("run-1")
				in.Subject = domain.Subject{Type: domain.SubjectSystem, ID: "sys", RunID: &runID}
			},
			wantErr: domain.ErrSubjectRunIDMismatch,
		},
		{
			name: "duplicate evidence artifact",
			mutate: func(in *domain.AttentionItemInput) {
				recipe := approvedRecipe
				a := domain.Artifact{ID: "dup", Type: "log", Digest: "sha256:d", Provenance: provenance(domain.ProducerVerifier, &recipe)}
				in.EvidenceSnapshot = []domain.Artifact{a, a}
			},
			wantErr: domain.ErrDuplicate,
		},
		{
			name: "empty run_id pointer",
			mutate: func(in *domain.AttentionItemInput) {
				empty := domain.RunID("")
				in.Subject.RunID = &empty
			},
			wantErr: domain.ErrEmptyID,
		},
		{
			name: "empty conversation_id pointer",
			mutate: func(in *domain.AttentionItemInput) {
				empty := domain.ConversationID("")
				in.ConversationID = &empty
			},
			wantErr: domain.ErrEmptyID,
		},
		{
			name: "zero expires_when",
			mutate: func(in *domain.AttentionItemInput) {
				var zero time.Time
				in.ExpiresWhen = &zero
			},
			wantErr: domain.ErrMissingTimestamp,
		},
		{
			name: "evidence head mismatch",
			mutate: func(in *domain.AttentionItemInput) {
				recipe := approvedRecipe
				in.PRHeadSHA = "head-A"
				in.EvidenceSnapshot = []domain.Artifact{{ID: "e1", Type: "log", Digest: "sha256:e", Provenance: provenance(domain.ProducerVerifier, &recipe)}}
			},
			wantErr: domain.ErrEvidenceHeadMismatch,
		},
		{
			name: "claim reuses evidence id",
			mutate: func(in *domain.AttentionItemInput) {
				recipe := approvedRecipe
				in.PRHeadSHA = "abc123" // matches provenance() SourceHeadSHA so head-binding passes
				in.EvidenceSnapshot = []domain.Artifact{{ID: "shared", Type: "log", Digest: "sha256:e", Provenance: provenance(domain.ProducerVerifier, &recipe)}}
				in.AgentClaims = []domain.AgentClaim{{Label: "x", Artifact: "shared", Digest: "sha256:c"}}
			},
			wantErr: domain.ErrArtifactIdentityConflict,
		},
		{
			name: "claim id maps to two digests",
			mutate: func(in *domain.AttentionItemInput) {
				in.AgentClaims = []domain.AgentClaim{
					{Label: "a", Artifact: "c1", Digest: "sha256:x"},
					{Label: "b", Artifact: "c1", Digest: "sha256:y"},
				}
			},
			wantErr: domain.ErrArtifactIdentityConflict,
		},
		{
			name:    "no requested decision",
			mutate:  func(in *domain.AttentionItemInput) { in.RequestedDecision = nil },
			wantErr: domain.ErrNoActions,
		},
		{
			name:    "invalid priority",
			mutate:  func(in *domain.AttentionItemInput) { in.Priority = "screaming" },
			wantErr: domain.ErrInvalidPriority,
		},
		{
			name:    "invalid status",
			mutate:  func(in *domain.AttentionItemInput) { in.Status = "vanished" },
			wantErr: domain.ErrInvalidItemStatus,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := validItemInput(domain.AttentionSpecApproval)
			tt.mutate(&in)
			_, err := domain.NewAttentionItem(in, nil)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}
