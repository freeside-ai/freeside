package domain

import (
	"fmt"
	"slices"
)

// Command is the durable, immutable record of one accepted client decision on
// an attention item (plan §4 lifecycle, §5.14 "Every mutation is a
// ClientCommand ...; retries return the original result"). It pins the exact
// bindings the decision was taken against — the accepted item version, the PR
// head SHA, and the artifact digest set rendered for the decision — so a later
// change to any of them invalidates a prepared command rather than letting a
// stale approval bind (the non-waivable stale-approval class, plan §3.1). The
// item body evolves through versioned transitions; this record does not, so it
// is the authoritative account of what was accepted, not the current item.
//
// It is write-once, keyed by CommandID (a client-generated idempotency key):
// the effectively-once contract (plan §5.9, §5.14 test 4) is that a retry under
// the same CommandID returns the original record and a changed body under that
// id is a conflict. The committed result — the client-visible revision the
// command applied at — is the store's as_of_revision for the record, so it is
// not carried in the body. Whether these bindings still match the live item is
// the store's cross-check at submission (a mismatch is rejected with the
// replacement item, §5.14 test 2), not a domain-local invariant: this type
// holds no item to compare against. The store also rejects a genuinely new
// command against an item whose status is no longer open (issue #55): binding
// equality alone cannot detect closure at the item's current version.
type Command struct {
	CommandID       string   `json:"command_id"`
	DeviceID        DeviceID `json:"device_id"`
	ItemID          ItemID   `json:"item_id"`
	ItemVersion     int      `json:"item_version"`
	PRHeadSHA       string   `json:"pr_head_sha"`
	ArtifactDigests []Digest `json:"artifact_digests"`
	Action          Action   `json:"action"`
	// Message and Attachments carry conversation content for the actions that
	// ride the conversation channel (discuss, plan §5.14: the transaction's
	// first step is "append message"); both are empty for pure decisions.
	// Unlike ArtifactDigests they are content, not a binding set: attachment
	// order is authored, so it is preserved, never canonicalized. Which
	// actions require or forbid them is the acceptance boundary's policy, not
	// a domain invariant.
	Message     string   `json:"message"`
	Attachments []Digest `json:"attachments"`
}

// CommandInput carries the caller-supplied fields of a Command. The bound
// digest set may arrive in any order; NewCommand canonicalizes it.
type CommandInput struct {
	CommandID       string
	DeviceID        DeviceID
	ItemID          ItemID
	ItemVersion     int
	PRHeadSHA       string
	ArtifactDigests []Digest
	Action          Action
	Message         string
	Attachments     []Digest
}

// NewCommand builds a validated Command whose bound digest set is in canonical
// (sorted, deduplicated) order, so the persisted body is byte-for-byte the one
// form a given accepted binding addresses: a reordered retry converges on the
// stored record instead of colliding with it under a false conflict (the #33
// canonical-body lesson). The copy also detaches the record from the caller's
// backing array.
func NewCommand(in CommandInput) (Command, error) {
	// append to a non-nil empty base so the bound set is always array-shaped:
	// an empty set serializes as "[]", matching the required, non-null
	// artifact_digests array the wire contract declares (api/openapi.yaml),
	// while staying byte-stable for the command_id-keyed write-once record.
	digests := append([]Digest{}, in.ArtifactDigests...)
	slices.Sort(digests)
	digests = slices.Compact(digests)
	c := Command{
		CommandID:       in.CommandID,
		DeviceID:        in.DeviceID,
		ItemID:          in.ItemID,
		ItemVersion:     in.ItemVersion,
		PRHeadSHA:       in.PRHeadSHA,
		ArtifactDigests: digests,
		Action:          in.Action,
		Message:         in.Message,
		// Copied, not canonicalized: attachment order is authored content, and
		// a retry resends the same stored byte-form, so the #33 reordering
		// concern does not arise. The non-nil base keeps the field
		// array-shaped ("[]") in the write-once record.
		Attachments: append([]Digest{}, in.Attachments...),
	}
	if err := c.Validate(); err != nil {
		return Command{}, err
	}
	return c, nil
}

// Validate reports whether the command record is well-formed: identified by a
// command id, device, item, and a positive accepted item version; carrying a
// valid action; and binding a canonical digest set. It is the reconstruction
// backstop for records that did not pass NewCommand (store decode, struct
// literals). pr_head_sha may be empty — an item without a PR (a spec awaiting
// approval) binds no head — as may the digest set; whether either matches the
// live item is the store's submission cross-check, not a domain invariant.
func (c Command) Validate() error {
	if c.CommandID == "" {
		return fmt.Errorf("command command_id: %w", ErrEmptyID)
	}
	if c.DeviceID == "" {
		return fmt.Errorf("command %s device_id: %w", c.CommandID, ErrEmptyID)
	}
	if c.ItemID == "" {
		return fmt.Errorf("command %s item_id: %w", c.CommandID, ErrEmptyID)
	}
	if c.ItemVersion < 1 {
		return fmt.Errorf("command %s item_version %d: %w", c.CommandID, c.ItemVersion, ErrNonPositive)
	}
	if !c.Action.valid() {
		return fmt.Errorf("command %s action %q: %w", c.CommandID, c.Action, ErrInvalidAction)
	}
	// The bound digest set is canonical (sorted, deduplicated): the write-once
	// record then has one byte-form per accepted binding, so a reordered retry
	// converges instead of colliding. Entries are content addresses, so an empty
	// one is malformed.
	seen := make(map[Digest]struct{}, len(c.ArtifactDigests))
	prev := Digest("")
	for idx, d := range c.ArtifactDigests {
		if d == "" {
			return fmt.Errorf("command %s artifact_digests[%d]: %w", c.CommandID, idx, ErrEmptyField)
		}
		if _, dup := seen[d]; dup {
			return fmt.Errorf("command %s artifact_digests[%d] %q: %w", c.CommandID, idx, d, ErrDuplicate)
		}
		if idx > 0 && d < prev {
			return fmt.Errorf("command %s artifact_digests[%d] %q after %q: %w", c.CommandID, idx, d, prev, ErrDigestsNotCanonical)
		}
		seen[d] = struct{}{}
		prev = d
	}
	// Attachment entries are content addresses like a message's (see
	// Message.validateUnsequenced): empty is malformed and a repeat is
	// authoring noise, but order is authored content, so no canonical-order
	// requirement.
	seenAtt := make(map[Digest]struct{}, len(c.Attachments))
	for idx, d := range c.Attachments {
		if d == "" {
			return fmt.Errorf("command %s attachments[%d]: %w", c.CommandID, idx, ErrEmptyField)
		}
		if _, dup := seenAtt[d]; dup {
			return fmt.Errorf("command %s attachments[%d] %q: %w", c.CommandID, idx, d, ErrDuplicate)
		}
		seenAtt[d] = struct{}{}
	}
	return nil
}

// BindsSameAs reports whether the command's pinned bindings still describe the
// item: the accepted item version, the PR head, and the exact rendered digest
// set. A false result is a stale submission — an input changed after the record
// was prepared — which the store rejects with the replacement item (plan §4
// lifecycle, §5.14 test 2). The item's ArtifactDigests is already canonical
// (derived by NewAttentionItem) and so is the command's, so equality is a
// direct slice compare.
func (c Command) BindsSameAs(item AttentionItem) bool {
	return c.ItemVersion == item.ItemVersion &&
		c.PRHeadSHA == item.PRHeadSHA &&
		slices.Equal(c.ArtifactDigests, item.ArtifactDigests)
}
