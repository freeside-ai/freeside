package publish

import (
	"context"
	"fmt"
	"strconv"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/publicationtext"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// The publisher-owned source-issue reference section (#1419 Part D, plan
// revision 68). The publisher, not the candidate prose, writes how a merged
// pull request refers to its source issue: Closes/Refs an issue number, or a
// descriptive link for a cross-repository source. The reference is a trust
// decision the publisher re-derives from durable state, never a bit the caller
// hands it: the candidate carries only the closure proposal's instance id, and
// the publisher reads the instance, the approval (whichever actor recorded it),
// the open item, and the run's resolved-policy gate mode, builds the merge it
// observes now, and asks the domain closure-outcome matrix
// (domain.ClosureOutcomeFor). A stale approval (one AuthorizesClose rejects for
// the current merge) closes nothing.
const (
	sourceReferenceMarkerName       = publicationtext.SourceReferenceMarkerName
	sourceReferenceOpenMarker       = "<!-- " + sourceReferenceMarkerName + " -->"
	sourceReferenceCloseMarker      = "<!-- /" + sourceReferenceMarkerName + " -->"
	sourceReferenceHeading          = publicationtext.SourceReferenceHeading
	maxRenderedSourceReferenceBytes = publicationtext.MaxRenderedSourceReferenceBytes
)

// policySourceIssueClosureGate names the resolved-policy switch that turns on the
// human gate for source-issue closures. Absent or false selects the default,
// policy-approval mode; an explicit true selects the human gate. The key lives
// under the gates. convention beside gates.spec_approval. Parsing is duplicated
// in the engine (engine.parseSourceIssueClosureHumanGate): the publisher reads
// the mode from the run's stored policy itself rather than trusting a caller
// field, and the domain-change ban keeps the constant out of a shared package.
const policySourceIssueClosureGate = "gates.source_issue_closure"

// parseSourceIssueClosureHumanGate reads the closure human-gate switch from a
// run's resolved policy. A malformed value fails closed to the human gate, so a
// configuration typo never becomes an unreviewed automatic close.
func parseSourceIssueClosureHumanGate(policy domain.ResolvedPolicy) bool {
	for _, k := range policy.Keys {
		if k.Key == policySourceIssueClosureGate {
			value, err := strconv.ParseBool(k.Value)
			if err != nil {
				return true
			}
			return value
		}
	}
	return false
}

func containsSourceReferenceMarker(body string) bool {
	return publicationtext.ContainsSourceReferenceMarker(body)
}

// closureResolution is the publisher's reduced view of a candidate's source
// issue: the outcome the matrix resolved (reference, hold, recommendation), the
// issue number a Closes/Refs names, and the descriptive-link URL for a source
// with no proposal. managed reports whether the publisher owns the pull
// request's draft state: only a gate-on closable source is managed, so a
// default-policy pull request opens mergeable and is never forced draft or
// ready by the publisher (plan revision 68).
type closureResolution struct {
	outcome   domain.ClosureOutcome
	target    int
	sourceURL string
	managed   bool
}

// resolveClosure reduces a candidate's closure state to a closureResolution. The
// reads are self-validating (GetProposalInstance re-runs the closable-source
// gate, AuthorizesClose re-checks the current merge), so a dedicated read
// transaction here preserves the trust boundary the same way the gate
// transaction would; the composed body is fixed before the gate transaction
// runs, so the reference must resolve here, ahead of it.
func (p *Publisher) resolveClosure(ctx context.Context, c Candidate, identity Identity) (closureResolution, error) {
	if c.ClosureInstanceID == "" {
		// No proposal: a cross-repository or absent source. The matrix yields a
		// descriptive link with no hold and no item; an empty URL renders nothing.
		outcome, err := domain.ClosureOutcomeFor(domain.ClosureOutcomeInput{
			Approval: domain.ClosureApprovalStateNone,
		})
		if err != nil {
			return closureResolution{}, fmt.Errorf("resolve closure: %w", err)
		}
		return closureResolution{outcome: outcome, sourceURL: c.SourceIssueURL}, nil
	}
	var res closureResolution
	err := p.storeDecision.store.Read(ctx, func(tx *store.ReadTx) error {
		policy, err := tx.GetResolvedPolicy(ctx, c.RunID)
		if err != nil {
			return fmt.Errorf("resolve closure policy: %w", err)
		}
		humanGate := parseSourceIssueClosureHumanGate(policy)
		instance, err := tx.GetProposalInstance(ctx, c.ClosureInstanceID)
		if err != nil {
			return fmt.Errorf("resolve closure instance: %w", err)
		}
		closure := instance.Proposal.ClosureProposal
		if instance.Proposal.Kind != domain.EffectSourceIssueClosure || closure == nil {
			return fmt.Errorf("resolve closure: instance %q is not a source-issue closure: %w",
				c.ClosureInstanceID, ErrUnauthorizedPublication)
		}
		// The instance id is a caller-supplied pointer on an exported struct and a
		// bit the engine decoded from an untrusted stored checkpoint; neither is
		// authority. Re-bind it to this run before it can authorize a Closes, so a
		// foreign or spoofed instance withholds the close rather than closing
		// another run's issue (the store re-gate below checks that the source is
		// still closable, not that the instance belongs to this work unit).
		if closure.SubjectHandle != domain.OpaqueSubjectHandle(domain.WorkUnitIDForRun(c.RunID)) {
			return fmt.Errorf("resolve closure: instance %q is bound to another work unit: %w",
				c.ClosureInstanceID, ErrUnauthorizedPublication)
		}
		approval, err := tx.ClosureApprovalForInstance(ctx, c.ClosureInstanceID)
		if err != nil {
			return fmt.Errorf("resolve closure approval: %w", err)
		}
		_, openMerge, openFound, err := tx.OpenEffectItemForInstance(ctx, c.ClosureInstanceID)
		if err != nil {
			return fmt.Errorf("resolve closure open item: %w", err)
		}
		merge := domain.ProspectiveMerge{
			PublicationIdentity: identity.Digest(),
			CandidateHeadSHA:    c.HeadSHA,
			BaseRef:             c.BaseRef,
			BaseSHA:             c.BaseSHA,
		}
		state := reduceClosureApproval(approval, instance.Proposal, merge, openFound, openMerge)
		outcome, err := domain.ClosureOutcomeFor(domain.ClosureOutcomeInput{
			Proposal: closure, HumanGate: humanGate, Approval: state,
		})
		if err != nil {
			return fmt.Errorf("resolve closure outcome: %w", err)
		}
		res = closureResolution{
			outcome:   outcome,
			target:    closure.Target.IssueNumber,
			sourceURL: c.SourceIssueURL,
			managed:   humanGate,
		}
		return nil
	})
	if err != nil {
		return closureResolution{}, err
	}
	return res, nil
}

// reduceClosureApproval collapses the durable approval and open-item state to the
// single ClosureApprovalState the matrix judges. An approval binds the current
// merge only when AuthorizesClose accepts it; a stale approval (a moved head or
// base) is reduced to no approval. With no binding approval, an item still open
// for the current merge means the decision is pending (none); anything else
// means the merge was acted on without an authorizing approval, or no item holds
// it (declined), which never holds the pull request.
func reduceClosureApproval(
	approval *domain.ClosureApproval,
	proposal domain.EffectProposal,
	merge domain.ProspectiveMerge,
	openFound bool,
	openMerge *domain.ProspectiveMerge,
) domain.ClosureApprovalState {
	if approval != nil && approval.AuthorizesClose(proposal, merge) {
		switch approval.Actor {
		case domain.ClosureApprovalActorPolicy:
			return domain.ClosureApprovalStatePolicy
		case domain.ClosureApprovalActorHuman:
			return domain.ClosureApprovalStateHuman
		}
	}
	if openFound && openMerge != nil && *openMerge == merge {
		return domain.ClosureApprovalStateNone
	}
	return domain.ClosureApprovalStateDeclined
}

// renderSourceReference renders the publisher-owned source-issue section from a
// resolved closure, or "" when there is no reference to write (a descriptive
// link with no source URL). The switch dispatches on the reference kind and
// omits default; the trailing error guards the invalid zero value the matrix
// never returns.
func renderSourceReference(res closureResolution) (string, error) {
	// The zero value carries no reference: a candidate with no closure instance
	// and no descriptive source (a v1 record) renders no section.
	if res.outcome.Reference == "" {
		return "", nil
	}
	var line string
	switch res.outcome.Reference {
	case domain.ClosureReferenceCloses:
		line = fmt.Sprintf("Closes #%d", res.target)
		if res.outcome.Recommended {
			line += "\n\nThe client recommended this issue; the approver confirmed it."
		}
	case domain.ClosureReferenceRefs:
		line = fmt.Sprintf("Refs #%d", res.target)
	case domain.ClosureReferenceDescriptiveLink:
		if res.sourceURL == "" {
			return "", nil
		}
		line = "Source issue: " + res.sourceURL
	default:
		return "", fmt.Errorf("invalid closure reference %q", res.outcome.Reference)
	}
	section := fmt.Sprintf("%s\n\n%s\n\n%s\n\n%s",
		sourceReferenceOpenMarker, sourceReferenceHeading, line, sourceReferenceCloseMarker)
	if len(section) > maxRenderedSourceReferenceBytes {
		return "", fmt.Errorf("source reference section exceeds %d bytes", maxRenderedSourceReferenceBytes)
	}
	return section, nil
}
