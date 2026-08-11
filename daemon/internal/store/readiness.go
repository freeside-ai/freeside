package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

func readinessBodyDigest(body string) string { return contentaddr.Sum([]byte(body)) }
func readinessAuthority(body string) string  { return readinessBodyDigest(body) + body }

func (tx *ReadTx) currentVerificationGeneration() uint64 {
	if tx.verificationFloorRegistryGeneration == 0 {
		return domain.CurrentVerificationFloorRegistryGeneration
	}
	return tx.verificationFloorRegistryGeneration
}

func (tx *ReadTx) gateRequirementResolution(r domain.RequirementResolution) error {
	if r.FloorRegistryGeneration != tx.currentVerificationGeneration() {
		return domain.ErrVerificationFloorRegressed
	}
	// Re-resolve the policy-bearing fields from the daemon-owned requirement
	// registry: a caller- or row-supplied applicability, kind, or class that
	// the registered definition would not have produced fails closed, as does
	// a resolution naming a set the daemon never registered.
	set, ok := tx.requirementSets[r.RequirementSetDigest]
	if !ok {
		return domain.ErrRequirementSetUntrusted
	}
	definition, ok := set[r.RequirementKey]
	if !ok || definition.Class != r.CheckClass || definition.Kind != r.Kind ||
		definition.Applicable != r.Applicable || definition.BaseDependent != r.BaseDependent {
		return domain.ErrRequirementDefinitionMismatch
	}
	return nil
}

// gateCheckProofRecipe re-runs the current recipe authority for a proof's
// check class at both persistence and reconstruction, dispatching by class so
// a new class must make an explicit authority decision. It fails closed for
// every class: no class returns a bare nil that would trust a caller-supplied
// recipe on its own.
//
// Independent review's configuration approval is run-trust-context-scoped
// (profile plus adoption), so it cannot be a daemon-owned Open-time registry
// like approvedRecipes. The store still refuses to trust the caller's recipe:
// a caller that has re-derived the run-scoped authority in the engine's
// evaluation boundary asserts it through AuthorizeIndependentReviewRecipe, and
// the gate rejects any independent-review recipe it has not asserted. This
// keeps the returned-object trust boundary (never trust a caller-supplied
// trust bit) intact for independent review, not only clean verification.
func (tx *ReadTx) gateCheckProofRecipe(resolution domain.RequirementResolution, proof domain.CheckProof) error {
	switch resolution.CheckClass {
	case domain.CheckClassCleanVerification:
		if !tx.approvedRecipes[proof.RecipeDigest] {
			return domain.ErrUnapprovedRecipe
		}
		return nil
	case domain.CheckClassIndependentReview:
		if !tx.authorizedReviewRecipes[proof.RecipeDigest] {
			return domain.ErrCheckProofAuthorityUnregistered
		}
		return nil
	case domain.CheckClassRepoChangePolicy:
		return domain.ErrCheckProofAuthorityUnregistered
	}
	return domain.ErrCheckProofAuthorityUnregistered
}

func (tx *ReadTx) gateDegradedWaiverAuthority(waiver domain.ValidatedDegradedWaiver) error {
	if !tx.waiverGrantApprovals[waiver.Authority][waiver.GrantDigest] {
		return domain.ErrWaiverInconsistent
	}
	return nil
}

const putRequirementResolutionSQL = `
INSERT INTO requirement_resolutions
    (digest, requirement_key, check_class, requirement_set_digest,
     floor_registry_generation, resolved_policy_digest, body_digest, body)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (digest) DO NOTHING`

// RecordRequirementResolution persists one immutable branch-shared resolution.
func (tx *InternalTx) RecordRequirementResolution(ctx context.Context, r domain.RequirementResolution) error {
	if err := r.Validate(); err != nil {
		return fmt.Errorf("put requirement resolution %q: %w", r.RequirementKey, err)
	}
	if err := tx.gateRequirementResolution(r); err != nil {
		return fmt.Errorf("put requirement resolution %q current gate: %w", r.RequirementKey, err)
	}
	body, err := encode(r)
	if err != nil {
		return fmt.Errorf("put requirement resolution %q: %w", r.RequirementKey, err)
	}
	if err := tx.putImmutable(ctx, putRequirementResolutionSQL,
		[]any{
			r.Digest, r.RequirementKey, r.CheckClass, r.RequirementSetDigest,
			r.FloorRegistryGeneration, r.ResolvedPolicyDigest, readinessBodyDigest(body), body,
		},
		`SELECT body_digest || body FROM requirement_resolutions WHERE digest = ?`,
		[]any{r.Digest}, readinessAuthority(body)); err != nil {
		return fmt.Errorf("put requirement resolution %q: %w", r.RequirementKey, err)
	}
	return nil
}

// GetRequirementResolution reconstructs a resolution and re-runs the current
// floor/registry gate rather than trusting the recorded generation.
func (tx *ReadTx) GetRequirementResolution(ctx context.Context, digest domain.Digest) (domain.RequirementResolution, error) {
	var key, class, setDigest, policyDigest, bodyDigest string
	var generation uint64
	var body []byte
	err := tx.tx.QueryRowContext(ctx, `SELECT requirement_key, check_class,
        requirement_set_digest, floor_registry_generation, resolved_policy_digest,
        body_digest, body FROM requirement_resolutions WHERE digest = ?`, digest).
		Scan(&key, &class, &setDigest, &generation, &policyDigest, &bodyDigest, &body)
	if err != nil {
		return domain.RequirementResolution{}, fmt.Errorf("get requirement resolution %q: %w", digest, notFoundOr(err))
	}
	if bodyDigest != readinessBodyDigest(string(body)) {
		return domain.RequirementResolution{}, fmt.Errorf("get requirement resolution %q: %w", digest, errRowInconsistent)
	}
	r, err := decode[domain.RequirementResolution](body)
	if err != nil {
		return domain.RequirementResolution{}, fmt.Errorf("get requirement resolution %q: %w", digest, err)
	}
	if r.Digest != digest || string(r.RequirementKey) != key || string(r.CheckClass) != class ||
		string(r.RequirementSetDigest) != setDigest || r.FloorRegistryGeneration != generation ||
		string(r.ResolvedPolicyDigest) != policyDigest {
		return domain.RequirementResolution{}, fmt.Errorf("get requirement resolution %q: %w", digest, errRowInconsistent)
	}
	if err := tx.gateRequirementResolution(r); err != nil {
		return domain.RequirementResolution{}, fmt.Errorf("get requirement resolution %q current gate: %w", digest, err)
	}
	return r, nil
}

const putCheckProofSQL = `
INSERT INTO check_proofs
    (digest, requirement_resolution_digest, candidate_head, recipe_digest, body_digest, body)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT (digest) DO NOTHING`

func (tx *InternalTx) RecordCheckProof(ctx context.Context, proof domain.CheckProof) error {
	resolution, err := tx.GetRequirementResolution(ctx, proof.RequirementResolutionDigest)
	if err != nil {
		return fmt.Errorf("put check proof %q resolution: %w", proof.Digest, err)
	}
	if _, err := domain.NewPassedCheckState(resolution, proof); err != nil {
		return fmt.Errorf("put check proof %q: %w", proof.Digest, err)
	}
	if err := tx.gateCheckProofRecipe(resolution, proof); err != nil {
		return fmt.Errorf("put check proof %q recipe gate: %w", proof.Digest, err)
	}
	body, err := encode(proof)
	if err != nil {
		return fmt.Errorf("put check proof %q: %w", proof.Digest, err)
	}
	if err := tx.putImmutable(ctx, putCheckProofSQL,
		[]any{
			proof.Digest, proof.RequirementResolutionDigest, proof.CandidateHead,
			proof.RecipeDigest, readinessBodyDigest(body), body,
		},
		`SELECT body_digest || body FROM check_proofs WHERE digest = ?`,
		[]any{proof.Digest}, readinessAuthority(body)); err != nil {
		return fmt.Errorf("put check proof %q: %w", proof.Digest, err)
	}
	return nil
}

func (tx *ReadTx) GetCheckProof(ctx context.Context, digest domain.Digest) (domain.CheckProof, error) {
	var resolutionDigest, head, recipe, bodyDigest string
	var body []byte
	err := tx.tx.QueryRowContext(ctx, `SELECT requirement_resolution_digest,
        candidate_head, recipe_digest, body_digest, body FROM check_proofs WHERE digest = ?`, digest).
		Scan(&resolutionDigest, &head, &recipe, &bodyDigest, &body)
	if err != nil {
		return domain.CheckProof{}, fmt.Errorf("get check proof %q: %w", digest, notFoundOr(err))
	}
	if bodyDigest != readinessBodyDigest(string(body)) {
		return domain.CheckProof{}, fmt.Errorf("get check proof %q: %w", digest, errRowInconsistent)
	}
	proof, err := decode[domain.CheckProof](body)
	if err != nil {
		return domain.CheckProof{}, fmt.Errorf("get check proof %q: %w", digest, err)
	}
	if proof.Digest != digest || string(proof.RequirementResolutionDigest) != resolutionDigest ||
		proof.CandidateHead != head || string(proof.RecipeDigest) != recipe {
		return domain.CheckProof{}, fmt.Errorf("get check proof %q: %w", digest, errRowInconsistent)
	}
	resolution, err := tx.GetRequirementResolution(ctx, proof.RequirementResolutionDigest)
	if err != nil {
		return domain.CheckProof{}, err
	}
	if _, err := domain.NewPassedCheckState(resolution, proof); err != nil {
		return domain.CheckProof{}, fmt.Errorf("get check proof %q binding: %w", digest, err)
	}
	if err := tx.gateCheckProofRecipe(resolution, proof); err != nil {
		return domain.CheckProof{}, fmt.Errorf("get check proof %q recipe gate: %w", digest, err)
	}
	return proof, nil
}

const putDegradedWaiverSQL = `
INSERT INTO degraded_waivers
    (waiver_id, requirement_resolution_digest, check_class, authority,
     floor_registry_generation, lifecycle_digest, body_digest, body)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (waiver_id) DO NOTHING`

// RecordValidatedDegradedWaiver atomically records a grant and its first active
// lifecycle event. Later state changes append through RecordWaiverLifecycleEvent.
func (tx *InternalTx) RecordValidatedDegradedWaiver(ctx context.Context, waiver domain.ValidatedDegradedWaiver, initial domain.WaiverLifecycleEvent) error {
	resolution, err := tx.GetRequirementResolution(ctx, waiver.RequirementResolutionDigest)
	if err != nil {
		return fmt.Errorf("put degraded waiver %q resolution: %w", waiver.ID, err)
	}
	if err := domain.ValidateDegradedWaiver(resolution, initial, waiver); err != nil {
		return fmt.Errorf("put degraded waiver %q: %w", waiver.ID, err)
	}
	if err := tx.gateDegradedWaiverAuthority(waiver); err != nil {
		return fmt.Errorf("put degraded waiver %q authority: %w", waiver.ID, err)
	}
	body, err := encode(waiver)
	if err != nil {
		return fmt.Errorf("put degraded waiver %q: %w", waiver.ID, err)
	}
	if err := tx.putImmutable(ctx, putDegradedWaiverSQL,
		[]any{
			waiver.ID, waiver.RequirementResolutionDigest, resolution.CheckClass,
			waiver.Authority, waiver.FloorRegistryGeneration, waiver.LifecycleDigest,
			readinessBodyDigest(body), body,
		},
		`SELECT body_digest || body FROM degraded_waivers WHERE waiver_id = ?`,
		[]any{waiver.ID}, readinessAuthority(body)); err != nil {
		return fmt.Errorf("put degraded waiver %q: %w", waiver.ID, err)
	}
	return tx.putWaiverLifecycleEvent(ctx, initial, true)
}

func (tx *ReadTx) getDegradedWaiverUnchecked(ctx context.Context, id domain.WaiverID) (domain.ValidatedDegradedWaiver, domain.RequirementResolution, error) {
	var resolutionDigest, class, authority, lifecycleDigest, bodyDigest string
	var generation uint64
	var body []byte
	err := tx.tx.QueryRowContext(ctx, `SELECT requirement_resolution_digest, check_class,
        authority, floor_registry_generation, lifecycle_digest, body_digest, body
        FROM degraded_waivers WHERE waiver_id = ?`, id).
		Scan(&resolutionDigest, &class, &authority, &generation, &lifecycleDigest, &bodyDigest, &body)
	if err != nil {
		return domain.ValidatedDegradedWaiver{}, domain.RequirementResolution{}, notFoundOr(err)
	}
	if bodyDigest != readinessBodyDigest(string(body)) {
		return domain.ValidatedDegradedWaiver{}, domain.RequirementResolution{}, errRowInconsistent
	}
	waiver, err := decode[domain.ValidatedDegradedWaiver](body)
	if err != nil {
		return domain.ValidatedDegradedWaiver{}, domain.RequirementResolution{}, err
	}
	resolution, err := tx.GetRequirementResolution(ctx, waiver.RequirementResolutionDigest)
	if err != nil {
		return domain.ValidatedDegradedWaiver{}, domain.RequirementResolution{}, err
	}
	if waiver.ID != id || string(waiver.RequirementResolutionDigest) != resolutionDigest ||
		string(resolution.CheckClass) != class || string(waiver.Authority) != authority ||
		waiver.FloorRegistryGeneration != generation || string(waiver.LifecycleDigest) != lifecycleDigest {
		return domain.ValidatedDegradedWaiver{}, domain.RequirementResolution{}, errRowInconsistent
	}
	return waiver, resolution, nil
}

func (tx *ReadTx) GetValidatedDegradedWaiver(ctx context.Context, id domain.WaiverID) (domain.ValidatedDegradedWaiver, error) {
	waiver, resolution, err := tx.getDegradedWaiverUnchecked(ctx, id)
	if err != nil {
		return domain.ValidatedDegradedWaiver{}, fmt.Errorf("get degraded waiver %q: %w", id, err)
	}
	lifecycle, err := tx.LatestWaiverLifecycleEvent(ctx, id)
	if err != nil {
		return domain.ValidatedDegradedWaiver{}, fmt.Errorf("get degraded waiver %q lifecycle: %w", id, err)
	}
	if err := domain.ValidateDegradedWaiver(resolution, lifecycle, waiver); err != nil {
		if lifecycle.Status != domain.WaiverLifecycleGranted {
			err = errors.Join(err, domain.ErrWaiverLifecycleInactive)
		}
		return domain.ValidatedDegradedWaiver{}, fmt.Errorf("get degraded waiver %q current gate: %w", id, err)
	}
	if err := tx.gateDegradedWaiverAuthority(waiver); err != nil {
		return domain.ValidatedDegradedWaiver{}, fmt.Errorf("get degraded waiver %q authority: %w", id, err)
	}
	return waiver, nil
}

// DegradedWaiverGate returns the current store-backed re-gate for readiness
// evaluation. It re-reads the latest lifecycle event and daemon-owned grant
// registry, rejecting decoded or cached waivers that are no longer current.
func (tx *ReadTx) DegradedWaiverGate(ctx context.Context) domain.DegradedWaiverGate {
	return func(resolution domain.RequirementResolution, waiver domain.ValidatedDegradedWaiver) error {
		current, err := tx.GetValidatedDegradedWaiver(ctx, waiver.ID)
		if err != nil {
			return err
		}
		if current != waiver || current.RequirementResolutionDigest != resolution.Digest {
			return domain.ErrWaiverInconsistent
		}
		return nil
	}
}

const putWaiverLifecycleEventSQL = `
INSERT INTO waiver_lifecycle_events
    (waiver_id, sequence, status, previous_digest, event_digest, recorded_at, body_digest, body)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (waiver_id, sequence) DO NOTHING`

func (tx *InternalTx) RecordWaiverLifecycleEvent(ctx context.Context, event domain.WaiverLifecycleEvent) error {
	return tx.putWaiverLifecycleEvent(ctx, event, false)
}

func (tx *InternalTx) putWaiverLifecycleEvent(ctx context.Context, event domain.WaiverLifecycleEvent, initial bool) error {
	waiver, _, err := tx.getDegradedWaiverUnchecked(ctx, event.WaiverID)
	if err != nil {
		return fmt.Errorf("put waiver lifecycle %q/%d grant: %w", event.WaiverID, event.Sequence, err)
	}
	if err := event.Validate(); err != nil {
		return fmt.Errorf("put waiver lifecycle %q/%d: %w", event.WaiverID, event.Sequence, err)
	}
	if initial {
		if event.Sequence != 1 || event.EventDigest != waiver.LifecycleDigest {
			return fmt.Errorf("put waiver lifecycle %q initial binding: %w", event.WaiverID, domain.ErrParentKeyMismatch)
		}
	} else {
		previous, err := tx.LatestWaiverLifecycleEvent(ctx, event.WaiverID)
		if err != nil {
			return fmt.Errorf("put waiver lifecycle %q previous: %w", event.WaiverID, err)
		}
		if event.Sequence != previous.Sequence+1 || event.PreviousDigest == nil ||
			*event.PreviousDigest != previous.EventDigest || previous.Status != domain.WaiverLifecycleGranted ||
			event.Status == domain.WaiverLifecycleGranted {
			return fmt.Errorf("put waiver lifecycle %q transition: %w", event.WaiverID, domain.ErrImmutableTransition)
		}
	}
	body, err := encode(event)
	if err != nil {
		return err
	}
	previous := ""
	if event.PreviousDigest != nil {
		previous = string(*event.PreviousDigest)
	}
	if err := tx.putImmutable(ctx, putWaiverLifecycleEventSQL,
		[]any{
			event.WaiverID, event.Sequence, event.Status, previous, event.EventDigest,
			formatTime(event.RecordedAt), readinessBodyDigest(body), body,
		},
		`SELECT body_digest || body FROM waiver_lifecycle_events WHERE waiver_id = ? AND sequence = ?`,
		[]any{event.WaiverID, event.Sequence}, readinessAuthority(body)); err != nil {
		return fmt.Errorf("put waiver lifecycle %q/%d: %w", event.WaiverID, event.Sequence, err)
	}
	return nil
}

func (tx *ReadTx) LatestWaiverLifecycleEvent(ctx context.Context, id domain.WaiverID) (domain.WaiverLifecycleEvent, error) {
	var sequence uint64
	err := tx.tx.QueryRowContext(ctx, `SELECT sequence FROM waiver_lifecycle_events
        WHERE waiver_id = ? ORDER BY sequence DESC LIMIT 1`, id).Scan(&sequence)
	if err != nil {
		return domain.WaiverLifecycleEvent{}, notFoundOr(err)
	}
	return tx.getWaiverLifecycleEvent(ctx, id, sequence)
}

func (tx *ReadTx) getWaiverLifecycleEvent(ctx context.Context, id domain.WaiverID, sequence uint64) (domain.WaiverLifecycleEvent, error) {
	var status, previous, eventDigest, recordedAt, bodyDigest string
	var body []byte
	err := tx.tx.QueryRowContext(ctx, `SELECT status, previous_digest, event_digest,
        recorded_at, body_digest, body FROM waiver_lifecycle_events WHERE waiver_id = ? AND sequence = ?`, id, sequence).
		Scan(&status, &previous, &eventDigest, &recordedAt, &bodyDigest, &body)
	if err != nil {
		return domain.WaiverLifecycleEvent{}, notFoundOr(err)
	}
	if bodyDigest != readinessBodyDigest(string(body)) {
		return domain.WaiverLifecycleEvent{}, errRowInconsistent
	}
	var event domain.WaiverLifecycleEvent
	if err := json.Unmarshal(body, &event); err != nil {
		return domain.WaiverLifecycleEvent{}, err
	}
	if err := event.Validate(); err != nil {
		return domain.WaiverLifecycleEvent{}, err
	}
	wantPrevious := ""
	if event.PreviousDigest != nil {
		wantPrevious = string(*event.PreviousDigest)
	}
	if event.WaiverID != id || event.Sequence != sequence || string(event.Status) != status ||
		wantPrevious != previous || string(event.EventDigest) != eventDigest ||
		formatTime(event.RecordedAt) != recordedAt {
		return domain.WaiverLifecycleEvent{}, errRowInconsistent
	}
	return event, nil
}
