package engine

import (
	"errors"
	"fmt"
)

// DurableTransition names a production workflow boundary whose before/after
// crash behavior is exercised by the deterministic restart matrix. The list is
// intentionally closed so a new boundary has one registration point.
type DurableTransition string

const (
	DurableTransitionElaborationOutcome    DurableTransition = "elaboration_outcome"
	DurableTransitionSpecificationApproval DurableTransition = "specification_approval"
	DurableTransitionVerificationEvidence  DurableTransition = "verification_evidence"
	DurableTransitionReviewRequest         DurableTransition = "review_request"
	DurableTransitionReviewResult          DurableTransition = "review_result"
	DurableTransitionFindingAdjudication   DurableTransition = "finding_adjudication"
	DurableTransitionPublicationEffect     DurableTransition = "publication_effect"
	DurableTransitionReadyItem             DurableTransition = "ready_attention_item"
	DurableTransitionTerminalCompletion    DurableTransition = "terminal_completion"
)

// AllDurableTransitions is the production engine's crash-matrix registry.
// Stage-driver seed/export boundaries and atomic AttentionItem resolution have
// sibling registries in their owning test suites.
var AllDurableTransitions = []DurableTransition{
	DurableTransitionElaborationOutcome,
	DurableTransitionSpecificationApproval,
	DurableTransitionVerificationEvidence,
	DurableTransitionReviewRequest,
	DurableTransitionReviewResult,
	DurableTransitionFindingAdjudication,
	DurableTransitionPublicationEffect,
	DurableTransitionReadyItem,
	DurableTransitionTerminalCompletion,
}

func (transition DurableTransition) valid() bool {
	switch transition {
	case DurableTransitionElaborationOutcome,
		DurableTransitionSpecificationApproval,
		DurableTransitionVerificationEvidence,
		DurableTransitionReviewRequest,
		DurableTransitionReviewResult,
		DurableTransitionFindingAdjudication,
		DurableTransitionPublicationEffect,
		DurableTransitionReadyItem,
		DurableTransitionTerminalCompletion:
		return true
	default:
		return false
	}
}

// DurableTransitionSide identifies which side of the durable boundary loses
// the process.
type DurableTransitionSide string

const (
	DurableTransitionBefore DurableTransitionSide = "before"
	DurableTransitionAfter  DurableTransitionSide = "after"
)

// AllDurableTransitionSides is the crash matrix's side registry.
var AllDurableTransitionSides = []DurableTransitionSide{
	DurableTransitionBefore,
	DurableTransitionAfter,
}

func (side DurableTransitionSide) valid() bool {
	switch side {
	case DurableTransitionBefore, DurableTransitionAfter:
		return true
	default:
		return false
	}
}

// DurableTransitionHook is a nil-default fault-injection seam. Production
// composition leaves it nil; tests return an error to model process loss.
type DurableTransitionHook func(DurableTransition, DurableTransitionSide) error

func runDurableTransitionHook(
	hook DurableTransitionHook,
	transition DurableTransition,
	side DurableTransitionSide,
) error {
	if !transition.valid() || !side.valid() {
		return fmt.Errorf("invalid durable transition boundary %q/%q", transition, side)
	}
	if hook == nil {
		return nil
	}
	if err := hook(transition, side); err != nil {
		return fmt.Errorf("%s %s: %w", transition, side,
			errors.Join(err, errProductionCrashSeam))
	}
	return nil
}
