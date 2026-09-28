package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	osexec "os/exec"
	"path/filepath"
	"slices"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/procbound"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/strictjson"
)

// publicationFormatsCheckTimeout bounds the installed daemon's
// publication-formats run, which only prints two constants.
const publicationFormatsCheckTimeout = 10 * time.Second

// publicationFormatsOutput is the publication-formats wire shape: per state
// file, the version the build writes and every version it accepts, so a later
// build that reads two versions can say so without a new shape.
type publicationFormatsOutput struct {
	InstallationAuthority      stateFormat `json:"installation_authority"`
	InstallationJanitorJournal stateFormat `json:"installation_janitor_journal"`
}

type stateFormat struct {
	Writes  int   `json:"writes"`
	Accepts []int `json:"accepts"`
}

// currentPublicationFormats reports this build, which reads only the version
// it writes.
func currentPublicationFormats() publicationFormatsOutput {
	versions := publish.CurrentStateFormatVersions()
	return publicationFormatsOutput{
		InstallationAuthority:      stateFormat{Writes: versions.InstallationAuthority, Accepts: []int{versions.InstallationAuthority}},
		InstallationJanitorJournal: stateFormat{Writes: versions.InstallationJanitorJournal, Accepts: []int{versions.InstallationJanitorJournal}},
	}
}

func runPublicationFormatsMain(args []string) {
	if err := runPublicationFormats(args, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "freesided publication-formats:", err)
		os.Exit(2)
	}
}

func runPublicationFormats(args []string, stdout io.Writer) error {
	if len(args) != 0 {
		return fmt.Errorf("unexpected arguments: %v", args)
	}
	return json.NewEncoder(stdout).Encode(currentPublicationFormats())
}

// checkProdPublicationFormats refuses unless the installed prod daemon at
// prodDaemon and this build each accept every App authority state format the
// other writes. A run that shares prod's App directories must never leave
// prod a file it refuses to read, nor start on state prod wrote that the run
// can't read. Any doubt fails closed: a failed command (including an
// installed build that predates the subcommand), malformed output, or a
// missing field, which decodes as version 0 or an empty list.
func checkProdPublicationFormats(ctx context.Context, prodDaemon string) error {
	if !filepath.IsAbs(prodDaemon) {
		return fmt.Errorf("-prod-daemon %q must be an absolute path", prodDaemon)
	}
	ctx, cancel := context.WithTimeout(ctx, publicationFormatsCheckTimeout)
	defer cancel()
	var stdout, stderr bytes.Buffer
	cmd := osexec.CommandContext(ctx, prodDaemon, "publication-formats") // #nosec G204 -- operator-selected installed daemon, fixed argument.
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := procbound.Run(cmd, procbound.DefaultWaitDelay); err != nil {
		return fmt.Errorf("installed prod daemon %q cannot report its publication formats (install a build that includes #1583): %w: %s",
			prodDaemon, err, bytes.TrimSpace(stderr.Bytes()))
	}
	var prod publicationFormatsOutput
	if err := strictjson.Decode(stdout.Bytes(), &prod, strictjson.RejectInvalidUTF8, 16<<10); err != nil {
		return fmt.Errorf("installed prod daemon %q printed malformed publication formats: %w", prodDaemon, err)
	}
	run := currentPublicationFormats()
	for _, format := range []struct {
		name      string
		prod, run stateFormat
	}{
		{"installation authority", prod.InstallationAuthority, run.InstallationAuthority},
		{"installation janitor journal", prod.InstallationJanitorJournal, run.InstallationJanitorJournal},
	} {
		if !slices.Contains(format.prod.Accepts, format.run.Writes) {
			return fmt.Errorf(
				"installed prod daemon %q does not accept %s version %d, which this build writes (it accepts %v); update the installed app first",
				prodDaemon, format.name, format.run.Writes, format.prod.Accepts)
		}
		if !slices.Contains(format.run.Accepts, format.prod.Writes) {
			return fmt.Errorf(
				"installed prod daemon %q writes %s version %d, which this build does not accept (it accepts %v); run a build that reads it",
				prodDaemon, format.name, format.prod.Writes, format.run.Accepts)
		}
	}
	return nil
}
