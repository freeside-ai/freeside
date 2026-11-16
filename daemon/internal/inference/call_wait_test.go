package inference_test

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/advisory"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/inference"
	"github.com/freeside-ai/freeside/daemon/internal/inference/fake"
)

const (
	waitDiscussionOutput = `{"reply":"answered"}`
	waitClassifierOutput = `{"materiality":"medium","confidence":"high","note":"answered"}`
)

// gateDriver answers each call when release closes, or after delay when it
// is set, and counts the calls it runs at once per site.
type gateDriver struct {
	release chan struct{}
	// delay, when set, answers after it instead of waiting for release, and
	// honors the call's context.
	delay time.Duration
	// blocks names the sites that wait; a site not in it answers at once.
	blocks map[string]bool

	mu      sync.Mutex
	running map[string]int
	peak    map[string]int
	calls   int
}

func newGateDriver(blocks ...string) *gateDriver {
	d := &gateDriver{
		release: make(chan struct{}), blocks: map[string]bool{},
		running: map[string]int{}, peak: map[string]int{},
	}
	for _, site := range blocks {
		d.blocks[site] = true
	}
	return d
}

func (d *gateDriver) Complete(ctx context.Context, request inference.Request, _ inference.Secret) (inference.Response, error) {
	d.mu.Lock()
	d.calls++
	d.running[request.SiteID]++
	d.peak[request.SiteID] = max(d.peak[request.SiteID], d.running[request.SiteID])
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		d.running[request.SiteID]--
		d.mu.Unlock()
	}()
	if d.blocks[request.SiteID] {
		if d.delay > 0 {
			select {
			case <-time.After(d.delay):
			case <-ctx.Done():
				return inference.Response{}, ctx.Err()
			}
		} else {
			<-d.release
		}
	}
	output := waitDiscussionOutput
	if request.SiteID == inference.ClassifierSiteID {
		output = waitClassifierOutput
	}
	return inference.Response{Output: []byte(output), ComputeUnits: 1}, nil
}

func (d *gateDriver) snapshot(site string) (running, peak, calls int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.running[site], d.peak[site], d.calls
}

func waitClient(t *testing.T, roles fake.Roles, sites ...inference.Site) (*inference.Client, string) {
	t.Helper()
	dir := t.TempDir()
	store, err := advisory.Open(filepath.Join(dir, "advisory.json"), 20, 16<<10)
	if err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(dir, "ledger.json")
	client, err := inference.New(inference.Config{
		StatePath: statePath, Roles: roles, Sites: sites, Advisory: store,
	})
	if err != nil {
		t.Fatal(err)
	}
	return client, statePath
}

type waitResult struct {
	result inference.CallResult
	err    error
}

func startCall(ctx context.Context, client *inference.Client, site inference.Site, project string) <-chan waitResult {
	done := make(chan waitResult, 1)
	go func() {
		result, err := client.Call(ctx, site.ID, project, "run-"+project, siteFields(site))
		done <- waitResult{result: result, err: err}
	}()
	return done
}

func answered(t *testing.T, name string, got waitResult) {
	t.Helper()
	if got.err != nil || got.result.Fallback || string(got.result.Output) == "" {
		t.Fatalf("%s: %+v, %v; want an answer", name, got.result, got.err)
	}
}

// TestBusySiteWaitsAtThePoolLimit: two tasks that call one site at once both
// get an answer and neither falls back. At a limit of one the second runs
// after the first; at two they run at once.
func TestBusySiteWaitsAtThePoolLimit(t *testing.T) {
	for _, limit := range []int{1, 2} {
		t.Run(map[int]string{1: "limit one", 2: "limit two"}[limit], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				site := inference.DiscussionSite(testBudget(10))
				driver := newGateDriver(site.ID)
				client, statePath := waitClient(t, fake.Roles{
					Provider: "fake", Model: "test", Driver: driver, MaxCallsPerSite: limit,
				}, site)
				first := startCall(t.Context(), client, site, "project-a")
				second := startCall(t.Context(), client, site, "project-b")
				synctest.Wait()
				if running, _, _ := driver.snapshot(site.ID); running != limit {
					t.Fatalf("calls running = %d, want %d", running, limit)
				}
				close(driver.release)
				answered(t, "first", <-first)
				answered(t, "second", <-second)
				if _, peak, calls := driver.snapshot(site.ID); peak != limit || calls != 2 {
					t.Fatalf("peak = %d over %d calls, want %d over 2", peak, calls, limit)
				}
				if records := ledgerCalls(t, statePath); len(records) != 2 {
					t.Fatalf("ledger holds %d records, want 2", len(records))
				}
			})
		})
	}
}

// TestWaitingCallHoldsNothingAnotherSiteNeeds: while one site is full and a
// call waits behind it, a call to another site runs.
func TestWaitingCallHoldsNothingAnotherSiteNeeds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		budget := testBudget(10)
		discussion, classifier := inference.DiscussionSite(budget), inference.ClassifierSite(budget)
		driver := newGateDriver(discussion.ID)
		client, _ := waitClient(t, fake.Roles{Provider: "fake", Model: "test", Driver: driver}, discussion, classifier)
		first := startCall(t.Context(), client, discussion, "project-a")
		waiting := startCall(t.Context(), client, discussion, "project-b")
		synctest.Wait()
		answered(t, "other site", <-startCall(t.Context(), client, classifier, "project-c"))
		select {
		case got := <-waiting:
			t.Fatalf("waiting call returned before its turn: %+v", got.result)
		default:
		}
		close(driver.release)
		answered(t, "first", <-first)
		answered(t, "waiting", <-waiting)
	})
}

// TestWaitingCallStopsWhenItsContextEnds: a waiter whose context ends returns
// what a canceled call returns, reserves no record, and leaves the running
// call and the queue behind it as they were.
func TestWaitingCallStopsWhenItsContextEnds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		site := inference.DiscussionSite(testBudget(10))
		driver := newGateDriver(site.ID)
		client, statePath := waitClient(t, fake.Roles{Provider: "fake", Model: "test", Driver: driver}, site)
		first := startCall(t.Context(), client, site, "project-a")
		synctest.Wait()
		ctx, cancel := context.WithCancel(t.Context())
		canceled := startCall(ctx, client, site, "project-b")
		synctest.Wait()
		behind := startCall(t.Context(), client, site, "project-c")
		synctest.Wait()
		cancel()
		got := <-canceled
		if got.err != nil || !got.result.Fallback || got.result.Reason != inference.FallbackDriverFailed ||
			got.result.Identity != nil {
			t.Fatalf("canceled waiter = %+v, %v; want driver_failed with no record", got.result, got.err)
		}
		close(driver.release)
		answered(t, "first", <-first)
		answered(t, "behind", <-behind)
		if records := ledgerCalls(t, statePath); len(records) != 2 {
			t.Fatalf("ledger holds %d records, want the two calls that ran", len(records))
		}
	})
}

// TestCallDeadlineStartsAfterTheWait: the site's timeout counts the provider
// call, not the wait, so a call that waited most of a timeout still gets all
// of it.
func TestCallDeadlineStartsAfterTheWait(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		site := inference.DiscussionSite(testBudget(10))
		site.Timeout = time.Second
		driver := newGateDriver(site.ID)
		driver.delay = 800 * time.Millisecond
		client, _ := waitClient(t, fake.Roles{Provider: "fake", Model: "test", Driver: driver}, site)
		first := startCall(t.Context(), client, site, "project-a")
		synctest.Wait()
		started := time.Now()
		second := startCall(t.Context(), client, site, "project-b")
		answered(t, "first", <-first)
		answered(t, "second", <-second)
		if elapsed := time.Since(started); elapsed <= site.Timeout {
			t.Fatalf("second call finished %s after it started, want past the %s timeout", elapsed, site.Timeout)
		}
	})
}

// TestWaitedCallIsAdmittedAgain: admission checks the credential against a
// deadline computed from now, so a call that waited is admitted again when
// its turn comes, and a role that stopped resolving meanwhile falls back.
func TestWaitedCallIsAdmittedAgain(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		site := inference.DiscussionSite(testBudget(10))
		driver := newGateDriver(site.ID)
		var unbound atomic.Bool
		client, _ := waitClient(t, fake.Roles{
			Provider: "fake", Model: "test", Driver: driver,
			Edit: func(domain.RoleName, *inference.RoleCall) error {
				if unbound.Load() {
					return errors.New("the line no longer resolves")
				}
				return nil
			},
		}, site)
		first := startCall(t.Context(), client, site, "project-a")
		synctest.Wait()
		second := startCall(t.Context(), client, site, "project-b")
		synctest.Wait()
		unbound.Store(true)
		close(driver.release)
		answered(t, "first", <-first)
		if got := <-second; !got.result.Fallback || got.result.Reason != inference.FallbackRoleUnbound {
			t.Fatalf("second = %+v, %v; want role_unbound from its admission after the wait", got.result, got.err)
		}
	})
}

// TestAbandonedCallKeepsItsPlace: a call abandoned at its timeout holds its
// place until the provider call ends, so a later call waits for the provider,
// not just for the timeout.
func TestAbandonedCallKeepsItsPlace(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		site := inference.DiscussionSite(testBudget(10))
		site.Timeout = time.Second
		release := make(chan struct{})
		var calls atomic.Int32
		driver := fallbackDriver(func(context.Context) (inference.Response, error) {
			if calls.Add(1) == 1 {
				<-release // ignores its context
			}
			return inference.Response{Output: []byte(waitDiscussionOutput), ComputeUnits: 1}, nil
		})
		client, _ := waitClient(t, fake.Roles{Provider: "fake", Model: "test", Driver: driver}, site)
		first := <-startCall(t.Context(), client, site, "project-a")
		if !first.result.Fallback || first.result.Reason != inference.FallbackDeadlineExceeded {
			t.Fatalf("first = %+v; want deadline_exceeded", first.result)
		}
		second := startCall(t.Context(), client, site, "project-b")
		synctest.Wait()
		select {
		case got := <-second:
			t.Fatalf("second call ran while the abandoned call held its place: %+v", got.result)
		default:
		}
		close(release)
		answered(t, "second", <-second)
	})
}
