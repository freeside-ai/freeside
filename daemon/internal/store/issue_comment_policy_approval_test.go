package store_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
)

// policyCommentFixture is one store holding an issue comment proposal on
// issue 41 of repository 123, under a run whose resolved policy carries gate
// as gates.issue_comment (absent when nil), and one admitted instance of it
// with neither a card nor an approval.
type policyCommentFixture struct {
	ledgerFixture
	proposal domain.EffectProposal
	instance domain.ProposalInstance
}

func seedPolicyComment(t *testing.T, ctx context.Context, gate *string) policyCommentFixture {
	t.Helper()
	path := filepath.Join(t.TempDir(), "store.db")
	st := storetest.Open(t, path, store.Options{})
	var extra map[string]string
	if gate != nil {
		extra = map[string]string{string(domain.WorkflowGateIssueComment): *gate}
	}
	fx := seedFilingWith(t, ctx, st, "project-policy-comment", "owner/repo", 123, extra)
	proposal := fx.commentProposal(t, seedComment(t, ctx, st, fx))
	instance, err := allocateFiling(ctx, st, proposal, "event-policy")
	if err != nil {
		t.Fatal(err)
	}
	return policyCommentFixture{
		ledgerFixture: ledgerFixture{path: path, st: st, fx: fx},
		proposal:      proposal, instance: instance,
	}
}

func recordPolicyComment(ctx context.Context, st *store.Store, id domain.ProposalInstanceID) error {
	return st.Write(ctx, func(tx *store.WriteTx) error {
		return tx.RecordPolicyIssueCommentApproval(ctx, id, ledgerAt)
	})
}

func approvedComment(ctx context.Context, st *store.Store, id domain.ProposalInstanceID) (store.ApprovedIssueComment, error) {
	return readLedger(ctx, st, func(tx *store.ReadTx) (store.ApprovedIssueComment, error) {
		return tx.ApprovedIssueComment(ctx, id)
	})
}

// TestPolicyIssueCommentApprovalAuthorizesPosting: with the gate off, a
// policy approval authorizes the comment as a human approve would, a repeat
// writes nothing, and the posted comment stays readable after the Workflow
// layers turn the gate on, because the reader uses the run's pinned policy.
func TestPolicyIssueCommentApprovalAuthorizesPosting(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := seedPolicyComment(t, ctx, nil)
	if _, err := approvedComment(ctx, f.st, f.instance.ID); !errors.Is(err, domain.ErrIssueCommentNotApproved) {
		t.Fatalf("read before approval = %v, want ErrIssueCommentNotApproved", err)
	}
	for range 2 {
		if err := recordPolicyComment(ctx, f.st, f.instance.ID); err != nil {
			t.Fatalf("record policy approval: %v", err)
		}
	}
	approved, err := approvedComment(ctx, f.st, f.instance.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !approved.ByPolicy || approved.RepositoryID != 123 || approved.IssueNumber != 41 || approved.RunID != f.fx.runID {
		t.Fatalf("approved comment = %+v", approved)
	}
	mustLedger(t, ctx, f.st, f.instance.ID, commentOpen, commentSet(), commentAttempt, commentSuccess(9001))
	if err := f.st.Write(ctx, func(tx *store.WriteTx) error {
		_, err := tx.AppendWorkflowPolicyRevision(ctx, domain.WorkflowPolicyInput{
			Scope: domain.WorkflowPolicyScopeGeneral, Key: domain.WorkflowGateIssueComment,
			Value: new("true"), SetBy: "device:device-1",
		}, ledgerAt.Add(time.Hour))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	intent, err := commentIntent(ctx, f.st, f.instance.ID)
	if err != nil {
		t.Fatalf("ledgered comment after the gate turned on: %v", err)
	}
	if intent.Terminal == nil || intent.Terminal.Outcome != domain.FollowUpFilingLedgered {
		t.Fatalf("intent = %+v, want ledgered", intent)
	}
	if posted, err := postedComment(ctx, f.st, 9001); err != nil || posted == nil {
		t.Fatalf("posted comment = %v, %v", posted, err)
	}
}

// TestPolicyIssueCommentApprovalFollowsTheRunGate: an explicit false is off;
// true and a value that does not parse keep a person on the path, and the
// refusal writes nothing.
func TestPolicyIssueCommentApprovalFollowsTheRunGate(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		gate    string
		refused bool
	}{
		{"false", false},
		{"true", true},
		{"sometimes", true},
	} {
		t.Run(tc.gate, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			f := seedPolicyComment(t, ctx, new(tc.gate))
			err := recordPolicyComment(ctx, f.st, f.instance.ID)
			if !tc.refused {
				if err != nil {
					t.Fatalf("record with the gate off: %v", err)
				}
				return
			}
			if !errors.Is(err, store.ErrIssueCommentGateOn) {
				t.Fatalf("record with gate %q = %v, want ErrIssueCommentGateOn", tc.gate, err)
			}
			if _, err := approvedComment(ctx, f.st, f.instance.ID); !errors.Is(err, domain.ErrIssueCommentNotApproved) {
				t.Fatalf("read after a refused approval = %v, want ErrIssueCommentNotApproved", err)
			}
		})
	}
}

// TestPolicyIssueCommentApprovalSecondActor: a policy approval and a person
// never both decide one comment. A card, decided or not, refuses the policy
// approval; a policy approval refuses any decision on a card opened later;
// and that card makes the approval unreadable.
func TestPolicyIssueCommentApprovalSecondActor(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := seedPolicyComment(t, ctx, nil)

	decided, approve := openFilingCard(t, ctx, f.st, f.fx, f.proposal, "event-decided", domain.ActionApprove)
	if err := decideFiling(ctx, f.st, decided, approve); err != nil {
		t.Fatal(err)
	}
	undecided, _ := openFilingCard(t, ctx, f.st, f.fx, f.proposal, "event-undecided", domain.ActionApprove)
	for _, instance := range []domain.ProposalInstance{decided, undecided} {
		if err := recordPolicyComment(ctx, f.st, instance.ID); !errors.Is(err, store.ErrIssueCommentApprovalActorConflict) {
			t.Fatalf("policy approval of %q beside a card = %v, want ErrIssueCommentApprovalActorConflict", instance.ID, err)
		}
	}

	if err := recordPolicyComment(ctx, f.st, f.instance.ID); err != nil {
		t.Fatal(err)
	}
	for _, action := range []domain.Action{domain.ActionApprove, domain.ActionDecline} {
		var command domain.Command
		if err := f.st.Write(ctx, func(tx *store.WriteTx) error {
			var err error
			command, err = filingItem(ctx, tx, f.fx, f.instance, action, nil)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if err := decideFiling(ctx, f.st, f.instance, command); !errors.Is(err, store.ErrIssueCommentApprovalActorConflict) {
			t.Fatalf("%s on a policy-approved comment = %v, want ErrIssueCommentApprovalActorConflict", action, err)
		}
	}
	if _, err := approvedComment(ctx, f.st, f.instance.ID); !errors.Is(err, store.ErrRowInconsistent) {
		t.Fatalf("policy approval beside a card = %v, want ErrRowInconsistent", err)
	}
}

// TestPolicyIssueCommentApprovalFailsClosed: a stored policy approval
// authorizes nothing once it disagrees with what it binds, beside a recorded
// decision, or under a run policy that turns the gate on.
func TestPolicyIssueCommentApprovalFailsClosed(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		gate   *string
		tamper func(f policyCommentFixture) string
	}{
		{"proposal digest", nil, func(f policyCommentFixture) string {
			return `UPDATE issue_comment_policy_approvals SET proposal_digest = 'sha256:` +
				"0000000000000000000000000000000000000000000000000000000000000000" + `' WHERE instance_id = '` +
				string(f.instance.ID) + `'`
		}},
		{"policy digest", nil, func(f policyCommentFixture) string {
			return `UPDATE issue_comment_policy_approvals SET policy_digest = 'sha256:` +
				"0000000000000000000000000000000000000000000000000000000000000000" + `' WHERE instance_id = '` +
				string(f.instance.ID) + `'`
		}},
		{"beside a decision", nil, func(f policyCommentFixture) string {
			return `INSERT INTO effect_proposal_decisions (instance_id, command_id, action, decided_at)
				VALUES ('` + string(f.instance.ID) + `', 'command-forged', 'decline', '2026-11-13T00:00:00Z')`
		}},
		{"gate on", new("true"), func(f policyCommentFixture) string {
			return `INSERT INTO issue_comment_policy_approvals (instance_id, proposal_digest, policy_digest, approved_at)
				VALUES ('` + string(f.instance.ID) + `', '` + string(f.instance.Proposal.Digest) + `', '` +
				string(f.fx.policy.Digest) + `', '2026-11-13T00:00:00Z')`
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			f := seedPolicyComment(t, ctx, tc.gate)
			if tc.gate == nil {
				if err := recordPolicyComment(ctx, f.st, f.instance.ID); err != nil {
					t.Fatal(err)
				}
			}
			statement := tc.tamper(f)
			f.raw(t, func(raw *sql.DB) {
				if _, err := raw.Exec(`PRAGMA foreign_keys = OFF`); err != nil {
					t.Fatal(err)
				}
				result, err := raw.Exec(statement)
				if err != nil {
					t.Fatalf("tamper: %v", err)
				}
				if changed, err := result.RowsAffected(); err != nil || changed != 1 {
					t.Fatalf("tamper changed %d rows, %v", changed, err)
				}
			})
			if _, err := approvedComment(ctx, f.st, f.instance.ID); !errors.Is(err, store.ErrRowInconsistent) {
				t.Fatalf("tampered policy approval = %v, want ErrRowInconsistent", err)
			}
			if tc.name == "policy digest" {
				// A repeat never accepts a row bound to something else.
				if err := recordPolicyComment(ctx, f.st, f.instance.ID); !errors.Is(err, store.ErrRowInconsistent) {
					t.Fatalf("repeat over a tampered binding = %v, want ErrRowInconsistent", err)
				}
			}
			if err := tryLedger(ctx, f.st, f.instance.ID, commentOpen); err == nil {
				t.Fatal("a tampered policy approval opened an intent")
			}
		})
	}
}
