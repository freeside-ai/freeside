package stage

import (
	"errors"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/export"
	"github.com/freeside-ai/freeside/daemon/internal/importer"
)

// releasedScopeConflict reads only the launcher-declared, digest-bound channel.
func releasedScopeConflict(out exportOutcome) (domain.ScopeConflict, bool, error) {
	var conflict domain.ScopeConflict
	present := false
	if !out.evidencePresent {
		return conflict, false, nil
	}
	for _, entry := range out.evidence.Entries {
		if entry.Label != export.ScopeConflictEvidenceLabel {
			continue
		}
		if present || entry.Size > int64(domain.MaxScopeConflictBytes) {
			return conflict, false, errors.New("ambiguous or oversized scope conflict")
		}
		body, err := readBoundedEvidenceBlob(out.dir, domain.Digest(entry.Digest), int64(domain.MaxScopeConflictBytes))
		if err != nil {
			return conflict, false, err
		}
		conflict, err = domain.DecodeScopeConflict(body)
		if err != nil {
			return conflict, false, err
		}
		canonical, err := domain.EncodeScopeConflict(conflict)
		if err != nil {
			return conflict, false, err
		}
		secret := importer.ContainsSecret(body) || importer.ContainsSecret(canonical) ||
			blockedOutcomeContainsSecret(domain.BlockedOutcome{Decisions: []domain.Decision{conflict.Decision}})
		for _, p := range conflict.Paths {
			secret = secret || importer.ContainsSecret([]byte(p))
		}
		if secret {
			return conflict, false, errors.New("scope conflict contains credential-shaped content")
		}
		present = true
	}
	return conflict, present, nil
}
