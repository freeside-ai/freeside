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

// textClaims wraps one otherwise-valid text claim around the given body;
// digest defaults to the computed content address so each rejection case
// isolates its own defect.
func textClaims(text domain.ClaimText, digest domain.Digest) []domain.AgentClaim {
	if digest == "" {
		digest = text.ComputeDigest()
	}
	return []domain.AgentClaim{{
		Label: "summary", Artifact: "art-1", Digest: digest,
		Text: &text, Provenance: provenance(domain.ProducerAgent, nil),
	}}
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
// TestAgentClaimWireExcludesPublishEligible proves the agent side of the
// evidence contract (#173) has no publish-eligibility input: neither the
// claim nor its provenance serializes a publish_eligible field, so a decoded
// claim cannot carry the trust bit trusted policy computes on Artifact.
func TestAgentClaimWireExcludesPublishEligible(t *testing.T) {
	claim := domain.AgentClaim{
		Label: "shot", Artifact: "c1", Digest: "sha256:aaa",
		Provenance: provenance(domain.ProducerAgent, nil),
	}
	if err := claim.Validate(); err != nil {
		t.Fatalf("fixture must be valid: %v", err)
	}
	b, err := json.Marshal(claim)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "publish_eligible") {
		t.Errorf("marshaled agent claim carries publish_eligible: %s", b)
	}
}

func TestNewAttentionItemDerivesBindingSet(t *testing.T) {
	recipe := approvedRecipe
	// Two evidence artifacts sharing a digest and one claim; the union
	// deduplicates and sorts. "sha256:aaa" < "sha256:zzz" gives the order.
	ev1 := domain.Artifact{ID: "e1", Type: "log", Digest: "sha256:zzz", Provenance: provenance(domain.ProducerVerifier, &recipe)}
	ev2 := domain.Artifact{ID: "e2", Type: "log", Digest: "sha256:zzz", Provenance: provenance(domain.ProducerVerifier, &recipe)}
	claim := domain.AgentClaim{Label: "shot", Artifact: "c1", Digest: "sha256:aaa", Provenance: provenance(domain.ProducerAgent, nil)}
	// A text claim joins the binding set through its computed content digest,
	// so an approval over the item binds the rendered summary too.
	text := domain.ClaimText{MediaType: domain.MediaTypeTextPlain, Content: "summary"}
	textClaim := domain.AgentClaim{
		Label: "summary", Artifact: "c2", Digest: text.ComputeDigest(),
		Text: &text, Provenance: provenance(domain.ProducerAgent, nil),
	}
	in := validItemInput(domain.AttentionReadyForFinalReview)
	in.PRHeadSHA = "abc123" // matches provenance() so evidence head-binding passes
	in.EvidenceSnapshot = []domain.Artifact{ev1, ev2}
	in.AgentClaims = []domain.AgentClaim{claim, textClaim}

	item, err := domain.NewAttentionItem(in, approvedRecipes())
	if err != nil {
		t.Fatal(err)
	}
	want := []domain.Digest{"sha256:aaa", "sha256:zzz", text.ComputeDigest()}
	slices.Sort(want)
	if !slices.Equal(item.ArtifactDigests, want) {
		t.Errorf("ArtifactDigests = %v, want %v", item.ArtifactDigests, want)
	}
}

// TestHeadIndependentEvidenceSurvivesRemediation is acceptance criterion 3:
// only explicitly head-independent evidence is preserved across a remediation
// head. Head-independent evidence (no source head) is admitted under any
// pr_head_sha, including one it was not produced against; head-bound evidence
// still must match the item's head.
func TestHeadIndependentEvidenceSurvivesRemediation(t *testing.T) {
	recipe := approvedRecipe
	indepProv := provenance(domain.ProducerVerifier, &recipe)
	indepProv.HeadBinding = domain.HeadIndependent
	indepProv.SourceHeadSHA = ""
	indep := domain.Artifact{ID: "lic", Type: "license_scan", Digest: "sha256:lic", Provenance: indepProv}

	// Same head-independent artifact under two different remediation heads: both
	// preserve it, since it is decoupled from head.
	for _, head := range []string{"head-A", "head-B-remediation"} {
		in := validItemInput(domain.AttentionReadyForFinalReview)
		in.PRHeadSHA = head
		in.EvidenceSnapshot = []domain.Artifact{indep}
		if _, err := domain.NewAttentionItem(in, approvedRecipes()); err != nil {
			t.Fatalf("head-independent evidence rejected under head %q: %v", head, err)
		}
	}

	// Control: a head-bound artifact whose head does not match the (remediated)
	// item head is still invalidated.
	bound := domain.Artifact{ID: "log", Type: "log", Digest: "sha256:log", Provenance: provenance(domain.ProducerVerifier, &recipe)} // SourceHeadSHA "abc123"
	in := validItemInput(domain.AttentionReadyForFinalReview)
	in.PRHeadSHA = "head-B-remediation"
	in.EvidenceSnapshot = []domain.Artifact{bound}
	if _, err := domain.NewAttentionItem(in, approvedRecipes()); !errors.Is(err, domain.ErrEvidenceHeadMismatch) {
		t.Fatalf("head-bound evidence under a mismatched head = %v, want ErrEvidenceHeadMismatch", err)
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

// TestNewAttentionItemActionless pins the #96 relaxation: an empty
// requested_decision is structurally valid for any type (which types must
// offer an action is signet policy, not domain), and an explicitly empty
// input serializes as the required non-null array the wire contract declares.
func TestNewAttentionItemActionless(t *testing.T) {
	for _, typ := range []domain.AttentionType{domain.AttentionBlocked, domain.AttentionSpecApproval} {
		t.Run(string(typ), func(t *testing.T) {
			in := validItemInput(typ)
			in.RequestedDecision = []domain.Action{}
			item, err := domain.NewAttentionItem(in, nil)
			if err != nil {
				t.Fatalf("NewAttentionItem(%s, no actions): %v", typ, err)
			}
			body, err := json.Marshal(item)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(body), `"requested_decision":[]`) {
				t.Errorf("serialized item missing requested_decision:[] — got %s", body)
			}
		})
	}
	// nil means the same thing; it round-trips through Validate too.
	in := validItemInput(domain.AttentionBlocked)
	in.RequestedDecision = nil
	if _, err := domain.NewAttentionItem(in, nil); err != nil {
		t.Fatalf("NewAttentionItem(blocked, nil actions): %v", err)
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

// TestWithDecidedAt covers the decision-instant writer (issue #171): a real
// UTC instant stamps once; zero, non-UTC, and re-stamping are rejected; and
// the constructor never sets the field.
func TestWithDecidedAt(t *testing.T) {
	ts := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	base, err := domain.NewAttentionItem(validItemInput(domain.AttentionSpecApproval), nil)
	if err != nil {
		t.Fatal(err)
	}
	if base.DecidedAt != nil {
		t.Fatalf("constructor set decided_at %v, want nil", base.DecidedAt)
	}

	decided, err := base.WithDecidedAt(ts)
	if err != nil {
		t.Fatalf("WithDecidedAt: %v", err)
	}
	if decided.DecidedAt == nil || !decided.DecidedAt.Equal(ts) {
		t.Fatalf("decided_at = %v, want %v", decided.DecidedAt, ts)
	}
	if err := decided.Validate(); err != nil {
		t.Fatalf("stamped item Validate: %v", err)
	}
	if base.DecidedAt != nil {
		t.Fatal("WithDecidedAt mutated its receiver")
	}

	if _, err := base.WithDecidedAt(time.Time{}); !errors.Is(err, domain.ErrMissingTimestamp) {
		t.Fatalf("zero instant = %v, want ErrMissingTimestamp", err)
	}
	if _, err := base.WithDecidedAt(ts.In(time.FixedZone("PST", -8*3600))); !errors.Is(err, domain.ErrTimestampNotUTC) {
		t.Fatalf("non-UTC instant = %v, want ErrTimestampNotUTC", err)
	}
	if _, err := decided.WithDecidedAt(ts.Add(time.Hour)); !errors.Is(err, domain.ErrImmutableTransition) {
		t.Fatalf("re-stamp = %v, want ErrImmutableTransition", err)
	}
}

// TestValidateDecidedAt covers the reconstruction backstop for the pointer
// field: nil is absent, a present pointer must carry a real UTC instant.
func TestValidateDecidedAt(t *testing.T) {
	base, err := domain.NewAttentionItem(validItemInput(domain.AttentionSpecApproval), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Run("zero", func(t *testing.T) {
		item := base
		item.DecidedAt = &time.Time{}
		if err := item.Validate(); !errors.Is(err, domain.ErrMissingTimestamp) {
			t.Fatalf("Validate() = %v, want ErrMissingTimestamp", err)
		}
	})
	t.Run("non-UTC", func(t *testing.T) {
		item := base
		local := time.Date(2026, 1, 2, 3, 4, 5, 0, time.FixedZone("PST", -8*3600))
		item.DecidedAt = &local
		if err := item.Validate(); !errors.Is(err, domain.ErrTimestampNotUTC) {
			t.Fatalf("Validate() = %v, want ErrTimestampNotUTC", err)
		}
	})
}

// TestAttentionItemCommitPlanNotice pins the plan-notice contract surface
// (plan §5.6; #212 owns emission): nil is absent and renders as an explicit
// null like decided_at, every registered reason survives a marshal/decode
// round trip through Validate, and an unregistered reason fails closed.
func TestAttentionItemCommitPlanNotice(t *testing.T) {
	for _, reason := range domain.AllCommitPlanNoticeReasons {
		t.Run(string(reason), func(t *testing.T) {
			in := validItemInput(domain.AttentionReadyForFinalReview)
			r := reason
			in.CommitPlanNotice = &r
			item, err := domain.NewAttentionItem(in, nil)
			if err != nil {
				t.Fatalf("NewAttentionItem: %v", err)
			}
			body, err := json.Marshal(item)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var decoded domain.AttentionItem
			if err := json.Unmarshal(body, &decoded); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if err := decoded.Validate(); err != nil {
				t.Fatalf("decoded item rejected: %v", err)
			}
			if decoded.CommitPlanNotice == nil || *decoded.CommitPlanNotice != reason {
				t.Fatalf("round trip lost the notice: %v", decoded.CommitPlanNotice)
			}
		})
	}
	t.Run("absent renders explicit null", func(t *testing.T) {
		item, err := domain.NewAttentionItem(validItemInput(domain.AttentionReadyForFinalReview), nil)
		if err != nil {
			t.Fatalf("NewAttentionItem: %v", err)
		}
		body, err := json.Marshal(item)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if !strings.Contains(string(body), `"commit_plan_notice":null`) {
			t.Fatalf("absent notice not rendered as explicit null: %s", body)
		}
	})
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
			name: "invalid commit plan notice",
			mutate: func(in *domain.AttentionItemInput) {
				bad := domain.CommitPlanNoticeReason("plan_rejected")
				in.CommitPlanNotice = &bad
			},
			wantErr: domain.ErrInvalidCommitPlanNotice,
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
			name: "agent claim without provenance",
			mutate: func(in *domain.AttentionItemInput) {
				in.AgentClaims = []domain.AgentClaim{{Label: "shot", Artifact: "art-1", Digest: "sha256:s"}}
			},
			wantErr: domain.ErrInvalidProducerClass,
		},
		{
			name: "agent claim with verifier provenance",
			mutate: func(in *domain.AttentionItemInput) {
				in.AgentClaims = []domain.AgentClaim{{
					Label: "shot", Artifact: "art-1", Digest: "sha256:s",
					Provenance: provenance(domain.ProducerVerifier, nil),
				}}
			},
			wantErr: domain.ErrNonAgentClaim,
		},
		{
			name: "agent claim with daemon provenance",
			mutate: func(in *domain.AttentionItemInput) {
				in.AgentClaims = []domain.AgentClaim{{
					Label: "shot", Artifact: "art-1", Digest: "sha256:s",
					Provenance: provenance(domain.ProducerDaemon, nil),
				}}
			},
			wantErr: domain.ErrNonAgentClaim,
		},
		{
			name: "high-sensitivity text claim",
			mutate: func(in *domain.AttentionItemInput) {
				// Inline content is barred from the high-sensitivity tier
				// (§5.14 no-high-sensitivity-at-rest: clients persist item
				// metadata to disk); only the referenced path may carry it.
				claims := textClaims(domain.ClaimText{MediaType: domain.MediaTypeTextPlain, Content: "secret"}, "")
				claims[0].Provenance.SensitivityClass = domain.SensitivityHigh
				in.AgentClaims = claims
			},
			wantErr: domain.ErrHighSensitivityClaimText,
		},
		{
			name: "text claim with unregistered media type",
			mutate: func(in *domain.AttentionItemInput) {
				in.AgentClaims = textClaims(domain.ClaimText{MediaType: "text/html", Content: "x"}, "")
			},
			wantErr: domain.ErrInvalidClaimMediaType,
		},
		{
			name: "text claim with empty content",
			mutate: func(in *domain.AttentionItemInput) {
				in.AgentClaims = textClaims(domain.ClaimText{MediaType: domain.MediaTypeTextPlain}, "")
			},
			wantErr: domain.ErrEmptyField,
		},
		{
			name: "text claim with invalid UTF-8 content",
			mutate: func(in *domain.AttentionItemInput) {
				// A raw 0xff survives json.Unmarshal (#180), so Validate must
				// catch it rather than trust the decode.
				in.AgentClaims = textClaims(domain.ClaimText{MediaType: domain.MediaTypeTextPlain, Content: "\xff"}, "")
			},
			wantErr: domain.ErrClaimTextNotUTF8,
		},
		{
			name: "text claim over the size cap",
			mutate: func(in *domain.AttentionItemInput) {
				oversize := strings.Repeat("a", domain.MaxClaimTextBytes+1)
				in.AgentClaims = textClaims(domain.ClaimText{MediaType: domain.MediaTypeTextPlain, Content: oversize}, "")
			},
			wantErr: domain.ErrClaimTextTooLarge,
		},
		{
			name: "text claim digest not over its content",
			mutate: func(in *domain.AttentionItemInput) {
				in.AgentClaims = textClaims(domain.ClaimText{MediaType: domain.MediaTypeTextPlain, Content: "shown text"}, "sha256:other")
			},
			wantErr: domain.ErrClaimTextDigestMismatch,
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
				in.AgentClaims = []domain.AgentClaim{{Label: "x", Artifact: "shared", Digest: "sha256:c", Provenance: provenance(domain.ProducerAgent, nil)}}
			},
			wantErr: domain.ErrArtifactIdentityConflict,
		},
		{
			name: "claim id maps to two digests",
			mutate: func(in *domain.AttentionItemInput) {
				in.AgentClaims = []domain.AgentClaim{
					{Label: "a", Artifact: "c1", Digest: "sha256:x", Provenance: provenance(domain.ProducerAgent, nil)},
					{Label: "b", Artifact: "c1", Digest: "sha256:y", Provenance: provenance(domain.ProducerAgent, nil)},
				}
			},
			wantErr: domain.ErrArtifactIdentityConflict,
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
