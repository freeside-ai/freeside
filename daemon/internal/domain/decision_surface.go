package domain

import (
	"encoding/json"
	"fmt"
	"slices"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
)

// DecisionSurface is the daemon-owned identity of the decision surface an
// attention item presents (plan §4). Each immutable authoritative
// recommendation source record (agent_judgment, daemon_policy, project_policy)
// commits to Digest, so a valid source output cannot be replayed onto a foreign
// or newer surface. The persisted record is authority: a decoded or
// caller-supplied value grants none, and the store re-gates the record against
// the item on every reconstruction of that item. Reading a record on its own
// (ReadTx.DecisionSurface) is not such a reconstruction and authenticates only
// the row against itself, so a caller pairing a record with an item must
// obtain that item through a gated read.
//
// Epoch is authenticated only within this record. Matches deliberately
// compares the structural fields and presented set, not the epoch, because
// nothing on the item side carries one while the record stays off the item
// body. A writer able to rewrite the persisted record alone can therefore
// choose an epoch: rolling one back revives a superseded commitment. Closing
// that requires binding the epoch to something the item side also carries,
// which is #917's call when it decides whether this record reaches the item
// body and the wire.
//
// The identity is a sequence, not a content address: Epoch starts at 1 and
// advances by exactly one each time the item's structural fields (Subject,
// RequestedDecision, PRHeadSHA) or its presented artifact set change, and
// Digest hashes only {item_id, epoch, subject, requested_decision,
// pr_head_sha}. Two presented sets on one item are therefore distinct epochs
// and two items are distinct ids, so distinct surfaces never share a digest,
// while the preimage holds no artifact hash, so no source artifact can be asked
// to commit to a set containing its own final digest and a producer can compute
// the identity of a prospective epoch before the artifact that opens it is
// finalized (NextDecisionSurface). Nothing else advances the epoch: timing,
// status, decided_at, expires_when, item_version, entity_version, readiness,
// base freshness, the PR reference, recovery bindings, and eligibility changes
// among source records are all non-surface, so a once-authoritative record is
// never stranded by telemetry or by a sibling source joining or leaving.
type DecisionSurface struct {
	ItemID ItemID `json:"item_id"`
	Epoch  int    `json:"epoch"`
	// Subject, RequestedDecision, and PRHeadSHA are the structural fields
	// copied from the item at this epoch; RequestedDecision is stored in its
	// canonical (sorted, deduplicated) form, so a reorder is not a change.
	Subject           Subject  `json:"subject"`
	RequestedDecision []Action `json:"requested_decision"`
	PRHeadSHA         string   `json:"pr_head_sha"`
	// PresentedArtifactDigests is the canonical set the item's presentation
	// slots reference at this epoch (PresentedArtifactDigests). It is
	// transition state only: a change opens a new epoch, but the set is never
	// part of the digest preimage.
	PresentedArtifactDigests []Digest `json:"presented_artifact_digests"`
	// Digest is the value a source record commits to; §5.14's
	// item_decision_surface_digest names it. It already encodes the item id
	// and epoch. Derived by ComputeDigest and enforced by Validate, never
	// caller-supplied.
	Digest Digest `json:"digest"`
}

// decisionSurfacePreimage is the digest preimage. Field order is part of the
// contract and is pinned by the decision_surface_preimage golden; it must never
// gain an artifact digest (the non-cyclic invariant, plan §4).
type decisionSurfacePreimage struct {
	ItemID            ItemID   `json:"item_id"`
	Epoch             int      `json:"epoch"`
	Subject           Subject  `json:"subject"`
	RequestedDecision []Action `json:"requested_decision"`
	PRHeadSHA         string   `json:"pr_head_sha"`
}

func (s DecisionSurface) preimage() ([]byte, error) {
	body, err := json.Marshal(decisionSurfacePreimage{
		ItemID:            s.ItemID,
		Epoch:             s.Epoch,
		Subject:           s.Subject,
		RequestedDecision: canonicalActions(s.RequestedDecision),
		PRHeadSHA:         s.PRHeadSHA,
	})
	if err != nil {
		return nil, fmt.Errorf("decision surface canonical encoding: %w", err)
	}
	return body, nil
}

// ComputeDigest hashes the canonical preimage: item id, epoch, and the
// structural fields, excluding the presented set and Digest itself.
func (s DecisionSurface) ComputeDigest() (Digest, error) {
	body, err := s.preimage()
	if err != nil {
		return "", err
	}
	return Digest(contentaddr.Sum(body)), nil
}

// Validate reports whether the record is well-formed: a positive epoch, a
// valid subject, canonical action and digest sets, and a Digest equal to its
// recomputation, so a decoded record with a tampered digest, epoch, or
// structural field is refused before any consumer compares against it.
func (s DecisionSurface) Validate() error {
	if s.ItemID == "" {
		return fmt.Errorf("decision surface item_id: %w", ErrEmptyID)
	}
	if s.Epoch < 1 {
		return fmt.Errorf("decision surface %s epoch %d: %w", s.ItemID, s.Epoch, ErrNonPositive)
	}
	if err := s.Subject.Validate(); err != nil {
		return fmt.Errorf("decision surface %s: %w", s.ItemID, err)
	}
	for _, a := range s.RequestedDecision {
		if !a.valid() {
			return fmt.Errorf("decision surface %s action %q: %w", s.ItemID, a, ErrInvalidAction)
		}
	}
	if !slices.Equal(s.RequestedDecision, canonicalActions(s.RequestedDecision)) {
		return fmt.Errorf("decision surface %s requested_decision %v is not canonical: %w",
			s.ItemID, s.RequestedDecision, ErrDecisionSurfaceNotCanonical)
	}
	for _, d := range s.PresentedArtifactDigests {
		if d == "" {
			return fmt.Errorf("decision surface %s presented artifact digest: %w", s.ItemID, ErrEmptyID)
		}
	}
	if !slices.Equal(s.PresentedArtifactDigests, canonicalDigests(s.PresentedArtifactDigests)) {
		return fmt.Errorf("decision surface %s presented_artifact_digests %v is not canonical: %w",
			s.ItemID, s.PresentedArtifactDigests, ErrDecisionSurfaceNotCanonical)
	}
	want, err := s.ComputeDigest()
	if err != nil {
		return err
	}
	if s.Digest != want {
		return fmt.Errorf("decision surface %s digest %q, recomputed %q: %w",
			s.ItemID, s.Digest, want, ErrDecisionSurfaceMismatch)
	}
	return nil
}

// canonicalActions returns the sorted, deduplicated form of an action list.
// It always returns a non-nil slice, so an item that offers no action encodes
// as "[]" in the preimage and the persisted record alike.
func canonicalActions(in []Action) []Action {
	return canonicalSet(in)
}

func canonicalDigests(in []Digest) []Digest {
	return canonicalSet(in)
}

func canonicalSet[T ~string](in []T) []T {
	out := make([]T, 0, len(in))
	out = append(out, in...)
	slices.Sort(out)
	return slices.Compact(out)
}

// PresentedArtifactDigests returns the canonical set of artifacts the item
// presents: exactly the digests its presentation slots reference
// (evidence_snapshot, agent_claims, and the finding_adjudication binding). It
// is the structural presented-slot predicate of plan §4: an artifact that only a
// recommendation provenance slot references (#917) is source-only and
// eligibility-correlated, so it must never be added here, while an artifact
// referenced by both a presentation slot and a provenance slot is presented.
// daemon_policy rule and input digests and project_policy application records
// are not artifacts and never appear. Today the set equals the item's
// artifact_digests binding set; #917 adds the provenance artifact to the
// binding set only, so the two diverge there and nowhere else.
func PresentedArtifactDigests(item AttentionItem) []Digest {
	return bindingDigests(item.EvidenceSnapshot, item.AgentClaims, item.FindingAdjudication)
}

// NewDecisionSurface returns the epoch-1 identity for a newly created item.
// It reads only the item's structural fields and presentation slots, so a
// migration backfill can derive it from a decoded row without the item
// passing the current full Validate; the returned record is validated.
func NewDecisionSurface(item AttentionItem) (DecisionSurface, error) {
	return decisionSurfaceAt(item, 1)
}

// NextDecisionSurface returns the identity item should carry given its stored
// current record: current itself, and false, when the structural fields and
// presented set are unchanged; otherwise the next epoch with a fresh digest,
// and true. A producer that must commit to the surface its own artifact will
// open (the finding adjudicator) calls this on the prospective item before
// finalizing the artifact; the admitting store write derives the same value.
// Epochs are one-way: a field returning to a prior value is a new epoch.
func NextDecisionSurface(current DecisionSurface, item AttentionItem) (DecisionSurface, bool, error) {
	if current.ItemID != item.ID {
		return DecisionSurface{}, false, fmt.Errorf("decision surface for item %s applied to item %s: %w",
			current.ItemID, item.ID, ErrParentKeyMismatch)
	}
	if current.Matches(item) {
		return current, false, nil
	}
	next, err := decisionSurfaceAt(item, current.Epoch+1)
	if err != nil {
		return DecisionSurface{}, false, err
	}
	return next, true, nil
}

// Matches reports whether s is the current identity of item's surface: the
// same item, structural fields, and presented set. The store's reconstruction
// re-gate uses it to fail an item closed when its persisted record disagrees.
func (s DecisionSurface) Matches(item AttentionItem) bool {
	return s.ItemID == item.ID &&
		sameSubject(s.Subject, item.Subject) &&
		slices.Equal(s.RequestedDecision, canonicalActions(item.RequestedDecision)) &&
		s.PRHeadSHA == item.PRHeadSHA &&
		slices.Equal(s.PresentedArtifactDigests, PresentedArtifactDigests(item))
}

// sameSubject compares subjects by their canonical JSON, so a field added to
// Subject is part of the surface without an edit here; an encoding failure
// counts as a difference, which fails closed into a fresh epoch or a refused
// reconstruction rather than a silent match.
//
// That automatic widening has a cost the caller must pay deliberately:
// changing what this derivation reads (a new Subject field, an edit to
// PresentedArtifactDigests, or canonicalizing a surface field in
// CanonicalizeStoredRow) makes every stored record disagree with its item, and
// the re-gate then refuses every existing item on both the read and write
// paths with no repair path. Such a change needs a paired data migration that
// re-derives attention_decision_surfaces in the same release.
func sameSubject(a, b Subject) bool {
	same, err := jsonEqual(a, b)
	return err == nil && same
}

func decisionSurfaceAt(item AttentionItem, epoch int) (DecisionSurface, error) {
	subject := item.Subject
	subject.RunID = clonePtr(subject.RunID)
	s := DecisionSurface{
		ItemID:                   item.ID,
		Epoch:                    epoch,
		Subject:                  subject,
		RequestedDecision:        canonicalActions(item.RequestedDecision),
		PRHeadSHA:                item.PRHeadSHA,
		PresentedArtifactDigests: PresentedArtifactDigests(item),
	}
	digest, err := s.ComputeDigest()
	if err != nil {
		return DecisionSurface{}, err
	}
	s.Digest = digest
	if err := s.Validate(); err != nil {
		return DecisionSurface{}, err
	}
	return s, nil
}

// VerifyDecisionSurfaceCommitment reports whether committed, the digest a
// source record carries, names the item's current identity. It is the
// per-record check #917 applies for each source kind, failing only that
// record's recommendation closed on mismatch: an empty commitment is a record
// that never bound a surface, and any other difference is a foreign or
// superseded surface.
func VerifyDecisionSurfaceCommitment(current DecisionSurface, committed Digest) error {
	if err := current.Validate(); err != nil {
		return err
	}
	if committed == "" {
		return fmt.Errorf("decision surface %s: committed digest: %w", current.ItemID, ErrEmptyID)
	}
	if committed != current.Digest {
		return fmt.Errorf("decision surface %s epoch %d: committed %q, current %q: %w",
			current.ItemID, current.Epoch, committed, current.Digest, ErrDecisionSurfaceMismatch)
	}
	return nil
}
