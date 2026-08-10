// Package stage implements the provider-neutral durable stage-driver state
// machine. A five-method Provider port supplies handoff rendering, prompt
// rendering, stable run and workspace identities, and the provider's
// preparation-failure status; persistence, recovery, import, and result
// delivery remain shared.
//
// The driver holds no store, transport, or runtime handle. Every side effect
// beyond its private intent directory arrives through a narrow port supplied
// by daemon composition. Reconstructed intent files are untrusted and are
// re-gated against durable admission and current policy before use.
package stage
