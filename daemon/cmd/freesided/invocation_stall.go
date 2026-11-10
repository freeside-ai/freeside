package main

import (
	"context"
	"strings"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/daemoncompose"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// concludeInvocationStallNotices resolves every open stall notice at
// startup. No writer wait survives a daemon restart, so any open notice is
// stale. A held-work notice raised by `freesided raise-item` goes in the
// same write, so a start costs one revision, not two.
func concludeInvocationStallNotices(ctx context.Context, st *store.Store) error {
	return st.Write(ctx, func(tx *store.WriteTx) error {
		open, err := daemoncompose.OpenInvocationStallNotices(ctx, tx, func(itemID string) bool {
			return strings.HasPrefix(itemID, daemoncompose.InvocationStalledItemPrefix) || isRaisedHeldWorkNotice(itemID)
		})
		if err != nil {
			return err
		}
		return daemoncompose.ResolveInvocationStallNotices(ctx, tx, open, time.Now().UTC(),
			"Resolved when the daemon restarted: no writer wait survives a restart.")
	})
}
