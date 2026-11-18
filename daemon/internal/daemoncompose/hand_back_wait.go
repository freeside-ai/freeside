package daemoncompose

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// handBackCommentMissingCode is the health diagnostic of the notice a
// handed-back head with no admitted comment raises (issue #3192).
const handBackCommentMissingCode = "hand_back_comment_missing"

// handBackWaitPrefix scopes the notice to the ready item that observed the
// wait, so an older ready item of the same run never resolves the current
// one's notice.
func handBackWaitPrefix(ready domain.ItemID) string {
	digest := sha256.Sum256([]byte(ready))
	return fmt.Sprintf("hand-back-comment-missing-%x-", digest[:])
}

// handBackWaitItemID names one notice per ready item and head.
func handBackWaitItemID(ready domain.ItemID, head string) domain.ItemID {
	return domain.ItemID(handBackWaitPrefix(ready) + head)
}

// convergeHandBackWait brings the hand-back notice for ready in line with
// this pass's gap, nil when the pull request does not wait on a comment. It
// resolves every open notice for another head, or all of them when there is
// no gap, and raises the notice once the gap persists: when the previous
// pass saw the same head wait for the same ready item. The one-pass grace
// covers a lander whose comment arrives just after its label. The memory is
// process-local, so a restart delays a notice by one pass.
//
// The notice is advisory and grants nothing. Its subject is the system, not
// the run: an open run-subject item with a requested decision reads as
// attention_required to run supervision, and a self-resolving notice must
// not end a supervised run (invocation_stall.go).
func (r *ActiveResourceReconciler) convergeHandBackWait(
	ctx context.Context, snapshots []store.Snapshotted[domain.AttentionItem],
	ready domain.AttentionItem, gap *engine.HandBackWait,
) error {
	persists := false
	if gap == nil {
		delete(r.handBackGapSeen, ready.ID)
	} else {
		persists = r.handBackGapSeen[ready.ID] == gap.HeadSHA
		if r.handBackGapSeen == nil {
			r.handBackGapSeen = make(map[domain.ItemID]string)
		}
		r.handBackGapSeen[ready.ID] = gap.HeadSHA
	}
	attention := signet.NewService(r.Store)
	prefix := handBackWaitPrefix(ready.ID)
	for _, snapshot := range snapshots {
		item := snapshot.Value
		if item.Type != domain.AttentionSystemHealth || item.Status != domain.StatusOpen ||
			!strings.HasPrefix(string(item.ID), prefix) ||
			(gap != nil && item.ID == handBackWaitItemID(ready.ID, gap.HeadSHA)) {
			continue
		}
		item, err := item.ConcludeByDaemon(domain.ConcludedByDaemon, r.Now().UTC(),
			"Resolved when a hand-back cycle started, the label came off, or the pull request moved on from this head.", nil)
		if err != nil {
			return err
		}
		item.ItemVersion++
		if err := attention.PutItem(ctx, item); err != nil {
			return err
		}
	}
	if !persists {
		return nil
	}
	itemID := handBackWaitItemID(ready.ID, gap.HeadSHA)
	subject := domain.Subject{Type: domain.SubjectSystem, ID: "daemon"}
	var (
		exists       bool
		displayNames *domain.DisplayNames
	)
	if err := r.Store.Read(ctx, func(tx *store.ReadTx) error {
		// Any status counts, so an acknowledged notice is not raised again
		// for the same head.
		_, err := tx.GetAttentionItem(ctx, itemID)
		switch {
		case err == nil:
			exists = true
			return nil
		case !errors.Is(err, store.ErrNotFound):
			return err
		}
		displayNames, err = tx.DisplayNamesFor(ctx, ready.ProjectID, subject)
		return err
	}); err != nil || exists {
		return err
	}
	missing := "No one has commented on this head."
	if len(gap.Commenters) > 0 {
		missing = fmt.Sprintf("Comments from %s are from accounts the trust profile's external-reviewer allowlist does not admit to drive a round.",
			strings.Join(gap.Commenters, ", "))
	}
	posture := domain.HealthPostureAdvisory
	createdAt := r.Now().UTC()
	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: itemID, ProjectID: ready.ProjectID, Subject: subject,
		Type: domain.AttentionSystemHealth, Priority: domain.PriorityNormal,
		Reason: fmt.Sprintf(
			"Pull request %s#%d carries the hand-back label on head %s, but no admitted account has "+
				"commented on that head, so no hand-back cycle starts. %s This notice resolves itself "+
				"when a hand-back cycle starts on the head or the label comes off.",
			gap.Repo, gap.PRNumber, gap.HeadSHA, missing),
		RequestedDecision: []domain.Action{domain.ActionAcknowledge},
		HealthDiagnostic: &domain.HealthDiagnostic{
			Code: handBackCommentMissingCode, Impairs: domain.ImpairedCapabilityNone,
		},
		DisplayNames: displayNames,
		ItemVersion:  1, InterruptionClass: domain.InterruptionExceptional,
		CreatedAt: &createdAt,
		Posture:   &posture, Status: domain.StatusOpen,
	}, nil)
	if err != nil {
		return err
	}
	return attention.PutItem(ctx, item)
}
