package domain

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/strictjson"
)

// InstanceSettingsSection names one section of the instance settings: the
// host and build bindings every project's runs share (plan §3.2, the
// instance scope; issue #2082). A revision sets or clears exactly one
// section, so each section validates alone.
type InstanceSettingsSection string

const (
	InstanceSectionHostTools              InstanceSettingsSection = "host_tools"
	InstanceSectionImages                 InstanceSettingsSection = "images"
	InstanceSectionSeedRoot               InstanceSettingsSection = "seed_root"
	InstanceSectionPrompts                InstanceSettingsSection = "prompts"
	InstanceSectionAgentTree              InstanceSettingsSection = "agent_tree"
	InstanceSectionReview                 InstanceSettingsSection = "review"
	InstanceSectionJudgment               InstanceSettingsSection = "judgment"
	InstanceSectionTimeouts               InstanceSettingsSection = "timeouts"
	InstanceSectionManualSubmissionConfig InstanceSettingsSection = "manual_submission_config"
	InstanceSectionBuild                  InstanceSettingsSection = "build"
	InstanceSectionMacosVerification      InstanceSettingsSection = "macos_verification"
	// InstanceSectionExecutionPause is the operator's pause of prod's
	// execution. Only freesided execution pause and resume write it; it
	// configures no run.
	InstanceSectionExecutionPause InstanceSettingsSection = "execution_pause"
	// InstanceSectionCordonedLaunches bounds how many cordoned launches run
	// at once (#3471). The admitting transaction reads it on each
	// admission, so a change applies to the next one without a restart.
	InstanceSectionCordonedLaunches InstanceSettingsSection = "cordoned_launches"
	// InstanceSectionExecution is the operator's switch that lets the
	// installed daemon compose its driver from the stored settings (#2083).
	// Unset reads as off, and settings import never writes it, so an import
	// alone starts no work. prod reads it only at start.
	InstanceSectionExecution InstanceSettingsSection = "execution"
	// InstanceSectionImageBuilds bounds how many project image builds run
	// at once (#3210). Unset reads as one. The daemon reads it only at
	// start.
	InstanceSectionImageBuilds InstanceSettingsSection = "image_builds"
	// InstanceSectionAgentBase names the one repository the daemon may build
	// the Claude agent base from (plan §5.7, Agent-base upkeep). Unset, no
	// base is built and project images keep the base they have.
	InstanceSectionAgentBase InstanceSettingsSection = "agent_base"
)

// AllInstanceSettingsSections lists every InstanceSettingsSection in the
// order a record shows them; it is the single registration point.
var AllInstanceSettingsSections = []InstanceSettingsSection{
	InstanceSectionHostTools, InstanceSectionImages, InstanceSectionSeedRoot, InstanceSectionPrompts,
	InstanceSectionAgentTree, InstanceSectionReview, InstanceSectionJudgment, InstanceSectionTimeouts,
	InstanceSectionManualSubmissionConfig, InstanceSectionBuild, InstanceSectionMacosVerification,
	InstanceSectionExecutionPause, InstanceSectionCordonedLaunches, InstanceSectionExecution,
	InstanceSectionImageBuilds, InstanceSectionAgentBase,
}

func (s InstanceSettingsSection) valid() bool {
	switch s {
	case InstanceSectionHostTools, InstanceSectionImages, InstanceSectionSeedRoot, InstanceSectionPrompts,
		InstanceSectionAgentTree, InstanceSectionReview, InstanceSectionJudgment, InstanceSectionTimeouts,
		InstanceSectionManualSubmissionConfig, InstanceSectionBuild, InstanceSectionMacosVerification,
		InstanceSectionExecutionPause, InstanceSectionCordonedLaunches, InstanceSectionExecution,
		InstanceSectionImageBuilds, InstanceSectionAgentBase:
		return true
	default:
		return false
	}
}

// ParseInstanceSettingsSection returns the section a name names.
func ParseInstanceSettingsSection(name string) (InstanceSettingsSection, error) {
	if s := InstanceSettingsSection(name); s.valid() {
		return s, nil
	}
	return "", fmt.Errorf("instance settings section %q: %w", name, ErrUnknownSettingsSection)
}

// Optional reports whether the instance settings are complete without the
// section, so a revision may clear it.
func (s InstanceSettingsSection) Optional() bool {
	switch s {
	case InstanceSectionJudgment, InstanceSectionManualSubmissionConfig, InstanceSectionMacosVerification,
		InstanceSectionExecutionPause, InstanceSectionCordonedLaunches, InstanceSectionExecution,
		InstanceSectionImageBuilds, InstanceSectionAgentBase:
		return true
	case InstanceSectionHostTools, InstanceSectionImages, InstanceSectionSeedRoot, InstanceSectionPrompts,
		InstanceSectionAgentTree, InstanceSectionReview, InstanceSectionTimeouts, InstanceSectionBuild:
		return false
	}
	return false
}

// HostToolsSettings holds the absolute paths of the host tools a run starts.
// The installed daemon runs under launchd, whose PATH need not hold them, so
// the container tool is named by path rather than looked up.
type HostToolsSettings struct {
	Container string `json:"container"`
}

// InstanceImageSettings are the digest-pinned images every project's runs
// share. Each project names its own agent image.
type InstanceImageSettings struct {
	Exporter ImageRef `json:"exporter"`
	Review   ImageRef `json:"review"`
}

// PromptSettings are the prompt packages and the vendor instructions the
// writers launch with.
type PromptSettings struct {
	PromptPackage              string `json:"prompt_package"`
	SpecificationPromptPackage string `json:"specification_prompt_package"`
	RemediationPromptPackage   string `json:"remediation_prompt_package"`
	VendorInstructions         string `json:"vendor_instructions"`
}

// AgentTreeSettings is the admitted-agent checkout and the commit it must
// hold. The adoption inputs that produce that commit are stored in the
// identity scope when the deputy adopts them, not here.
type AgentTreeSettings struct {
	Checkout string `json:"checkout"`
	Commit   string `json:"commit"`
}

// InstanceReviewSettings are the reviewer's inputs. AuthMode is the credential form
// the reviewer takes; whether a mode suits the reviewer's harness is checked
// once its line is resolved, as it is for the daemon's flags.
type InstanceReviewSettings struct {
	InputRoot    string `json:"input_root"`
	AuthMode     string `json:"auth_mode"`
	AuthSnapshot string `json:"auth_snapshot"`
	Instructions string `json:"instructions"`
}

// JudgmentSettings name the pinned Claude CLI subscription judgments run and
// the publication author's prompt. The section is absent when judgments are
// off.
type JudgmentSettings struct {
	ClaudeBin               string `json:"claude_bin"`
	ClaudeSHA256            string `json:"claude_sha256"`
	PublicationAuthorPrompt string `json:"publication_author_prompt"`
}

// TimeoutSettings are Go durations; "0s" selects the ward default.
type TimeoutSettings struct {
	WriterStop  string `json:"writer_stop"`
	WriterStall string `json:"writer_stall"`
}

// Durations parses both timeouts, naming the field that does not parse or is
// negative.
func (t TimeoutSettings) Durations() (writerStop, writerStall time.Duration, err error) {
	if writerStop, err = nonNegativeDuration(t.WriterStop); err != nil {
		return 0, 0, fmt.Errorf("timeouts.writer_stop: %w", err)
	}
	if writerStall, err = nonNegativeDuration(t.WriterStall); err != nil {
		return 0, 0, fmt.Errorf("timeouts.writer_stall: %w", err)
	}
	return writerStop, writerStall, nil
}

// BuildSettings are the project-image rebuild inputs; all empty disables
// rebuilds. Without a base ref the proxy still names preflight's build
// egress, but there is no rebuild for DNS servers to serve.
type BuildSettings struct {
	Proxy        string   `json:"proxy"`
	BaseBuildRef string   `json:"base_build_ref"`
	DNS          []string `json:"dns"`
}

// MacosVerificationSettings names the macOS verification executor macos_host
// recipe commands run on (#2978): the same values as the -macverify-* flags,
// for configuration that comes from settings rather than flags. It feeds the
// configuration the executor's conformance pass proves and a later restore
// re-checks. Every key but port_base is required; the slots have no
// default. port_base alone may be omitted, for macverify.DefaultPortBase: no
// port base disables anything, and slot-run refuses a job whose ports.conf
// disagrees with it.
type MacosVerificationSettings struct {
	Root       string   `json:"root"`
	Slots      []string `json:"slots"`
	XcodeBuild string   `json:"xcode_build"`
	Swift      string   `json:"swift"`
	PortBase   int      `json:"port_base,omitempty"`
}

// Validate refuses settings that cannot name an executor, one line naming
// the field. It refuses an empty value rather than reading it as "none", so
// a half-written section cannot quietly disable the executor. The
// toolchain tokens' formats stay with macverify.New, which enforces them at
// daemon start for the flags too. The deputy settings file's check calls it,
// so the file and the store hold one rule.
func (m MacosVerificationSettings) Validate() error {
	if !filepath.IsAbs(m.Root) {
		return errors.New("macos_verification.root must be an absolute path")
	}
	if filepath.Clean(m.Root) != m.Root {
		return errors.New("macos_verification.root must be a clean path")
	}
	if len(m.Slots) == 0 {
		return errors.New("macos_verification.slots is empty")
	}
	for i, slot := range m.Slots {
		if slot == "" {
			return errors.New("macos_verification.slots holds an empty name")
		}
		if slices.Contains(m.Slots[:i], slot) {
			return fmt.Errorf("macos_verification.slots names %q twice", slot)
		}
	}
	if m.XcodeBuild == "" {
		return errors.New("macos_verification.xcode_build is required")
	}
	if m.Swift == "" {
		return errors.New("macos_verification.swift is required")
	}
	return nil
}

// ExecutionPauseState is how far an operator's pause of prod's execution has
// gone. The zero value is invalid.
type ExecutionPauseState string

const (
	// ExecutionPauseRequested: the operator asked prod to pause; it admits
	// no new work and waits for running work to end.
	ExecutionPauseRequested ExecutionPauseState = "requested"
	// ExecutionPauseReleased: running work ended and prod gave up the App
	// authority; it starts driverless until the pause clears.
	ExecutionPauseReleased ExecutionPauseState = "released"
)

// AllExecutionPauseStates is the single registration point for
// ExecutionPauseState.
var AllExecutionPauseStates = []ExecutionPauseState{ExecutionPauseRequested, ExecutionPauseReleased}

func (s ExecutionPauseState) valid() bool {
	switch s {
	case ExecutionPauseRequested, ExecutionPauseReleased:
		return true
	default:
		return false
	}
}

// ExecutionPauseSettings is the execution_pause section's value. The section
// is absent when no pause is in force.
type ExecutionPauseSettings struct {
	State ExecutionPauseState `json:"state"`
}

// CordonedLaunchSettings is the cordoned_launches section's value: the most
// cordoned launches that may run at once. A cordoned launch has no VM, so it
// reserves nothing against the memory budget; this count is what bounds it.
// The section unset means no cordoned launch starts.
type CordonedLaunchSettings struct {
	MaxConcurrent int `json:"max_concurrent"`
}

// ImageBuildSettings is the image_builds section's value: the most project
// image builds that may run at once. Builds still take turns on the host's
// one builder VM and on one repository's image pointer; the limit bounds
// the rest of a build (the recipe read, the probes, the publish).
type ImageBuildSettings struct {
	MaxConcurrent int `json:"max_concurrent"`
}

// ImageBuildLimit is the most project image builds that may run at once:
// the image_builds section's value, or one when the section is unset.
func (s InstanceSettings) ImageBuildLimit() int {
	if s.ImageBuilds == nil {
		return 1
	}
	return s.ImageBuilds.MaxConcurrent
}

// EnabledSettings is the value of a switch section: the instance's
// execution section and a project's enabled section. The key is required,
// so a value always says which way the switch is set; an unset section
// reads as off.
type EnabledSettings struct {
	Enabled bool `json:"enabled"`
}

// AgentBaseSettings is the agent_base section's value: the forge identity
// and slug of the repository whose AgentBaseSourceDir defines the Claude
// agent base. The ID is the binding; the slug is what the daemon compares
// with its own project's repository and shows the operator.
type AgentBaseSettings struct {
	RepositoryID int64  `json:"repository_id"`
	Repository   string `json:"repository"`
}

// InstanceSettings is every instance section's value. An optional section is
// a pointer, nil when unset. The rules here are shape rules only; the rules
// of the packages that use a value (the allowed review auth modes, the build
// proxy's form) are re-run by the settings gate in daemoncompose.
type InstanceSettings struct {
	HostTools              HostToolsSettings          `json:"host_tools"`
	Images                 InstanceImageSettings      `json:"images"`
	SeedRoot               string                     `json:"seed_root"`
	Prompts                PromptSettings             `json:"prompts"`
	AgentTree              AgentTreeSettings          `json:"agent_tree"`
	Review                 InstanceReviewSettings     `json:"review"`
	Judgment               *JudgmentSettings          `json:"judgment"`
	Timeouts               TimeoutSettings            `json:"timeouts"`
	ManualSubmissionConfig *string                    `json:"manual_submission_config"`
	Build                  BuildSettings              `json:"build"`
	MacosVerification      *MacosVerificationSettings `json:"macos_verification"`
	ExecutionPause         *ExecutionPauseSettings    `json:"execution_pause"`
	CordonedLaunches       *CordonedLaunchSettings    `json:"cordoned_launches"`
	Execution              *EnabledSettings           `json:"execution"`
	ImageBuilds            *ImageBuildSettings        `json:"image_builds"`
	AgentBase              *AgentBaseSettings         `json:"agent_base"`
}

// Validate checks every section that is set and that every required one
// holds a value.
func (s InstanceSettings) Validate() error {
	for _, section := range AllInstanceSettingsSections {
		if section.Optional() && s.unset(section) {
			continue
		}
		if err := s.ValidateSection(section); err != nil {
			return err
		}
	}
	return nil
}

// unset reports whether an optional section is nil.
func (s InstanceSettings) unset(section InstanceSettingsSection) bool {
	switch section {
	case InstanceSectionJudgment:
		return s.Judgment == nil
	case InstanceSectionManualSubmissionConfig:
		return s.ManualSubmissionConfig == nil
	case InstanceSectionMacosVerification:
		return s.MacosVerification == nil
	case InstanceSectionExecutionPause:
		return s.ExecutionPause == nil
	case InstanceSectionCordonedLaunches:
		return s.CordonedLaunches == nil
	case InstanceSectionExecution:
		return s.Execution == nil
	case InstanceSectionImageBuilds:
		return s.ImageBuilds == nil
	case InstanceSectionAgentBase:
		return s.AgentBase == nil
	case InstanceSectionHostTools, InstanceSectionImages, InstanceSectionSeedRoot, InstanceSectionPrompts,
		InstanceSectionAgentTree, InstanceSectionReview, InstanceSectionTimeouts, InstanceSectionBuild:
		return false
	}
	return false
}

// ValidateSection checks one section's value alone. Every cross-field rule
// the deputy settings file enforces on a stored field sits inside one
// section, so a section needs no other to validate. Each refusal names the
// field.
func (s InstanceSettings) ValidateSection(section InstanceSettingsSection) error {
	switch section {
	case InstanceSectionHostTools:
		return requireCleanAbsolutePath("host_tools.container", s.HostTools.Container)
	case InstanceSectionImages:
		if s.Images.Exporter.Validate() != nil {
			return errors.New("images.exporter is not a digest-pinned image reference")
		}
		if s.Images.Review.Validate() != nil {
			return errors.New("images.review is not a digest-pinned image reference")
		}
		return nil
	case InstanceSectionSeedRoot:
		return requireCleanAbsolutePath("seed_root", s.SeedRoot)
	case InstanceSectionPrompts:
		return validatePromptSettings(s.Prompts)
	case InstanceSectionAgentTree:
		if err := requireCleanAbsolutePath("agent_tree.checkout", s.AgentTree.Checkout); err != nil {
			return err
		}
		if s.AgentTree.Commit == "" {
			return errors.New("agent_tree.commit is required")
		}
		return nil
	case InstanceSectionReview:
		return validateReviewSettings(s.Review)
	case InstanceSectionJudgment:
		return validateJudgmentSettings(s.Judgment)
	case InstanceSectionTimeouts:
		_, _, err := s.Timeouts.Durations()
		return err
	case InstanceSectionManualSubmissionConfig:
		if s.ManualSubmissionConfig == nil {
			return errors.New("manual_submission_config is required")
		}
		return requireCleanAbsolutePath("manual_submission_config", *s.ManualSubmissionConfig)
	case InstanceSectionBuild:
		if s.Build.BaseBuildRef == "" && len(s.Build.DNS) != 0 {
			return errors.New("build.dns requires build.base_build_ref")
		}
		return nil
	case InstanceSectionMacosVerification:
		if s.MacosVerification == nil {
			return errors.New("macos_verification is required")
		}
		return s.MacosVerification.Validate()
	case InstanceSectionExecutionPause:
		if s.ExecutionPause == nil {
			return errors.New("execution_pause is required")
		}
		if !s.ExecutionPause.State.valid() {
			return fmt.Errorf("execution_pause.state %q is not requested or released", s.ExecutionPause.State)
		}
		return nil
	case InstanceSectionCordonedLaunches:
		if s.CordonedLaunches == nil {
			return errors.New("cordoned_launches is required")
		}
		if s.CordonedLaunches.MaxConcurrent < 1 {
			return fmt.Errorf("cordoned_launches.max_concurrent %d must be at least 1", s.CordonedLaunches.MaxConcurrent)
		}
		return nil
	case InstanceSectionExecution:
		if s.Execution == nil {
			return errors.New("execution is required")
		}
		return nil
	case InstanceSectionImageBuilds:
		if s.ImageBuilds == nil {
			return errors.New("image_builds is required")
		}
		if s.ImageBuilds.MaxConcurrent < 1 {
			return fmt.Errorf("image_builds.max_concurrent %d must be at least 1", s.ImageBuilds.MaxConcurrent)
		}
		return nil
	case InstanceSectionAgentBase:
		if s.AgentBase == nil {
			return errors.New("agent_base is required")
		}
		if err := validAgentBaseSource(s.AgentBase.RepositoryID, s.AgentBase.Repository); err != nil {
			return fmt.Errorf("agent_base: %w", err)
		}
		return nil
	}
	return fmt.Errorf("instance settings section %q: %w", section, ErrUnknownSettingsSection)
}

func validatePromptSettings(p PromptSettings) error {
	for _, path := range []struct{ name, value string }{
		{"prompts.prompt_package", p.PromptPackage},
		{"prompts.specification_prompt_package", p.SpecificationPromptPackage},
		{"prompts.remediation_prompt_package", p.RemediationPromptPackage},
		{"prompts.vendor_instructions", p.VendorInstructions},
	} {
		if err := requireCleanAbsolutePath(path.name, path.value); err != nil {
			return err
		}
	}
	if p.VendorInstructions == string(filepath.Separator) {
		return errors.New("prompts.vendor_instructions must not be the root directory")
	}
	return nil
}

func validateReviewSettings(r InstanceReviewSettings) error {
	for _, path := range []struct{ name, value string }{
		{"review.input_root", r.InputRoot},
		{"review.auth_snapshot", r.AuthSnapshot},
		{"review.instructions", r.Instructions},
	} {
		if err := requireCleanAbsolutePath(path.name, path.value); err != nil {
			return err
		}
	}
	if r.AuthMode == "" {
		return errors.New("review.auth_mode is required")
	}
	return nil
}

// validateJudgmentSettings holds the file's all-or-nothing rule as presence:
// a set section names every field, and judgments off is no section.
func validateJudgmentSettings(j *JudgmentSettings) error {
	if j == nil {
		return errors.New("judgment is required")
	}
	if err := requireCleanAbsolutePath("judgment.claude_bin", j.ClaudeBin); err != nil {
		return err
	}
	if j.ClaudeSHA256 == "" {
		return errors.New("judgment.claude_sha256 is required")
	}
	return requireCleanAbsolutePath("judgment.publication_author_prompt", j.PublicationAuthorPrompt)
}

// field returns a pointer to the field that holds section, for decoding a
// revision's value into and encoding it from.
func (s *InstanceSettings) field(section InstanceSettingsSection) any {
	switch section {
	case InstanceSectionHostTools:
		return &s.HostTools
	case InstanceSectionImages:
		return &s.Images
	case InstanceSectionSeedRoot:
		return &s.SeedRoot
	case InstanceSectionPrompts:
		return &s.Prompts
	case InstanceSectionAgentTree:
		return &s.AgentTree
	case InstanceSectionReview:
		return &s.Review
	case InstanceSectionJudgment:
		return &s.Judgment
	case InstanceSectionTimeouts:
		return &s.Timeouts
	case InstanceSectionManualSubmissionConfig:
		return &s.ManualSubmissionConfig
	case InstanceSectionBuild:
		return &s.Build
	case InstanceSectionMacosVerification:
		return &s.MacosVerification
	case InstanceSectionExecutionPause:
		return &s.ExecutionPause
	case InstanceSectionCordonedLaunches:
		return &s.CordonedLaunches
	case InstanceSectionExecution:
		return &s.Execution
	case InstanceSectionImageBuilds:
		return &s.ImageBuilds
	case InstanceSectionAgentBase:
		return &s.AgentBase
	}
	return nil
}

// SectionValue encodes one section's value as a revision stores it, nil for
// an unset optional section.
func (s InstanceSettings) SectionValue(section InstanceSettingsSection) (json.RawMessage, error) {
	if !section.valid() {
		return nil, fmt.Errorf("instance settings section %q: %w", section, ErrUnknownSettingsSection)
	}
	if section.Optional() && s.unset(section) {
		return nil, nil
	}
	return json.Marshal(s.field(section))
}

// DecodeInstanceSection strictly decodes a non-null value into a fresh
// settings value's section and validates it there, returning that settings
// value and the section's canonical encoding.
func DecodeInstanceSection(section InstanceSettingsSection, raw json.RawMessage) (InstanceSettings, json.RawMessage, error) {
	var s InstanceSettings
	if !section.valid() {
		return InstanceSettings{}, nil, fmt.Errorf("instance settings section %q: %w", section, ErrUnknownSettingsSection)
	}
	if err := decodeSettingsValue(raw, s.field(section)); err != nil {
		return InstanceSettings{}, nil, fmt.Errorf("%s: %w", section, err)
	}
	if err := s.ValidateSection(section); err != nil {
		return InstanceSettings{}, nil, err
	}
	canonical, err := json.Marshal(s.field(section))
	if err != nil {
		return InstanceSettings{}, nil, fmt.Errorf("%s: %w", section, err)
	}
	return s, canonical, nil
}

// InstanceSettingsInput carries the caller-supplied fields of an
// InstanceSettingsRevision. It has no Revision: the store assigns it inside
// the write transaction.
type InstanceSettingsInput struct {
	Section InstanceSettingsSection
	// Value is the section's new value as JSON; nil (or null) clears it.
	Value json.RawMessage
	// SetBy is who set it: cli:<username> for an operator command.
	SetBy string
}

// InstanceSettingsRevision is one change to one instance section: it sets the
// section's value, or clears it when Value is null. Revisions number the
// instance's changes from 1 and are never rewritten, so a revision number
// names one state of the instance settings for good: the latest value of each
// section among revisions 1 to N.
type InstanceSettingsRevision struct {
	Revision int                     `json:"revision"`
	Section  InstanceSettingsSection `json:"section"`
	Value    json.RawMessage         `json:"value"`
	SetBy    string                  `json:"set_by"`
	SetAt    time.Time               `json:"set_at"`
}

// NewInstanceSettingsRevision builds a validated revision: a set value is
// strictly decoded, checked against its section's rules, and stored in its
// canonical encoding, and the timestamp is normalized to UTC.
func NewInstanceSettingsRevision(in InstanceSettingsInput, revision int, setAt time.Time) (InstanceSettingsRevision, error) {
	r := InstanceSettingsRevision{Revision: revision, Section: in.Section, SetBy: in.SetBy, SetAt: setAt.UTC()}
	if !settingsValueCleared(in.Value) && in.Section.valid() {
		_, canonical, err := DecodeInstanceSection(in.Section, in.Value)
		if err != nil {
			return InstanceSettingsRevision{}, fmt.Errorf("instance settings revision %d: %w", revision, err)
		}
		r.Value = canonical
	}
	if err := r.Validate(); err != nil {
		return InstanceSettingsRevision{}, err
	}
	return r, nil
}

// Validate reports whether the revision is well-formed: a known section, a
// value its section accepts or a clear of an optional section, and who set
// it and when. The store re-runs it on every decode.
func (r InstanceSettingsRevision) Validate() error {
	if r.Revision < 1 {
		return fmt.Errorf("instance settings revision %d: %w", r.Revision, ErrNonPositive)
	}
	if !r.Section.valid() {
		return fmt.Errorf("instance settings revision %d section %q: %w", r.Revision, r.Section, ErrUnknownSettingsSection)
	}
	if r.Cleared() {
		if !r.Section.Optional() {
			return fmt.Errorf("instance settings revision %d: %s: %w", r.Revision, r.Section, ErrRequiredSettingsSectionCleared)
		}
	} else if _, _, err := DecodeInstanceSection(r.Section, r.Value); err != nil {
		return fmt.Errorf("instance settings revision %d: %w", r.Revision, err)
	}
	if err := validateSettingsSetter(r.SetBy, r.SetAt); err != nil {
		return fmt.Errorf("instance settings revision %d %w", r.Revision, err)
	}
	return nil
}

// Cleared reports whether the revision clears its section.
func (r InstanceSettingsRevision) Cleared() bool { return settingsValueCleared(r.Value) }

// InstanceSettingsRecord is the instance settings folded from its revisions:
// the head revision and the revision that holds each set section, in
// AllInstanceSettingsSections order.
type InstanceSettingsRecord struct {
	Revision int                        `json:"revision"`
	Sections []InstanceSettingsRevision `json:"sections"`
}

// NewInstanceSettingsRecord folds revisions, which must number 1, 2, 3, and so
// on in order: a later change to a section replaces an earlier one, and a
// clear removes it.
func NewInstanceSettingsRecord(revisions []InstanceSettingsRevision) (InstanceSettingsRecord, error) {
	var record InstanceSettingsRecord
	held := make(map[InstanceSettingsSection]InstanceSettingsRevision)
	for _, r := range revisions {
		if r.Revision != record.Revision+1 {
			return InstanceSettingsRecord{}, fmt.Errorf("instance settings revision %d after %d: %w",
				r.Revision, record.Revision, ErrSettingsRevisionOrder)
		}
		record.Revision = r.Revision
		if r.Cleared() {
			delete(held, r.Section)
		} else {
			held[r.Section] = r
		}
	}
	for _, section := range AllInstanceSettingsSections {
		if r, ok := held[section]; ok {
			record.Sections = append(record.Sections, r)
		}
	}
	return record, nil
}

// Settings is the record's settings. It refuses a record that lacks a
// required section and names the section.
func (r InstanceSettingsRecord) Settings() (InstanceSettings, error) {
	var settings InstanceSettings
	set := make([]InstanceSettingsSection, 0, len(r.Sections))
	for _, revision := range r.Sections {
		if err := decodeSettingsValue(revision.Value, settings.field(revision.Section)); err != nil {
			return InstanceSettings{}, fmt.Errorf("instance settings revision %d: %s: %w", revision.Revision, revision.Section, err)
		}
		set = append(set, revision.Section)
	}
	for _, section := range AllInstanceSettingsSections {
		if !section.Optional() && !slices.Contains(set, section) {
			return InstanceSettings{}, fmt.Errorf("instance settings: %s: %w", section, ErrSettingsSectionMissing)
		}
	}
	if err := settings.Validate(); err != nil {
		return InstanceSettings{}, fmt.Errorf("instance settings: %w", err)
	}
	return settings, nil
}

// ExecutionPause is the execution pause the record holds, or nil when none
// is in force. It reads only that section, so it works on a record that
// lacks the sections a run needs, as prod's does until it composes from
// settings.
func (r InstanceSettingsRecord) ExecutionPause() (*ExecutionPauseSettings, error) {
	settings, err := r.sectionAlone(InstanceSectionExecutionPause)
	return settings.ExecutionPause, err
}

// CordonedLaunches is the cordoned launch cap the record holds, or nil when
// none is set. Like ExecutionPause it reads only that section, so the cap's
// gate works on a record that lacks the sections a run needs.
func (r InstanceSettingsRecord) CordonedLaunches() (*CordonedLaunchSettings, error) {
	settings, err := r.sectionAlone(InstanceSectionCordonedLaunches)
	return settings.CordonedLaunches, err
}

// ExecutionEnabled reports whether the operator turned execution on. It
// reads only that section, so it answers for a record that lacks the
// sections a run needs; an unset section is off.
func (r InstanceSettingsRecord) ExecutionEnabled() (bool, error) {
	settings, err := r.sectionAlone(InstanceSectionExecution)
	return settings.Execution != nil && settings.Execution.Enabled, err
}

// AgentBase is the agent-base source the record names, or nil when none is
// set. It reads only that section, so agent-base upkeep works on a record
// that lacks the sections a run needs.
func (r InstanceSettingsRecord) AgentBase() (*AgentBaseSettings, error) {
	settings, err := r.sectionAlone(InstanceSectionAgentBase)
	return settings.AgentBase, err
}

// sectionAlone decodes and validates one section of the record into an
// otherwise empty settings value, which is empty when the section is unset.
func (r InstanceSettingsRecord) sectionAlone(section InstanceSettingsSection) (InstanceSettings, error) {
	var settings InstanceSettings
	for _, revision := range r.Sections {
		if revision.Section != section {
			continue
		}
		if err := decodeSettingsValue(revision.Value, settings.field(revision.Section)); err != nil {
			return InstanceSettings{}, fmt.Errorf("instance settings revision %d: %s: %w", revision.Revision, revision.Section, err)
		}
		if err := settings.ValidateSection(section); err != nil {
			return InstanceSettings{}, fmt.Errorf("instance settings revision %d: %w", revision.Revision, err)
		}
		return settings, nil
	}
	return InstanceSettings{}, nil
}

// maxSettingsValueBytes bounds one section's value: the largest is a list of
// allowed paths, far under this.
const maxSettingsValueBytes = strictjson.Limit(64 << 10)

// decodeSettingsValue strictly decodes one section's value: unknown keys,
// duplicate keys, and invalid UTF-8 are refused, and so is an object that
// leaves out a key, as the deputy settings file refuses one, so a value
// never holds a zero nobody wrote.
func decodeSettingsValue(raw json.RawMessage, dst any) error {
	if err := strictjson.Decode(raw, dst, strictjson.RejectInvalidUTF8, maxSettingsValueBytes); err != nil {
		return err
	}
	return requireSettingsKeys(raw, reflect.TypeOf(dst))
}

// requireSettingsKeys refuses an object value that lacks one of its type's
// keys; a key tagged omitempty is optional. A value whose type is not a
// struct has no keys to require.
func requireSettingsKeys(raw json.RawMessage, t reflect.Type) error {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return nil
	}
	var present map[string]json.RawMessage
	if err := json.Unmarshal(raw, &present); err != nil {
		return err
	}
	for i := range t.NumField() {
		field := t.Field(i)
		name, options, _ := strings.Cut(field.Tag.Get("json"), ",")
		if name == "" || name == "-" || slices.Contains(strings.Split(options, ","), "omitempty") {
			continue
		}
		if _, ok := present[name]; !ok {
			return fmt.Errorf("%s: %w", name, ErrSettingsKeyMissing)
		}
	}
	return nil
}

// settingsValueCleared reports whether a revision's value clears its
// section: absent, or JSON null as a decoded body holds it.
func settingsValueCleared(raw json.RawMessage) bool {
	return len(raw) == 0 || string(raw) == "null"
}

func validateSettingsSetter(setBy string, setAt time.Time) error {
	if setBy == "" {
		return fmt.Errorf("set_by: %w", ErrEmptyField)
	}
	if setAt.IsZero() {
		return fmt.Errorf("set_at: %w", ErrMissingTimestamp)
	}
	if setAt.Location() != time.UTC {
		return fmt.Errorf("set_at: %w", ErrTimestampNotUTC)
	}
	return nil
}

func requireCleanAbsolutePath(name, value string) error {
	if !filepath.IsAbs(value) {
		return fmt.Errorf("%s must be an absolute path", name)
	}
	if filepath.Clean(value) != value {
		return fmt.Errorf("%s must be a clean path", name)
	}
	return nil
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
