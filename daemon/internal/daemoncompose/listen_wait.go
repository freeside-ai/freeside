package daemoncompose

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/daemonshell"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// ListenWait is how the installed daemon starts when its -listen names a
// Tailscale address that Tailscale does not report yet, as at login before
// Tailscale connects (#2083). The start waits up to Wait for the address,
// then serves on loopback at the same port with a card that says the phone
// cannot reach it. It never exits for this: launchd would restart it in a
// loop, and the Mac app would lose its daemon. Any other listen failure, such
// as a taken port, still fails the start.
type ListenWait struct {
	// Wait bounds how long the start waits for the address. Zero tries once.
	Wait time.Duration
	// TailscaleIPs reads this host's Tailscale addresses;
	// daemonshell.ReadTailscaleIPs in production.
	TailscaleIPs func() ([]netip.Addr, error)
}

// listenWaitPoll is how often the start asks Tailscale again while it waits.
const listenWaitPoll = time.Second

// listenWaiting binds addr as daemonshell.ListenPrivileged does. With a
// ListenWait, an address Tailscale does not report is retried until the wait
// ends, and then the listener binds loopback at the same port and absent
// carries why, for the card. Without one, the first failure is returned.
func listenWaiting(ctx context.Context, addr string, wait *ListenWait) (listener net.Listener, absent, err error) {
	if wait == nil {
		listener, err = daemonshell.ListenPrivileged(addr)
		return listener, nil, err
	}
	deadline := time.Now().Add(wait.Wait)
	for {
		listener, err = daemonshell.ListenPrivilegedReading(addr, wait.TailscaleIPs)
		if err == nil || !errors.Is(err, daemonshell.ErrTailscaleAddressAbsent) {
			return listener, nil, err
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		case <-time.After(min(listenWaitPoll, remaining)):
		}
	}
	// ErrTailscaleAddressAbsent is only ever about an address in Tailscale's
	// ranges, which always has a loopback twin.
	loopback, _, twinErr := daemonshell.LoopbackTwinListenAddress(addr)
	if twinErr != nil {
		return nil, nil, errors.Join(err, twinErr)
	}
	listener, bindErr := daemonshell.ListenPrivileged(loopback)
	if bindErr != nil {
		return nil, nil, bindErr
	}
	return listener, err, nil
}

const listenerFallbackItemPrefix = "system-health-listener-fallback-"

// ListenerFallbackCode is the card a daemon serving on loopback in place of
// its Tailscale address raises.
const ListenerFallbackCode = "listener_tailscale_absent"

// convergeListenerFallback keeps the listener card in step with this start:
// open while the daemon serves on loopback because Tailscale did not report
// its address (absent), concluded once a start binds the address it was
// given. The card is advisory, since the daemon serves this Mac in full.
func convergeListenerFallback(
	ctx context.Context, st *store.Store, configured string, bound net.Addr, absent error, now func() time.Time,
) error {
	at := nowOrWallClock(now)().UTC()
	reason := ""
	if absent != nil {
		reason = fmt.Sprintf("This daemon is serving on %s only, so a paired iPhone cannot reach it. "+
			"Tailscale did not report its listen address %s when it started (%s). Once Tailscale is "+
			"connected on this Mac, restart the daemon with launchctl kickstart -k gui/$(id -u)/ai.freeside.daemon.",
			bound, configured, absent)
	}
	return st.Write(ctx, func(tx *store.WriteTx) error {
		items, err := tx.ListOpenAttentionItems(ctx, domain.AttentionSystemHealth)
		if err != nil {
			return err
		}
		found := false
		for _, item := range items {
			if !strings.HasPrefix(string(item.ID), listenerFallbackItemPrefix) {
				continue
			}
			if absent != nil && item.Reason == reason {
				found = true
				continue
			}
			resolution := "Resolved when the daemon bound its configured listen address."
			if absent != nil {
				resolution = "Replaced by the card for this start."
			}
			item, err := item.ConcludeByDaemon(domain.ConcludedByDaemon, at, resolution, nil)
			if err != nil {
				return err
			}
			item.ItemVersion++
			if err := tx.PutAttentionItem(ctx, item); err != nil {
				return err
			}
		}
		if absent == nil || found {
			return nil
		}
		serverState, err := tx.ServerState(ctx)
		if err != nil {
			return err
		}
		subject := domain.Subject{Type: domain.SubjectSystem, ID: "daemon"}
		names, err := tx.DisplayNamesFor(ctx, "project-system", subject)
		if err != nil {
			return err
		}
		posture := domain.HealthPostureAdvisory
		item, err := domain.NewAttentionItem(domain.AttentionItemInput{
			ID:        domain.ItemID(fmt.Sprintf("%s%d", listenerFallbackItemPrefix, serverState.Revision+1)),
			ProjectID: "project-system", Subject: subject,
			Type: domain.AttentionSystemHealth, Priority: domain.PriorityNormal,
			Reason:            reason,
			RequestedDecision: []domain.Action{domain.ActionAcknowledge},
			HealthDiagnostic: &domain.HealthDiagnostic{
				Code: ListenerFallbackCode, Impairs: domain.ImpairedCapabilityRunVisibility,
			},
			DisplayNames: names, ItemVersion: 1,
			InterruptionClass: domain.InterruptionExceptional,
			CreatedAt:         &at, Posture: &posture, Status: domain.StatusOpen,
		}, nil)
		if err != nil {
			return err
		}
		return tx.PutAttentionItem(ctx, item)
	})
}
