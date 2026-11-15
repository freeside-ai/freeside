package domain

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
)

// ProjectSettingsSection names one section of a project's settings: what a
// run of that project publishes to and may change (plan §3.2, the project
// scope; issue #2082). A revision sets or clears exactly one section, so
// each section validates alone.
type ProjectSettingsSection string

const (
	ProjectSectionRepository     ProjectSettingsSection = "repository"
	ProjectSectionAllowedPaths   ProjectSettingsSection = "allowed_paths"
	ProjectSectionApprovedRecipe ProjectSettingsSection = "approved_recipe"
	ProjectSectionAgentImage     ProjectSettingsSection = "agent_image"
	ProjectSectionHandBackLabel  ProjectSettingsSection = "hand_back_label"
)

// AllProjectSettingsSections lists every ProjectSettingsSection in the order
// a record shows them; it is the single registration point.
var AllProjectSettingsSections = []ProjectSettingsSection{
	ProjectSectionRepository, ProjectSectionAllowedPaths, ProjectSectionApprovedRecipe,
	ProjectSectionAgentImage, ProjectSectionHandBackLabel,
}

func (s ProjectSettingsSection) valid() bool {
	switch s {
	case ProjectSectionRepository, ProjectSectionAllowedPaths, ProjectSectionApprovedRecipe,
		ProjectSectionAgentImage, ProjectSectionHandBackLabel:
		return true
	default:
		return false
	}
}

// ParseProjectSettingsSection returns the section a name names.
func ParseProjectSettingsSection(name string) (ProjectSettingsSection, error) {
	if s := ProjectSettingsSection(name); s.valid() {
		return s, nil
	}
	return "", fmt.Errorf("project settings section %q: %w", name, ErrUnknownSettingsSection)
}

// Optional reports whether a project's settings are complete without the
// section, so a revision may clear it.
func (s ProjectSettingsSection) Optional() bool {
	switch s {
	case ProjectSectionHandBackLabel:
		return true
	case ProjectSectionRepository, ProjectSectionAllowedPaths, ProjectSectionApprovedRecipe,
		ProjectSectionAgentImage:
		return false
	}
	return false
}

// ProjectRepositorySettings is the repository a project's runs publish to and
// the branch they start from. The repository id must agree with the
// project's projects binding when one exists; the store checks that on
// write and the loader on read.
type ProjectRepositorySettings struct {
	Repo         string `json:"repo"`
	RepositoryID int64  `json:"repository_id"`
	BaseRef      string `json:"base_ref"`
}

// ProjectSettings is every project section's value. The optional hand-back
// label is a pointer, nil when unset. The rules here are shape rules only;
// the rules of the packages that use a value (the explicit-path rule, the
// transport's repository, branch, and label forms) are re-run by the
// settings gate in daemoncompose.
type ProjectSettings struct {
	Repository     ProjectRepositorySettings `json:"repository"`
	AllowedPaths   []string                  `json:"allowed_paths"`
	ApprovedRecipe Digest                    `json:"approved_recipe"`
	AgentImage     ImageRef                  `json:"agent_image"`
	// HandBackLabel is the label the landing batch puts on a pull request it
	// hands back for rework (#3160); unset leaves hand-backs with a person.
	HandBackLabel *string `json:"hand_back_label"`
}

// Validate checks every section that is set and that every required one
// holds a value.
func (s ProjectSettings) Validate() error {
	for _, section := range AllProjectSettingsSections {
		if section == ProjectSectionHandBackLabel && s.HandBackLabel == nil {
			continue
		}
		if err := s.ValidateSection(section); err != nil {
			return err
		}
	}
	return nil
}

// ValidateSection checks one section's value alone, each refusal naming the
// field.
func (s ProjectSettings) ValidateSection(section ProjectSettingsSection) error {
	switch section {
	case ProjectSectionRepository:
		return validateProjectRepositorySettings(s.Repository)
	case ProjectSectionAllowedPaths:
		if len(s.AllowedPaths) == 0 {
			return errors.New("allowed_paths is empty")
		}
		for _, path := range s.AllowedPaths {
			if strings.TrimSpace(path) == "" || strings.Contains(path, ",") {
				return errors.New("allowed_paths holds an empty entry or a comma")
			}
		}
		return nil
	case ProjectSectionApprovedRecipe:
		if !contentaddr.Valid(string(s.ApprovedRecipe)) {
			return errors.New("approved_recipe is not a sha256 digest")
		}
		return nil
	case ProjectSectionAgentImage:
		if s.AgentImage.Validate() != nil {
			return errors.New("agent_image is not a digest-pinned image reference")
		}
		return nil
	case ProjectSectionHandBackLabel:
		if s.HandBackLabel == nil || *s.HandBackLabel == "" {
			return errors.New("hand_back_label is required")
		}
		return nil
	}
	return fmt.Errorf("project settings section %q: %w", section, ErrUnknownSettingsSection)
}

func validateProjectRepositorySettings(r ProjectRepositorySettings) error {
	if !projectRepositoryPattern.MatchString(r.Repo) || strings.Contains(r.Repo, "..") {
		return errors.New("repository.repo is not owner/name")
	}
	if r.RepositoryID <= 0 {
		return errors.New("repository.repository_id must be positive")
	}
	if r.BaseRef == "" {
		return errors.New("repository.base_ref is required")
	}
	return nil
}

// field returns a pointer to the field that holds section, for decoding a
// revision's value into and encoding it from.
func (s *ProjectSettings) field(section ProjectSettingsSection) any {
	switch section {
	case ProjectSectionRepository:
		return &s.Repository
	case ProjectSectionAllowedPaths:
		return &s.AllowedPaths
	case ProjectSectionApprovedRecipe:
		return &s.ApprovedRecipe
	case ProjectSectionAgentImage:
		return &s.AgentImage
	case ProjectSectionHandBackLabel:
		return &s.HandBackLabel
	}
	return nil
}

// SectionValue encodes one section's value as a revision stores it, nil for
// an unset optional section.
func (s ProjectSettings) SectionValue(section ProjectSettingsSection) (json.RawMessage, error) {
	if !section.valid() {
		return nil, fmt.Errorf("project settings section %q: %w", section, ErrUnknownSettingsSection)
	}
	if section == ProjectSectionHandBackLabel && s.HandBackLabel == nil {
		return nil, nil
	}
	return json.Marshal(s.field(section))
}

// DecodeProjectSection strictly decodes a non-null value into a fresh
// settings value's section and validates it there, returning that settings
// value and the section's canonical encoding.
func DecodeProjectSection(section ProjectSettingsSection, raw json.RawMessage) (ProjectSettings, json.RawMessage, error) {
	var s ProjectSettings
	if !section.valid() {
		return ProjectSettings{}, nil, fmt.Errorf("project settings section %q: %w", section, ErrUnknownSettingsSection)
	}
	if err := decodeSettingsValue(raw, s.field(section)); err != nil {
		return ProjectSettings{}, nil, fmt.Errorf("%s: %w", section, err)
	}
	if err := s.ValidateSection(section); err != nil {
		return ProjectSettings{}, nil, err
	}
	canonical, err := json.Marshal(s.field(section))
	if err != nil {
		return ProjectSettings{}, nil, fmt.Errorf("%s: %w", section, err)
	}
	return s, canonical, nil
}

// ProjectSettingsInput carries the caller-supplied fields of a
// ProjectSettingsRevision. It has no Revision: the store assigns it inside
// the write transaction.
type ProjectSettingsInput struct {
	ProjectID ProjectID
	Section   ProjectSettingsSection
	// Value is the section's new value as JSON; nil (or null) clears it.
	Value json.RawMessage
	// SetBy is who set it: cli:<username> for an operator command.
	SetBy string
}

// ProjectSettingsRevision is one change to one section of one project's
// settings: it sets the section's value, or clears it when Value is null. A
// project's revisions number its changes from 1 and are never rewritten, so
// a revision number names one state of that project's settings for good.
type ProjectSettingsRevision struct {
	ProjectID ProjectID              `json:"project_id"`
	Revision  int                    `json:"revision"`
	Section   ProjectSettingsSection `json:"section"`
	Value     json.RawMessage        `json:"value"`
	SetBy     string                 `json:"set_by"`
	SetAt     time.Time              `json:"set_at"`
}

// NewProjectSettingsRevision builds a validated revision: a set value is
// strictly decoded, checked against its section's rules, and stored in its
// canonical encoding, and the timestamp is normalized to UTC.
func NewProjectSettingsRevision(in ProjectSettingsInput, revision int, setAt time.Time) (ProjectSettingsRevision, error) {
	r := ProjectSettingsRevision{
		ProjectID: in.ProjectID, Revision: revision, Section: in.Section, SetBy: in.SetBy, SetAt: setAt.UTC(),
	}
	if !settingsValueCleared(in.Value) && in.Section.valid() {
		_, canonical, err := DecodeProjectSection(in.Section, in.Value)
		if err != nil {
			return ProjectSettingsRevision{}, fmt.Errorf("project %q settings revision %d: %w", in.ProjectID, revision, err)
		}
		r.Value = canonical
	}
	if err := r.Validate(); err != nil {
		return ProjectSettingsRevision{}, err
	}
	return r, nil
}

// Validate reports whether the revision is well-formed: a project, a known
// section, a value its section accepts or a clear of an optional section,
// and who set it and when. The store re-runs it on every decode.
func (r ProjectSettingsRevision) Validate() error {
	if r.ProjectID == "" {
		return fmt.Errorf("project settings revision %d project id: %w", r.Revision, ErrEmptyID)
	}
	if r.Revision < 1 {
		return fmt.Errorf("project %q settings revision %d: %w", r.ProjectID, r.Revision, ErrNonPositive)
	}
	if !r.Section.valid() {
		return fmt.Errorf("project %q settings revision %d section %q: %w",
			r.ProjectID, r.Revision, r.Section, ErrUnknownSettingsSection)
	}
	if r.Cleared() {
		if !r.Section.Optional() {
			return fmt.Errorf("project %q settings revision %d: %s: %w",
				r.ProjectID, r.Revision, r.Section, ErrRequiredSettingsSectionCleared)
		}
	} else if _, _, err := DecodeProjectSection(r.Section, r.Value); err != nil {
		return fmt.Errorf("project %q settings revision %d: %w", r.ProjectID, r.Revision, err)
	}
	if err := validateSettingsSetter(r.SetBy, r.SetAt); err != nil {
		return fmt.Errorf("project %q settings revision %d %w", r.ProjectID, r.Revision, err)
	}
	return nil
}

// Cleared reports whether the revision clears its section.
func (r ProjectSettingsRevision) Cleared() bool { return settingsValueCleared(r.Value) }

// ProjectSettingsRecord is one project's settings folded from its revisions:
// the head revision and the revision that holds each set section, in
// AllProjectSettingsSections order.
type ProjectSettingsRecord struct {
	ProjectID ProjectID                 `json:"project_id"`
	Revision  int                       `json:"revision"`
	Sections  []ProjectSettingsRevision `json:"sections"`
}

// NewProjectSettingsRecord folds one project's revisions, which must number
// 1, 2, 3, and so on in order: a later change to a section replaces an
// earlier one, and a clear removes it.
func NewProjectSettingsRecord(project ProjectID, revisions []ProjectSettingsRevision) (ProjectSettingsRecord, error) {
	record := ProjectSettingsRecord{ProjectID: project}
	held := make(map[ProjectSettingsSection]ProjectSettingsRevision)
	for _, r := range revisions {
		if r.ProjectID != project {
			return ProjectSettingsRecord{}, fmt.Errorf("project %q settings revision %d belongs to project %q: %w",
				project, r.Revision, r.ProjectID, ErrSettingsRevisionOrder)
		}
		if r.Revision != record.Revision+1 {
			return ProjectSettingsRecord{}, fmt.Errorf("project %q settings revision %d after %d: %w",
				project, r.Revision, record.Revision, ErrSettingsRevisionOrder)
		}
		record.Revision = r.Revision
		if r.Cleared() {
			delete(held, r.Section)
		} else {
			held[r.Section] = r
		}
	}
	for _, section := range AllProjectSettingsSections {
		if r, ok := held[section]; ok {
			record.Sections = append(record.Sections, r)
		}
	}
	return record, nil
}

// Settings is the record's settings. It refuses a record that lacks a
// required section and names the section.
func (r ProjectSettingsRecord) Settings() (ProjectSettings, error) {
	var settings ProjectSettings
	set := make([]ProjectSettingsSection, 0, len(r.Sections))
	for _, revision := range r.Sections {
		if err := decodeSettingsValue(revision.Value, settings.field(revision.Section)); err != nil {
			return ProjectSettings{}, fmt.Errorf("project %q settings revision %d: %s: %w",
				r.ProjectID, revision.Revision, revision.Section, err)
		}
		set = append(set, revision.Section)
	}
	for _, section := range AllProjectSettingsSections {
		if !section.Optional() && !slices.Contains(set, section) {
			return ProjectSettings{}, fmt.Errorf("project %q settings: %s: %w", r.ProjectID, section, ErrSettingsSectionMissing)
		}
	}
	if err := settings.Validate(); err != nil {
		return ProjectSettings{}, fmt.Errorf("project %q settings: %w", r.ProjectID, err)
	}
	return settings, nil
}
