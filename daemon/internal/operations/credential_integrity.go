package operations

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

const (
	credentialIntegrityCode = "credential_integrity" //nolint:gosec // G101: a diagnostic code, not a credential
	// credentialIntegrityItemPrefix must not start with doctorItemPrefix:
	// converge files and clears blocking items by that prefix, and a marked
	// credential is advisory.
	credentialIntegrityItemPrefix = "system-health-credential-integrity-"
)

// CredentialIntegrityOutcome is one enrollment's result from a live probe
// pass. It carries ids and fixed codes only; a probe's error text stays with
// the caller that logs it.
type CredentialIntegrityOutcome struct {
	EnrollmentID domain.ClientEnrollmentID
	// NotChecked is the probe's fixed reason code for a store it could not
	// finish observing, and empty for a store it checked.
	NotChecked string
	// CorruptionChecked reports that a checked store was compared with its
	// generation's recorded digest. It is false for a store the daemon
	// refreshes in place, whose bytes legitimately move past that digest.
	CorruptionChecked bool
	// CorruptionUnconfirmed reports a corruption finding a second
	// observation did not reproduce, which recorded no mark.
	CorruptionUnconfirmed bool
}

// credentialIntegrityItemID names the one item a marked generation gets. It
// is a function of the generation alone, so a later pass finds the item an
// earlier pass filed whatever its status.
func credentialIntegrityItemID(enrollment domain.ClientEnrollmentID, ordinal int) domain.ItemID {
	return domain.ItemID(fmt.Sprintf("%s%s-%d",
		credentialIntegrityItemPrefix, contentaddr.Hex(contentaddr.Sum([]byte(enrollment))), ordinal))
}

// markedGeneration is an enrollment's current generation that carries at
// least one integrity mark.
type markedGeneration struct {
	identity   domain.AuthIdentity
	enrollment domain.ClientEnrollmentID
	ordinal    int
	findings   []domain.CredentialIntegrityFinding
}

func (m markedGeneration) findingList() string {
	names := make([]string, len(m.findings))
	for i, finding := range m.findings {
		names[i] = string(finding)
	}
	return strings.Join(names, ", ")
}

// markedCurrentGenerations reads every enrollment's current generation and
// keeps the marked ones. A mark on a superseded generation is history, not a
// finding: re-enrollment appended past it.
func markedCurrentGenerations(ctx context.Context, tx *store.ReadTx) ([]markedGeneration, error) {
	identities, err := tx.ListAuthIdentities(ctx)
	if err != nil {
		return nil, err
	}
	var marked []markedGeneration
	for _, identity := range identities {
		enrollments, err := tx.ListClientEnrollments(ctx, identity.ID)
		if err != nil {
			return nil, err
		}
		for _, enrollment := range enrollments {
			generation, err := tx.CurrentEnrollmentGeneration(ctx, enrollment.ID)
			if errors.Is(err, store.ErrNotFound) {
				continue
			}
			if err != nil {
				return nil, err
			}
			marks, err := tx.GenerationIntegrityMarks(ctx, enrollment.ID, generation.Ordinal)
			if err != nil {
				return nil, err
			}
			if len(marks) == 0 {
				continue
			}
			entry := markedGeneration{
				identity: identity, enrollment: enrollment.ID, ordinal: generation.Ordinal,
			}
			for _, mark := range marks {
				entry.findings = append(entry.findings, mark.Finding)
			}
			marked = append(marked, entry)
		}
	}
	return marked, nil
}

// credentialIntegrity runs the live probe when one is wired, converges the
// advisory items against the recorded marks, and reports the finding. The
// finding reads the store, not the probe's return value, so a pass with no
// probe still reports what an earlier pass recorded.
func (d Doctor) credentialIntegrity(ctx context.Context) (DoctorFinding, error) {
	var outcomes []CredentialIntegrityOutcome
	probed := d.CredentialIntegrityProbe != nil
	if probed {
		var err error
		outcomes, err = d.CredentialIntegrityProbe(ctx)
		if err != nil {
			return DoctorFinding{}, fmt.Errorf("doctor: credential integrity probe: %w", err)
		}
	}
	marked, err := d.convergeCredentialIntegrityItems(ctx)
	if err != nil {
		return DoctorFinding{}, err
	}
	return credentialIntegrityFinding(marked, probed, outcomes), nil
}

func credentialIntegrityFinding(
	marked []markedGeneration, probed bool, outcomes []CredentialIntegrityOutcome,
) DoctorFinding {
	var detail []string
	if probed {
		detail = append(detail, fmt.Sprintf("probed %d enrollments", len(outcomes)))
	} else {
		detail = append(detail, "recorded marks only, no live probe ran")
	}
	if len(marked) == 0 {
		detail = append(detail, "no current generation is marked")
	} else {
		entries := make([]string, len(marked))
		for i, m := range marked {
			entries[i] = fmt.Sprintf("%s generation %d (%s)", m.enrollment, m.ordinal, m.findingList())
		}
		detail = append(detail, "marked: "+strings.Join(entries, ", "))
	}
	var notChecked, corruptionUnconfirmed, corruptionNotRun []string
	for _, outcome := range outcomes {
		switch {
		case outcome.NotChecked != "":
			notChecked = append(notChecked, fmt.Sprintf("%s (%s)", outcome.EnrollmentID, outcome.NotChecked))
		case outcome.CorruptionUnconfirmed:
			corruptionUnconfirmed = append(corruptionUnconfirmed, string(outcome.EnrollmentID))
		case !outcome.CorruptionChecked:
			corruptionNotRun = append(corruptionNotRun, string(outcome.EnrollmentID))
		}
	}
	if len(notChecked) != 0 {
		detail = append(detail, "not checked: "+strings.Join(notChecked, ", "))
	}
	if len(corruptionUnconfirmed) != 0 {
		detail = append(detail,
			"corruption finding not reproduced, no mark recorded: "+strings.Join(corruptionUnconfirmed, ", "))
	}
	if len(corruptionNotRun) != 0 {
		detail = append(detail,
			"corruption check not run on a store the daemon refreshes: "+strings.Join(corruptionNotRun, ", "))
	}
	return DoctorFinding{
		Code: credentialIntegrityCode, Healthy: len(marked) == 0,
		Detail: strings.Join(detail, "; "),
	}
}

// convergeCredentialIntegrityItems keeps one advisory item per marked current
// generation and resolves the item of a generation that is no longer
// current. It returns the marked generations it converged against.
//
// The plan is computed in a read first, because a write transaction always
// advances the client-visible revision and most passes change nothing. A
// pass that does change something recomputes the plan inside its write, so
// two overlapping passes cannot both file one generation's item.
func (d Doctor) convergeCredentialIntegrityItems(ctx context.Context) ([]markedGeneration, error) {
	var (
		marked []markedGeneration
		puts   []domain.AttentionItem
	)
	if err := d.Store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		marked, puts, err = d.planCredentialIntegrityItems(ctx, tx)
		return err
	}); err != nil {
		return nil, fmt.Errorf("doctor: read credential integrity: %w", err)
	}
	if len(puts) == 0 {
		return marked, nil
	}
	if err := d.Store.Write(ctx, func(tx *store.WriteTx) error {
		var err error
		marked, puts, err = d.planCredentialIntegrityItems(ctx, &tx.ReadTx)
		if err != nil {
			return err
		}
		for _, item := range puts {
			if err := signet.ValidateItemIntake(item); err != nil {
				return err
			}
			if err := tx.PutAttentionItem(ctx, item); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return nil, fmt.Errorf("doctor: converge credential integrity items: %w", err)
	}
	return marked, nil
}

// planCredentialIntegrityItems returns the marked current generations and
// the item writes that bring the items in line with them.
func (d Doctor) planCredentialIntegrityItems(
	ctx context.Context, tx *store.ReadTx,
) ([]markedGeneration, []domain.AttentionItem, error) {
	marked, err := markedCurrentGenerations(ctx, tx)
	if err != nil {
		return nil, nil, err
	}
	stored, err := tx.ListAttentionItems(ctx)
	if err != nil {
		return nil, nil, err
	}
	// Items are matched in every project and every status: the id names a
	// generation, and an item the operator already handled is never refiled.
	existing := make(map[domain.ItemID]domain.AttentionItem)
	for _, entry := range stored {
		item := entry.Value
		if item.Type == domain.AttentionSystemHealth &&
			strings.HasPrefix(string(item.ID), credentialIntegrityItemPrefix) {
			existing[item.ID] = item
		}
	}
	subject := domain.Subject{Type: domain.SubjectSystem, ID: "daemon"}
	var puts []domain.AttentionItem
	current := make(map[domain.ItemID]bool, len(marked))
	for _, m := range marked {
		id := credentialIntegrityItemID(m.enrollment, m.ordinal)
		current[id] = true
		reason := d.credentialIntegrityReason(m)
		item, filed := existing[id]
		switch {
		case !filed:
			displayNames, err := tx.DisplayNamesFor(ctx, d.ProjectID, subject)
			if err != nil {
				return nil, nil, err
			}
			createdAt := time.Now().UTC()
			if d.Now != nil {
				createdAt = d.Now().UTC()
			}
			posture := domain.HealthPostureAdvisory
			item, err := domain.NewAttentionItem(domain.AttentionItemInput{
				ID: id, ProjectID: d.ProjectID, Subject: subject,
				Type: domain.AttentionSystemHealth, Priority: domain.PriorityHigh,
				Reason:            reason,
				RequestedDecision: []domain.Action{domain.ActionAcknowledge},
				HealthDiagnostic: &domain.HealthDiagnostic{
					Code: credentialIntegrityCode, Impairs: domain.ImpairedCapabilityAgentCredential,
				},
				DisplayNames: displayNames,
				ItemVersion:  1, InterruptionClass: domain.InterruptionExceptional,
				CreatedAt: &createdAt,
				Posture:   &posture,
				Status:    domain.StatusOpen,
			}, nil)
			if err != nil {
				return nil, nil, fmt.Errorf("construct %s item: %w", credentialIntegrityCode, err)
			}
			puts = append(puts, item)
		case item.Status == domain.StatusOpen && item.Reason != reason:
			// A second finding landed on a generation already reported.
			item.ItemVersion++
			item.Reason = reason
			puts = append(puts, item)
		}
	}
	for id, item := range existing {
		if item.Status == domain.StatusOpen && !current[id] {
			item.ItemVersion++
			item.Status = domain.StatusResolved
			puts = append(puts, item)
		}
	}
	return marked, puts, nil
}

// credentialIntegrityReason names the identity by its id and masked label,
// never by its account binding.
func (d Doctor) credentialIntegrityReason(m markedGeneration) string {
	identity := string(m.identity.ID)
	if d.IdentityLabel != nil {
		if label := d.IdentityLabel(m.identity); label != "" {
			identity += " (" + label + ")"
		}
	}
	return fmt.Sprintf(
		"Stored credential for identity %s failed its integrity check: enrollment %s, generation %d, %s. "+
			"Nothing new is admitted on this generation until the enrollment is re-enrolled.",
		identity, m.enrollment, m.ordinal, m.findingList())
}
