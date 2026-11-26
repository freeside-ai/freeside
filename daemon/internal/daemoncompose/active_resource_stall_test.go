package daemoncompose

import (
	"context"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
)

// TestActiveResourceServesOtherProjectWhileOneStalls: while one project's
// pull request read and its repository's tip read hang on GitHub, the
// reconciler still observes another project's ready item and records its
// repository's tip, on the startup pass and again one interval later, and
// never reads the stalled project or repository a second time (#3210).
func TestActiveResourceServesOtherProjectWhileOneStalls(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		st := schedTestStore(t)
		activeReadyItemIn(t, st, activeReadyItemSeed{
			runID: "run-stalled", itemID: "item-ready-stalled", project: "project-stalled", prNumber: 449, head: "c0ffee00",
		})
		activeReadyItem(t, st)
		stalledWatch := repositoryTipWatch{Repo: "owner/stalled", RepositoryID: tipWatchRepositoryID + 1, Branch: "main"}
		var stalledPulls, servedPulls, stalledTips, servedTips atomic.Int32
		r := ActiveResourceReconciler{
			Store: st,
			Pull: func(ctx context.Context, _ string, number int) (publish.PullObservation, error) {
				if number == 449 {
					stalledPulls.Add(1)
					<-ctx.Done()
					return publish.PullObservation{}, ctx.Err()
				}
				servedPulls.Add(1)
				return exactPull("open", false), nil
			},
			tipWatches: []repositoryTipWatch{stalledWatch, testTipWatch},
			observeTip: func(ctx context.Context, repo, _ string) (string, error) {
				if repo == stalledWatch.Repo {
					stalledTips.Add(1)
					<-ctx.Done()
					return "", ctx.Err()
				}
				servedTips.Add(1)
				return tipA, nil
			},
			upkeepTipAdvance: func(context.Context, domain.RepositoryTipObservation) error { return nil },
			Now:              func() time.Time { return activeResourceTestTime },
		}
		// Seeding the items woke the store's ready-item signal; drain it so
		// the startup pass is the only pass before the first interval.
		select {
		case <-st.ReadyItemCreated():
		default:
		}
		ctx, cancel := context.WithCancel(t.Context())
		ran := make(chan error, 1)
		go func() {
			ran <- r.Run(ctx, DefaultActiveResourceInterval, OperatorActiveResourceInterval, nil)
		}()

		synctest.Wait()
		if servedPulls.Load() != 1 || servedTips.Load() != 1 {
			t.Fatalf("startup pass read %d served pulls and %d served tips; want one each",
				servedPulls.Load(), servedTips.Load())
		}
		if got, found := readTipObservation(t, st); !found || got.TipSHA != tipA {
			t.Fatalf("served tip = %+v (found %t), want %s recorded", got, found, tipA)
		}
		time.Sleep(DefaultActiveResourceInterval)
		synctest.Wait()
		if servedPulls.Load() != 2 || servedTips.Load() != 2 {
			t.Fatalf("after one interval: %d served pulls and %d served tips; want the next pass to read both again",
				servedPulls.Load(), servedTips.Load())
		}
		if stalledPulls.Load() != 1 || stalledTips.Load() != 1 {
			t.Fatalf("stalled reads = %d pulls and %d tips; want the next pass to skip the stalled ones",
				stalledPulls.Load(), stalledTips.Load())
		}
		cancel()
		if err := <-ran; err != nil {
			t.Fatal(err)
		}
	})
}

// TestActiveResourceWakeReachesAProjectBusyAtTheWake: a ready item created
// while a pass is still observing another of the project's items wakes a
// pass that skips the busy project, so the pass that was busy starts one
// more as it returns, and the project is observed again without waiting out
// an interval (#3210).
func TestActiveResourceWakeReachesAProjectBusyAtTheWake(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		st := schedTestStore(t)
		activeReadyItem(t, st)
		select {
		case <-st.ReadyItemCreated():
		default:
		}
		release := make(chan struct{})
		var busyPulls, newPulls atomic.Int32
		r := ActiveResourceReconciler{
			Store: st,
			Pull: func(_ context.Context, _ string, number int) (publish.PullObservation, error) {
				if number == 449 {
					newPulls.Add(1)
					observed := exactPull("open", false)
					observed.Number, observed.HeadSHA = 449, "c0ffee00"
					return observed, nil
				}
				if busyPulls.Add(1) == 1 {
					<-release
				}
				return exactPull("open", false), nil
			},
			Now: func() time.Time { return activeResourceTestTime },
		}
		ctx, cancel := context.WithCancel(t.Context())
		ran := make(chan error, 1)
		go func() {
			ran <- r.Run(ctx, DefaultActiveResourceInterval, OperatorActiveResourceInterval, nil)
		}()
		synctest.Wait()
		activeReadyItemIn(t, st, activeReadyItemSeed{
			runID: "run-woken", itemID: "item-ready-woken", project: "project-2", prNumber: 449, head: "c0ffee00",
		})
		synctest.Wait()
		if busyPulls.Load() != 1 || newPulls.Load() != 1 {
			t.Fatalf("after the wake: %d busy and %d new pulls; want the woken pass to read only the new item",
				busyPulls.Load(), newPulls.Load())
		}
		close(release)
		synctest.Wait()
		if busyPulls.Load() != 2 {
			t.Fatalf("busy project read %d times once its pass returned; want one more read without an interval",
				busyPulls.Load())
		}
		cancel()
		if err := <-ran; err != nil {
			t.Fatal(err)
		}
	})
}
