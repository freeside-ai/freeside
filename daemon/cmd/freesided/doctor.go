package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/operations"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

func runDoctorMain(args []string) {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := runDoctorCommand(ctx, args, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "freesided doctor:", err)
		os.Exit(1)
	}
}

func runDoctorCommand(
	ctx context.Context, args []string, stdout, stderr io.Writer,
) (err error) {
	flags := flag.NewFlagSet("freesided doctor", flag.ContinueOnError)
	flags.SetOutput(stderr)
	dbPath := flags.String("db", "", "SQLite database path (required)")
	projectID := flags.String("project", "project-system", "project id for system_health items")
	operatingMode := flags.String(
		"operating-mode", string(domain.ModeAttendedDev),
		"operating mode: attended_dev (default) or unattended")
	configurationDigest := flags.String(
		"backend-configuration-digest", "",
		"active backend configuration digest (required)")
	reviewConfigurationDigest := flags.String(
		"review-configuration-digest", "",
		"effective review configuration digest (required in unattended mode)")
	approvedRecipes := digestSetFlag{}
	flags.Var(&approvedRecipes, "approved-recipe",
		"approved verification-recipe digest (repeatable)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments: %v", flags.Args())
	}
	if *dbPath == "" {
		return errors.New("-db is required")
	}
	if !contentaddr.Valid(*configurationDigest) {
		return errors.New("-backend-configuration-digest must be a canonical sha256 digest")
	}
	mode, err := parseOperatingMode(*operatingMode)
	if err != nil {
		return err
	}
	if *reviewConfigurationDigest != "" && !contentaddr.Valid(*reviewConfigurationDigest) {
		return errors.New("-review-configuration-digest must be a canonical sha256 digest")
	}
	if mode == domain.ModeUnattended && *reviewConfigurationDigest == "" {
		return errors.New("-review-configuration-digest is required in unattended mode")
	}
	blobs, err := signet.NewBlobStore(*dbPath + ".blobs")
	if err != nil {
		return fmt.Errorf("open artifact store: %w", err)
	}
	files, err := store.NewDefaultLocalBackupFiles(*dbPath)
	if err != nil {
		return err
	}
	health, err := files.NewCheckpointHealthSource(
		blobs,
		approvedRecipes,
		backupPayloadExtractors(),
	)
	if err != nil {
		return err
	}
	st, _, err := openStoreWithTopicKey(ctx, *dbPath, store.Options{
		ApprovedRecipes: approvedRecipes, BackupHealthSource: health,
	})
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, st.Close()) }()
	attention := signet.NewService(st, signet.WithBlobStore(blobs))
	report, err := (operations.Doctor{
		Store: st, Attention: attention,
		ProjectID:                 domain.ProjectID(*projectID),
		Backend:                   domain.BackendFreshVMReadOnlyVolumeHandoff,
		ConfigurationDigest:       domain.Digest(*configurationDigest),
		ReviewConfigurationDigest: domain.Digest(*reviewConfigurationDigest),
		Mode:                      mode,
	}).Run(ctx)
	if err != nil {
		return err
	}
	if err := json.NewEncoder(stdout).Encode(report); err != nil {
		return fmt.Errorf("write doctor result: %w", err)
	}
	if !report.Healthy {
		return errors.New("one or more doctor checks are unhealthy")
	}
	return nil
}

type digestSetFlag map[domain.Digest]bool

func (f digestSetFlag) String() string {
	return fmt.Sprint(map[domain.Digest]bool(f))
}

func (f *digestSetFlag) Set(raw string) error {
	if !contentaddr.Valid(raw) {
		return fmt.Errorf("%q is not a canonical sha256 digest", raw)
	}
	if *f == nil {
		*f = digestSetFlag{}
	}
	(*f)[domain.Digest(raw)] = true
	return nil
}

func backupPayloadExtractors() map[string]store.BackupPayloadDigestExtractor {
	return map[string]store.BackupPayloadDigestExtractor{
		engine.FakePublicationTaskKind:              engine.FakePublicationBackupPayloadDigests,
		engine.FakePublicationInvocationOwnerKind:   engine.FakePublicationInvocationOwnerBackupPayloadDigests,
		signet.AgentInvocationRequestedKind:         signet.AgentInvocationBackupPayloadDigests,
		signet.PublicationReevaluationRequestedKind: signet.PublicationReevaluationBackupPayloadDigests,
		signet.PublicationReevaluationCompletedKind: signet.PublicationReevaluationCompletionBackupPayloadDigests,
		engine.KindProductionInvocationRequested:    engine.ProductionInvocationBackupPayloadDigests,
		engine.KindProductionPublicationRequested:   engine.ProductionPublicationBackupPayloadDigests,
		engine.KindRemediationInvocationRequested:   engine.RemediationInvocationBackupPayloadDigests,
		engine.KindElaborationInvocationRequested:   engine.ElaborationInvocationBackupPayloadDigests,
		engine.KindElaborationImplementationClaim:   engine.ElaborationImplementationClaimBackupPayloadDigests,
		publish.IntentKindReservation:               publish.ReservationBackupPayloadDigests,
		publish.IntentKindPublication:               publish.PublicationBackupPayloadDigests,
	}
}
