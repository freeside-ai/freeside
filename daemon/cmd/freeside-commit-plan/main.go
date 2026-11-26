// Command freeside-commit-plan validates a writer workspace's
// .freeside-commit-plan.json against the workspace's change set using the
// importer's own checks and prints a machine-readable JSON report.
//
// This command is a convenience for writers that grants no authority and is
// not on the Tier A list. The daemon's importer (daemon/internal/importer)
// remains the sole authority at handoff.
//
// Build for the Claude agent image as a static linux/arm64 binary:
//
//	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath ./cmd/freeside-commit-plan
package main

import (
	"context"
	"os"

	"github.com/freeside-ai/freeside/daemon/internal/commitplancheck"
)

func main() {
	os.Exit(commitplancheck.Run(context.Background(), os.Args[1:], os.Stdout, os.Stderr))
}
