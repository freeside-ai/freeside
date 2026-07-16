// Package publish owns all GitHub credentials (docs/plan.md §5.1): no
// other component holds, mints, or transports them, and no GitHub
// write credential ever enters any workspace (§5.4).
//
// This unit (issue #80) is App authentication with contained
// credentials: registration via the manifest flow (Registrar), with
// the private key landing directly in protected storage (Keystore,
// §10); App JWT construction (AppJWT); per-repository installation
// tokens with the pinned minimum permission set (Minter,
// PublishPermissions); a per-mint audit record (MintRecord, Recorder);
// and the redaction boundary every credential value lives behind
// (Secret). The credentials directory is structurally disjoint from
// the state directory, so the key stays out of backup checkpoints
// (§5.10; recovery may require reauthentication) and workspace mounts.
//
// Deterministic publication identities and reconciliation (issue #81,
// §5.9, §5.11): a publication identity derives purely from the
// candidate's digests (DeriveIdentity), naming the branch and the PR
// marker; every external effect is check-before-create under that
// identity (Publisher), with the intent recorded through the outbox
// port (IntentLedger) keyed by invocation ID before dispatch; and the
// branch and PR reconcile per resource with conditional requests
// (Reconciler), no global cursor.
//
// Later units: effectively-once publication with kill tests (#82), and
// the EvidencePublisher (1B, §5.15).
//
// Lane: publish. See docs/plan.md §5.5 (the CI trust boundary), §5.9
// (durability: effectively-once), §5.11 (GitHub integration), and §10
// (registration and protected storage).
package publish
