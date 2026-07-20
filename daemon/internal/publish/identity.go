package publish

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// identityEncodingVersion tags the canonical encoding DeriveIdentity
// digests. Any change to the encoding (field set, ordering, separator
// discipline) is a new version: two daemon builds must never derive
// different identities for the same candidate, or retries across an
// upgrade would duplicate branches and PRs (plan §5.9).
const identityEncodingVersion = "freeside-publication/v1"

// branchPrefix namespaces every publication branch the daemon creates.
const branchPrefix = "freeside/publish/"

// branchDigestHexLen is how many leading hex digits of the identity
// digest the branch name carries: 16 (64 bits) keeps refs readable
// while a collision within one repository's publication set stays
// negligible; the full digest travels in the PR marker.
const branchDigestHexLen = 16

// markerPrefix and markerSuffix frame the deterministic PR-section
// marker (plan §5.15 rule 4) that binds a pull request to its
// publication identity.
const (
	markerPrefix = "<!-- freeside:publication-identity="
	markerSuffix = " -->"
)

// IdentityInput is the candidate material a publication identity is
// derived from: where the result lives (repository, base ref), the
// exact candidate revision, and the digests of the evidence artifacts
// it publishes. The producing invocation is deliberately absent — see
// DeriveIdentity.
type IdentityInput struct {
	// Repo is the target repository ("owner/name").
	Repo string
	// BaseRef is the base branch the publication PR targets.
	BaseRef string
	// SourceHeadSHA is the candidate commit being published.
	SourceHeadSHA string
	// ArtifactDigests are the content addresses of the published
	// evidence artifacts. Order does not matter; the set does.
	ArtifactDigests []domain.Digest
	// RecipeDigest is the verification recipe the candidate's evidence
	// was produced under, when there is one.
	RecipeDigest *domain.Digest
}

// canonicalIdentity is the versioned canonical form whose JSON encoding
// is digested. Field order is pinned by the struct declaration and the
// identity golden test; changing either is an encoding-version bump.
type canonicalIdentity struct {
	Version         string   `json:"version"`
	Repo            string   `json:"repo"`
	BaseRef         string   `json:"base_ref"`
	SourceHeadSHA   string   `json:"source_head_sha"`
	ArtifactDigests []string `json:"artifact_digests"`
	RecipeDigest    *string  `json:"verification_recipe_digest"`
}

// Identity is a derived publication identity. DeriveIdentity is the
// only constructor; the zero value is invalid and derives nothing.
type Identity struct {
	digest domain.Digest
}

// DeriveIdentity computes the publication identity as a pure function
// of the candidate's digests: the same input always yields the same
// branch and PR identity, and any content difference yields a
// different one (issue #81 acceptance 1). The producing invocation ID
// is excluded on purpose: identity answers "what result should exist
// on GitHub", so a *new* invocation over the same candidate must
// converge on the one existing branch and PR rather than mint a fresh
// pair per attempt. The invocation ID lives on the attempt axis — the
// outbox intent key (IntentKey) — not the content axis.
func DeriveIdentity(in IdentityInput) (Identity, error) {
	if in.Repo == "" {
		return Identity{}, errors.New("identity: empty repository")
	}
	if in.BaseRef == "" {
		return Identity{}, errors.New("identity: empty base ref")
	}
	if in.SourceHeadSHA == "" {
		return Identity{}, errors.New("identity: empty source head sha")
	}
	if len(in.ArtifactDigests) == 0 {
		return Identity{}, errors.New("identity: no artifact digests")
	}
	digests := make([]string, len(in.ArtifactDigests))
	for i, d := range in.ArtifactDigests {
		if d == "" {
			return Identity{}, errors.New("identity: empty artifact digest")
		}
		digests[i] = string(d)
	}
	// The artifact set, not its incidental order, is the content:
	// sort for a canonical sequence, and refuse duplicates rather
	// than silently collapsing them.
	slices.Sort(digests)
	for i := 1; i < len(digests); i++ {
		if digests[i] == digests[i-1] {
			return Identity{}, fmt.Errorf("identity: duplicate artifact digest %s", digests[i])
		}
	}
	var recipe *string
	if in.RecipeDigest != nil {
		if *in.RecipeDigest == "" {
			return Identity{}, errors.New("identity: empty recipe digest")
		}
		r := string(*in.RecipeDigest)
		recipe = &r
	}
	enc, err := json.Marshal(canonicalIdentity{
		Version:         identityEncodingVersion,
		Repo:            in.Repo,
		BaseRef:         in.BaseRef,
		SourceHeadSHA:   in.SourceHeadSHA,
		ArtifactDigests: digests,
		RecipeDigest:    recipe,
	})
	if err != nil {
		return Identity{}, fmt.Errorf("identity: encode canonical form: %w", err)
	}
	sum := sha256.Sum256(enc)
	return Identity{digest: domain.Digest("sha256:" + hex.EncodeToString(sum[:]))}, nil
}

// Digest returns the full identity digest ("sha256:<64 hex>").
func (id Identity) Digest() domain.Digest { return id.digest }

// BranchName returns the deterministic publication branch for this
// identity: branchPrefix plus the digest's leading hex digits.
func (id Identity) BranchName() string {
	hexPart := strings.TrimPrefix(string(id.digest), "sha256:")
	return branchPrefix + hexPart[:branchDigestHexLen]
}

// Marker returns the deterministic PR-section marker line that binds a
// pull request body to this identity (plan §5.15 rule 4). It carries
// the full digest, so a branch-name prefix collision cannot alias two
// identities at the PR.
func (id Identity) Marker() string {
	return markerPrefix + string(id.digest) + markerSuffix
}

// ParseMarker extracts the publication identity a PR body is bound to.
// It fails closed: it reports ok only when the body carries exactly
// one distinct well-formed marker digest, so a body with no marker, a
// malformed one, or markers naming two different identities never
// converges as ours. The body is a returned object (GitHub's), so the
// digest format is validated strictly rather than trusted.
func ParseMarker(body string) (domain.Digest, bool) {
	var found domain.Digest
	for line := range strings.Lines(body) {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, markerPrefix) || !strings.HasSuffix(line, markerSuffix) {
			continue
		}
		raw := line[len(markerPrefix) : len(line)-len(markerSuffix)]
		if !validIdentityDigest(raw) {
			return "", false
		}
		if found != "" && found != domain.Digest(raw) {
			return "", false
		}
		found = domain.Digest(raw)
	}
	return found, found != ""
}

// validIdentityDigest reports whether raw is exactly "sha256:" plus 64
// lowercase hex digits — the only form DeriveIdentity produces.
func validIdentityDigest(raw string) bool {
	return contentaddr.Valid(raw)
}
