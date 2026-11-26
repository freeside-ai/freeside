package main

import (
	"context"
	"os"

	"github.com/freeside-ai/freeside/daemon/internal/commitplancheck"
)

// runCommitPlanMain runs the commit-plan validator without opening a database
// or reading daemon state. It is a convenience for writers and operators that
// grants no authority; the importer remains the authority at handoff.
func runCommitPlanMain(args []string) {
	os.Exit(commitplancheck.Run(context.Background(), args, os.Stdout, os.Stderr))
}
