// Package signet is the attention service: items, deliveries,
// conversations, client synchronization, and devices.
//
// Lane: signet. See docs/plan.md §4 (the attention model) and §5.14
// (client synchronization and conversations).
//
// This unit (#65) carries the item read model and the ClientCommand
// acceptance boundary: version- and binding-checked command acceptance,
// status-lifecycle gating, idempotent retries by command_id returning
// the original committed result, and transactional version-checked
// resolutions. The in-process Service is the whole surface; the HTTP
// projection of the same checks lands with the sync-surface unit (#66),
// per-type allowed action sets with #23, and devices, conversations,
// and deliveries with their own units.
package signet
