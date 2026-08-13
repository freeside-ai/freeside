package store

import (
	"context"
	"errors"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/migrations"
)

// TestGetAgentClaimsRejectsTamperedRow: a stored agent_claims body that bypassed
// the Put boundary must fail the read. decode re-runs agentClaimsRecord.Validate
// (which re-runs AgentClaim.Validate), and the record's invocation_id is
// cross-checked against the row key, so a laundered producer class, a rebound
// invocation, an unbound text claim, or an empty set all fail closed rather than
// reconstructing as trusted claims. Internal test: encode would reject these, so
// the corrupt rows are written past the Put boundary as raw JSON.
func TestGetAgentClaimsRejectsTamperedRow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openRaw(t)
	if err := migrate(ctx, db, migrations.FS); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := seedEpoch(ctx, db); err != nil {
		t.Fatalf("seedEpoch: %v", err)
	}
	s := &Store{db: db}

	// A parent invocation to satisfy the agent_claims foreign key; the read never
	// reads it back, so a minimal raw row suffices.
	if _, err := db.ExecContext(ctx,
		`INSERT INTO agent_invocations (id, entity_version, as_of_revision, body) VALUES ('inv-1', 1, 1, '{}')`); err != nil {
		t.Fatalf("insert parent invocation: %v", err)
	}

	const agentProvenance = `{"producer_class":"agent","producer_invocation_id":"inv-1","head_binding":"head_bound","source_head_sha":"cafebabe","verification_recipe_digest":null,"sensitivity_class":"normal"}`
	const verifierProvenance = `{"producer_class":"verifier","producer_invocation_id":"inv-1","head_binding":"head_bound","source_head_sha":"cafebabe","verification_recipe_digest":null,"sensitivity_class":"normal"}`

	cases := []struct {
		name    string
		body    string
		wantErr error
	}{
		{
			// Provenance.Validate admits the verifier class, so the agent-pin in
			// AgentClaim.Validate is the gate that refuses a laundered claim.
			"laundered producer class",
			`{"invocation_id":"inv-1","claims":[{"label":"screenshot","artifact_id":"art-2","digest":"sha256:img","provenance":` + verifierProvenance + `,"text":null}]}`,
			domain.ErrNonAgentClaim,
		},
		{
			"body invocation_id disagrees with row key",
			`{"invocation_id":"inv-2","claims":[{"label":"screenshot","artifact_id":"art-2","digest":"sha256:img","provenance":` + agentProvenance + `,"text":null}]}`,
			errRowInconsistent,
		},
		{
			// A text claim whose digest does not address its content: the binding
			// rule (a claim cannot display one text while binding another digest).
			"text claim digest unbound from content",
			`{"invocation_id":"inv-1","claims":[{"label":"summary","artifact_id":"art-3","digest":"sha256:wrong","provenance":` + agentProvenance + `,"text":{"media_type":"text/markdown","content":"hello"}}]}`,
			domain.ErrClaimTextDigestMismatch,
		},
		{
			"empty claim set",
			`{"invocation_id":"inv-1","claims":[]}`,
			domain.ErrEmptyField,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := db.ExecContext(ctx, `DELETE FROM agent_claims`); err != nil {
				t.Fatalf("reset: %v", err)
			}
			if _, err := db.ExecContext(ctx,
				`INSERT INTO agent_claims (invocation_id, entity_version, as_of_revision, body) VALUES ('inv-1', 1, 1, ?)`,
				tc.body); err != nil {
				t.Fatalf("insert corrupt row: %v", err)
			}
			err := s.Read(ctx, func(tx *ReadTx) error {
				_, err := tx.GetAgentClaims(ctx, "inv-1")
				return err
			})
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("GetAgentClaims error = %v, want %v", err, tc.wantErr)
			}
		})
	}
}
