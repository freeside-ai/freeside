package verify

import (
	"bytes"
	"context"
	"fmt"
)

// loadTrustedRecipeBytes resolves the trusted recipe bytes from the
// declared source (§5.8). The config source returns the approved bytes
// as snapshotted by the caller; the base-commit source reads the recipe
// blob at the enforced base SHA from the daemon-owned checkout. Every
// state other than a readable regular blob fails closed: a trusted
// source that cannot be read is a configuration failure, never a reason
// to fall back to candidate content.
func loadTrustedRecipeBytes(ctx context.Context, g *gitRunner, src RecipeSource, baseSHA, recipePath string, maxBytes int64) ([]byte, error) {
	if !src.valid() {
		return nil, fmt.Errorf("recipe source is unset: %w", ErrInvalidOptions)
	}
	if !src.fromBase {
		return src.raw, nil
	}
	content, state, err := g.blobAt(ctx, baseSHA, recipePath, maxBytes)
	if err != nil {
		return nil, err
	}
	switch state {
	case blobPresent:
		return content, nil
	case blobAbsent:
		return nil, fmt.Errorf("recipe %s absent at trusted base %s: %w", recipePath, baseSHA, ErrRecipeUnreadable)
	case blobNotRegular:
		return nil, fmt.Errorf("recipe %s at trusted base %s is not a regular blob: %w", recipePath, baseSHA, ErrRecipeUnreadable)
	case blobTooLarge:
		return nil, fmt.Errorf("recipe %s at trusted base %s exceeds the %d-byte cap: %w", recipePath, baseSHA, maxBytes, ErrRecipeUnreadable)
	}
	return nil, fmt.Errorf("recipe %s at trusted base %s: unknown blob state: %w", recipePath, baseSHA, ErrRecipeUnreadable)
}

// recipeDivergence compares the candidate head's in-tree copy of the
// recipe path against the trusted bytes that will actually execute
// (acceptance: a workspace-modified recipe is never executed, and the
// divergence is detected and flagged). A differing, oversized, or
// non-regular head copy is a divergence; an absent head copy is a
// divergence only when the trusted source is the base commit, since a
// config-sourced recipe need not exist in the tree at all.
func recipeDivergence(ctx context.Context, g *gitRunner, src RecipeSource, headSHA, recipePath string, trusted []byte, maxBytes int64) ([]Finding, error) {
	content, state, err := g.blobAt(ctx, headSHA, recipePath, maxBytes)
	if err != nil {
		return nil, err
	}
	detail := ""
	switch state {
	case blobPresent:
		if bytes.Equal(content, trusted) {
			return nil, nil
		}
		detail = "candidate copy differs from the trusted recipe; the trusted source was executed"
	case blobAbsent:
		if !src.fromBase {
			return nil, nil
		}
		detail = "candidate deleted the trusted base recipe; the trusted source was executed"
	case blobNotRegular:
		detail = "candidate replaced the recipe path with a non-blob; the trusted source was executed"
	case blobTooLarge:
		detail = "candidate copy exceeds the recipe read cap so cannot equal the trusted recipe; the trusted source was executed"
	}
	return []Finding{{Path: recipePath, Kind: FindingRecipeDivergence, Detail: detail}}, nil
}
