package publish_test

import (
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/golden"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
)

func fixtureOutcome() publish.Outcome {
	return publish.Outcome{
		Identity:         "sha256:01c663f9a986e10d214b2c31c75fa5088e2995674a8e8f2ba959111e06a23fb8",
		Repo:             "freeside-ai/evidence-repo",
		BaseRef:          "main",
		HeadSHA:          "6dcb09b5b57875f334f61aebed695e2e4193db5e",
		Branch:           "freeside/publish/01c663f9a986e10d",
		PRNumber:         101,
		EvidenceEligible: true,
	}
}

// TestOutcomeGolden pins the encoded inbox payload: the outcome row
// outlives any single daemon build, so a later read must decode what an
// older build recorded.
func TestOutcomeGolden(t *testing.T) {
	payload, err := fixtureOutcome().Encode()
	if err != nil {
		t.Fatal(err)
	}
	golden.Assert(t, "publication-outcome", append(payload, '\n'))
}

// TestOutcomeRoundTrip: Encode then DecodeOutcome returns the same
// outcome.
func TestOutcomeRoundTrip(t *testing.T) {
	payload, err := fixtureOutcome().Encode()
	if err != nil {
		t.Fatal(err)
	}
	got, err := publish.DecodeOutcome(payload)
	if err != nil {
		t.Fatal(err)
	}
	if got != fixtureOutcome() {
		t.Errorf("round trip = %+v, want %+v", got, fixtureOutcome())
	}
}

// TestOutcomeValidation: a malformed outcome neither encodes nor
// decodes; a decoded inbox row is a reconstructed value, so decode
// re-validates rather than trusting it.
func TestOutcomeValidation(t *testing.T) {
	cases := map[string]func(*publish.Outcome){
		"empty identity":      func(o *publish.Outcome) { o.Identity = "" },
		"non-digest identity": func(o *publish.Outcome) { o.Identity = "freeside/publish/abcd" },
		"truncated identity":  func(o *publish.Outcome) { o.Identity = "sha256:abcd" },
		"empty repo":          func(o *publish.Outcome) { o.Repo = "" },
		"empty base ref":      func(o *publish.Outcome) { o.BaseRef = "" },
		"empty head sha":      func(o *publish.Outcome) { o.HeadSHA = "" },
		"empty branch":        func(o *publish.Outcome) { o.Branch = "" },
		"mismatched branch":   func(o *publish.Outcome) { o.Branch = "freeside/publish/ffffffffffffffff" },
		"zero pr number":      func(o *publish.Outcome) { o.PRNumber = 0 },
		"negative pr number":  func(o *publish.Outcome) { o.PRNumber = -1 },
		"ineligible evidence": func(o *publish.Outcome) { o.EvidenceEligible = false },
	}
	for name, mutate := range cases {
		o := fixtureOutcome()
		mutate(&o)
		if _, err := o.Encode(); err == nil {
			t.Errorf("%s: Encode accepted, want error", name)
		}
	}

	if _, err := publish.DecodeOutcome([]byte(`not json`)); err == nil {
		t.Error("DecodeOutcome accepted non-JSON, want error")
	}
	// Unknown fields fail closed: a payload this build cannot fully
	// interpret must not be trusted as a recorded result.
	payload, err := fixtureOutcome().Encode()
	if err != nil {
		t.Fatal(err)
	}
	widened := strings.Replace(string(payload), "{", `{"force":true,`, 1)
	if _, err := publish.DecodeOutcome([]byte(widened)); err == nil {
		t.Error("DecodeOutcome accepted an unknown field, want error")
	}
	// Trailing data fails closed the same way.
	for name, trailer := range map[string]string{
		"second JSON value": ` {"other":true}`,
		"garbage":           `garbage`,
	} {
		if _, err := publish.DecodeOutcome([]byte(string(payload) + trailer)); err == nil {
			t.Errorf("DecodeOutcome accepted trailing %s, want error", name)
		}
	}
}

// TestOutcomeKeyIsKindNamespacedFullDigest: the inbox key is the kind
// prefix plus the full identity digest — namespaced so it cannot collide
// with another inbox kind, and carrying the full digest (not the 16-hex
// branch prefix) so two identities sharing a branch-name prefix cannot
// alias one outcome row.
func TestOutcomeKeyIsKindNamespacedFullDigest(t *testing.T) {
	recipe := testRecipe
	id, err := publish.DeriveIdentity(publish.IdentityInput{
		Repo:            "freeside-ai/evidence-repo",
		BaseRef:         "main",
		SourceHeadSHA:   testHeadSHA,
		ArtifactDigests: []domain.Digest{testArtifactD},
		RecipeDigest:    &recipe,
	})
	if err != nil {
		t.Fatal(err)
	}
	key := publish.OutcomeKey(id)
	want := publish.IntentKindOutcome + "/" + string(id.Digest())
	if key != want {
		t.Errorf("OutcomeKey = %q, want %q", key, want)
	}
	// The full digest must be present, not merely the branch's 16-hex
	// prefix.
	if !strings.Contains(key, string(id.Digest())) {
		t.Errorf("OutcomeKey %q must carry the full digest %q", key, id.Digest())
	}
	if strings.Contains(id.BranchName(), key) {
		t.Errorf("OutcomeKey %q must be wider than the branch prefix %q", key, id.BranchName())
	}
}
