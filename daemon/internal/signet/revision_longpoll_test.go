package signet_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
)

// The revision long-poll waits on real stdlib time (RevisionWaitBound), so
// every case runs in a testing/synctest bubble with its fixture built inside
// it: the bubble's clock jumps to the bound only once every goroutine is
// durably blocked, and the bubble fails the test if a waiter outlives it.

type revisionAnswer struct {
	revision signet.ServerRevision
	err      error
}

// startRevisionAfter runs RevisionAfter in the background and waits until it
// has either answered or parked on its select.
func startRevisionAfter(
	ctx context.Context, service *signet.Service, after int64,
) <-chan revisionAnswer {
	done := make(chan revisionAnswer, 1)
	go func() {
		revision, err := service.RevisionAfter(ctx, after)
		done <- revisionAnswer{revision, err}
	}()
	synctest.Wait()
	return done
}

func requireWaiting(t *testing.T, done <-chan revisionAnswer, when string) {
	t.Helper()
	select {
	case answer := <-done:
		t.Fatalf("%s: answered %+v, want still waiting", when, answer)
	default:
	}
}

func requireAnswered(t *testing.T, done <-chan revisionAnswer, when string) signet.ServerRevision {
	t.Helper()
	select {
	case answer := <-done:
		if answer.err != nil {
			t.Fatalf("%s: RevisionAfter: %v", when, answer.err)
		}
		return answer.revision
	default:
		t.Fatalf("%s: still waiting, want an answer", when)
		return signet.ServerRevision{}
	}
}

func currentRevision(t *testing.T, service *signet.Service) signet.ServerRevision {
	t.Helper()
	revision, err := service.Revision(context.Background())
	if err != nil {
		t.Fatalf("Revision: %v", err)
	}
	return revision
}

func TestRevisionAfterAnswersOnCommit(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t)
		start := currentRevision(t, f.service)
		began := time.Now()

		done := startRevisionAfter(context.Background(), f.service, start.Revision)
		requireWaiting(t, done, "before any commit")

		if _, err := f.service.Revoke(context.Background(), f.device.ID, f.device.ID); err != nil {
			t.Fatalf("Revoke: %v", err)
		}
		synctest.Wait()
		got := requireAnswered(t, done, "after a commit")
		if got.Revision != start.Revision+1 || got.SyncEpoch != start.SyncEpoch {
			t.Fatalf("answer = %+v, want revision %d in epoch %s", got, start.Revision+1, start.SyncEpoch)
		}
		if waited := time.Since(began); waited != 0 {
			t.Fatalf("answer took %v of bubble time, want none", waited)
		}
	})
}

func TestRevisionAfterAnswersUnchangedStateAtTheBound(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t)
		start := currentRevision(t, f.service)

		done := startRevisionAfter(context.Background(), f.service, start.Revision)
		time.Sleep(signet.RevisionWaitBound - time.Nanosecond)
		synctest.Wait()
		requireWaiting(t, done, "just before the bound")

		time.Sleep(time.Nanosecond)
		synctest.Wait()
		if got := requireAnswered(t, done, "at the bound"); got.Revision != start.Revision || got.SyncEpoch != start.SyncEpoch {
			t.Fatalf("answer = %+v, want the unchanged %+v", got, start)
		}
	})
}

// A cursor that differs from the revision, in either direction, answers at
// once: a restore can lower the revision, so a cursor above it is the
// earlier-epoch case and must not be held.
func TestRevisionAfterAnswersAtOnceWhenTheCursorDiffers(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t)
		start := currentRevision(t, f.service)
		for name, after := range map[string]int64{
			"below": start.Revision - 1,
			"above": start.Revision + 1,
		} {
			done := startRevisionAfter(context.Background(), f.service, after)
			if got := requireAnswered(t, done, name); got.Revision != start.Revision {
				t.Fatalf("%s: answer = %+v, want revision %d", name, got, start.Revision)
			}
		}
	})
}

func TestRevisionAfterAnswersOnEpochChange(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t)
		start := currentRevision(t, f.service)

		done := startRevisionAfter(context.Background(), f.service, start.Revision)
		state, err := f.store.NewEpoch(context.Background())
		if err != nil {
			t.Fatalf("NewEpoch: %v", err)
		}
		synctest.Wait()
		got := requireAnswered(t, done, "after an epoch change")
		if got.SyncEpoch != state.SyncEpoch || got.SyncEpoch == start.SyncEpoch || got.Revision != start.Revision {
			t.Fatalf("answer = %+v, want new epoch %s at revision %d", got, state.SyncEpoch, start.Revision)
		}
	})
}

func TestRevisionAfterReleasesACancelledWaiter(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t)
		start := currentRevision(t, f.service)

		ctx, cancel := context.WithCancel(context.Background())
		done := startRevisionAfter(ctx, f.service, start.Revision)
		cancel()
		synctest.Wait()
		select {
		case answer := <-done:
			if !errors.Is(answer.err, context.Canceled) {
				t.Fatalf("cancelled waiter = %+v, want context.Canceled", answer)
			}
		default:
			t.Fatal("cancelled waiter is still waiting")
		}
	})
}

func TestRevisionAfterReleasesWaitersOnShutdown(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t)
		start := currentRevision(t, f.service)

		done := startRevisionAfter(context.Background(), f.service, start.Revision)
		f.service.Shutdown()
		synctest.Wait()
		if got := requireAnswered(t, done, "after Shutdown"); got.Revision != start.Revision {
			t.Fatalf("answer = %+v, want the unchanged revision %d", got, start.Revision)
		}

		// A request that arrives after Shutdown does not wait either.
		late := startRevisionAfter(context.Background(), f.service, start.Revision)
		requireAnswered(t, late, "a request after Shutdown")
		f.service.Shutdown() // idempotent
	})
}

// Backup health changes the unattended verdict without a transaction, so it
// wakes no waiter; the bound's fresh read is what carries it to the client.
func TestRevisionAfterCarriesAVerdictChangeAtTheBound(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		// The waiter reads health on its own goroutine at the bound.
		var health atomic.Pointer[domain.BackupHealth]
		healthy := healthyBackup()
		health.Store(&healthy)
		service, item := newBackupHealthService(t, func() domain.BackupHealth { return *health.Load() })
		start := currentRevision(t, service)
		requireOpen(t, "healthy backup", start.UnattendedOperation)

		done := startRevisionAfter(context.Background(), service, start.Revision)
		unhealthy := healthyBackup()
		unhealthy.Encryption = domain.BackupHealthUnhealthy
		health.Store(&unhealthy)
		synctest.Wait()
		requireWaiting(t, done, "after a verdict-only change")

		time.Sleep(signet.RevisionWaitBound)
		synctest.Wait()
		got := requireAnswered(t, done, "at the bound")
		stops := got.UnattendedOperation.Stops
		if got.Revision != start.Revision || got.UnattendedOperation.Admission != domain.UnattendedAdmissionStopped ||
			len(stops) != 1 || stops[0].ItemID == nil || *stops[0].ItemID != item.ID {
			t.Fatalf("answer = %+v, want revision %d with the waiver notice stop", got, start.Revision)
		}
	})
}

func TestRevisionLongPollRejectsAMalformedAfter(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	handler := signet.NewHTTPHandler(f.service, testAuthorizer)
	for _, query := range []string{"after=abc", "after=-1", "after=1.5", "after=", "after=1&after=2"} {
		response := authenticatedRequest(t, handler, http.MethodGet, "/sync/revision?"+query, nil)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("%s: status = %d body=%s, want 400", query, response.Code, response.Body.String())
		}
		var body map[string]string
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || body["message"] == "" {
			t.Fatalf("%s: body = %s, want the error shape", query, response.Body.String())
		}
	}
}

func TestRevisionLongPollAnswersOverHTTPAfterACommit(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t)
		handler := signet.NewHTTPHandler(f.service, testAuthorizer)
		start := currentRevision(t, f.service)

		response := serveInBackground(handler, "Bearer test-device-1", start.Revision)
		requireNoResponse(t, response)
		if _, err := f.service.Revoke(context.Background(), f.device.ID, f.device.ID); err != nil {
			t.Fatalf("Revoke: %v", err)
		}
		synctest.Wait()
		recorder := <-response
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d body=%s, want 200", recorder.Code, recorder.Body.String())
		}
		var got signet.ServerRevision
		if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if got.Revision != start.Revision+1 {
			t.Fatalf("answer = %+v, want revision %d", got, start.Revision+1)
		}
	})
}

// The authenticated wrapper checks the credential before a wait that can last
// RevisionWaitBound; a device revoked meanwhile must get 401, not state.
func TestRevisionLongPollRefusesADeviceRevokedDuringTheWait(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t)
		handler := signet.NewHTTPHandler(f.service, signet.NewRequestAuthorizer(f.store))
		token, deviceID := pairedDevice(t, f, "Waiting device")
		start := currentRevision(t, f.service)

		response := serveInBackground(handler, "Bearer "+token, start.Revision)
		requireNoResponse(t, response)
		if _, err := f.service.Revoke(context.Background(), f.device.ID, deviceID); err != nil {
			t.Fatalf("Revoke: %v", err)
		}
		synctest.Wait()
		recorder := <-response
		if recorder.Code != http.StatusUnauthorized || recorder.Header().Get("WWW-Authenticate") != "Bearer" {
			t.Fatalf("status = %d headers=%v body=%s, want 401 with a Bearer challenge",
				recorder.Code, recorder.Header(), recorder.Body.String())
		}
	})
}

func serveInBackground(handler http.Handler, authorization string, after int64) <-chan *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodGet, "/sync/revision?after="+strconv.FormatInt(after, 10), nil)
	request.Header.Set("Authorization", authorization)
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		done <- recorder
	}()
	synctest.Wait()
	return done
}

func requireNoResponse(t *testing.T, response <-chan *httptest.ResponseRecorder) {
	t.Helper()
	select {
	case recorder := <-response:
		t.Fatalf("answered %d %s before any commit, want a held request", recorder.Code, recorder.Body.String())
	default:
	}
}
