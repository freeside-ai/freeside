// Command freeside-export is the §5.6 trusted export helper: the static
// binary the ward runs inside its pinned exporter image, in a fresh
// credential-free VM with the agent workspace mounted read-only
// (docs/spikes/workspace-handoff.md, Required backend contract check 6).
// It emits digest-addressed content blobs plus the normalized manifest
// onto the exporter's own root filesystem and exits; the ward collects
// the output via container export of the stopped exporter. All logic
// lives in internal/export; this command only binds flags to it.
//
// Build for the exporter image as a static linux/arm64 binary:
//
//	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath ./cmd/freeside-export
package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/freeside-ai/freeside/daemon/internal/export"
)

func main() {
	if err := run(os.Args[1:], os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "freeside-export:", err)
		os.Exit(1)
	}
}

func run(args []string, stderr io.Writer) error {
	flags := flag.NewFlagSet("freeside-export", flag.ContinueOnError)
	flags.SetOutput(stderr)
	workspace := flags.String("workspace", "/workspace", "read-only workspace mount to export")
	out := flags.String("out", "/handoff", "output directory for the manifest and content blobs")
	maxBlobBytes := flags.Int64("max-blob-bytes", 100<<20,
		"largest file that still gets a content blob; larger files are recorded blob_omitted; 0 disables the cap")
	maxTotalBlobBytes := flags.Int64("max-total-blob-bytes", 1<<30,
		"aggregate bytes written under blobs/ before further blobs are recorded blob_omitted; 0 disables the cap")
	maxEntries := flags.Int("max-entries", 1_000_000,
		"fail the export when the walk touches more names (files and directories) than this; 0 disables the cap")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %v", flags.Args())
	}

	m, err := export.Export(os.DirFS(*workspace), *out, export.Options{
		MaxBlobBytes:      *maxBlobBytes,
		MaxTotalBlobBytes: *maxTotalBlobBytes,
		MaxEntries:        *maxEntries,
	})
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(stderr, "exported %d entries to %s\n", len(m.Entries), *out)
	return nil
}
