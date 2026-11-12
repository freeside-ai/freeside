package daemoncompose

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/exec/fake"
	"github.com/freeside-ai/freeside/daemon/internal/export"
	"github.com/freeside-ai/freeside/daemon/internal/importer"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/specify"
	specifyfake "github.com/freeside-ai/freeside/daemon/internal/specify/fake"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/verify"
)

// scenarioStageDriver answers every stage start from the scenario. It
// scripts the invocation on the fake stage driver at Start, from the spec the
// engine admitted, then starts it there; the fake's state lives under the
// scenario directory, so a restart rejoins started invocations.
//
// The specifier returns the scenario's specification as its structured
// result. The implementer's delivery is the scenario's files: Start exports
// them, imports them over the admitted base under the options the engine
// will replay, and records the execution export the way the ward pipeline
// does, before the fake reports the stage complete.
type scenarioStageDriver struct {
	*fake.StageDriver
	scenario Scenario
	store    *store.Store
	blobs    *signet.BlobStore
	imports  ScenarioImportAuthority
	exports  ScenarioExportLookup
	// baseDir is the repository the delivery imports over; workDir holds
	// each delivery's workspace, handoff, and checkout while it is built.
	baseDir string
	workDir string
}

// ScenarioImportAuthority reconstructs a delivery's import options from its
// admission records, as the publication lane will replay them. Its method
// is stage.AdmissionAuthority's ImportOptionsRecord.
type ScenarioImportAuthority interface {
	ImportOptionsRecord(
		ctx context.Context, id domain.InvocationID, spec exec.StartSpec, base importer.Options,
	) (importer.Options, error)
}

// ScenarioExportLookup reports whether an invocation's execution export is
// already recorded. Its method is stage.ExportRecorder's
// LookupExecutionExportRecord.
type ScenarioExportLookup interface {
	LookupExecutionExportRecord(
		ctx context.Context, id domain.InvocationID,
	) (domain.ExecutionExport, bool, error)
}

func (d scenarioStageDriver) Start(ctx context.Context, id domain.InvocationID, spec exec.StartSpec) error {
	if _, started := d.StartSpec(id); started {
		// The fake refuses a second start of the same id.
		return d.StageDriver.Start(ctx, id, spec)
	}
	if engine.IsSpecificationInvocationIdentity(id, spec.RunID, spec.StageID) {
		if err := specifyfake.Script(d.StageDriver, id, 0, 0, specificationOutput(d.scenario)); err != nil {
			return err
		}
		return d.StageDriver.Start(ctx, id, spec)
	}
	if refused, err := d.refuseOnQuota(ctx, id, spec); err != nil || refused {
		return err
	}
	if err := d.deliver(ctx, id, spec); err != nil {
		return fmt.Errorf("scenario %s: deliver %s: %w", d.scenario.Name, id, err)
	}
	d.Script(id, fake.StageScript{
		PendingInspects: 1, Outcome: fake.OutcomeComplete,
		Result: exec.StageResult{Summary: "The scenario implementer delivered its files."},
	})
	return d.StageDriver.Start(ctx, id, spec)
}

// refuseOnQuota starts the scenario's quota refusal in place of a delivery
// when the scenario has one and the run is its campaign's first attempt. A
// later attempt delivers.
func (d scenarioStageDriver) refuseOnQuota(
	ctx context.Context, id domain.InvocationID, spec exec.StartSpec,
) (bool, error) {
	refusal := d.scenario.QuotaRefusal
	if refusal == nil {
		return false, nil
	}
	var run domain.Run
	if err := d.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		run, err = tx.GetRun(ctx, spec.RunID)
		return err
	}); err != nil {
		return false, fmt.Errorf("scenario %s: read run %s: %w", d.scenario.Name, spec.RunID, err)
	}
	if run.AttemptNumber > 1 {
		return false, nil
	}
	quota := &domain.QuotaRefusal{Window: refusal.Window}
	if refusal.ResetAfter > 0 {
		resetsAt := time.Now().UTC().Add(refusal.ResetAfter)
		quota.ResetsAt = &resetsAt
	}
	d.Script(id, fake.StageScript{
		Outcome: fake.OutcomeFail,
		Result: exec.StageResult{
			Summary:      "The scenario provider refused the run: its quota window is exhausted.",
			Cause:        domain.FailureCauseQuota,
			QuotaRefusal: quota,
		},
	})
	return true, d.StageDriver.Start(ctx, id, spec)
}

// scenarioCommitDate is every delivery's commit date. The replay carries
// it, so a fixed date gives the same head on every run and every root.
var scenarioCommitDate = time.Unix(1700000000, 0).UTC()

// deliver records the implementation export for one invocation. A start
// retried after a crash finds the export already recorded and records
// nothing.
func (d scenarioStageDriver) deliver(ctx context.Context, id domain.InvocationID, spec exec.StartSpec) error {
	if _, recorded, err := d.exports.LookupExecutionExportRecord(ctx, id); err != nil || recorded {
		return err
	}
	var admission domain.ExecutionAdmission
	if err := d.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		admission, err = tx.GetExecutionAdmissionRecord(ctx, id)
		return err
	}); err != nil {
		return fmt.Errorf("read admission: %w", err)
	}
	dir := filepath.Join(d.workDir, string(id))
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	workspace := filepath.Join(dir, "workspace")
	if err := writeTree(workspace, d.scenario.workspace()); err != nil {
		return err
	}
	handoff := filepath.Join(dir, "handoff")
	manifest, err := export.Export(os.DirFS(workspace), handoff, export.Options{})
	if err != nil {
		return fmt.Errorf("export workspace: %w", err)
	}
	manifestDigest, err := d.putExport(manifest, handoff)
	if err != nil {
		return err
	}
	checkout := filepath.Join(dir, "checkout")
	if _, err := scenarioGit(d.workDir, "clone", "-q", "--no-hardlinks", d.baseDir, checkout); err != nil {
		return err
	}
	options, err := d.imports.ImportOptionsRecord(
		ctx, id, spec, importer.Options{BaseSHA: spec.Base.BaseSHA, CommitDate: scenarioCommitDate},
	)
	if err != nil {
		return fmt.Errorf("import options: %w", err)
	}
	imported, err := importer.Import(ctx, handoff, checkout, options)
	if err != nil {
		return fmt.Errorf("import delivery: %w", err)
	}
	if !importer.AllAdvisory(imported.Findings, options.Policy.CandidateAutomationChanges) || imported.CommitSHA == "" {
		return fmt.Errorf("import delivery: refused with findings %v", imported.Findings)
	}
	record, err := domain.NewExecutionExport(domain.ExecutionExportInput{
		InvocationID: id, AdmissionID: admission.ID, ObservedBaseSHA: spec.Base.BaseSHA,
		HeadSHA: imported.CommitSHA, ManifestDigest: manifestDigest, RecordedAt: admission.AdmittedAt,
	})
	if err != nil {
		return err
	}
	return engine.RecordExecutionExport(ctx, d.store, record, engine.ProductionReplay{
		InvocationID: id, ObservedBaseSHA: spec.Base.BaseSHA, HeadSHA: imported.CommitSHA,
		Manifest: manifest, ManifestDigest: manifestDigest, ImportOptions: options,
	}, nil, nil)
}

// putExport stores the export's manifest and every regular file's blob,
// which the publication lane reads to replay the import.
func (d scenarioStageDriver) putExport(manifest export.Manifest, handoff string) (domain.Digest, error) {
	body, err := manifest.Encode()
	if err != nil {
		return "", err
	}
	digest := scenarioDigest(body)
	if _, err := d.blobs.Put(digest, bytes.NewReader(body)); err != nil {
		return "", fmt.Errorf("store export manifest: %w", err)
	}
	for _, entry := range manifest.Entries {
		if entry.Kind != export.EntryRegular || entry.Digest == nil {
			continue
		}
		blob := domain.Digest(*entry.Digest)
		content, err := os.ReadFile(filepath.Join(handoff, "blobs", "sha256", contentaddr.Hex(string(blob)))) //nolint:gosec // the driver's own export directory and a manifest digest
		if err != nil {
			return "", err
		}
		if _, err := d.blobs.Put(blob, bytes.NewReader(content)); err != nil {
			return "", fmt.Errorf("store export blob: %w", err)
		}
	}
	return digest, nil
}

func specificationOutput(sc Scenario) specify.Output {
	specification := sc.Specification
	return specify.Output{Specification: &specification}
}

// scenarioReviewSource completes every review the scenario's way. Unit 1's
// scenario has one: a clean review of exactly the head it was asked about.
type scenarioReviewSource struct {
	*fake.ReviewSource
}

func (s scenarioReviewSource) RequestReview(ctx context.Context, id domain.InvocationID, req exec.ReviewRequest) error {
	s.Script(id, fake.ReviewScript{
		Outcome: fake.OutcomeComplete,
		Result:  exec.ReviewResult{HeadSHA: req.HeadSHA, Findings: []domain.Finding{}},
	})
	return s.ReviewSource.RequestReview(ctx, id, req)
}

// scenarioRoom runs verification as host processes. Its recipe is the one
// the scenario's project image records, as a real image room reads it from
// the image.
type scenarioRoom struct {
	verify.ProcRoom
}

func (scenarioRoom) ReadRecipe(context.Context) ([]byte, error) {
	return bytes.Clone(ScenarioRecipe), nil
}
