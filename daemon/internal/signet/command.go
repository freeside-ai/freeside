package signet

import (
	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// ClientCommand is the in-process form of the API contract's ClientCommand
// (api/openapi.yaml): a client-prepared decision submission. The provisional
// expected_bindings map is deliberately not carried: for a decision command
// the authoritative binding set is the payload's item_version, pr_head_sha,
// and artifact_digests, which the acceptance boundary cross-checks against
// the live item (the spec's own note on expected_bindings).
type ClientCommand struct {
	// CommandID is the client-generated idempotency key: a retry with the same
	// CommandID returns the original recorded result, never a second effect.
	CommandID string
	DeviceID  domain.DeviceID
	// ExpectedEntityVersion is the store's per-row entity_version the command
	// was prepared against; distinct from the payload's domain ItemVersion. A
	// mismatch rejects the command with the replacement item.
	ExpectedEntityVersion int64
	Payload               DecisionPayload
}

// DecisionPayload mirrors the API contract's DecisionPayload: the decision and
// the exact bindings it was rendered against.
type DecisionPayload struct {
	ItemID      domain.ItemID
	Action      domain.Action
	ItemVersion int
	PRHeadSHA   string
	// ArtifactDigests is the binding set exactly as rendered to the user;
	// acceptance canonicalizes it (domain.NewCommand), so order and duplicates
	// do not affect the recorded command.
	ArtifactDigests []domain.Digest
}

// CommandResult is the committed outcome of an accepted command: the durable
// decision record and the server revision of the transaction that applied it.
// A retry of the same CommandID returns this exact value (§5.14 test 4).
type CommandResult struct {
	Record   domain.Command `json:"record"`
	Revision int64          `json:"revision"`
}
