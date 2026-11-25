package daemoncompose

import (
	"errors"
	"net"
	"net/netip"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/daemonshell"
)

// TestListenWaitingWaitsForTailscaleThenServesLoopback pins the wait's
// real-time loop: an address Tailscale never reports is asked about once a
// second until the wait ends and then served on loopback at the same port; an
// address Tailscale starts reporting ends the wait; and a failure that is not
// about Tailscale fails at once.
func TestListenWaitingWaitsForTailscaleThenServesLoopback(t *testing.T) {
	t.Parallel()
	const tailscale = "100.64.0.7:0"
	unassigned := errors.New("tailscale ip: not connected")
	synctest.Test(t, func(t *testing.T) {
		var asked atomic.Int64
		start := time.Now()
		listener, absent, err := listenWaiting(t.Context(), tailscale, &ListenWait{
			Wait: 5 * time.Second,
			TailscaleIPs: func() ([]netip.Addr, error) {
				asked.Add(1)
				return nil, unassigned
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = listener.Close() })
		if bound := listener.Addr().(*net.TCPAddr); !bound.IP.IsLoopback() {
			t.Fatalf("bound %s, want loopback", bound)
		}
		if !errors.Is(absent, daemonshell.ErrTailscaleAddressAbsent) || !errors.Is(absent, unassigned) {
			t.Fatalf("absent = %v, want Tailscale's failure", absent)
		}
		if waited := time.Since(start); waited != 5*time.Second || asked.Load() != 6 {
			t.Fatalf("waited %s asking %d times, want 5s and 6", waited, asked.Load())
		}
	})

	synctest.Test(t, func(t *testing.T) {
		var asked atomic.Int64
		_, absent, err := listenWaiting(t.Context(), tailscale, &ListenWait{
			Wait: time.Minute,
			TailscaleIPs: func() ([]netip.Addr, error) {
				if asked.Add(1) < 3 {
					return nil, unassigned
				}
				return []netip.Addr{netip.MustParseAddr("100.64.0.7")}, nil
			},
		})
		// This host does not hold the address, so the bind itself fails:
		// the wait ended because Tailscale reported it.
		if err == nil || errors.Is(err, daemonshell.ErrTailscaleAddressAbsent) || absent != nil || asked.Load() != 3 {
			t.Fatalf("err = %v, absent = %v, asked %d times; want the bind's failure on the third ask",
				err, absent, asked.Load())
		}
	})

	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		_, _, err := listenWaiting(t.Context(), "192.0.2.7:0", &ListenWait{
			Wait:         time.Minute,
			TailscaleIPs: func() ([]netip.Addr, error) { return nil, unassigned },
		})
		if err == nil || errors.Is(err, daemonshell.ErrTailscaleAddressAbsent) || time.Since(start) != 0 {
			t.Fatalf("err = %v after %s, want an immediate refusal", err, time.Since(start))
		}
	})
}
