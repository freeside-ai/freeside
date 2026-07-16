// Package signet is the attention service: items, deliveries,
// conversations, client synchronization, and devices.
//
// Lane: signet. See docs/plan.md §4 (the attention model) and §5.14
// (client synchronization and conversations).
//
// The Service owns the item read model, ClientCommand acceptance (including
// the discuss transaction), per-type action policy, device pairing and
// revocation with credential verification (NewRequestAuthorizer), the
// delivery pipeline with its ntfy channel (delivery.go, ntfy.go), and the
// HTTP synchronization surface: canonical bootstrap, partial resource
// snapshots, revision heartbeat, and command projection.
package signet
