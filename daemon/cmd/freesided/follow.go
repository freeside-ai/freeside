package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/freeside-ai/freeside/daemon/internal/observe"
)

// freesided follow (issue #409; contract in issue #394, plan §8): selects a
// run by id and follows its observed timeline from submission through
// admission or hold, invocation start, terminal collection and import, to a
// final outcome.
//
// The verb itself is internal/observe.Run; this shim only supplies the
// process's streams, its interrupt, and its exit code. That split is what
// makes the containment boundary assertable: a file in package main can call
// its siblings without importing them, so containment is proven where a
// package boundary actually constrains it (internal/observe's import
// allowlist), not here.

// runFollowMain runs the follow verb. Exit contract: 0 observed (including an
// interrupted follow, and including a run whose own outcome is blocked or
// failed), 1 could not observe, 2 invocation mistake.
func runFollowMain(args []string) {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	err := observe.Run(ctx, args, os.Stdout, os.Stderr)
	if err == nil {
		return
	}
	fmt.Fprintln(os.Stderr, "freesided follow:", err)
	stop()
	os.Exit(observe.ExitCode(err))
}
