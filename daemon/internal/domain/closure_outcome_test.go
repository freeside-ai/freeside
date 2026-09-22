package domain_test

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/golden"
)

// closureOutcomeParams builds a structurally valid closure proposal parameter
// for the outcome matrix. A resolving daemon_fallback is deliberately not
// filtered here: it is an invalid proposal, and the walk below skips it exactly
// as ClosureOutcomeFor rejects it.
func closureOutcomeParams(
	provenance domain.ClosureProvenance, resolves bool, origin domain.ClosureFlagOrigin,
) domain.SourceIssueClosureParameters {
	return domain.SourceIssueClosureParameters{
		SubjectHandle: "subject-opaque-1",
		Target:        domain.IssueSubjectRef{Repo: "octo/repo", RepositoryID: 42, IssueNumber: 7},
		Resolves:      resolves,
		Provenance:    provenance,
		Origin:        origin,
	}
}

// coreCell keys the outcome table over the four dimensions that decide the
// reference, hold, and item. Provenance is orthogonal (it only sets Recommended)
// and so is not part of the key; the test runs every cell under both
// provenances and checks Recommended separately.
type coreCell struct {
	resolves bool
	origin   domain.ClosureFlagOrigin
	gate     bool
	approval domain.ClosureApprovalState
}

const (
	ps = domain.ClosureFlagOriginProposeSite
	df = domain.ClosureFlagOriginDaemonFallback

	none     = domain.ClosureApprovalStateNone
	declined = domain.ClosureApprovalStateDeclined
	human    = domain.ClosureApprovalStateHuman
	policy   = domain.ClosureApprovalStatePolicy
)

// closureOutcomeCells is the truth table the plan pins (issue #1487 matrix plus
// the cells its "Settled in planning" section spells out), keyed over the four
// deciding dimensions. Every valid (provenance × resolves × origin × gate ×
// approval) input maps here; the completeness walk fails if any lacks a row.
// A resolving daemon_fallback is invalid, so no df row carries resolves=true.
var closureOutcomeCells = map[coreCell]struct {
	reference domain.ClosureReference
	hold      domain.ClosureHold
	item      domain.ClosureItem
}{
	// propose_site, gate off: an approval that binds the current merge closes when
	// the proposal resolves (whoever recorded it); everything else references.
	{true, ps, false, none}:      {domain.ClosureReferenceRefs, domain.ClosureHoldNone, domain.ClosureItemNone},
	{true, ps, false, declined}:  {domain.ClosureReferenceRefs, domain.ClosureHoldNone, domain.ClosureItemNone},
	{true, ps, false, human}:     {domain.ClosureReferenceCloses, domain.ClosureHoldNone, domain.ClosureItemNone},
	{true, ps, false, policy}:    {domain.ClosureReferenceCloses, domain.ClosureHoldNone, domain.ClosureItemNone},
	{false, ps, false, none}:     {domain.ClosureReferenceRefs, domain.ClosureHoldNone, domain.ClosureItemNone},
	{false, ps, false, declined}: {domain.ClosureReferenceRefs, domain.ClosureHoldNone, domain.ClosureItemNone},
	{false, ps, false, human}:    {domain.ClosureReferenceRefs, domain.ClosureHoldNone, domain.ClosureItemNone},
	{false, ps, false, policy}:   {domain.ClosureReferenceRefs, domain.ClosureHoldNone, domain.ClosureItemNone},

	// propose_site, gate on: an undecided merge is held as a planned gate; a
	// decision (or a bound approval) releases it, and closes when it resolves.
	{true, ps, true, none}:      {domain.ClosureReferenceRefs, domain.ClosureHoldDraft, domain.ClosureItemPlannedGate},
	{true, ps, true, declined}:  {domain.ClosureReferenceRefs, domain.ClosureHoldNone, domain.ClosureItemNone},
	{true, ps, true, human}:     {domain.ClosureReferenceCloses, domain.ClosureHoldNone, domain.ClosureItemNone},
	{true, ps, true, policy}:    {domain.ClosureReferenceCloses, domain.ClosureHoldNone, domain.ClosureItemNone},
	{false, ps, true, none}:     {domain.ClosureReferenceRefs, domain.ClosureHoldDraft, domain.ClosureItemPlannedGate},
	{false, ps, true, declined}: {domain.ClosureReferenceRefs, domain.ClosureHoldNone, domain.ClosureItemNone},
	{false, ps, true, human}:    {domain.ClosureReferenceRefs, domain.ClosureHoldNone, domain.ClosureItemNone},
	{false, ps, true, policy}:   {domain.ClosureReferenceRefs, domain.ClosureHoldNone, domain.ClosureItemNone},

	// daemon_fallback, gate off: an unacted fallback shows the non-holding notice;
	// once a person acts (decline, or a human approve on the notice) it is gone.
	// A policy state cannot arise here (the store refuses recording it against a
	// fallback); the cell exists so the function stays total, and references.
	{false, df, false, none}:     {domain.ClosureReferenceRefs, domain.ClosureHoldNone, domain.ClosureItemFallbackNotice},
	{false, df, false, declined}: {domain.ClosureReferenceRefs, domain.ClosureHoldNone, domain.ClosureItemNone},
	{false, df, false, human}:    {domain.ClosureReferenceRefs, domain.ClosureHoldNone, domain.ClosureItemNone},
	{false, df, false, policy}:   {domain.ClosureReferenceRefs, domain.ClosureHoldNone, domain.ClosureItemNone},

	// daemon_fallback, gate on: the gate holds the undecided merge as a planned
	// gate just as it does a propose_site one; a decision releases it.
	{false, df, true, none}:     {domain.ClosureReferenceRefs, domain.ClosureHoldDraft, domain.ClosureItemPlannedGate},
	{false, df, true, declined}: {domain.ClosureReferenceRefs, domain.ClosureHoldNone, domain.ClosureItemNone},
	{false, df, true, human}:    {domain.ClosureReferenceRefs, domain.ClosureHoldNone, domain.ClosureItemNone},
	{false, df, true, policy}:   {domain.ClosureReferenceRefs, domain.ClosureHoldNone, domain.ClosureItemNone},
}

// TestClosureOutcomeForCells checks every listed cell under both provenances,
// and asserts Recommended tracks provenance without touching the other three
// facts. It also collects the reachable reference/hold/item values so the
// registration check below can prove every enum member is produced.
func TestClosureOutcomeForCells(t *testing.T) {
	seenRef := map[domain.ClosureReference]bool{}
	seenHold := map[domain.ClosureHold]bool{}
	seenItem := map[domain.ClosureItem]bool{}

	for cell, want := range closureOutcomeCells {
		for _, provenance := range domain.AllClosureProvenances {
			params := closureOutcomeParams(provenance, cell.resolves, cell.origin)
			got, err := domain.ClosureOutcomeFor(domain.ClosureOutcomeInput{
				Proposal: &params, HumanGate: cell.gate, Approval: cell.approval,
			})
			if err != nil {
				t.Fatalf("cell %+v provenance %q: unexpected error %v", cell, provenance, err)
			}
			wantOutcome := domain.ClosureOutcome{
				Reference: want.reference, Hold: want.hold, Item: want.item,
				Recommended: provenance == domain.ClosureProvenanceRecommended,
			}
			if got != wantOutcome {
				t.Fatalf("cell %+v provenance %q: outcome = %+v, want %+v", cell, provenance, got, wantOutcome)
			}
			seenRef[got.Reference] = true
			seenHold[got.Hold] = true
			seenItem[got.Item] = true
		}
	}

	// The no-source row: a nil proposal is a descriptive link with no hold and no
	// item, whatever the approval state (row 7 of the matrix).
	for _, approval := range domain.AllClosureApprovalStates {
		got, err := domain.ClosureOutcomeFor(domain.ClosureOutcomeInput{Proposal: nil, Approval: approval})
		if err != nil {
			t.Fatalf("nil proposal approval %q: unexpected error %v", approval, err)
		}
		want := domain.ClosureOutcome{
			Reference: domain.ClosureReferenceDescriptiveLink, Hold: domain.ClosureHoldNone, Item: domain.ClosureItemNone,
		}
		if got != want {
			t.Fatalf("nil proposal approval %q: outcome = %+v, want %+v", approval, got, want)
		}
		seenRef[got.Reference] = true
		seenHold[got.Hold] = true
		seenItem[got.Item] = true
	}

	// Every registered outcome value is reachable, so the table exercises the
	// whole vocabulary and a new enum member cannot go unproduced.
	for _, r := range domain.AllClosureReferences {
		if !seenRef[r] {
			t.Errorf("reference %q produced by no cell", r)
		}
	}
	for _, h := range domain.AllClosureHolds {
		if !seenHold[h] {
			t.Errorf("hold %q produced by no cell", h)
		}
	}
	for _, i := range domain.AllClosureItems {
		if !seenItem[i] {
			t.Errorf("item %q produced by no cell", i)
		}
	}
}

// TestClosureOutcomeForCompleteness walks every (provenance × resolves × origin
// × gate × approval) input, skips the ones that fail Validate (a resolving
// fallback), and fails on any valid input the table does not list. This is the
// guard the plan asks for: an enum member that gains a value with no row makes
// the walk produce a valid input with no cell, and the test fails.
func TestClosureOutcomeForCompleteness(t *testing.T) {
	for _, provenance := range domain.AllClosureProvenances {
		for _, resolves := range []bool{true, false} {
			for _, origin := range domain.AllClosureFlagOrigins {
				for _, gate := range []bool{true, false} {
					for _, approval := range domain.AllClosureApprovalStates {
						params := closureOutcomeParams(provenance, resolves, origin)
						if params.Validate() != nil {
							continue // an invalid proposal has no cell by design.
						}
						cell := coreCell{resolves: resolves, origin: origin, gate: gate, approval: approval}
						if _, ok := closureOutcomeCells[cell]; !ok {
							t.Errorf("valid input has no listed cell: %+v provenance %q", cell, provenance)
						}
					}
				}
			}
		}
	}
}

// TestClosureOutcomeForErrors proves the two error inputs: an invalid approval
// state, and an invalid proposal (a resolving daemon_fallback).
func TestClosureOutcomeForErrors(t *testing.T) {
	params := closureOutcomeParams(domain.ClosureProvenanceVerified, true, ps)
	if _, err := domain.ClosureOutcomeFor(domain.ClosureOutcomeInput{
		Proposal: &params, Approval: "bogus",
	}); !errors.Is(err, domain.ErrClosureOutcomeInconsistent) {
		t.Fatalf("invalid approval error = %v, want ErrClosureOutcomeInconsistent", err)
	}

	resolvingFallback := closureOutcomeParams(domain.ClosureProvenanceVerified, true, df)
	if _, err := domain.ClosureOutcomeFor(domain.ClosureOutcomeInput{
		Proposal: &resolvingFallback, Approval: none,
	}); !errors.Is(err, domain.ErrEffectProposalInconsistent) {
		t.Fatalf("resolving fallback error = %v, want ErrEffectProposalInconsistent", err)
	}
}

// TestClosureOutcomeGolden pins one outcome's JSON rendering: a recommended,
// policy-approved resolving closure, which exercises the closes reference and
// the recommended marker together.
func TestClosureOutcomeGolden(t *testing.T) {
	params := closureOutcomeParams(domain.ClosureProvenanceRecommended, true, ps)
	outcome, err := domain.ClosureOutcomeFor(domain.ClosureOutcomeInput{
		Proposal: &params, HumanGate: false, Approval: policy,
	})
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.MarshalIndent(outcome, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	golden.Assert(t, "closure_outcome", append(body, '\n'))
}
