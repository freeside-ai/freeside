package operations

import (
	"context"
	"fmt"
	"strings"

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
	Store               *store.Store
	Attention           *signet.Service
	ProjectID           domain.ProjectID
	Backend             domain.RunnerBackendClass
	ConfigurationDigest domain.Digest
	Mode                domain.OperatingMode
}

// Run checks conformance/workspace handoff and every backup-health dimension.
// Operational source failures are errors, not clean findings.
func (d Doctor) Run(ctx context.Context) (DoctorReport, error) {
	if d.Store == nil || d.Attention == nil || d.ProjectID == "" ||
		d.Backend == "" || d.ConfigurationDigest == "" {
		return DoctorReport{}, fmt.Errorf("doctor: nil or invalid dependency")
	}
	switch d.Mode {
	case domain.ModeAttendedDev, domain.ModeUnattended:
	default:
		return DoctorReport{}, fmt.Errorf("doctor: invalid operating mode %q", d.Mode)
	}
	var (
		conformance domain.BackendConformance
		found       bool
	)
	if err := d.Store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		conformance, found, err = tx.LatestBackendConformance(ctx, d.Backend)
		return err
	}); err != nil {
		return DoctorReport{}, fmt.Errorf("doctor: read conformance: %w", err)
	}
	baseConformant := found && conformance.Outcome == domain.ConformancePassed &&
		conformance.ConfigurationBound() &&
		conformance.ConfigurationDigest == d.ConfigurationDigest
	requiredCapabilities := domain.RequiredCapabilities(
		d.Mode,
		domain.NewCapabilitySnapshot(
			domain.CapDetachableWorkspace,
			domain.CapPostExitExport,
			domain.CapReadOnlyRemount,
		),
	)
	var missingCapabilities []domain.RunnerCapability
	if baseConformant {
		missingCapabilities = domain.MissingCapabilities(
			conformance.Capabilities, requiredCapabilities,
		)
	}
	conformant := baseConformant && len(missingCapabilities) == 0
	handoff := conformant
	findings := []DoctorFinding{
		{
			Code: "conformance", Healthy: conformant,
			Detail: conformanceDetail(
				found, conformance, d.ConfigurationDigest, missingCapabilities,
			),
		},
		{
			Code: "workspace_handoff", Healthy: handoff,
			Detail: workspaceHandoffDetail(
				found, conformance, d.ConfigurationDigest, requiredCapabilities,
			),
		},
	}
	backup, err := d.Store.BackupHealth(ctx)
	if err != nil {
		return DoctorReport{}, fmt.Errorf("doctor: backup health: %w", err)
	}
	findings = append(findings,
		backupFinding("checkpoint_encryption", backup.Encryption),
		backupFinding("checkpoint_currency", backup.CheckpointCurrency),
		backupFinding("artifact_closure", backup.ArtifactClosure),
		backupFinding("restore_test_age", backup.RestoreTestAge),
	)
	if err := d.converge(ctx, findings); err != nil {
		return DoctorReport{}, err
	}
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

func workspaceHandoffDetail(
	found bool, c domain.BackendConformance, configurationDigest domain.Digest,
	requiredCapabilities []domain.RunnerCapability,
) string {
	if !found {
		return "no durable conformance result"
	}
	if c.Outcome != domain.ConformancePassed || !c.ConfigurationBound() ||
		c.ConfigurationDigest != configurationDigest {
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
		return fmt.Sprintf("no durable conformance result for active configuration %s",
			configurationDigest)
	}
	if c.ConfigurationDigest != configurationDigest {
		return fmt.Sprintf(
			"generation %d is %s for stale configuration %s; active configuration is %s",
			c.Generation, c.Outcome, c.ConfigurationDigest, configurationDigest)
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

func (d Doctor) converge(ctx context.Context, findings []DoctorFinding) error {
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
	if err := d.Store.Read(ctx, func(tx *store.ReadTx) error {
		state, err := tx.ServerState(ctx)
		revision = state.Revision
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
				existing.Status = domain.StatusResolved
				if err := d.Attention.PutItem(ctx, existing); err != nil {
					return fmt.Errorf("doctor: clear %s finding: %w", finding.Code, err)
				}
			}
			continue
		}
		if len(existingItems) > 1 {
			for _, duplicate := range existingItems[1:] {
				duplicate.ItemVersion++
				duplicate.Status = domain.StatusResolved
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
				ItemVersion: 1, InterruptionClass: domain.InterruptionExceptional,
				Status: domain.StatusOpen,
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
