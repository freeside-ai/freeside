package domain

import (
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
)

// WorkflowPolicyKey names a resolved-policy key the Workflow settings pane
// exposes (plan §3.2, revision 121). The operator sets each one at two layers
// in the store, general and per project, and a task resolves it project over
// general over the key's default, never from the manual submission config
// file. The readers keep their own copies of the names (the closure gate in
// engine and publish, the caps in publish) and read the run's stored policy.
type WorkflowPolicyKey string

const (
	WorkflowGateFollowUpFiling     WorkflowPolicyKey = "gates.follow_up_filing"
	WorkflowGateIssueComment       WorkflowPolicyKey = "gates.issue_comment"
	WorkflowGateSourceIssueClosure WorkflowPolicyKey = "gates.source_issue_closure"
	WorkflowFollowUpMaxPerDay      WorkflowPolicyKey = "follow_up_filing.max_per_day"
	WorkflowFollowUpMaxPerRun      WorkflowPolicyKey = "follow_up_filing.max_per_run"
	WorkflowFollowUpMaxDepth       WorkflowPolicyKey = "follow_up_filing.max_depth"
)

// AllWorkflowPolicyKeys lists every WorkflowPolicyKey in the order the pane
// shows them; it is the single registration point.
var AllWorkflowPolicyKeys = []WorkflowPolicyKey{
	WorkflowGateFollowUpFiling, WorkflowGateIssueComment, WorkflowGateSourceIssueClosure,
	WorkflowFollowUpMaxPerDay, WorkflowFollowUpMaxPerRun, WorkflowFollowUpMaxDepth,
}

func (k WorkflowPolicyKey) valid() bool {
	switch k {
	case WorkflowGateFollowUpFiling, WorkflowGateIssueComment, WorkflowGateSourceIssueClosure,
		WorkflowFollowUpMaxPerDay, WorkflowFollowUpMaxPerRun, WorkflowFollowUpMaxDepth:
		return true
	default:
		return false
	}
}

// IsWorkflowPolicyKey reports whether a resolved-policy key is one the
// Workflow pane exposes, so it resolves from the store and never from the
// manual submission config file.
func IsWorkflowPolicyKey(key string) bool { return WorkflowPolicyKey(key).valid() }

// Default is the value a key resolves to when neither layer sets it. A gate is
// off, so the autonomous path is the unmarked state; the caps match the
// follow-up filer's own defaults (publish.defaultFollowUpFilingCaps).
func (k WorkflowPolicyKey) Default() string {
	switch k {
	case WorkflowGateFollowUpFiling, WorkflowGateIssueComment, WorkflowGateSourceIssueClosure:
		return "false"
	case WorkflowFollowUpMaxPerDay:
		return "10"
	case WorkflowFollowUpMaxPerRun:
		return "5"
	case WorkflowFollowUpMaxDepth:
		return "2"
	}
	return ""
}

// ValidateValue reports whether value is the canonical form of a value the
// key takes: a gate exactly "true" or "false", a cap a positive decimal
// integer with no sign or leading zero. Canonical forms keep one setting one
// string, so an unchanged value is recognized as unchanged. The error never
// repeats the value, which is request text.
func (k WorkflowPolicyKey) ValidateValue(value string) error {
	switch k {
	case WorkflowGateFollowUpFiling, WorkflowGateIssueComment, WorkflowGateSourceIssueClosure:
		if value != "true" && value != "false" {
			return fmt.Errorf("workflow policy %s: %w", k, ErrInvalidWorkflowPolicyValue)
		}
		return nil
	case WorkflowFollowUpMaxPerDay, WorkflowFollowUpMaxPerRun, WorkflowFollowUpMaxDepth:
		if n, err := strconv.Atoi(value); err != nil || n < 1 || strconv.Itoa(n) != value {
			return fmt.Errorf("workflow policy %s: %w", k, ErrInvalidWorkflowPolicyValue)
		}
		return nil
	}
	return ErrInvalidWorkflowPolicyKey
}

// WorkflowPolicyScope is the layer a workflow policy revision sets: general,
// which every project inherits, or one project's override.
type WorkflowPolicyScope string

const (
	WorkflowPolicyScopeGeneral WorkflowPolicyScope = "general"
	WorkflowPolicyScopeProject WorkflowPolicyScope = "project"
)

// AllWorkflowPolicyScopes lists every valid WorkflowPolicyScope.
var AllWorkflowPolicyScopes = []WorkflowPolicyScope{WorkflowPolicyScopeGeneral, WorkflowPolicyScopeProject}

func (s WorkflowPolicyScope) valid() bool {
	switch s {
	case WorkflowPolicyScopeGeneral, WorkflowPolicyScopeProject:
		return true
	default:
		return false
	}
}

// WorkflowPolicyOrigin is the layer a key's effective value came from.
type WorkflowPolicyOrigin string

const (
	WorkflowPolicyOriginProject WorkflowPolicyOrigin = "project"
	WorkflowPolicyOriginGeneral WorkflowPolicyOrigin = "general"
	WorkflowPolicyOriginDefault WorkflowPolicyOrigin = "default"
)

// AllWorkflowPolicyOrigins lists every valid WorkflowPolicyOrigin.
var AllWorkflowPolicyOrigins = []WorkflowPolicyOrigin{
	WorkflowPolicyOriginProject, WorkflowPolicyOriginGeneral, WorkflowPolicyOriginDefault,
}

func (o WorkflowPolicyOrigin) valid() bool {
	switch o {
	case WorkflowPolicyOriginProject, WorkflowPolicyOriginGeneral, WorkflowPolicyOriginDefault:
		return true
	default:
		return false
	}
}

// provenance is the resolved-policy provenance source a key from this origin
// records: the default is the built-in preset.
func (o WorkflowPolicyOrigin) provenance() ProvenanceSource {
	switch o {
	case WorkflowPolicyOriginProject:
		return ProvenanceProject
	case WorkflowPolicyOriginGeneral:
		return ProvenanceGeneral
	case WorkflowPolicyOriginDefault:
		return ProvenancePreset
	}
	return ""
}

// validateWorkflowPolicyLayerChange checks one layer change's shape: a known
// scope and key, a project id exactly when the scope is project, and a value
// the key takes, nil clearing the layer. Whether the daemon serves the
// project is the caller's question.
func validateWorkflowPolicyLayerChange(
	scope WorkflowPolicyScope, project *ProjectID, key WorkflowPolicyKey, value *string,
) error {
	if !scope.valid() {
		return ErrInvalidWorkflowPolicyScope
	}
	if (scope == WorkflowPolicyScopeProject) != (project != nil) || project != nil && *project == "" {
		return fmt.Errorf("workflow policy %s scope: %w", scope, ErrWorkflowPolicyProjectMismatch)
	}
	if !key.valid() {
		return ErrInvalidWorkflowPolicyKey
	}
	if value != nil {
		return key.ValidateValue(*value)
	}
	return nil
}

// WorkflowPolicyInput carries the caller-supplied fields of a
// WorkflowPolicyRevision. It has no Revision: the store assigns it inside the
// write transaction.
type WorkflowPolicyInput struct {
	Scope     WorkflowPolicyScope
	ProjectID *ProjectID
	Key       WorkflowPolicyKey
	// Value is the layer's new value; nil clears the layer, so the key falls
	// through to the next one.
	Value *string
	// SetBy is the operator identity: device:<device_id> for a client.
	SetBy string
}

// WorkflowPolicyRevision is one change to one layer of one workflow policy
// key. Revisions number the deployment's changes from 1 across every layer and
// key, and a revision is never edited, so a resolved key's provenance digest
// names the change it came from.
type WorkflowPolicyRevision struct {
	Revision  int                 `json:"revision"`
	Scope     WorkflowPolicyScope `json:"scope"`
	ProjectID *ProjectID          `json:"project_id"`
	Key       WorkflowPolicyKey   `json:"key"`
	Value     *string             `json:"value"`
	SetBy     string              `json:"set_by"`
	SetAt     time.Time           `json:"set_at"`
}

// NewWorkflowPolicyRevision builds a validated revision with its timestamp
// normalized to UTC.
func NewWorkflowPolicyRevision(in WorkflowPolicyInput, revision int, setAt time.Time) (WorkflowPolicyRevision, error) {
	r := WorkflowPolicyRevision{
		Revision: revision, Scope: in.Scope, ProjectID: clonePtr(in.ProjectID), Key: in.Key,
		Value: clonePtr(in.Value), SetBy: in.SetBy, SetAt: setAt.UTC(),
	}
	if err := r.Validate(); err != nil {
		return WorkflowPolicyRevision{}, err
	}
	return r, nil
}

// Validate reports whether the revision is well-formed. The store re-runs it
// on every decode.
func (r WorkflowPolicyRevision) Validate() error {
	if r.Revision < 1 {
		return fmt.Errorf("workflow policy revision %d: %w", r.Revision, ErrNonPositive)
	}
	if err := validateWorkflowPolicyLayerChange(r.Scope, r.ProjectID, r.Key, r.Value); err != nil {
		return fmt.Errorf("workflow policy revision %d: %w", r.Revision, err)
	}
	if r.SetBy == "" {
		return fmt.Errorf("workflow policy revision %d set_by: %w", r.Revision, ErrEmptyField)
	}
	if r.SetAt.IsZero() {
		return fmt.Errorf("workflow policy revision %d set_at: %w", r.Revision, ErrMissingTimestamp)
	}
	if r.SetAt.Location() != time.UTC {
		return fmt.Errorf("workflow policy revision %d set_at: %w", r.Revision, ErrTimestampNotUTC)
	}
	return nil
}

// Digest is the revision's content address, the provenance digest a key it
// sets resolves with.
func (r WorkflowPolicyRevision) Digest() Digest {
	body, err := json.Marshal(r)
	if err != nil {
		// Every field marshals; a failure is a programming error.
		panic(fmt.Sprintf("workflow policy revision digest: %v", err))
	}
	return Digest(contentaddr.Sum(body))
}

// WorkflowPolicySetting is one key's effective value for the general section
// or a project: the value, the layer it came from, and the revision that set
// it, nil for the default.
type WorkflowPolicySetting struct {
	Key      WorkflowPolicyKey
	Value    string
	Origin   WorkflowPolicyOrigin
	Revision *WorkflowPolicyRevision
}

// workflowPolicyLayer addresses one layer of one key; project is empty for
// the general scope.
type workflowPolicyLayer struct {
	scope   WorkflowPolicyScope
	project ProjectID
	key     WorkflowPolicyKey
}

func layerOf(scope WorkflowPolicyScope, project *ProjectID, key WorkflowPolicyKey) workflowPolicyLayer {
	layer := workflowPolicyLayer{scope: scope, key: key}
	if project != nil {
		layer.project = *project
	}
	return layer
}

// WorkflowPolicyLayers is the current value of every layer, folded from the
// revisions in order. It holds the one precedence rule, project over general
// over default, that task resolution and the client projection both read.
type WorkflowPolicyLayers struct {
	revision int
	held     map[workflowPolicyLayer]WorkflowPolicyRevision
}

// NewWorkflowPolicyLayers folds revisions, which must be in ascending
// revision order: a later change to a layer replaces an earlier one, and a nil
// value clears it.
func NewWorkflowPolicyLayers(revisions []WorkflowPolicyRevision) (WorkflowPolicyLayers, error) {
	layers := WorkflowPolicyLayers{held: make(map[workflowPolicyLayer]WorkflowPolicyRevision)}
	for _, r := range revisions {
		if r.Revision <= layers.revision {
			return WorkflowPolicyLayers{}, fmt.Errorf("workflow policy revision %d after %d: %w",
				r.Revision, layers.revision, ErrKeysNotCanonical)
		}
		layers.revision = r.Revision
		layer := layerOf(r.Scope, r.ProjectID, r.Key)
		if r.Value == nil {
			delete(layers.held, layer)
		} else {
			layers.held[layer] = r
		}
	}
	return layers, nil
}

// Revision is the newest revision folded, 0 when none was ever set. Every
// change to any layer advances it, so a client's expected revision names the
// whole state it saw.
func (l WorkflowPolicyLayers) Revision() int { return l.revision }

// Layer returns the revision that sets one layer, false when the layer is
// unset.
func (l WorkflowPolicyLayers) Layer(
	scope WorkflowPolicyScope, project *ProjectID, key WorkflowPolicyKey,
) (WorkflowPolicyRevision, bool) {
	r, ok := l.held[layerOf(scope, project, key)]
	return r, ok
}

// GeneralSetting is the key's effective value for the general section: the
// general layer, else the default.
func (l WorkflowPolicyLayers) GeneralSetting(key WorkflowPolicyKey) WorkflowPolicySetting {
	if r, ok := l.Layer(WorkflowPolicyScopeGeneral, nil, key); ok {
		return WorkflowPolicySetting{Key: key, Value: *r.Value, Origin: WorkflowPolicyOriginGeneral, Revision: &r}
	}
	return WorkflowPolicySetting{Key: key, Value: key.Default(), Origin: WorkflowPolicyOriginDefault}
}

// ProjectSetting is the key's effective value for a project: its project
// layer, else the general setting.
func (l WorkflowPolicyLayers) ProjectSetting(project ProjectID, key WorkflowPolicyKey) WorkflowPolicySetting {
	if r, ok := l.Layer(WorkflowPolicyScopeProject, &project, key); ok {
		return WorkflowPolicySetting{Key: key, Value: *r.Value, Origin: WorkflowPolicyOriginProject, Revision: &r}
	}
	return l.GeneralSetting(key)
}

// PolicyKeys resolves every workflow policy key for a project, each with the
// winning layer as its provenance: a store layer cites its revision's digest,
// and the default the digest of its key and value.
func (l WorkflowPolicyLayers) PolicyKeys(project ProjectID) []PolicyKey {
	keys := make([]PolicyKey, 0, len(AllWorkflowPolicyKeys))
	for _, key := range AllWorkflowPolicyKeys {
		setting := l.ProjectSetting(project, key)
		var digest Digest
		if setting.Revision != nil {
			digest = setting.Revision.Digest()
		} else {
			digest = workflowPolicyDefaultDigest(key, setting.Value)
		}
		keys = append(keys, PolicyKey{
			Key: string(key), Value: setting.Value,
			Provenance: KeyProvenance{Source: setting.Origin.provenance(), Digest: digest},
		})
	}
	return keys
}

// Overlay returns base without any workflow policy key, followed by the
// project's resolved workflow policy keys, so an exposed key never resolves
// from base.
func (l WorkflowPolicyLayers) Overlay(project ProjectID, base []PolicyKey) []PolicyKey {
	return append(WithoutWorkflowPolicyKeys(base), l.PolicyKeys(project)...)
}

// WithoutWorkflowPolicyKeys returns a copy of keys less every key the
// Workflow pane exposes.
func WithoutWorkflowPolicyKeys(keys []PolicyKey) []PolicyKey {
	return slices.DeleteFunc(slices.Clone(keys), func(k PolicyKey) bool { return IsWorkflowPolicyKey(k.Key) })
}

func workflowPolicyDefaultDigest(key WorkflowPolicyKey, value string) Digest {
	body, err := json.Marshal(struct {
		Key   WorkflowPolicyKey `json:"key"`
		Value string            `json:"value"`
	}{key, value})
	if err != nil {
		panic(fmt.Sprintf("workflow policy default digest: %v", err))
	}
	return Digest(contentaddr.Sum(body))
}

// WorkflowPolicyChangeRequest is a set_workflow_policy command as accepted.
// ExpectedRevision is nil when the client saw no revision.
type WorkflowPolicyChangeRequest struct {
	CommandIdentity
	Scope            WorkflowPolicyScope `json:"scope"`
	ProjectID        *ProjectID          `json:"project_id"`
	Key              WorkflowPolicyKey   `json:"key"`
	Value            *string             `json:"value"`
	ExpectedRevision *int                `json:"expected_revision"`
}

// Validate reports whether the request is well-formed: a valid layer change
// and an expected revision of at least 1 when set.
func (r WorkflowPolicyChangeRequest) Validate() error {
	if err := r.validate(); err != nil {
		return err
	}
	if err := validateWorkflowPolicyLayerChange(r.Scope, r.ProjectID, r.Key, r.Value); err != nil {
		return err
	}
	if r.ExpectedRevision != nil && *r.ExpectedRevision < 1 {
		return fmt.Errorf("workflow policy expected revision: %w", ErrNonPositive)
	}
	return nil
}

// Same reports whether two requests ask for the same change against the same
// revision.
func (r WorkflowPolicyChangeRequest) Same(other WorkflowPolicyChangeRequest) bool {
	return r.CommandIdentity == other.CommandIdentity && r.Scope == other.Scope && r.Key == other.Key &&
		samePtr(r.ProjectID, other.ProjectID) && samePtr(r.Value, other.Value) &&
		samePtr(r.ExpectedRevision, other.ExpectedRevision)
}

// WorkflowPolicyChangeReceipt records a workflow policy change. Recorded is
// false when the layer already held the value asked for. Revision is the
// workflow policy revision current after the request: the new one when
// recorded, else the one the client expected.
type WorkflowPolicyChangeReceipt struct {
	WorkflowPolicyChangeRequest
	Recorded bool `json:"recorded"`
	Revision int  `json:"revision"`
}

func (r WorkflowPolicyChangeReceipt) Validate() error {
	if err := r.WorkflowPolicyChangeRequest.Validate(); err != nil {
		return fmt.Errorf("workflow policy change receipt: %w", err)
	}
	// A change is accepted only against the current revision, so a recorded
	// one follows it and an unrecorded one is it.
	expected := 0
	if r.ExpectedRevision != nil {
		expected = *r.ExpectedRevision
	}
	if r.Recorded && r.Revision != expected+1 || !r.Recorded && r.Revision != expected {
		return fmt.Errorf("workflow policy change receipt %q revision %d after %d: %w",
			r.CommandID, r.Revision, expected, ErrParentKeyMismatch)
	}
	return nil
}
