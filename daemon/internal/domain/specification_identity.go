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
// every invocation from the run. Two identifier families exist. A run minted
// before the rename (#986) keeps its original prefix forever, together with
// the stage and invocation identifiers derived from it, because outbox and
// inbox idempotency keys, attention item subjects, and backup manifests
// embed those bytes. The family is a pure function of the run ID prefix, so
// every site that mints or parses a derived identifier goes through these
// helpers instead of spelling a prefix.
const (
	specificationRunIDPrefix        = "run-specification-"
	specificationStageIDPrefix      = "specify-"
	specificationInvocationIDPrefix = "inv-specify-"
	specificationRunSeed            = "freeside.specification-run/v1"
)

// SpecificationRunIDForImplementation derives the specification run identity
// from the operator-visible implementation identity. A run derived before the
// rename has the legacy family's identity instead; see
// LegacySpecificationRunIDForImplementation.
func SpecificationRunIDForImplementation(implementationRunID RunID) RunID {
	return derivedSpecificationRunID(specificationRunSeed, specificationRunIDPrefix, implementationRunID)
}

func derivedSpecificationRunID(seed, prefix string, implementationRunID RunID) RunID {
	sum := sha256.Sum256([]byte(seed + "\x00" + string(implementationRunID)))
	return RunID(prefix + hex.EncodeToString(sum[:]))
}

// SpecificationRunIDMatchesImplementation reports whether the specification
// run identity is the one derived from the implementation run in either
// identifier family.
func SpecificationRunIDMatchesImplementation(specificationRunID, implementationRunID RunID) bool {
	return specificationRunID == SpecificationRunIDForImplementation(implementationRunID) ||
		specificationRunID == LegacySpecificationRunIDForImplementation(implementationRunID)
}

// SpecificationStageID names the single stage of a specification run.
func SpecificationStageID(runID RunID) StageID {
	if LegacySpecificationRun(runID) {
		return StageID(legacySpecificationStageIDPrefix + string(runID))
	}
	return StageID(specificationStageIDPrefix + string(runID))
}

// SpecificationInvocationIDPrefix is the prefix shared by every invocation of
// the run, ending in the separator before the iteration number.
func SpecificationInvocationIDPrefix(runID RunID) string {
	if LegacySpecificationRun(runID) {
		return legacySpecificationInvocationIDPrefix + string(runID) + "-"
	}
	return specificationInvocationIDPrefix + string(runID) + "-"
}

// SpecificationInvocationID names one iteration of the run's specification
// invocation.
func SpecificationInvocationID(runID RunID, iteration int) InvocationID {
	return InvocationID(fmt.Sprintf("%s%d", SpecificationInvocationIDPrefix(runID), iteration))
}

// SpecificationRunIDFromInvocationID recovers the run from a specification
// invocation identity in either family. It accepts only the canonical
// spelling: the iteration must be a positive decimal without leading zeros
// and the whole identifier must re-mint from its parts.
func SpecificationRunIDFromInvocationID(id InvocationID) (RunID, bool) {
	raw := string(id)
	var prefix string
	switch {
	case strings.HasPrefix(raw, specificationInvocationIDPrefix):
		prefix = specificationInvocationIDPrefix
	case strings.HasPrefix(raw, legacySpecificationInvocationIDPrefix):
		prefix = legacySpecificationInvocationIDPrefix
	default:
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
// invocation identity or idempotency key in either family.
func SpecificationDiscussionCommandID(id string) (string, bool) {
	if commandID, ok := strings.CutPrefix(id, specificationDiscussionInvocationIDPrefix); ok {
		return commandID, commandID != ""
	}
	commandID, ok := strings.CutPrefix(id, legacySpecificationDiscussionInvocationIDPrefix)
	return commandID, ok && commandID != ""
}

// specificationDiscussionInvocationIDMatches reports whether id names the
// command's discussion invocation in either family.
func specificationDiscussionInvocationIDMatches(id InvocationID, commandID string) bool {
	got, ok := SpecificationDiscussionCommandID(string(id))
	return ok && got == commandID
}
