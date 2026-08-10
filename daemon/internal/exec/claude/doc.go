// Package claude adapts the provider-neutral stage driver to the pinned Claude
// CLI. It owns provider-specific prompt rendering, run and workspace identity,
// credential-volume resolution, and ward handoff construction while preserving
// the existing Config, New, Driver, replay, and sentinel surface.
package claude
