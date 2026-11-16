package daemoncompose

import "github.com/freeside-ai/freeside/daemon/internal/publish"

// AppAuthority is how a Claude-mode daemon comes to hold the App authority
// writer lock it opens the authority under.
type AppAuthority struct {
	// Lock is a lock the caller already holds on the Claude configuration's
	// StateRoot: a deputy's, or main's for a daemon started by hand with a
	// driver. Start owns it from the call.
	Lock *publish.AuthorityWriterLock
}
