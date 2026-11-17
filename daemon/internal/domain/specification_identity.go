package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/big"
	"strings"
)

// The specification lane's identifiers derive from one another: the
// specification run from the implementation run it precedes, the stage and
// every invocation from the run. Outbox and inbox idempotency keys,
// attention item subjects, and backup manifests embed those bytes, so every
// site that mints or parses a derived identifier goes through these helpers
// instead of spelling a prefix.
const (
	specificationRunIDPrefix        = "run-specification-"
	specificationStageIDPrefix      = "specify-"
	specificationInvocationIDPrefix = "inv-specify-"
	specificationRunSeed            = "freeside.specification-run/v1"
)

// SpecificationRunIDForImplementation derives the specification run identity
// from the operator-visible implementation identity.
func SpecificationRunIDForImplementation(implementationRunID RunID) RunID {
	sum := sha256.Sum256([]byte(specificationRunSeed + "\x00" + string(implementationRunID)))
	return RunID(specificationRunIDPrefix + hex.EncodeToString(sum[:]))
}

// SpecificationRunIDMatchesImplementation reports whether the specification
// run identity is the one derived from the implementation run.
func SpecificationRunIDMatchesImplementation(specificationRunID, implementationRunID RunID) bool {
	return specificationRunID == SpecificationRunIDForImplementation(implementationRunID)
}

// SpecificationStageID names the single stage of a specification run.
func SpecificationStageID(runID RunID) StageID {
	return StageID(specificationStageIDPrefix + string(runID))
}

// SpecificationInvocationIDPrefix is the prefix shared by every invocation of
// the run, ending in the separator before the iteration number.
func SpecificationInvocationIDPrefix(runID RunID) string {
	return specificationInvocationIDPrefix + string(runID) + "-"
}

// SpecificationInvocationID names one iteration of the run's specification
// invocation.
func SpecificationInvocationID(runID RunID, iteration int) InvocationID {
	return InvocationID(fmt.Sprintf("%s%d", SpecificationInvocationIDPrefix(runID), iteration))
}

// SpecificationRunIDFromInvocationID recovers the run from a specification
// invocation identity. It accepts only the canonical spelling: the iteration
// must be a positive decimal without leading zeros and the whole identifier
// must re-mint from its parts.
func SpecificationRunIDFromInvocationID(id InvocationID) (RunID, bool) {
	raw := string(id)
	prefix := specificationInvocationIDPrefix
	if !strings.HasPrefix(raw, prefix) {
		return "", false
	}
	lastDash := strings.LastIndexByte(raw, '-')
	if lastDash <= len(prefix) || lastDash == len(raw)-1 {
		return "", false
	}
	suffix := raw[lastDash+1:]
	iteration, ok := new(big.Int).SetString(suffix, 10)
	if !ok || iteration.Sign() < 1 || iteration.String() != suffix || !iteration.IsInt64() {
		return "", false
	}
	runID := RunID(raw[len(prefix):lastDash])
	return runID, SpecificationInvocationID(runID, int(iteration.Int64())) == id
}

const specificationDiscussionInvocationIDPrefix = "specification-discussion-"

// SpecificationDiscussionInvocationID names the daemon-owned invocation that
// answers one operator discussion command. Client-generated discuss
// invocation IDs occupy the inv- namespace; this stays disjoint even when a
// client chooses a command ID that resembles a discussion identity.
func SpecificationDiscussionInvocationID(commandID string) InvocationID {
	return InvocationID(specificationDiscussionInvocationIDPrefix + commandID)
}

// SpecificationDiscussionCommandID recovers the command from a discussion
// invocation identity or idempotency key.
func SpecificationDiscussionCommandID(id string) (string, bool) {
	commandID, ok := strings.CutPrefix(id, specificationDiscussionInvocationIDPrefix)
	return commandID, ok && commandID != ""
}

// specificationDiscussionInvocationIDMatches reports whether id names the
// command's discussion invocation.
func specificationDiscussionInvocationIDMatches(id InvocationID, commandID string) bool {
	got, ok := SpecificationDiscussionCommandID(string(id))
	return ok && got == commandID
}

// A specification revision's feedback artifact, an answered discussion's
// artifact, and a specification approval card carry fixed ID prefixes.
// Stored artifacts, attention items, and the revision gates that re-derive
// them embed these bytes, and none has a legacy spelling, so every site that
// mints or matches one goes through these helpers instead of spelling a
// prefix.
const (
	specificationFeedbackArtifactIDPrefix   = "spec-feedback-"
	specificationDiscussionArtifactIDPrefix = "spec-discussion-"
	specificationApprovalItemIDPrefix       = "spec-approval-"
)

// SpecificationFeedbackArtifactID names the feedback artifact one operator
// command (a request_changes or an answer) contributes to a specification
// revision.
func SpecificationFeedbackArtifactID(commandID string) ArtifactID {
	return ArtifactID(specificationFeedbackArtifactIDPrefix + commandID)
}

// HasSpecificationFeedbackArtifactPrefix reports whether id starts with the
// feedback artifact prefix, including the bare prefix.
func HasSpecificationFeedbackArtifactPrefix(id ArtifactID) bool {
	return strings.HasPrefix(string(id), specificationFeedbackArtifactIDPrefix)
}

// SpecificationFeedbackCommandID recovers the command from a feedback
// artifact ID. It rejects the bare prefix, which names no command.
func SpecificationFeedbackCommandID(id ArtifactID) (string, bool) {
	commandID, ok := strings.CutPrefix(string(id), specificationFeedbackArtifactIDPrefix)
	return commandID, ok && commandID != ""
}

// SpecificationDiscussionArtifactID names the artifact that records the
// answer to one operator discussion command. It differs from the discussion
// invocation SpecificationDiscussionInvocationID names for the same command.
func SpecificationDiscussionArtifactID(commandID string) ArtifactID {
	return ArtifactID(specificationDiscussionArtifactIDPrefix + commandID)
}

// HasSpecificationDiscussionArtifactPrefix reports whether id starts with the
// discussion artifact prefix, including the bare prefix.
func HasSpecificationDiscussionArtifactPrefix(id ArtifactID) bool {
	return strings.HasPrefix(string(id), specificationDiscussionArtifactIDPrefix)
}

// SpecificationApprovalItemID names the approval card for one specification
// iteration of an implementation run.
func SpecificationApprovalItemID(implementationRunID RunID, iteration int) ItemID {
	return ItemID(fmt.Sprintf("%s%d", SpecificationApprovalItemIDPrefix(implementationRunID), iteration))
}

// SpecificationApprovalItemIDPrefix is the prefix shared by every approval
// card of the implementation run, ending in the separator before the
// iteration number.
func SpecificationApprovalItemIDPrefix(implementationRunID RunID) string {
	return specificationApprovalItemIDPrefix + string(implementationRunID) + "-"
}

// CutSpecificationApprovalPrefix removes the approval card prefix from s and
// returns what follows. It rejects the bare prefix. It takes a string because
// callers cut both a whole item ID and an item ID whose iteration they have
// already removed.
func CutSpecificationApprovalPrefix(s string) (string, bool) {
	rest, ok := strings.CutPrefix(s, specificationApprovalItemIDPrefix)
	return rest, ok && rest != ""
}
