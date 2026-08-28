package publish_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/golden"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
)

// fixtureIdentityInput is the identity fixture; the golden test pins
// everything derived from it.
func fixtureIdentityInput() publish.IdentityInput {
	recipe := domain.Digest("sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	return publish.IdentityInput{
		Repo:          "freeside-ai/evidence-repo",
		BaseRef:       "main",
		SourceHeadSHA: "6dcb09b5b57875f334f61aebed695e2e4193db5e",
		ArtifactDigests: []domain.Digest{
			"sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
			"sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		},
		RecipeDigest: &recipe,
	}
}

// TestIdentityGolden pins the derived digest, branch name, and marker
// for the fixture input (issue #81 acceptance 1). The digest pins the
// canonical encoding transitively: any change to the encoded field
// set, order, or version tag changes the digest and fails here.
func TestIdentityGolden(t *testing.T) {
	t.Parallel()
	id, err := publish.DeriveIdentity(fixtureIdentityInput())
	if err != nil {
		t.Fatalf("DeriveIdentity: %v", err)
	}
	got, err := json.MarshalIndent(map[string]string{
		"digest":      string(id.Digest()),
		"branch_name": id.BranchName(),
		"marker":      id.Marker(),
	}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	golden.Assert(t, "publication-identity", append(got, '\n'))
}

func TestValidateCandidateBodyReservesPublisherOwnedSections(t *testing.T) {
	t.Parallel()
	low, high := 0, 64<<10
	for low < high {
		mid := (low + high + 1) / 2
		if err := publish.ValidateCandidateBody(strings.Repeat("x", mid)); err == nil {
			low = mid
		} else {
			high = mid - 1
		}
	}
	if err := publish.ValidateCandidateBody(strings.Repeat("x", low)); err != nil {
		t.Fatalf("exact composed body limit: %v", err)
	}
	if err := publish.ValidateCandidateBody(strings.Repeat("x", low+1)); err == nil {
		t.Fatal("candidate body consumed a publisher-owned section reserve")
	}
}

func TestValidateCandidateBodyRejectsDispositionHistoryMarkers(t *testing.T) {
	t.Parallel()
	for name, body := range map[string]string{
		"open":        "<!-- freeside:disposition-history version=freeside-disposition-history/v1 -->",
		"close":       "<!-- /freeside:disposition-history -->",
		"case":        "<!-- FREESIDE:DISPOSITION-HISTORY -->",
		"indent":      "   <!-- freeside:disposition-history -->",
		"prefix":      "quote <!-- freeside:disposition-history -->",
		"suffix":      "<!-- freeside:disposition-history-extra -->",
		"duplication": "<!-- freeside:disposition-history -->\n<!-- freeside:disposition-history -->",
		"nesting":     "<!-- freeside:disposition-history -->\n<!-- /freeside:disposition-history -->",
	} {
		t.Run(name, func(t *testing.T) {
			if err := publish.ValidateCandidateBody(body); err == nil {
				t.Fatalf("ValidateCandidateBody accepted marker-shaped prose %q", body)
			}
		})
	}
}

// TestDeriveIdentityDeterministic: the same candidate always derives
// the same identity, independent of artifact-digest order.
func TestDeriveIdentityDeterministic(t *testing.T) {
	t.Parallel()
	a, err := publish.DeriveIdentity(fixtureIdentityInput())
	if err != nil {
		t.Fatal(err)
	}
	b, err := publish.DeriveIdentity(fixtureIdentityInput())
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Errorf("identical inputs derived %s and %s", a.Digest(), b.Digest())
	}

	permuted := fixtureIdentityInput()
	permuted.ArtifactDigests[0], permuted.ArtifactDigests[1] = permuted.ArtifactDigests[1], permuted.ArtifactDigests[0]
	c, err := publish.DeriveIdentity(permuted)
	if err != nil {
		t.Fatal(err)
	}
	if a != c {
		t.Error("artifact-digest order changed the identity; the set, not the order, is the content")
	}
}

// TestDeriveIdentityDistinguishesEachInput varies every input field
// independently and asserts each variation derives a distinct identity
// (issue #81 acceptance 1: different input, different identity).
func TestDeriveIdentityDistinguishesEachInput(t *testing.T) {
	t.Parallel()
	otherRecipe := domain.Digest("sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd")
	variations := map[string]func(*publish.IdentityInput){
		"repo":            func(in *publish.IdentityInput) { in.Repo = "freeside-ai/other-repo" },
		"base ref":        func(in *publish.IdentityInput) { in.BaseRef = "release" },
		"source head sha": func(in *publish.IdentityInput) { in.SourceHeadSHA = "0000000000000000000000000000000000000000" },
		"artifact digest": func(in *publish.IdentityInput) { in.ArtifactDigests[0] = otherRecipe },
		"added artifact":  func(in *publish.IdentityInput) { in.ArtifactDigests = append(in.ArtifactDigests, otherRecipe) },
		"recipe digest":   func(in *publish.IdentityInput) { in.RecipeDigest = &otherRecipe },
		"absent recipe":   func(in *publish.IdentityInput) { in.RecipeDigest = nil },
	}

	base, err := publish.DeriveIdentity(fixtureIdentityInput())
	if err != nil {
		t.Fatal(err)
	}
	seen := map[domain.Digest]string{base.Digest(): "base"}
	for name, mutate := range variations {
		in := fixtureIdentityInput()
		mutate(&in)
		id, err := publish.DeriveIdentity(in)
		if err != nil {
			t.Fatalf("DeriveIdentity(%s): %v", name, err)
		}
		if prior, dup := seen[id.Digest()]; dup {
			t.Errorf("variation %q derived the same identity as %q", name, prior)
		}
		seen[id.Digest()] = name
		if id.BranchName() == base.BranchName() {
			t.Errorf("variation %q derived the same branch name as the base", name)
		}
	}
}

// TestDeriveIdentityValidation covers the fail-fast input checks.
func TestDeriveIdentityValidation(t *testing.T) {
	t.Parallel()
	empty := domain.Digest("")
	cases := map[string]func(*publish.IdentityInput){
		"empty repo":              func(in *publish.IdentityInput) { in.Repo = "" },
		"empty base ref":          func(in *publish.IdentityInput) { in.BaseRef = "" },
		"empty source head":       func(in *publish.IdentityInput) { in.SourceHeadSHA = "" },
		"no artifacts":            func(in *publish.IdentityInput) { in.ArtifactDigests = nil },
		"empty artifact digest":   func(in *publish.IdentityInput) { in.ArtifactDigests[0] = "" },
		"duplicate artifact":      func(in *publish.IdentityInput) { in.ArtifactDigests[0] = in.ArtifactDigests[1] },
		"empty recipe behind ptr": func(in *publish.IdentityInput) { in.RecipeDigest = &empty },
	}
	for name, mutate := range cases {
		in := fixtureIdentityInput()
		mutate(&in)
		if _, err := publish.DeriveIdentity(in); err == nil {
			t.Errorf("%s accepted, want error", name)
		}
	}
}

// TestZeroIdentityNamesNoBranch: an Identity that DeriveIdentity never
// produced holds no digest to slice, and BranchName must say so rather
// than panic. Two exported paths hand one to a caller outside the
// package — DeriveIdentity returns it alongside its error, and
// GatedHead.Identity reads it out of an unminted capability — so the
// crash would be reachable from any consumer that ignored an error or
// held the zero capability.
func TestZeroIdentityNamesNoBranch(t *testing.T) {
	t.Parallel()
	if got := (publish.Identity{}).BranchName(); got != "" {
		t.Errorf("zero Identity branch = %q, want empty", got)
	}
	if got := (publish.GatedHead{}).Identity().BranchName(); got != "" {
		t.Errorf("ungated head branch = %q, want empty", got)
	}
	// The empty name is not merely non-panicking: every branch gate must
	// refuse it, so it cannot become a ref.
	if err := publish.ValidateBranchName(""); err == nil {
		t.Error("empty branch name accepted by the branch gate")
	}
}

// TestParseMarker covers the round trip and the adversarial input
// space of a returned PR body: absent, malformed, truncated,
// wrong-case, and conflicting markers all fail closed.
func TestParseMarker(t *testing.T) {
	t.Parallel()
	id, err := publish.DeriveIdentity(fixtureIdentityInput())
	if err != nil {
		t.Fatal(err)
	}
	other, err := publish.DeriveIdentity(publish.IdentityInput{
		Repo:            "freeside-ai/other-repo",
		BaseRef:         "main",
		SourceHeadSHA:   "6dcb09b5b57875f334f61aebed695e2e4193db5e",
		ArtifactDigests: []domain.Digest{"sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},
	})
	if err != nil {
		t.Fatal(err)
	}

	hexDigest := strings.TrimPrefix(string(id.Digest()), "sha256:")
	accept := map[string]string{
		"marker alone":        id.Marker(),
		"marker in body":      "Evidence PR.\n\n" + id.Marker() + "\n\nDetails follow.",
		"indented marker":     "  " + id.Marker(),
		"repeated same":       id.Marker() + "\n" + id.Marker(),
		"no trailing newline": "body\n" + id.Marker(),
	}
	for name, body := range accept {
		got, ok := publish.ParseMarker(body)
		if !ok || got != id.Digest() {
			t.Errorf("%s: ParseMarker = (%q, %t), want (%q, true)", name, got, ok, id.Digest())
		}
	}

	reject := map[string]string{
		"no marker":            "just an ordinary PR body",
		"empty body":           "",
		"truncated digest":     "<!-- freeside:publication-identity=sha256:" + hexDigest[:40] + " -->",
		"overlong digest":      "<!-- freeside:publication-identity=sha256:" + hexDigest + "ff -->",
		"uppercase hex":        "<!-- freeside:publication-identity=sha256:" + strings.ToUpper(hexDigest) + " -->",
		"wrong algorithm":      "<!-- freeside:publication-identity=sha512:" + hexDigest + " -->",
		"non-hex digest":       "<!-- freeside:publication-identity=sha256:" + strings.Repeat("zz", 32) + " -->",
		"missing terminator":   "<!-- freeside:publication-identity=" + string(id.Digest()),
		"conflicting markers":  id.Marker() + "\n" + other.Marker(),
		"conflict with forged": id.Marker() + "\n<!-- freeside:publication-identity=sha256:zz -->",
		"embedded mid-line":    "text " + id.Marker(),
	}
	for name, body := range reject {
		if got, ok := publish.ParseMarker(body); ok {
			t.Errorf("%s: ParseMarker accepted %q", name, got)
		}
	}
}
