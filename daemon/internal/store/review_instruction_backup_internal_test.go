package store

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/migrations"
)

func TestCheckpointArtifactDigestsIncludesCodexReviewInstructionClosure(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openRaw(t)
	if err := migrate(ctx, db, migrations.FS); err != nil {
		t.Fatal(err)
	}
	host := domain.Digest("sha256:" + strings.Repeat("a", 64))
	source := domain.Digest("sha256:" + strings.Repeat("b", 64))
	result := domain.Digest("sha256:" + strings.Repeat("c", 64))
	body := validReviewRequestBody(t, host, source, result)
	if _, err := db.ExecContext(ctx, `INSERT INTO codex_review_requests
		(invocation_id, body_digest, body) VALUES (?, ?, ?)`,
		"review-1", codexReviewBodyDigest(body), string(body)); err != nil {
		t.Fatal(err)
	}
	legacy := []byte(`{"run_id":"legacy-run","round":1}`)
	if _, err := db.ExecContext(ctx, `INSERT INTO codex_review_requests
		(invocation_id, body_digest, body) VALUES (?, ?, ?)`,
		"review-legacy", codexReviewBodyDigest(legacy), string(legacy)); err != nil {
		t.Fatal(err)
	}
	closure, err := checkpointArtifactDigests(ctx, db, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(closure.digests)
	want := []domain.Digest{host, source, result}
	slices.Sort(want)
	if !slices.Equal(closure.digests, want) {
		t.Fatalf("review instruction closure = %v, want %v", closure.digests, want)
	}
}

func TestCheckpointArtifactDigestsRejectsUnknownCurrentReviewRequestFields(t *testing.T) {
	t.Parallel()
	host := domain.Digest("sha256:" + strings.Repeat("a", 64))
	source := domain.Digest("sha256:" + strings.Repeat("b", 64))
	result := domain.Digest("sha256:" + strings.Repeat("c", 64))
	for name, mutate := range map[string]func(map[string]any){
		"top-level": func(request map[string]any) { request["unknown"] = true },
		"nested instructions": func(request map[string]any) {
			request["instructions"].(map[string]any)["unknown"] = true
		},
	} {
		t.Run(name, func(t *testing.T) {
			var request map[string]any
			if err := json.Unmarshal(validReviewRequestBody(t, host, source, result), &request); err != nil {
				t.Fatal(err)
			}
			mutate(request)
			body, err := json.Marshal(request)
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			db := openRaw(t)
			if err := migrate(ctx, db, migrations.FS); err != nil {
				t.Fatal(err)
			}
			if _, err := db.ExecContext(ctx, `INSERT INTO codex_review_requests
				(invocation_id, body_digest, body) VALUES (?, ?, ?)`,
				"review-unknown", codexReviewBodyDigest(body), string(body)); err != nil {
				t.Fatal(err)
			}
			if _, err := checkpointArtifactDigests(ctx, db, nil, nil); err == nil {
				t.Fatal("current review request with an unknown field was accepted")
			}
		})
	}
}

func validReviewRequestBody(t *testing.T, host, source, result domain.Digest) []byte {
	t.Helper()
	body, err := json.Marshal(exec.ReviewRequest{
		RunID: "run-1", Round: 1, Repo: "owner/repo", RepositoryID: 1, BaseRef: "main",
		BaseSHA: strings.Repeat("a", 40), HeadSHA: strings.Repeat("b", 40), Workspace: "/workspace",
		Verification: exec.ReviewVerificationEvidence{Outcome: domain.VerificationPassed, RecipeDigest: host, EvidenceSnapshotDigest: source, ArtifactDigests: []domain.Digest{result}},
		Instructions: exec.ReviewInstructionBinding{CompositionVersion: "codex_explicit_bundle_v1", HostDigest: &host, RepositorySources: []exec.ReviewInstructionSource{{Path: "AGENTS.md", Digest: source}}, ResultDigest: result},
		RequestedAt:  time.Unix(0, 0).UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func TestCheckpointArtifactDigestsRejectsAmbiguousInstructionField(t *testing.T) {
	t.Parallel()
	result := "sha256:" + strings.Repeat("c", 64)
	valid := `{"composition_version":"codex_explicit_bundle_v1",` +
		`"host_digest":null,"repository_sources":[],"result_digest":"` + result + `"}`
	for _, body := range []string{
		`{"instructions":null}`,
		`{"instructions":` + valid + `,"Instructions":null}`,
	} {
		t.Run(body, func(t *testing.T) {
			ctx := context.Background()
			db := openRaw(t)
			if err := migrate(ctx, db, migrations.FS); err != nil {
				t.Fatal(err)
			}
			if _, err := db.ExecContext(ctx, `INSERT INTO codex_review_requests
				(invocation_id, body_digest, body) VALUES (?, ?, ?)`,
				"review-ambiguous", codexReviewBodyDigest([]byte(body)), body); err != nil {
				t.Fatal(err)
			}
			if _, err := checkpointArtifactDigests(ctx, db, nil, nil); err == nil {
				t.Fatal("ambiguous instruction field was accepted")
			}
		})
	}
}
