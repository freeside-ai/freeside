package store

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

const putPublicationAuthoringSQL = `
INSERT INTO publication_authorings
    (content_digest, run_id, created_at, body_digest, body)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT (content_digest) DO NOTHING`

const selectPublicationAuthoringColumns = `content_digest, run_id, created_at, body_digest, body`

// gatePublicationAuthoring is the single evidence trust boundary for one
// authoring artifact, run on put (caller-supplied) and on every read
// (reconstructed), so no path can persist or return an artifact whose prose
// links evidence it must not. It re-resolves each referenced artifact through
// GetArtifact, which independently re-runs domain.ValidatePublishEligibility
// against the current approved-recipe set, then requires a matching digest,
// PublishEligible true, and a class no more restrictive than the authoring
// artifact's target class. The gate fails closed: a missing artifact, a recipe
// that lost approval, a digest disagreement, an ineligible artifact, or a more
// restrictive class each stops the operation. ValidatePublishEligibility alone
// is not enough, because it passes a legal artifact whose PublishEligible is
// false; and publish eligibility ignores sensitivity, which is why the class
// check is separate.
func (tx *ReadTx) gatePublicationAuthoring(
	ctx context.Context, artifact domain.PublicationAuthoring,
) error {
	for _, ref := range artifact.EvidenceRefs {
		evidence, err := tx.GetArtifact(ctx, ref.ArtifactID)
		if err != nil {
			return fmt.Errorf("publication authoring evidence %q: %w", ref.ArtifactID, err)
		}
		if evidence.Digest != ref.Digest {
			return fmt.Errorf("publication authoring evidence %q digest %q, stored %q: %w",
				ref.ArtifactID, ref.Digest, evidence.Digest, domain.ErrPublicationAuthoringInconsistent)
		}
		if !evidence.PublishEligible {
			return fmt.Errorf("publication authoring evidence %q is not publish-eligible: %w",
				ref.ArtifactID, domain.ErrPublicationAuthoringInconsistent)
		}
		if evidence.Provenance.SensitivityClass.MoreRestrictiveThan(artifact.SensitivityClass) {
			return fmt.Errorf("publication authoring evidence %q class %q more restrictive than target %q: %w",
				ref.ArtifactID, evidence.Provenance.SensitivityClass, artifact.SensitivityClass,
				domain.ErrPublicationAuthoringInconsistent)
		}
	}
	return nil
}

// PutPublicationAuthoring persists one immutable authoring artifact keyed by its
// content digest. A byte-identical replay converges; a different body under the
// same digest is an immutable conflict. The evidence gate runs before the write,
// so an idempotent replay is gated too, and the run foreign key is enforced by
// the database (the store runs with foreign_keys=ON).
func (tx *WriteTx) PutPublicationAuthoring(
	ctx context.Context, artifact domain.PublicationAuthoring,
) error {
	if err := artifact.Validate(); err != nil {
		return fmt.Errorf("put publication authoring %q: %w", artifact.Digest, err)
	}
	if err := tx.gatePublicationAuthoring(ctx, artifact); err != nil {
		return fmt.Errorf("put publication authoring %q gate: %w", artifact.Digest, err)
	}
	encoded, err := artifact.Encode()
	if err != nil {
		return fmt.Errorf("put publication authoring %q: %w", artifact.Digest, err)
	}
	body := string(encoded)
	if err := tx.putImmutable(ctx, putPublicationAuthoringSQL,
		[]any{artifact.Digest, artifact.RunID, formatTime(artifact.CreatedAt), reviewBodyDigest(body), body},
		`SELECT body_digest || body FROM publication_authorings WHERE content_digest = ?`,
		[]any{artifact.Digest}, reviewBodyAuthority(body)); err != nil {
		return fmt.Errorf("put publication authoring %q run %q: %w", artifact.Digest, artifact.RunID, err)
	}
	return nil
}

type publicationAuthoringRow struct {
	contentDigest string
	runID         string
	createdAt     string
	bodyDigest    string
	body          []byte
}

func scanPublicationAuthoringRow(sc scanner) (publicationAuthoringRow, error) {
	var row publicationAuthoringRow
	err := sc.Scan(&row.contentDigest, &row.runID, &row.createdAt, &row.bodyDigest, &row.body)
	return row, err
}

// reconstructPublicationAuthoring decodes one row and re-runs every check a
// decoded trust bit demands: the body integrity digest, the strict decode plus
// full validation backstop (which recomputes the content digest), the agreement
// of every extracted lookup column with the decoded body, and the evidence gate.
// No copied column is trusted.
func (tx *ReadTx) reconstructPublicationAuthoring(
	ctx context.Context, row publicationAuthoringRow,
) (domain.PublicationAuthoring, error) {
	if row.bodyDigest != reviewBodyDigest(string(row.body)) {
		return domain.PublicationAuthoring{}, errRowInconsistent
	}
	artifact, err := domain.DecodePublicationAuthoring(row.body)
	if err != nil {
		return domain.PublicationAuthoring{}, err
	}
	if string(artifact.Digest) != row.contentDigest ||
		string(artifact.RunID) != row.runID ||
		formatTime(artifact.CreatedAt) != row.createdAt {
		return domain.PublicationAuthoring{}, errRowInconsistent
	}
	if err := tx.gatePublicationAuthoring(ctx, artifact); err != nil {
		return domain.PublicationAuthoring{}, err
	}
	return artifact, nil
}

// GetPublicationAuthoring reconstructs one artifact by its content digest.
func (tx *ReadTx) GetPublicationAuthoring(
	ctx context.Context, digest domain.Digest,
) (domain.PublicationAuthoring, error) {
	row, err := scanPublicationAuthoringRow(tx.tx.QueryRowContext(ctx,
		`SELECT `+selectPublicationAuthoringColumns+
			` FROM publication_authorings WHERE content_digest = ?`, digest))
	if err != nil {
		return domain.PublicationAuthoring{}, fmt.Errorf("get publication authoring %q: %w", digest, notFoundOr(err))
	}
	artifact, err := tx.reconstructPublicationAuthoring(ctx, row)
	if err != nil {
		return domain.PublicationAuthoring{}, fmt.Errorf("get publication authoring %q: %w", digest, err)
	}
	return artifact, nil
}

// ListPublicationAuthoringsForRun returns one run's authoring artifacts oldest
// first by created_at, then by content digest, reconstructing every row through
// the shared gate. The reconstruction cross-check ties each returned artifact's
// decoded run_id to the queried run, so a row that survives cannot belong to
// another run. The list fails as a whole if any row fails the gate.
//
// The oldest-first order is computed in Go from the decoded time.Time, not from
// a SQL ORDER BY on the stored column: formatTime writes RFC3339Nano, whose
// trimmed fractional part is variable width, so a text sort would place
// "...05.5Z" before "...05Z" and invert two same-second timestamps. The
// (run_id, ...) index still serves the run filter.
func (tx *ReadTx) ListPublicationAuthoringsForRun(
	ctx context.Context, runID domain.RunID,
) ([]domain.PublicationAuthoring, error) {
	rows, err := tx.tx.QueryContext(ctx,
		`SELECT `+selectPublicationAuthoringColumns+
			` FROM publication_authorings WHERE run_id = ?`, runID)
	if err != nil {
		return nil, fmt.Errorf("list publication authorings %q: %w", runID, err)
	}
	var raw []publicationAuthoringRow
	for rows.Next() {
		row, err := scanPublicationAuthoringRow(rows)
		if err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("list publication authorings %q row %d: %w", runID, len(raw)+1, err)
		}
		raw = append(raw, row)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, fmt.Errorf("list publication authorings %q: %w", runID, err)
	}
	out := make([]domain.PublicationAuthoring, 0, len(raw))
	for i, row := range raw {
		artifact, err := tx.reconstructPublicationAuthoring(ctx, row)
		if err != nil {
			return nil, fmt.Errorf("list publication authorings %q row %d: %w", runID, i+1, err)
		}
		out = append(out, artifact)
	}
	slices.SortFunc(out, func(a, b domain.PublicationAuthoring) int {
		if c := a.CreatedAt.Compare(b.CreatedAt); c != 0 {
			return c
		}
		return strings.Compare(string(a.Digest), string(b.Digest))
	})
	return out, nil
}
