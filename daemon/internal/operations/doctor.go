package operations

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

const doctorItemPrefix = "system-health-doctor-"

// DoctorFinding is one independently evaluated operational invariant.
type DoctorFinding struct {
	Code    string `json:"code"`
	Healthy bool   `json:"healthy"`
	Detail  string `json:"detail"`
}

// DoctorReport is the one-shot and scheduled diagnostic result.
type DoctorReport struct {
	Healthy        bool                 `json:"healthy"`
	OperatingMode  domain.OperatingMode `json:"operating_mode"`
	IsolationClass string               `json:"isolation_class"`
	Findings       []DoctorFinding      `json:"findings"`
}

// Doctor evaluates existing durable conformance and live backup primitives,
// then converges their system_health items.
type Doctor struct {
	Store     *store.Store
	Attention *signet.Service
	// BackupHealthSource optionally narrows a one-shot caller's checkpoint
	// policy. The store validates its result against a coherent live snapshot;
	// nil uses the store's ordinary source.
	BackupHealthSource store.BackupHealthSource
	ProjectID          domain.ProjectID
	Backend            domain.RunnerBackendClass
	// ConfigurationDigests are the backend configurations whose conformance
	// the doctor judges: for the ward backend, which holds a proof per
	// writer image (#3211), the configuration of every image it tracks.
	// conformance and workspace_handoff are healthy when at least one's
	// newest row is a covering pass.
	ConfigurationDigests []domain.Digest
	// ReviewConfigurationDigest is the effective Freeside-invoked reviewer
	// configuration. It is required only in unattended mode, where activated
	// trust profiles bind it.
	ReviewConfigurationDigest domain.Digest
	Mode                      domain.OperatingMode
	Now                       func() time.Time
	// CredentialIntegrityProbe optionally runs the live credential-integrity
	// probe before the credential_integrity finding is read. Nil reports the
	// marks earlier passes recorded and observes no store.
	CredentialIntegrityProbe func(context.Context) ([]CredentialIntegrityOutcome, error)
	// IdentityLabel optionally renders an identity's masked label for a
	// credential-integrity item. Nil names the identity by its id alone.
	IdentityLabel func(domain.AuthIdentity) string
	// CordonConfigurationDigest optionally names the configuration of the
	// cordon the daemon runs on its own host, which adds that host's
	// cordoned_conformance finding and requires its pass to be for this
	// configuration. Empty when the daemon runs no cordon; every other
	// execution host is reported either way.
	CordonConfigurationDigest domain.Digest
}

// ConvergeReviewConfiguration re-evaluates only the activated-profile
// reviewer configuration finding. The unattended engine calls this narrow
// pass before dispatch so a repaired profile clears its stale blocker without
// waiting for the next full scheduled doctor run.
func (d Doctor) ConvergeReviewConfiguration(ctx context.Context) error {
	if d.Store == nil || d.Attention == nil || d.ProjectID == "" {
		return fmt.Errorf("doctor review configuration: nil or invalid dependency")
	}
	switch d.Mode {
	case domain.ModeAttendedDev, domain.ModeUnattended:
	default:
		return fmt.Errorf("doctor review configuration: invalid operating mode %q", d.Mode)
	}
	if d.Mode == domain.ModeUnattended && d.ReviewConfigurationDigest == "" {
		return fmt.Errorf("doctor review configuration: empty unattended review configuration digest")
	}
	finding, err := d.reviewConfigurationFinding(ctx)
	if err != nil {
		return err
	}
	if err := d.converge(ctx, []DoctorFinding{finding}, domain.HealthPostureBlocking); err != nil {
		return fmt.Errorf("doctor review configuration: %w", err)
	}
	if finding.Healthy {
		if err := d.resolveReviewConfigurationItemsAcrossProjects(ctx); err != nil {
			return err
		}
	}
	return nil
}

// imageUpkeepHostFaultCode names the finding image upkeep converges.
const imageUpkeepHostFaultCode = "image_upkeep_host_fault"

// ConvergeImageUpkeep converges the image-upkeep host-fault finding (plan
// §5.7, Project-image upkeep): fault says what kept the builder from
// reaching a verdict, and empty clears the item. The item is advisory, since
// a blocking one would hold every unattended run, including runs whose
// image still serves their base.
func (d Doctor) ConvergeImageUpkeep(ctx context.Context, fault string) error {
	if d.Store == nil || d.Attention == nil || d.ProjectID == "" {
		return fmt.Errorf("doctor image upkeep: nil or invalid dependency")
	}
	finding := DoctorFinding{Code: imageUpkeepHostFaultCode, Healthy: fault == "", Detail: fault}
	if err := d.converge(ctx, []DoctorFinding{finding}, domain.HealthPostureAdvisory); err != nil {
		return fmt.Errorf("doctor image upkeep: %w", err)
	}
	return nil
}

// ProjectNotReadyCode names the finding a project's readiness converges.
const ProjectNotReadyCode = "project_not_ready"

// ConvergeProjectReadiness converges one project's readiness card: one
// Blocking system_health item, filed under the project itself rather than
// d.ProjectID, while any check has not passed, naming the project and each
// such check; concluded once every check passes. The item is blocking
// because the operating gate (store.RequireUnattendedOperationOpen) then
// holds every unattended admission, which is the hold readiness exists for.
func (d Doctor) ConvergeProjectReadiness(ctx context.Context, readiness domain.ProjectReadiness) error {
	if d.Store == nil || d.Attention == nil {
		return fmt.Errorf("doctor project readiness: nil dependency")
	}
	if err := readiness.Validate(); err != nil {
		return fmt.Errorf("doctor project readiness: %w", err)
	}
	d.ProjectID = readiness.ProjectID
	finding := DoctorFinding{
		Code: ProjectNotReadyCode, Healthy: readiness.Ready(), Detail: projectReadinessDetail(readiness),
	}
	if err := d.converge(ctx, []DoctorFinding{finding}, domain.HealthPostureBlocking); err != nil {
		return fmt.Errorf("doctor project readiness: %w", err)
	}
	return nil
}

// ConcludeProjectReadinessExcept concludes every open project readiness
// card filed under a project other than keep, or every one when keep is
// empty. A card is converged only by the daemon composed for its project,
// so a daemon that no longer evaluates that project (its settings now name
// another, or a composition that keeps no readiness record) would otherwise
// hold all unattended admission behind it forever. A daemon serves one
// project (#3212 revisits this when several share one), so a start that
// evaluates keep, or none, owns every readiness card.
func (d Doctor) ConcludeProjectReadinessExcept(ctx context.Context, keep domain.ProjectID) error {
	if d.Attention == nil {
		return fmt.Errorf("doctor project readiness: nil dependency")
	}
	snapshots, err := d.Attention.ListAttentionItems(ctx)
	if err != nil {
		return fmt.Errorf("doctor project readiness: list items: %w", err)
	}
	prefix := doctorItemPrefix + ProjectNotReadyCode + "-"
	for _, snapshot := range snapshots {
		item := snapshot.Item
		if item.Type != domain.AttentionSystemHealth || item.Status != domain.StatusOpen ||
			item.ProjectID == keep || !strings.HasPrefix(string(item.ID), prefix) {
			continue
		}
		item.ItemVersion++
		item, err = item.ConcludeByDaemon(domain.ConcludedByDaemon, d.now(),
			"Resolved when the daemon started without evaluating this project's readiness.", nil)
		if err != nil {
			return fmt.Errorf("doctor project readiness: clear item %s: %w", item.ID, err)
		}
		if err := d.Attention.PutItem(ctx, item); err != nil {
			return fmt.Errorf("doctor project readiness: clear item %s: %w", item.ID, err)
		}
	}
	return nil
}

// projectReadinessDetail names the project and each check that did not
// pass, with its status and why.
func projectReadinessDetail(readiness domain.ProjectReadiness) string {
	failed := readiness.NotPassed()
	if len(failed) == 0 {
		return fmt.Sprintf("project %s is ready", readiness.ProjectID)
	}
	parts := make([]string, 0, len(failed))
	for _, result := range failed {
		parts = append(parts, fmt.Sprintf("%s %s (%s)", result.Check, result.Status, result.Detail))
	}
	return fmt.Sprintf("project %s is not ready: %s", readiness.ProjectID, strings.Join(parts, "; "))
}

// now is the doctor's clock: Now when injected, else the wall clock, in UTC.
func (d Doctor) now() time.Time {
	if d.Now != nil {
		return d.Now().UTC()
	}
	return time.Now().UTC()
}

func (d Doctor) resolveReviewConfigurationItemsAcrossProjects(ctx context.Context) error {
	snapshots, err := d.Attention.ListAttentionItems(ctx)
	if err != nil {
		return fmt.Errorf("doctor review configuration: list cross-project items: %w", err)
	}
	prefix := doctorItemPrefix + "review_configuration-"
	for _, snapshot := range snapshots {
		item := snapshot.Item
		if item.Type != domain.AttentionSystemHealth || item.Status != domain.StatusOpen ||
			!strings.HasPrefix(string(item.ID), prefix) {
			continue
		}
		item.ItemVersion++
		item, err = item.ConcludeByDaemon(domain.ConcludedByDaemon, d.now(),
			"Resolved when doctor found the review configuration healthy again.", nil)
		if err != nil {
			return fmt.Errorf("doctor review configuration: clear cross-project item %s: %w", item.ID, err)
		}
		if err := d.Attention.PutItem(ctx, item); err != nil {
			return fmt.Errorf("doctor review configuration: clear cross-project item %s: %w", item.ID, err)
		}
	}
	return nil
}

// Run checks conformance/workspace handoff, every backup-health dimension,
// and stored-credential integrity.
// Operational source failures are errors, not clean findings.
func (d Doctor) Run(ctx context.Context) (DoctorReport, error) {
	if d.Store == nil || d.Attention == nil || d.ProjectID == "" ||
		d.Backend == "" || slices.Contains(d.ConfigurationDigests, "") {
		return DoctorReport{}, fmt.Errorf("doctor: nil or invalid dependency")
	}
	switch d.Mode {
	case domain.ModeAttendedDev, domain.ModeUnattended:
	default:
		return DoctorReport{}, fmt.Errorf("doctor: invalid operating mode %q", d.Mode)
	}
	if d.Mode == domain.ModeUnattended && d.ReviewConfigurationDigest == "" {
		return DoctorReport{}, fmt.Errorf("doctor: empty unattended review configuration digest")
	}
	requiredCapabilities, err := domain.RequiredCapabilities(
		d.Mode,
		d.Backend,
		domain.NewCapabilitySnapshot(
			domain.CapDetachableWorkspace,
			domain.CapPostExitExport,
			domain.CapReadOnlyRemount,
		),
	)
	if err != nil {
		return DoctorReport{}, fmt.Errorf("doctor: %w", err)
	}
	conformance, handoff, err := d.conformanceFindings(ctx, requiredCapabilities)
	if err != nil {
		return DoctorReport{}, err
	}
	findings := []DoctorFinding{conformance, handoff}
	reviewConfiguration, err := d.reviewConfigurationFinding(ctx)
	if err != nil {
		return DoctorReport{}, err
	}
	findings = append(findings, reviewConfiguration)
	var backup domain.BackupHealth
	if d.BackupHealthSource == nil {
		backup, err = d.Store.BackupHealth(ctx)
	} else {
		backup, err = d.Store.BackupHealthWithSource(ctx, d.BackupHealthSource)
	}
	if err != nil {
		return DoctorReport{}, fmt.Errorf("doctor: backup health: %w", err)
	}
	findings = append(findings,
		backupFinding("checkpoint_encryption", backup.Encryption),
		backupFinding("checkpoint_currency", backup.CheckpointCurrency),
		backupFinding("artifact_closure", backup.ArtifactClosure),
		backupFinding("restore_test_age", backup.RestoreTestAge),
	)
	if err := d.converge(ctx, findings, domain.HealthPostureBlocking); err != nil {
		return DoctorReport{}, err
	}
	// The credential-integrity finding stays out of converge, which files a
	// blocking item for every unhealthy code: a marked credential is
	// advisory, and its items converge per marked generation. It runs after
	// converge so a probe failure cannot hold back the findings above.
	integrity, err := d.credentialIntegrity(ctx)
	if err != nil {
		return DoctorReport{}, err
	}
	findings = append(findings, integrity)
	cordoned, err := d.cordonedConformanceFindings(ctx)
	if err != nil {
		return DoctorReport{}, err
	}
	findings = append(findings, cordoned...)
	report := DoctorReport{
		Healthy: true, OperatingMode: d.Mode,
		IsolationClass: string(d.Backend), Findings: findings,
	}
	for _, finding := range findings {
		if !finding.Healthy {
			report.Healthy = false
		}
	}
	return report, nil
}

func (d Doctor) reviewConfigurationFinding(ctx context.Context) (DoctorFinding, error) {
	const code = "review_configuration"
	if d.Mode == domain.ModeAttendedDev {
		return DoctorFinding{
			Code: code, Healthy: true,
			Detail: "not applicable in attended_dev mode",
		}, nil
	}
	var inspections []store.CurrentTrustProfileInspection
	var mismatches []string
	if err := d.Store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		inspections, err = tx.InspectLatestTrustProfiles(ctx)
		if err != nil {
			return err
		}
		for _, inspection := range inspections {
			if inspection.ReconstructionError != nil {
				mismatches = append(mismatches, fmt.Sprintf(
					"%s current activated profile is unreadable: %v",
					inspection.Repo, inspection.ReconstructionError,
				))
				continue
			}
			profile := inspection.Profile
			if profile.Review.ConfigDigest == d.ReviewConfigurationDigest {
				continue
			}
			mismatches = append(mismatches, fmt.Sprintf(
				"%s pins %s; daemon effective is %s",
				profile.Repo, profile.Review.ConfigDigest, d.ReviewConfigurationDigest,
			))
		}
		return nil
	}); err != nil {
		return DoctorFinding{}, fmt.Errorf("doctor: read trust profile activations: %w", err)
	}
	if len(mismatches) != 0 {
		return DoctorFinding{
			Code: code, Healthy: false, Detail: strings.Join(mismatches, "; "),
		}, nil
	}
	return DoctorFinding{
		Code: code, Healthy: true,
		Detail: fmt.Sprintf(
			"daemon effective configuration %s satisfies %d activated trust profiles",
			d.ReviewConfigurationDigest, len(inspections),
		),
	}, nil
}

// conformanceFindings judges each of d.ConfigurationDigests by its newest
// conformance row. Each finding is healthy when at least one configuration
// passes it, and its detail names every configuration with its own verdict.
// Converge files an unhealthy finding as a blocking item that holds every
// unattended attempt, so one configuration's failed or running pass must
// not make it unhealthy: the store gate already holds that configuration's
// attempts, and only a backend with no passing configuration has nothing
// left to admit.
func (d Doctor) conformanceFindings(
	ctx context.Context, requiredCapabilities []domain.RunnerCapability,
) (conformance, handoff DoctorFinding, err error) {
	conformance = DoctorFinding{Code: "conformance"}
	handoff = DoctorFinding{Code: "workspace_handoff"}
	if len(d.ConfigurationDigests) == 0 {
		conformance.Detail = "the backend tracks no configuration"
		handoff.Detail = conformance.Detail
		return conformance, handoff, nil
	}
	var details [2][]string
	for _, digest := range d.ConfigurationDigests {
		var (
			record domain.BackendConformance
			found  bool
		)
		if err := d.Store.Read(ctx, func(tx *store.ReadTx) error {
			var err error
			record, found, err = tx.LatestConfigurationConformance(ctx, domain.DaemonHostID, d.Backend, digest)
			return err
		}); err != nil {
			return DoctorFinding{}, DoctorFinding{}, fmt.Errorf("doctor: read conformance: %w", err)
		}
		passed := found && record.Outcome == domain.ConformancePassed && record.ConfigurationBound()
		var missing []domain.RunnerCapability
		if passed {
			missing = domain.MissingCapabilities(record.Capabilities, requiredCapabilities)
		}
		ok := passed && len(missing) == 0
		details[0] = append(details[0], conformanceDetail(found, record, digest, missing))
		details[1] = append(details[1], workspaceHandoffDetail(found, record, digest, requiredCapabilities))
		conformance.Healthy = conformance.Healthy || ok
		handoff.Healthy = handoff.Healthy || ok
	}
	conformance.Detail = strings.Join(details[0], "; ")
	handoff.Detail = strings.Join(details[1], "; ")
	return conformance, handoff, nil
}

// cordonedConformanceFindings reports one cordoned_conformance finding per
// execution host (§5.7, Conformance per host), in id order: the host's
// newest cordoned record, its proof generation, configuration digest, and
// time, and the host's conditions. A host is healthy when that record is a
// pass and the host isn't in ExecutionHostProofNotCurrent. The daemon's
// host must also hold its pass for CordonConfigurationDigest, the
// configuration it runs now, and is left out when that is empty. The
// findings stay out of converge, whose blocking item holds every unattended
// attempt: a host that failed its pass holds only the cordoned class there,
// which admission refuses by that host's record, so the ward's writers keep
// running.
func (d Doctor) cordonedConformanceFindings(ctx context.Context) ([]DoctorFinding, error) {
	type hostRecord struct {
		host   domain.ExecutionHost
		record domain.BackendConformance
		found  bool
	}
	var records []hostRecord
	if err := d.Store.Read(ctx, func(tx *store.ReadTx) error {
		hosts, err := tx.ListExecutionHosts(ctx)
		if err != nil {
			return err
		}
		for _, host := range hosts {
			if host.ID == domain.DaemonHostID && d.CordonConfigurationDigest == "" {
				continue
			}
			record, found, err := tx.LatestBackendConformance(ctx, host.ID, domain.BackendCordonedHostProcess)
			if err != nil {
				return err
			}
			records = append(records, hostRecord{host: host, record: record, found: found})
		}
		return nil
	}); err != nil {
		return nil, fmt.Errorf("doctor: read cordoned conformance: %w", err)
	}
	findings := make([]DoctorFinding, 0, len(records))
	for _, r := range records {
		findings = append(findings, d.cordonedConformanceFinding(r.host, r.record, r.found))
	}
	return findings, nil
}

func (d Doctor) cordonedConformanceFinding(
	host domain.ExecutionHost, record domain.BackendConformance, found bool,
) DoctorFinding {
	var running domain.Digest
	if host.ID == domain.DaemonHostID {
		running = d.CordonConfigurationDigest
	}
	conditions := "none"
	notCurrent := false
	if len(host.Conditions) > 0 {
		names := make([]string, len(host.Conditions))
		for i, observed := range host.Conditions {
			names[i] = string(observed.Condition)
			notCurrent = notCurrent || observed.Condition == domain.ExecutionHostProofNotCurrent
		}
		conditions = strings.Join(names, ", ")
	}
	prefix := fmt.Sprintf("%s: host %s: ", domain.BackendCordonedHostProcess, host.ID)
	suffix := "; conditions: " + conditions
	finding := DoctorFinding{Code: "cordoned_conformance"}
	if !found {
		detail := "no durable conformance result"
		if running != "" {
			detail += " for configuration " + string(running)
		}
		finding.Detail = prefix + detail + suffix
		return finding
	}
	detail := fmt.Sprintf("generation %d is %s for configuration %s at %s",
		record.Generation, record.Outcome, record.ConfigurationDigest,
		record.ProvedAt.UTC().Format(time.RFC3339))
	stale := running != "" && record.ConfigurationDigest != running
	if stale {
		detail += ", not the running configuration " + string(running)
	}
	finding.Healthy = record.Outcome == domain.ConformancePassed && record.ConfigurationBound() && !stale && !notCurrent
	finding.Detail = prefix + detail + suffix
	return finding
}

func workspaceHandoffDetail(
	found bool, c domain.BackendConformance, configurationDigest domain.Digest,
	requiredCapabilities []domain.RunnerCapability,
) string {
	if !found {
		return fmt.Sprintf("no durable conformance result for configuration %s", configurationDigest)
	}
	if c.Outcome != domain.ConformancePassed || !c.ConfigurationBound() {
		return conformanceDetail(true, c, configurationDigest, nil)
	}
	missing := domain.MissingCapabilities(c.Capabilities, requiredCapabilities)
	if len(missing) != 0 {
		return fmt.Sprintf("configuration %s is missing %v", c.ConfigurationDigest, missing)
	}
	return fmt.Sprintf("configuration %s proves detachable read-only handoff", c.ConfigurationDigest)
}

func conformanceDetail(
	found bool, c domain.BackendConformance, configurationDigest domain.Digest,
	missingCapabilities []domain.RunnerCapability,
) string {
	if !found {
		return fmt.Sprintf("no durable conformance result for configuration %s",
			configurationDigest)
	}
	if len(missingCapabilities) != 0 {
		return fmt.Sprintf(
			"generation %d is missing %v for configuration %s",
			c.Generation, missingCapabilities, c.ConfigurationDigest,
		)
	}
	return fmt.Sprintf("generation %d is %s for configuration %s",
		c.Generation, c.Outcome, c.ConfigurationDigest)
}

func backupFinding(code string, status domain.BackupHealthStatus) DoctorFinding {
	return DoctorFinding{
		Code: code, Healthy: status == domain.BackupHealthHealthy,
		Detail: string(status),
	}
}

// converge files an item with posture for each unhealthy finding and clears
// the items of each healthy one.
func (d Doctor) converge(ctx context.Context, findings []DoctorFinding, posture domain.HealthPosture) error {
	snapshots, err := d.Attention.ListAttentionItems(ctx)
	if err != nil {
		return fmt.Errorf("doctor: list health items: %w", err)
	}
	open := make(map[string][]domain.AttentionItem)
	for _, snapshot := range snapshots {
		item := snapshot.Item
		if item.Type == domain.AttentionSystemHealth &&
			item.ProjectID == d.ProjectID &&
			item.Status == domain.StatusOpen &&
			strings.HasPrefix(string(item.ID), doctorItemPrefix) {
			for _, finding := range findings {
				if strings.HasPrefix(string(item.ID), doctorItemPrefix+finding.Code+"-") {
					open[finding.Code] = append(open[finding.Code], item)
				}
			}
		}
	}
	var revision int64
	var displayNames *domain.DisplayNames
	if err := d.Store.Read(ctx, func(tx *store.ReadTx) error {
		state, err := tx.ServerState(ctx)
		if err != nil {
			return err
		}
		revision = state.Revision
		displayNames, err = tx.DisplayNamesFor(
			ctx, d.ProjectID, domain.Subject{Type: domain.SubjectSystem, ID: "daemon"},
		)
		return err
	}); err != nil {
		return fmt.Errorf("doctor: read revision: %w", err)
	}
	for _, finding := range findings {
		existingItems := open[finding.Code]
		reason := fmt.Sprintf(
			"Doctor check %s is unhealthy: %s", finding.Code, finding.Detail)
		if finding.Healthy {
			for _, existing := range existingItems {
				existing.ItemVersion++
				existing, err := existing.ConcludeByDaemon(domain.ConcludedByDaemon, d.now(),
					fmt.Sprintf("Resolved when doctor check %s passed.", finding.Code), nil)
				if err != nil {
					return fmt.Errorf("doctor: clear %s finding: %w", finding.Code, err)
				}
				if err := d.Attention.PutItem(ctx, existing); err != nil {
					return fmt.Errorf("doctor: clear %s finding: %w", finding.Code, err)
				}
			}
			continue
		}
		if len(existingItems) > 1 {
			for _, duplicate := range existingItems[1:] {
				duplicate.ItemVersion++
				duplicate, err := duplicate.ConcludeByDaemon(domain.ConcludedByDaemon, d.now(),
					fmt.Sprintf("Resolved as a duplicate of the open doctor check %s item.", finding.Code), nil)
				if err != nil {
					return fmt.Errorf("doctor: clear duplicate %s finding: %w", finding.Code, err)
				}
				if err := d.Attention.PutItem(ctx, duplicate); err != nil {
					return fmt.Errorf("doctor: clear duplicate %s finding: %w", finding.Code, err)
				}
			}
		}
		var (
			existing domain.AttentionItem
			exists   bool
		)
		if len(existingItems) != 0 {
			existing = existingItems[0]
			exists = true
		}
		switch {
		case !exists:
			createdAt := time.Now().UTC()
			if d.Now != nil {
				createdAt = d.Now().UTC()
			}
			item, err := domain.NewAttentionItem(domain.AttentionItemInput{
				ID:        domain.ItemID(fmt.Sprintf("%s%s-%d", doctorItemPrefix, finding.Code, revision)),
				ProjectID: d.ProjectID,
				Subject:   domain.Subject{Type: domain.SubjectSystem, ID: "daemon"},
				Type:      domain.AttentionSystemHealth, Priority: domain.PriorityHigh,
				Reason: reason,
				RequestedDecision: []domain.Action{
					domain.ActionRunDoctor,
					domain.ActionAcknowledge,
					domain.ActionStopUnattended,
				},
				HealthDiagnostic: &domain.HealthDiagnostic{
					Code: finding.Code, Impairs: domain.ImpairedCapabilityUnattendedAdmission,
				},
				DisplayNames: displayNames,
				ItemVersion:  1, InterruptionClass: domain.InterruptionExceptional,
				CreatedAt: &createdAt,
				Posture:   &posture,
				Status:    domain.StatusOpen,
			}, nil)
			if err != nil {
				return fmt.Errorf("doctor: construct %s finding: %w", finding.Code, err)
			}
			if err := d.Attention.PutItem(ctx, item); err != nil {
				return fmt.Errorf("doctor: file %s finding: %w", finding.Code, err)
			}
			revision++
		case existing.Reason != reason:
			existing.ItemVersion++
			existing.Reason = reason
			if err := d.Attention.PutItem(ctx, existing); err != nil {
				return fmt.Errorf("doctor: update %s finding: %w", finding.Code, err)
			}
		}
	}
	return nil
}
