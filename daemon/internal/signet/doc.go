// Package signet is the attention service: items, deliveries,
// conversations, client synchronization, and devices.
//
// Lane: signet. See docs/plan.md §4 (the attention model) and §5.14
// (client synchronization and conversations).
//
// The Service owns the item read model, ClientCommand acceptance, per-type
// action policy, and the HTTP synchronization surface: canonical bootstrap,
// partial resource snapshots, revision heartbeat, and command projection.
// Device credential verification is a required injected HTTP dependency until
// the device lifecycle unit (#67) supplies its implementation; conversation
// mutation and delivery production remain with their own units.
package signet
