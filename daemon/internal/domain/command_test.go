package domain_test

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// validCommandInput is a minimal, valid CommandInput a test mutates one field of.
func validCommandInput() domain.CommandInput {
	return domain.CommandInput{
		CommandID:       "cmd-1",
		DeviceID:        "device-1",
		ItemID:          "item-1",
		ItemVersion:     1,
		PRHeadSHA:       "cafebabe",
		ArtifactDigests: []domain.Digest{"sha256:log"},
		Action:          domain.ActionApprove,
	}
}

// TestNewCommandCanonicalizes checks the constructor sorts and deduplicates the
// bound digest set, so the write-once record has one byte-form per binding and a
// reordered retry converges instead of colliding.
func TestNewCommandCanonicalizes(t *testing.T) {
	in := validCommandInput()
	in.ArtifactDigests = []domain.Digest{"sha256:c", "sha256:a", "sha256:c", "sha256:b"}
	c, err := domain.NewCommand(in)
	if err != nil {
		t.Fatal(err)
	}
	want := []domain.Digest{"sha256:a", "sha256:b", "sha256:c"}
	if !slices.Equal(c.ArtifactDigests, want) {
		t.Errorf("ArtifactDigests = %v, want %v", c.ArtifactDigests, want)
	}
}

// TestNewCommandEmptyDigestsIsArray checks an empty binding set (an item that
// renders no artifacts) canonicalizes to a non-nil empty slice, so it
// serializes as "[]" rather than null and matches the required, non-null
// artifact_digests array the wire contract declares.
func TestNewCommandEmptyDigestsIsArray(t *testing.T) {
	in := validCommandInput()
	in.ArtifactDigests = nil
	c, err := domain.NewCommand(in)
	if err != nil {
		t.Fatal(err)
	}
	if c.ArtifactDigests == nil || len(c.ArtifactDigests) != 0 {
		t.Fatalf("ArtifactDigests = %v, want non-nil empty slice", c.ArtifactDigests)
	}
	body, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"artifact_digests":[]`) {
		t.Errorf("serialized command = %s, want artifact_digests:[]", body)
	}
}

func TestNewCommandRejects(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*domain.CommandInput)
		wantErr error
	}{
		{"empty command_id", func(in *domain.CommandInput) { in.CommandID = "" }, domain.ErrEmptyID},
		{"empty device_id", func(in *domain.CommandInput) { in.DeviceID = "" }, domain.ErrEmptyID},
		{"empty item_id", func(in *domain.CommandInput) { in.ItemID = "" }, domain.ErrEmptyID},
		{"non-positive item_version", func(in *domain.CommandInput) { in.ItemVersion = 0 }, domain.ErrNonPositive},
		{"invalid action", func(in *domain.CommandInput) { in.Action = "teleport" }, domain.ErrInvalidAction},
		{"empty digest entry", func(in *domain.CommandInput) { in.ArtifactDigests = []domain.Digest{"sha256:a", ""} }, domain.ErrEmptyField},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := validCommandInput()
			tt.mutate(&in)
			if _, err := domain.NewCommand(in); !errors.Is(err, tt.wantErr) {
				t.Fatalf("error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

// TestCommandValidateBackstop covers the reconstruction path (store decode,
// struct literal) that bypasses NewCommand's canonicalization: a non-canonical
// or duplicate-bearing body must not validate.
func TestCommandValidateBackstop(t *testing.T) {
	base := func() domain.Command {
		c, err := domain.NewCommand(validCommandInput())
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	for _, tc := range []struct {
		name    string
		digests []domain.Digest
		wantErr error
	}{
		{"unsorted", []domain.Digest{"sha256:b", "sha256:a"}, domain.ErrDigestsNotCanonical},
		{"duplicate", []domain.Digest{"sha256:a", "sha256:a"}, domain.ErrDuplicate},
		{"empty entry", []domain.Digest{""}, domain.ErrEmptyField},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := base()
			c.ArtifactDigests = tc.digests
			if err := c.Validate(); !errors.Is(err, tc.wantErr) {
				t.Fatalf("Validate() = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

// TestCommandBindsSameAs checks the staleness predicate: a command still binds
// the item when its accepted version, head, and digest set all match, and stops
// binding when any of the three drifts (plan §4 lifecycle, §5.14 test 2).
func TestCommandBindsSameAs(t *testing.T) {
	recipe := approvedRecipe
	in := validItemInput(domain.AttentionReadyForFinalReview)
	in.PRHeadSHA = "abc123" // matches provenance() head
	in.EvidenceSnapshot = []domain.Artifact{{ID: "e1", Type: "log", Digest: "sha256:log", Provenance: provenance(domain.ProducerVerifier, &recipe)}}
	item, err := domain.NewAttentionItem(in, approvedRecipes())
	if err != nil {
		t.Fatal(err)
	}

	cmd, err := domain.NewCommand(domain.CommandInput{
		CommandID: "cmd-1", DeviceID: "device-1", ItemID: item.ID,
		ItemVersion: item.ItemVersion, PRHeadSHA: item.PRHeadSHA,
		ArtifactDigests: item.ArtifactDigests, Action: domain.ActionOpenPR,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !cmd.BindsSameAs(item) {
		t.Fatal("BindsSameAs = false for a matching command")
	}

	for _, tc := range []struct {
		name  string
		drift func(*domain.AttentionItem)
	}{
		{"version advanced", func(i *domain.AttentionItem) { i.ItemVersion = 2 }},
		{"head changed", func(i *domain.AttentionItem) { i.PRHeadSHA = "def456" }},
		{"digest set changed", func(i *domain.AttentionItem) { i.ArtifactDigests = []domain.Digest{"sha256:other"} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			drifted := item
			tc.drift(&drifted)
			if cmd.BindsSameAs(drifted) {
				t.Errorf("BindsSameAs = true after %s", tc.name)
			}
		})
	}
}
