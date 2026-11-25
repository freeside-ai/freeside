package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/publish"
)

const (
	waitRegistrationID     = 3001
	waitHeldInstallationID = 160625767
)

// heldSiblingFault is the fault a pass publishes when a sibling
// installation's mint hold refuses its grant read, wrapped as reconcileApp
// wraps it.
func heldSiblingFault(heldUntil time.Time) publish.JanitorRegistrationFault {
	return publish.JanitorRegistrationFault{
		RegistrationID: waitRegistrationID,
		Err: fmt.Errorf("installation janitor: registration %d installation %d grants: %w",
			waitRegistrationID, waitHeldInstallationID,
			&publish.JanitorMintHeldError{InstallationID: waitHeldInstallationID, HeldUntil: heldUntil}),
	}
}

// fakeJanitor stands in for the janitor's Run: it returns nil once canceled.
func fakeJanitor(t *testing.T) (context.CancelFunc, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		<-ctx.Done()
		done <- nil
	}()
	return cancel, done
}

func notReady() (bool, error) { return false, nil }

func TestInstallationWaitFailsAtOnceOnHoldOutlastingIt(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		cancel, done := fakeJanitor(t)
		start := time.Now()
		// A hold ending exactly at the deadline already leaves no time to
		// cover the registration.
		heldUntil := start.Add(time.Hour)
		faults := func() []publish.JanitorRegistrationFault {
			return []publish.JanitorRegistrationFault{heldSiblingFault(heldUntil)}
		}
		var stderr bytes.Buffer
		err := awaitTrustedInstallation(notReady, faults, waitRegistrationID, time.Hour, &stderr, cancel, done)
		if elapsed := time.Since(start); elapsed > time.Second {
			t.Fatalf("wait took %s, want an immediate failure", elapsed)
		}
		if !errors.Is(err, publish.ErrJanitorMintHeld) || errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v, want the mint hold, not a timeout", err)
		}
		want := fmt.Sprintf("installation %d's mint hold ends at %s",
			waitHeldInstallationID, heldUntil.UTC().Format(time.RFC3339))
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("err = %v, want it to contain %q", err, want)
		}
		if got := strings.Count(stderr.String(), "janitor cannot cover registration"); got != 1 {
			t.Fatalf("stderr = %q, want one fault line", stderr.String())
		}
		select {
		case err := <-done:
			t.Fatalf("wait returned without receiving the janitor's result %v", err)
		default:
		}
	})
}

func TestInstallationWaitReportsFaultChangesAndNamesTheLastOnTimeout(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		cancel, done := fakeJanitor(t)
		start := time.Now()
		// A second before the deadline: the hold leaves the wait possible.
		held := heldSiblingFault(start.Add(time.Hour - time.Second))
		listFault := publish.JanitorRegistrationFault{
			RegistrationID: waitRegistrationID,
			Err:            errors.New("installation janitor: list installations: github api: /app/installations returned status 502"),
		}
		other := publish.JanitorRegistrationFault{RegistrationID: 9999, Err: errors.New("other registration")}
		faults := func() []publish.JanitorRegistrationFault {
			// Each pass republishes a fresh error with the same text.
			if time.Since(start) < 20*time.Minute {
				return []publish.JanitorRegistrationFault{held, other}
			}
			return []publish.JanitorRegistrationFault{
				{RegistrationID: listFault.RegistrationID, Err: errors.New(listFault.Err.Error())}, other,
			}
		}
		var stderr bytes.Buffer
		err := awaitTrustedInstallation(notReady, faults, waitRegistrationID, time.Hour, &stderr, cancel, done)
		if elapsed := time.Since(start); elapsed != time.Hour {
			t.Fatalf("wait took %s, want the whole hour", elapsed)
		}
		if !errors.Is(err, context.DeadlineExceeded) ||
			!strings.Contains(err.Error(), "last fault for registration 3001: "+listFault.Err.Error()) {
			t.Fatalf("err = %v, want a timeout naming the last fault", err)
		}
		want := fmt.Sprintf("onboard: janitor cannot cover registration %d yet: %v\n"+
			"onboard: janitor cannot cover registration %d yet: %v\n",
			waitRegistrationID, held.Err, waitRegistrationID, listFault.Err)
		if stderr.String() != want {
			t.Fatalf("stderr = %q, want %q", stderr.String(), want)
		}
	})
}

func TestInstallationWaitReportsAClearedFaultAndReturnsReady(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		cancel, done := fakeJanitor(t)
		defer func() {
			cancel()
			if err := <-done; err != nil {
				t.Error(err)
			}
		}()
		start := time.Now()
		held := heldSiblingFault(start.Add(10 * time.Minute))
		faults := func() []publish.JanitorRegistrationFault {
			if time.Since(start) < 10*time.Minute {
				return []publish.JanitorRegistrationFault{held}
			}
			return nil
		}
		ready := func() (bool, error) { return time.Since(start) >= 15*time.Minute, nil }
		var stderr bytes.Buffer
		if err := awaitTrustedInstallation(ready, faults, waitRegistrationID, time.Hour, &stderr, cancel, done); err != nil {
			t.Fatal(err)
		}
		if elapsed := time.Since(start); elapsed != 15*time.Minute {
			t.Fatalf("wait took %s, want 15m", elapsed)
		}
		want := fmt.Sprintf("onboard: janitor cannot cover registration %d yet: %v\n"+
			"onboard: janitor fault for registration %d cleared\n",
			waitRegistrationID, held.Err, waitRegistrationID)
		if stderr.String() != want {
			t.Fatalf("stderr = %q, want %q", stderr.String(), want)
		}
	})
}
