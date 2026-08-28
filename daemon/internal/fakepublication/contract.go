// Package fakepublication owns the durable attended fake-publication task and
// terminal-item contracts. It has no store dependency, so both the engine and
// migrations authenticate the same historical bytes.
package fakepublication

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/strictjson"
)

const (
	TaskKind                = "engine.fake_publication"
	TaskVersion             = "freeside.engine.fake-publication/v1"
	OperatingModeAttended   = "attended_dev"
	maxCommitTimestamp      = int64(4102444800)
	terminalBindingPrefix   = "<!-- freeside:fake-publication-terminal="
	terminalBindingSuffix   = " -->"
	publicationMarkerPrefix = "<!-- freeside:publication-identity="
	maxPullRequestBodyBytes = 64 << 10
)

// Task is the immutable fake-publication outbox payload.
type Task struct {
	Version                  string              `json:"version"`
	RunID                    domain.RunID        `json:"run_id"`
	ProjectID                domain.ProjectID    `json:"project_id"`
	StoreEpoch               string              `json:"store_epoch"`
	WorkspaceDir             string              `json:"workspace_dir"`
	HandoffDir               string              `json:"handoff_dir"`
	HandoffDigest            domain.Digest       `json:"handoff_digest"`
	Repo                     string              `json:"repo"`
	BaseRef                  string              `json:"base_ref"`
	BaseSHA                  string              `json:"base_sha"`
	AllowedPaths             []string            `json:"allowed_paths"`
	RecipeDigest             domain.Digest       `json:"recipe_digest"`
	RecipePath               string              `json:"recipe_path"`
	TrustProfileDigest       domain.Digest       `json:"trust_profile_digest"`
	VerificationInvocationID domain.InvocationID `json:"verification_invocation_id"`
	PublicationInvocationID  domain.InvocationID `json:"publication_invocation_id"`
	Title                    string              `json:"title"`
	Body                     string              `json:"body"`
	CommitDate               time.Time           `json:"commit_date"`
	CommitDateExplicit       bool                `json:"commit_date_explicit"`
	StartedAt                time.Time           `json:"started_at"`
	OperatingMode            string              `json:"operating_mode"`
}

func (task Task) Validate() error {
	if task.Version != TaskVersion {
		return fmt.Errorf("unknown task version %q", task.Version)
	}
	if err := ValidateTaskUTF8(task); err != nil {
		return err
	}
	if task.RunID == "" || task.ProjectID == "" || task.StoreEpoch == "" ||
		task.VerificationInvocationID == "" || task.PublicationInvocationID == "" {
		return domain.ErrEmptyID
	}
	if task.VerificationInvocationID == task.PublicationInvocationID {
		return errors.New("task reuses one invocation across verification and publication")
	}
	if task.OperatingMode != OperatingModeAttended {
		return fmt.Errorf("task operating mode %q is not %s", task.OperatingMode, OperatingModeAttended)
	}
	if task.WorkspaceDir == "" || !filepath.IsAbs(task.WorkspaceDir) ||
		task.HandoffDir == "" || !filepath.IsAbs(task.HandoffDir) {
		return errors.New("task workspace and handoff paths must be absolute")
	}
	if err := ValidateRepository(task.Repo); err != nil {
		return fmt.Errorf("task repository %q: %w", task.Repo, err)
	}
	if err := ValidateBranchName(task.BaseRef); err != nil {
		return fmt.Errorf("task base ref %q: %w", task.BaseRef, err)
	}
	if !validCommitSHA(task.BaseSHA) || task.RecipeDigest == "" || task.RecipePath == "" ||
		task.TrustProfileDigest == "" || task.Title == "" ||
		!contentaddr.Valid(string(task.HandoffDigest)) {
		return domain.ErrEmptyField
	}
	if task.CommitDate.IsZero() || task.StartedAt.IsZero() ||
		task.CommitDate.Location() != time.UTC || task.StartedAt.Location() != time.UTC {
		return errors.New("task timestamps must be non-zero UTC")
	}
	if err := ValidateCommitDate(task.CommitDate); err != nil {
		return fmt.Errorf("task %w", err)
	}
	if len(task.AllowedPaths) == 0 {
		return errors.New("task has no candidate path allowlist")
	}
	for _, allowedPath := range task.AllowedPaths {
		if allowedPath == "" {
			return errors.New("task allowed path is empty")
		}
	}
	if err := ValidateAllowlist(task.AllowedPaths); err != nil {
		return fmt.Errorf("task allowlist: %w", err)
	}
	if err := ValidateRecipePath(task.RecipePath); err != nil {
		return fmt.Errorf("task recipe path: %w", err)
	}
	if err := ValidateCandidateBody(task.Body); err != nil {
		return fmt.Errorf("task publication body: %w", err)
	}
	return nil
}

func EncodeTask(task Task) ([]byte, error) {
	if err := task.Validate(); err != nil {
		return nil, err
	}
	payload, err := json.Marshal(task)
	if err != nil {
		return nil, fmt.Errorf("encode fake publication task: %w", err)
	}
	return payload, nil
}

func DecodeTask(payload []byte) (Task, error) {
	var task Task
	if err := strictjson.Decode(payload, &task, strictjson.TolerateInvalidUTF8, strictjson.NoLimit); err != nil {
		if errors.Is(err, strictjson.ErrTrailingData) {
			return Task{}, errors.New("decode fake publication task: trailing data")
		}
		return Task{}, fmt.Errorf("decode fake publication task: %w", err)
	}
	if err := task.Validate(); err != nil {
		return Task{}, err
	}
	return task, nil
}

func TaskKey(runID domain.RunID) string { return TaskKind + "/" + string(runID) }

func ReadyItemID(runID domain.RunID) domain.ItemID { return terminalItemID("ready", runID) }

func BlockedItemID(runID domain.RunID) domain.ItemID { return terminalItemID("blocked", runID) }

func terminalItemID(kind string, runID domain.RunID) domain.ItemID {
	sum := sha256.Sum256([]byte(TaskKind + "\x00" + kind + "\x00" + string(runID)))
	return domain.ItemID("fake-publication-" + kind + "-" + hex.EncodeToString(sum[:]))
}

func ParseTerminalReason(reason string) (string, string, bool) {
	separator := "\n\n" + terminalBindingPrefix
	offset := strings.LastIndex(reason, separator)
	if offset < 0 || !strings.HasSuffix(reason, terminalBindingSuffix) {
		return "", "", false
	}
	digest := reason[offset+len(separator) : len(reason)-len(terminalBindingSuffix)]
	return reason[:offset], digest, digest != ""
}

// ValidateTerminalBinding accepts the current binding and the frozen
// pre-PRReference v1 encoding used by deployed fake-publication rows.
func ValidateTerminalBinding(task Task, item domain.AttentionItem) (domain.AttentionItem, error) {
	reason, got, ok := ParseTerminalReason(item.Reason)
	if !ok {
		return domain.AttentionItem{}, fmt.Errorf("missing task binding: %w", domain.ErrParentKeyMismatch)
	}
	item.Reason = reason
	want, err := TerminalDigest(task, item)
	if err != nil {
		return domain.AttentionItem{}, err
	}
	if got != string(want) {
		legacyWant, legacyErr := TerminalDigestBeforePRReference(task, item)
		if legacyErr != nil {
			return domain.AttentionItem{}, legacyErr
		}
		if got != string(legacyWant) {
			return domain.AttentionItem{}, fmt.Errorf(
				"task binding %q, want %q: %w", got, want, domain.ErrParentKeyMismatch,
			)
		}
	}
	return item, nil
}

func TerminalDigest(task Task, item domain.AttentionItem) (domain.Digest, error) {
	item.ItemVersion = 1
	item.Status = domain.StatusOpen
	item.DecidedAt = nil
	item.Timing = domain.TimingSummary{}
	// The terminal digest binds derived publication facts, not the instant at
	// which a recovery pass first persisted the item.
	item.CreatedAt = nil
	payload, err := json.Marshal(struct {
		Task Task                 `json:"task"`
		Item domain.AttentionItem `json:"item"`
	}{Task: task, Item: item})
	if err != nil {
		return "", fmt.Errorf("encode terminal binding: %w", err)
	}
	sum := sha256.Sum256(payload)
	return domain.Digest(contentaddr.Format(sum[:])), nil
}

// TerminalDigestBeforePRReference reproduces the immutable item shape used by
// fake-publication v1 before AttentionItem gained pr_reference.
func TerminalDigestBeforePRReference(task Task, item domain.AttentionItem) (domain.Digest, error) {
	type legacyAttentionItem struct {
		ID                          domain.ItemID                              `json:"id"`
		ProjectID                   domain.ProjectID                           `json:"project_id"`
		Subject                     domain.Subject                             `json:"subject"`
		Type                        domain.AttentionType                       `json:"type"`
		Priority                    domain.Priority                            `json:"priority"`
		Reason                      string                                     `json:"reason"`
		RequestedDecision           []domain.Action                            `json:"requested_decision"`
		EvidenceSnapshot            []domain.Artifact                          `json:"evidence_snapshot"`
		AgentClaims                 []domain.AgentClaim                        `json:"agent_claims"`
		ArtifactDigests             []domain.Digest                            `json:"artifact_digests"`
		PRHeadSHA                   string                                     `json:"pr_head_sha"`
		CommitPlanNotice            *domain.CommitPlanNoticeReason             `json:"commit_plan_notice"`
		BaseFreshness               *domain.BaseFreshness                      `json:"base_freshness"`
		ReadinessInvalidation       *domain.ReadinessInvalidation              `json:"readiness_invalidation"`
		ReviewRecoveryBinding       *domain.ReviewRecoveryBinding              `json:"review_recovery_binding"`
		ReviewConfigurationRecovery *domain.ReviewConfigurationRecoveryBinding `json:"review_configuration_recovery"`
		ItemVersion                 int                                        `json:"item_version"`
		InterruptionClass           domain.InterruptionClass                   `json:"interruption_class"`
		ConversationID              *domain.ConversationID                     `json:"conversation_id"`
		Timing                      domain.TimingSummary                       `json:"timing"`
		ExpiresWhen                 *time.Time                                 `json:"expires_when"`
		DecidedAt                   *time.Time                                 `json:"decided_at"`
		Posture                     *domain.HealthPosture                      `json:"posture"`
		BlockingSupersession        *domain.BlockingSupersession               `json:"blocking_supersession"`
		Status                      domain.ItemStatus                          `json:"status"`
	}
	item.ItemVersion = 1
	item.Status = domain.StatusOpen
	item.DecidedAt = nil
	item.Timing = domain.TimingSummary{}
	legacy := legacyAttentionItem{
		ID: item.ID, ProjectID: item.ProjectID, Subject: item.Subject, Type: item.Type,
		Priority: item.Priority, Reason: item.Reason, RequestedDecision: item.RequestedDecision,
		EvidenceSnapshot: item.EvidenceSnapshot, AgentClaims: item.AgentClaims,
		ArtifactDigests: item.ArtifactDigests, PRHeadSHA: item.PRHeadSHA,
		CommitPlanNotice: item.CommitPlanNotice, BaseFreshness: item.BaseFreshness,
		ReadinessInvalidation:       item.ReadinessInvalidation,
		ReviewRecoveryBinding:       item.ReviewRecoveryBinding,
		ReviewConfigurationRecovery: item.ReviewConfigurationRecovery,
		ItemVersion:                 item.ItemVersion, InterruptionClass: item.InterruptionClass,
		ConversationID: item.ConversationID, Timing: item.Timing, ExpiresWhen: item.ExpiresWhen,
		DecidedAt: item.DecidedAt, Posture: item.Posture,
		BlockingSupersession: item.BlockingSupersession, Status: item.Status,
	}
	payload, err := json.Marshal(struct {
		Task Task                `json:"task"`
		Item legacyAttentionItem `json:"item"`
	}{Task: task, Item: legacy})
	if err != nil {
		return "", fmt.Errorf("encode legacy terminal binding: %w", err)
	}
	sum := sha256.Sum256(payload)
	return domain.Digest(contentaddr.Format(sum[:])), nil
}

func ValidateCommitDate(commitDate time.Time) error {
	if commitDate.Before(time.Unix(0, 0).UTC()) {
		return errors.New("commit date must not precede the Unix epoch")
	}
	if !commitDate.Before(time.Unix(maxCommitTimestamp, 0).UTC()) {
		return errors.New("commit date must precede 2100-01-01 UTC")
	}
	return nil
}

func ValidateTaskUTF8(task Task) error {
	fields := []struct{ name, value string }{
		{"version", task.Version},
		{"run_id", string(task.RunID)},
		{"project_id", string(task.ProjectID)},
		{"store_epoch", task.StoreEpoch},
		{"workspace_dir", task.WorkspaceDir},
		{"handoff_dir", task.HandoffDir},
		{"handoff_digest", string(task.HandoffDigest)},
		{"repo", task.Repo},
		{"base_ref", task.BaseRef},
		{"base_sha", task.BaseSHA},
		{"recipe_digest", string(task.RecipeDigest)},
		{"recipe_path", task.RecipePath},
		{"trust_profile_digest", string(task.TrustProfileDigest)},
		{"verification_invocation_id", string(task.VerificationInvocationID)},
		{"publication_invocation_id", string(task.PublicationInvocationID)},
		{"title", task.Title},
		{"body", task.Body},
		{"operating_mode", task.OperatingMode},
	}
	for _, field := range fields {
		if !utf8.ValidString(field.value) {
			return fmt.Errorf("task %s is not valid UTF-8", field.name)
		}
	}
	for index, pattern := range task.AllowedPaths {
		if !utf8.ValidString(pattern) {
			return fmt.Errorf("task allowed_paths[%d] is not valid UTF-8", index)
		}
	}
	return nil
}

func ValidateAllowlist(patterns []string) error {
	for _, pattern := range patterns {
		for _, segment := range strings.Split(pattern, "/") {
			if segment == "**" {
				continue
			}
			if _, err := path.Match(segment, ""); err != nil {
				return fmt.Errorf("invalid candidate path allowlist pattern %q: %w", pattern, err)
			}
		}
	}
	return nil
}

func ValidateRecipePath(recipePath string) error {
	if recipePath == "" || strings.HasPrefix(recipePath, "/") ||
		strings.ContainsAny(recipePath, `:\*?[]`) {
		return errors.New("must be a relative slash path without colon, backslash, or glob metacharacters")
	}
	for _, component := range strings.Split(recipePath, "/") {
		if component == "" || component == "." || component == ".." {
			return fmt.Errorf("component %q is not allowed", component)
		}
	}
	return nil
}

func ValidateRepository(repo string) error {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return fmt.Errorf("repository %q is not owner/name", repo)
	}
	for _, segment := range []string{owner, name} {
		if strings.HasPrefix(segment, "-") || strings.HasPrefix(segment, ".") {
			return fmt.Errorf("repository %q is not transport-safe", repo)
		}
		for _, char := range segment {
			switch {
			case char >= 'a' && char <= 'z', char >= 'A' && char <= 'Z',
				char >= '0' && char <= '9', char == '-', char == '_', char == '.':
			default:
				return fmt.Errorf("repository %q is not transport-safe", repo)
			}
		}
	}
	return nil
}

func ValidateBranchName(name string) error {
	if name == "" || len(name) > 255 || strings.HasPrefix(name, "-") ||
		strings.Contains(name, "@{") || strings.HasSuffix(name, ".") {
		return errors.New("not a valid branch name")
	}
	for _, char := range name {
		if char <= ' ' || char == 0x7f || strings.ContainsRune(":?*[\\~^", char) {
			return errors.New("not a valid branch name")
		}
	}
	for _, component := range strings.Split(name, "/") {
		if component == "" || strings.HasPrefix(component, ".") || strings.HasSuffix(component, ".") ||
			strings.HasSuffix(component, ".lock") || strings.Contains(component, "..") {
			return errors.New("not a valid branch name")
		}
	}
	return nil
}

func ValidateCandidateBody(body string) error {
	const markerSuffixLen = len(" -->")
	maxCandidateBodyBytes := maxPullRequestBodyBytes - len("\n\n") - len(publicationMarkerPrefix) -
		(len("sha256:") + sha256.Size*2) - markerSuffixLen
	if len(body) > maxPullRequestBodyBytes {
		return fmt.Errorf("candidate body exceeds %d bytes", maxPullRequestBodyBytes)
	}
	if prose := strings.TrimRight(body, "\n"); prose != "" && len(prose) > maxCandidateBodyBytes {
		return fmt.Errorf(
			"candidate body exceeds %d bytes after reserving the publication identity marker",
			maxCandidateBodyBytes,
		)
	}
	for line := range strings.Lines(body) {
		if strings.HasPrefix(strings.TrimSpace(line), publicationMarkerPrefix) {
			return errors.New("candidate body contains a publication identity marker")
		}
	}
	// Publisher-owned sections (the disposition history and the
	// control-plane advisories, by marker or heading) are refused in
	// candidate prose, mirroring publish.ValidateCandidateBody.
	lower := strings.ToLower(body)
	for _, owned := range []string{
		"freeside:disposition-history", "freeside:control-plane-advisories",
		"## freeside control-plane advisories",
	} {
		if strings.Contains(lower, owned) {
			return errors.New("candidate body contains a publisher-owned section marker")
		}
	}
	return nil
}

func validCommitSHA(sha string) bool {
	if len(sha) != 40 {
		return false
	}
	for _, char := range sha {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}
