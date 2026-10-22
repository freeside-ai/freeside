package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/projectimage"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/strictjson"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

const (
	deputySettingsVersion  = "freeside-deputy-settings-v1"
	maxDeputySettingsBytes = 1 << 20
)

// deputySettings is <root>/daemon/settings.json: every binding the real-work
// script took from its environment, saved once by the operator. Each value
// lands in a durable admission record, so nothing here has a default: a key
// that is absent refuses the launch rather than letting the record attest to
// a value nobody wrote. An empty string is a value only where the field says
// so (judgment, manual_submission_config, build). The one key that may be
// absent is agent_tree.review_harness, tagged omitempty: every file written
// before it existed ran a Codex reviewer, so its absence means codex_cli.
type deputySettings struct {
	Version string `json:"version"`
	// Listen is the leased listener paired clients reach the run at.
	Listen   string `json:"listen"`
	SeedRoot string `json:"seed_root"`
	Images   struct {
		Agent    string `json:"agent"`
		Exporter string `json:"exporter"`
		Review   string `json:"review"`
	} `json:"images"`
	ApprovedRecipe domain.Digest `json:"approved_recipe"`
	Repository     struct {
		Repo         string           `json:"repo"`
		RepositoryID int64            `json:"repository_id"`
		BaseRef      string           `json:"base_ref"`
		BaseSHA      string           `json:"base_sha"`
		AllowedPaths []string         `json:"allowed_paths"`
		Checkout     string           `json:"checkout"`
		Project      domain.ProjectID `json:"project"`
	} `json:"repository"`
	Prompts struct {
		PromptPackage              string `json:"prompt_package"`
		SpecificationPromptPackage string `json:"specification_prompt_package"`
		RemediationPromptPackage   string `json:"remediation_prompt_package"`
		VendorInstructions         string `json:"vendor_instructions"`
	} `json:"prompts"`
	// AgentTree holds the admitted-agent checkout and the inputs auth adopt
	// needs to emit the tree that commit must hold. The dated inputs are
	// pinned because adoption defaults each to today, and a tree committed
	// on one day would stop matching on the next.
	AgentTree struct {
		Checkout           string `json:"checkout"`
		Commit             string `json:"commit"`
		AuthIdentity       string `json:"auth_identity"`
		ReviewAuthIdentity string `json:"review_auth_identity"`
		CostOwner          string `json:"cost_owner"`
		ReviewCostOwner    string `json:"review_cost_owner"`
		ClaudeAccount      string `json:"claude_account"`
		TermsBasisDate     string `json:"terms_basis_date"`
		PricingRevision    string `json:"pricing_revision"`
		OfferNotAfter      string `json:"offer_not_after"`
		AuthVolume         string `json:"auth_volume"`
		// ReviewHarness is the harness the reviewer line's agent runs (#397):
		// codex_cli, the meaning of an absent key, or claude_code. It picks
		// the review identity's adoption and the review.auth_mode it takes.
		ReviewHarness domain.HarnessClientKind `json:"review_harness,omitempty"`
	} `json:"agent_tree"`
	Review struct {
		InputRoot    string             `json:"input_root"`
		AuthMode     ward.CodexAuthMode `json:"auth_mode"`
		AuthSnapshot string             `json:"auth_snapshot"`
		Instructions string             `json:"instructions"`
		Model        string             `json:"model"`
		Effort       domain.EffortLevel `json:"effort"`
	} `json:"review"`
	// Writers is what each writer role launches with: the exact Claude
	// model id its offer names and the effort it asks. Every role names
	// both; adoption refuses a tree that would leave either to the CLI.
	Writers struct {
		Specifier   deputyWriterSelection `json:"specifier"`
		Implementer deputyWriterSelection `json:"implementer"`
		Remediator  deputyWriterSelection `json:"remediator"`
	} `json:"writers"`
	// Judgment is all empty (subscription judgments off) or all set.
	Judgment struct {
		ClaudeBin               string             `json:"claude_bin"`
		ClaudeSHA256            string             `json:"claude_sha256"`
		Model                   string             `json:"model"`
		Effort                  domain.EffortLevel `json:"effort"`
		PublicationAuthorPrompt string             `json:"publication_author_prompt"`
	} `json:"judgment"`
	// Prod names the installed prod daemon's App directories, used in place
	// under the rig lease (#1583), and its binary, whose publication formats
	// this build must match.
	Prod struct {
		AppStateDir       string `json:"app_state_dir"`
		AppCredentialsDir string `json:"app_credentials_dir"`
		InstalledDaemon   string `json:"installed_daemon"`
	} `json:"prod"`
	// Timeouts are Go durations; "0s" selects the ward default.
	Timeouts struct {
		WriterStop  string `json:"writer_stop"`
		WriterStall string `json:"writer_stall"`
	} `json:"timeouts"`
	// ManualSubmissionConfig is the client-task policy file, or "" for none.
	ManualSubmissionConfig string `json:"manual_submission_config"`
	// Build is the project-image rebuild input; all empty disables rebuilds.
	Build struct {
		Proxy        string   `json:"proxy"`
		BaseBuildRef string   `json:"base_build_ref"`
		DNS          []string `json:"dns"`
	} `json:"build"`
}

// loadedDeputySettings is the decoded file and the digest of its exact bytes,
// which the composition manifest names as the settings revision.
type loadedDeputySettings struct {
	deputySettings
	Digest   domain.Digest
	stopTime time.Duration
	stallFor time.Duration
}

// loadDeputySettings reads and validates the settings file. Every refusal is
// one line naming the field, so the operator can fix it without a log.
func loadDeputySettings(path string) (loadedDeputySettings, error) {
	body, err := readOwnerOnlyFile(path)
	if err != nil {
		return loadedDeputySettings{}, err
	}
	if err := ward.RejectDuplicateJSONKeys(body); err != nil {
		return loadedDeputySettings{}, fmt.Errorf("settings: %w", err)
	}
	var settings deputySettings
	if err := strictjson.Decode(body, &settings, strictjson.RejectInvalidUTF8, strictjson.Limit(maxDeputySettingsBytes)); err != nil {
		return loadedDeputySettings{}, fmt.Errorf("settings: %w", err)
	}
	if err := requireEveryKey(body, reflect.TypeOf(settings), ""); err != nil {
		return loadedDeputySettings{}, fmt.Errorf("settings: %w", err)
	}
	loaded := loadedDeputySettings{deputySettings: settings, Digest: domain.Digest(contentaddr.Sum(body))}
	if err := loaded.validate(); err != nil {
		return loadedDeputySettings{}, fmt.Errorf("settings: %w", err)
	}
	return loaded, nil
}

// readOwnerOnlyFile refuses a file another account could have written or
// read: the settings name credential locations and cost owners. The checks
// run on the opened file, not the path, so a swap between check and read
// cannot pass one file and read another. O_NONBLOCK keeps a FIFO at the
// path from hanging the open before the regular-file check refuses it.
func readOwnerOnlyFile(path string) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0) //nolint:gosec // G304: the operator's own settings file under the root they named
	if errors.Is(err, syscall.ELOOP) {
		return nil, fmt.Errorf("settings: %s is not a regular file", path)
	}
	if err != nil {
		return nil, fmt.Errorf("settings: %w", err)
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("settings: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("settings: %s is not a regular file", path)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		return nil, fmt.Errorf("settings: %s has mode %04o; want 0600", path, perm)
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); !ok || int(stat.Uid) != os.Getuid() {
		return nil, fmt.Errorf("settings: %s is not owned by this account", path)
	}
	body, err := io.ReadAll(io.LimitReader(f, maxDeputySettingsBytes+1))
	if err != nil {
		return nil, fmt.Errorf("settings: %w", err)
	}
	if len(body) > maxDeputySettingsBytes {
		return nil, fmt.Errorf("settings: %s exceeds %d bytes", path, maxDeputySettingsBytes)
	}
	return body, nil
}

// deputyWriterSelection is one writer role's model and effort.
type deputyWriterSelection struct {
	Model  string             `json:"model"`
	Effort domain.EffortLevel `json:"effort"`
}

// writerSelections lists the writer roles' selections by their adoption
// flag prefix, which is the role name.
func (s *loadedDeputySettings) writerSelections() []struct {
	role      domain.RoleName
	selection deputyWriterSelection
} {
	return []struct {
		role      domain.RoleName
		selection deputyWriterSelection
	}{
		{domain.RoleSpecifier, s.Writers.Specifier},
		{domain.RoleImplementer, s.Writers.Implementer},
		{domain.RoleRemediator, s.Writers.Remediator},
	}
}

// requireEveryKey refuses an object that omits, or sets to null, any key the
// struct declares, except that a key tagged omitempty may be omitted. Strict
// decoding already refuses unknown keys; this is the other half, because
// encoding/json leaves a missing field at its zero value.
func requireEveryKey(body []byte, t reflect.Type, prefix string) error {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(body, &object); err != nil {
		return fmt.Errorf("%s: want an object", strings.TrimSuffix(prefix, "."))
	}
	for i := range t.NumField() {
		field := t.Field(i)
		name, options, _ := strings.Cut(field.Tag.Get("json"), ",")
		raw, ok := object[name]
		if !ok && options == "omitempty" {
			continue
		}
		if !ok || string(raw) == "null" {
			return fmt.Errorf("%s%s is required", prefix, name)
		}
		if field.Type.Kind() == reflect.Struct {
			if err := requireEveryKey(raw, field.Type, prefix+name+"."); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *loadedDeputySettings) validate() error {
	if s.Version != deputySettingsVersion {
		return fmt.Errorf("version %q is not %s", s.Version, deputySettingsVersion)
	}
	if s.Listen == "" {
		return errors.New("listen is required")
	}
	for _, image := range []struct{ name, ref string }{
		{"images.agent", s.Images.Agent}, {"images.exporter", s.Images.Exporter}, {"images.review", s.Images.Review},
	} {
		if domain.ImageRef(image.ref).Validate() != nil || imageDigestFromRef(image.ref) == "" {
			return fmt.Errorf("%s is not a digest-pinned image reference", image.name)
		}
	}
	if !contentaddr.Valid(string(s.ApprovedRecipe)) {
		return errors.New("approved_recipe is not a sha256 digest")
	}
	if publish.ValidateRepository(s.Repository.Repo) != nil {
		return errors.New("repository.repo is not owner/name")
	}
	if s.Repository.RepositoryID <= 0 {
		return errors.New("repository.repository_id must be positive")
	}
	if publish.ValidateBranchName(s.Repository.BaseRef) != nil {
		return errors.New("repository.base_ref is not a branch name")
	}
	if publish.ValidateCommitSHA(s.Repository.BaseSHA) != nil {
		return errors.New("repository.base_sha is not a full commit SHA")
	}
	if len(s.Repository.AllowedPaths) == 0 {
		return errors.New("repository.allowed_paths is empty")
	}
	for _, path := range s.Repository.AllowedPaths {
		if strings.TrimSpace(path) == "" || strings.Contains(path, ",") {
			return errors.New("repository.allowed_paths holds an empty entry or a comma")
		}
	}
	// The Claude driver refuses anything else at start, after the preflight
	// has already kept a passing manifest; refuse it here, before either.
	if !engine.ExplicitAllowedPaths(s.Repository.AllowedPaths) {
		return errors.New("repository.allowed_paths must name explicit declared paths, such as src/**, not a match-everything or malformed pattern")
	}
	if s.Repository.Project == "" {
		return errors.New("repository.project is required")
	}
	if s.AgentTree.Commit == "" {
		return errors.New("agent_tree.commit is required")
	}
	for _, value := range []struct{ name, value string }{
		{"agent_tree.auth_identity", s.AgentTree.AuthIdentity},
		{"agent_tree.review_auth_identity", s.AgentTree.ReviewAuthIdentity},
		{"agent_tree.cost_owner", s.AgentTree.CostOwner},
		{"agent_tree.review_cost_owner", s.AgentTree.ReviewCostOwner},
		{"agent_tree.claude_account", s.AgentTree.ClaudeAccount},
		{"agent_tree.terms_basis_date", s.AgentTree.TermsBasisDate},
		{"agent_tree.pricing_revision", s.AgentTree.PricingRevision},
		{"agent_tree.offer_not_after", s.AgentTree.OfferNotAfter},
		{"agent_tree.auth_volume", s.AgentTree.AuthVolume},
		{"review.model", s.Review.Model},
	} {
		if value.value == "" {
			return fmt.Errorf("%s is required", value.name)
		}
	}
	if s.AgentTree.AuthIdentity == s.AgentTree.ReviewAuthIdentity {
		return errors.New("agent_tree.review_auth_identity must differ from agent_tree.auth_identity")
	}
	if err := validateDeputyReviewHarness(s.deputySettings); err != nil {
		return err
	}
	if s.Review.Effort == "" {
		return errors.New("review.effort is required")
	}
	for _, writer := range s.writerSelections() {
		if writer.selection.Model == "" || writer.selection.Effort == "" {
			return fmt.Errorf("writers.%s.model and writers.%s.effort are required", writer.role, writer.role)
		}
	}
	judgment := []string{
		s.Judgment.ClaudeBin, s.Judgment.ClaudeSHA256, s.Judgment.Model, string(s.Judgment.Effort),
		s.Judgment.PublicationAuthorPrompt,
	}
	set := 0
	for _, value := range judgment {
		if value != "" {
			set++
		}
	}
	if set != 0 && set != len(judgment) {
		return errors.New("judgment fields must be all empty or all set")
	}
	paths := []struct{ name, value string }{
		{"seed_root", s.SeedRoot},
		{"repository.checkout", s.Repository.Checkout},
		{"prompts.prompt_package", s.Prompts.PromptPackage},
		{"prompts.specification_prompt_package", s.Prompts.SpecificationPromptPackage},
		{"prompts.remediation_prompt_package", s.Prompts.RemediationPromptPackage},
		{"prompts.vendor_instructions", s.Prompts.VendorInstructions},
		{"agent_tree.checkout", s.AgentTree.Checkout},
		{"review.input_root", s.Review.InputRoot},
		{"review.auth_snapshot", s.Review.AuthSnapshot},
		{"review.instructions", s.Review.Instructions},
		{"prod.app_state_dir", s.Prod.AppStateDir},
		{"prod.app_credentials_dir", s.Prod.AppCredentialsDir},
		{"prod.installed_daemon", s.Prod.InstalledDaemon},
	}
	if set != 0 {
		paths = append(paths,
			struct{ name, value string }{"judgment.claude_bin", s.Judgment.ClaudeBin},
			struct{ name, value string }{"judgment.publication_author_prompt", s.Judgment.PublicationAuthorPrompt})
	}
	if s.ManualSubmissionConfig != "" {
		paths = append(paths, struct{ name, value string }{"manual_submission_config", s.ManualSubmissionConfig})
	}
	for _, path := range paths {
		if !filepath.IsAbs(path.value) {
			return fmt.Errorf("%s must be an absolute path", path.name)
		}
		if filepath.Clean(path.value) != path.value {
			return fmt.Errorf("%s must be a clean path", path.name)
		}
	}
	if s.Prompts.VendorInstructions == string(filepath.Separator) {
		return errors.New("prompts.vendor_instructions must not be the root directory")
	}
	var err error
	if s.stopTime, err = nonNegativeDuration(s.Timeouts.WriterStop); err != nil {
		return fmt.Errorf("timeouts.writer_stop: %w", err)
	}
	if s.stallFor, err = nonNegativeDuration(s.Timeouts.WriterStall); err != nil {
		return fmt.Errorf("timeouts.writer_stall: %w", err)
	}
	// As in the script: without a base ref the proxy still names preflight's
	// build egress, but there is no rebuild for DNS servers to serve.
	if s.Build.BaseBuildRef == "" {
		if len(s.Build.DNS) != 0 {
			return errors.New("build.dns requires build.base_build_ref")
		}
		if err := projectimage.ValidateBuildProxy(s.Build.Proxy); err != nil {
			return fmt.Errorf("build.proxy: %w", err)
		}
	} else if err := s.rebuildInputs().Validate(); err != nil {
		return fmt.Errorf("build: %w", err)
	}
	return nil
}

// rebuildInputs are the daemon's rebuild flags, which it takes only with a
// base ref.
func (s *loadedDeputySettings) rebuildInputs() projectimage.BuildInputs {
	if s.Build.BaseBuildRef == "" {
		return projectimage.BuildInputs{}
	}
	return projectimage.BuildInputs{BaseBuildRef: s.Build.BaseBuildRef, BuildProxy: s.Build.Proxy, DNS: s.Build.DNS}
}

func nonNegativeDuration(raw string) (time.Duration, error) {
	value, err := time.ParseDuration(raw)
	if err != nil {
		return 0, errors.New("not a duration such as 10m or 0s")
	}
	if value < 0 {
		return 0, errors.New("must not be negative")
	}
	return value, nil
}

// reviewHarness is agent_tree.review_harness, with an absent key read as
// codex_cli.
func (s deputySettings) reviewHarness() domain.HarnessClientKind {
	if s.AgentTree.ReviewHarness == "" {
		return domain.HarnessClientCodexCLI
	}
	return s.AgentTree.ReviewHarness
}

// validateDeputyReviewHarness pairs the reviewer's harness with the only
// review.auth_mode the deputy can adopt for it: a Codex reviewer's
// subscription store, or a Claude reviewer's setup token.
func validateDeputyReviewHarness(s deputySettings) error {
	switch s.reviewHarness() {
	case domain.HarnessClientCodexCLI:
		if s.Review.AuthMode != ward.CodexAuthSubscription {
			return errors.New("review.auth_mode must be subscription; api_key enrollment is not supported yet (#1677)")
		}
		return nil
	case domain.HarnessClientClaudeCode:
		if s.Review.AuthMode != ward.CodexAuthSetupToken {
			return errors.New("review.auth_mode must be setup_token with agent_tree.review_harness claude_code")
		}
		return nil
	}
	return fmt.Errorf("agent_tree.review_harness %q is not codex_cli or claude_code", s.AgentTree.ReviewHarness)
}
