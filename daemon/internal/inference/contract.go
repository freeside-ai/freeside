// Package inference owns daemon-side model calls made at explicit judgment
// sites. It is separate from execution drivers: requests carry no tool,
// workspace, or ward capability.
package inference

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"
)

// AuthorityMode is the exhaustive terminal sink of one judgment site.
type AuthorityMode string

const (
	AuthorityAnnotate AuthorityMode = "annotate"
	AuthorityPropose  AuthorityMode = "propose"
	AuthorityExplain  AuthorityMode = "explain"
	AuthorityChoose   AuthorityMode = "choose"
)

var AllAuthorityModes = []AuthorityMode{AuthorityAnnotate, AuthorityPropose, AuthorityExplain, AuthorityChoose}

func (m AuthorityMode) valid() bool {
	switch m {
	case AuthorityAnnotate, AuthorityPropose, AuthorityExplain, AuthorityChoose:
		return true
	default:
		return false
	}
}

// Sensitivity classifies outbound model input before any call is attempted.
type Sensitivity string

const (
	SensitivityPublic      Sensitivity = "public"
	SensitivityRepository  Sensitivity = "repository"
	SensitivityOperational Sensitivity = "operational"
)

var AllSensitivities = []Sensitivity{SensitivityPublic, SensitivityRepository, SensitivityOperational}

func (s Sensitivity) valid() bool {
	switch s {
	case SensitivityPublic, SensitivityRepository, SensitivityOperational:
		return true
	default:
		return false
	}
}

// Secret is credential material whose implicit renderings are always redacted.
type Secret string

func (s Secret) Reveal() string { return string(s) }
func (Secret) String() string   { return "[REDACTED]" }
func (Secret) GoString() string { return "[REDACTED]" }
func (Secret) Format(f fmt.State, _ rune) {
	_, _ = io.WriteString(f, "[REDACTED]")
}
func (Secret) MarshalText() ([]byte, error) { return []byte("[REDACTED]"), nil }

// Limits are cumulative maxima inside one window. Zero disables a sink.
type Limits struct {
	Calls          int64         `json:"calls"`
	ComputeUnits   int64         `json:"compute_units"`
	AttentionItems int64         `json:"attention_items"`
	Starvation     time.Duration `json:"starvation"`
}

// Budget declares compositional site, project, global, and lineage bounds.
type Budget struct {
	Window               time.Duration
	Site                 Limits
	Project              Limits
	Global               Limits
	MaxCallsPerRoot      int64
	MaxStarvationPerRoot time.Duration
}

// FieldPolicy is one explicit outbound field permission.
type FieldPolicy struct {
	Name        string
	Sensitivity Sensitivity
}

// AnnotationOutput is one lattice cell with a declared downstream effect.
type AnnotationOutput struct {
	Materiality string
	Confidence  string
}

// SeverityMapping makes one producer-native scale mapping inspectable.
type SeverityMapping struct {
	Source     string
	Native     string
	Normalized string
}

// SeverityCeiling declares minimum daemon handling for one normalized severity.
type SeverityCeiling struct {
	NormalizedSeverity string
	MinimumHandling    string
}

// SecondAdjudicationRule declares when output must reach a distinct
// adjudicator or durable attention rather than disappear silently.
type SecondAdjudicationRule struct {
	NormalizedSeverity string
	ConfidenceBelow    string
	Fallback           string
}

// AnnotationContract is the inspectable lattice and ceilings for a
// ceiling-bounded annotation site.
type AnnotationContract struct {
	Materiality                []string
	Confidence                 []string
	ReducesWork                []AnnotationOutput
	SeverityMappings           []SeverityMapping
	UnknownSeverityFallback    string
	NormalizedSeverityCeilings []SeverityCeiling
	SecondAdjudicationRules    []SecondAdjudicationRule
}

// Site is the complete authority and resource contract for one call site.
type Site struct {
	ID              string
	Authority       AuthorityMode
	Fields          []FieldPolicy
	FailSafe        string
	Retention       time.Duration
	Timeout         time.Duration
	MaxInputBytes   int
	MaxOutputBytes  int
	MaxComputeUnits int64
	Budget          Budget
	AuditEvery      int64
	Annotation      *AnnotationContract
	ValidateOutput  func([]byte) error
}

func (s Site) validate() error {
	if s.ID == "" || !s.Authority.valid() || s.FailSafe == "" || s.Retention <= 0 || s.Timeout <= 0 ||
		s.MaxInputBytes < 1 || s.MaxOutputBytes < 1 || s.Budget.Window <= 0 ||
		s.MaxComputeUnits < 1 || s.Budget.MaxCallsPerRoot < 1 || s.Budget.MaxStarvationPerRoot <= 0 ||
		s.AuditEvery < 1 || s.ValidateOutput == nil || len(s.Fields) == 0 {
		return errors.New("invalid inference site")
	}
	names := make(map[string]bool, len(s.Fields))
	for _, field := range s.Fields {
		if field.Name == "" || !field.Sensitivity.valid() || names[field.Name] {
			return errors.New("invalid inference field policy")
		}
		names[field.Name] = true
	}
	for _, limits := range []Limits{s.Budget.Site, s.Budget.Project, s.Budget.Global} {
		if limits.Calls < 1 || limits.ComputeUnits < 1 || limits.AttentionItems < 0 || limits.Starvation <= 0 {
			return errors.New("invalid inference budget limits")
		}
	}
	if s.Authority == AuthorityAnnotate {
		if err := s.Annotation.validate(); err != nil {
			return err
		}
	} else if s.Annotation != nil {
		return errors.New("non-annotation site carries annotation contract")
	}
	return nil
}

func (c *AnnotationContract) validate() error {
	if c == nil || len(c.Materiality) == 0 || len(c.Confidence) == 0 || len(c.ReducesWork) == 0 ||
		len(c.SeverityMappings) == 0 || c.UnknownSeverityFallback == "" ||
		len(c.NormalizedSeverityCeilings) == 0 || len(c.SecondAdjudicationRules) == 0 {
		return errors.New("invalid annotation contract")
	}
	materiality := make(map[string]bool, len(c.Materiality))
	confidence := make(map[string]bool, len(c.Confidence))
	for _, value := range c.Materiality {
		if value == "" || materiality[value] {
			return errors.New("invalid annotation materiality lattice")
		}
		materiality[value] = true
	}
	for _, value := range c.Confidence {
		if value == "" || confidence[value] {
			return errors.New("invalid annotation confidence lattice")
		}
		confidence[value] = true
	}
	for _, output := range c.ReducesWork {
		if !materiality[output.Materiality] || !confidence[output.Confidence] {
			return errors.New("work-reducing output is outside annotation lattice")
		}
	}
	normalizedSeverities := map[string]bool{"critical": true, "high": true, "medium": true, "low": true}
	if !normalizedSeverities[c.UnknownSeverityFallback] {
		return errors.New("invalid unknown-severity fallback")
	}
	mappings := make(map[string]bool, len(c.SeverityMappings))
	for _, mapping := range c.SeverityMappings {
		key := mapping.Source + "\x00" + mapping.Native
		if mapping.Source == "" || mapping.Native == "" || !normalizedSeverities[mapping.Normalized] || mappings[key] {
			return errors.New("invalid severity mapping")
		}
		mappings[key] = true
	}
	for _, ceiling := range c.NormalizedSeverityCeilings {
		if !normalizedSeverities[ceiling.NormalizedSeverity] || ceiling.MinimumHandling == "" {
			return errors.New("invalid raw-severity ceiling")
		}
	}
	for _, rule := range c.SecondAdjudicationRules {
		if !normalizedSeverities[rule.NormalizedSeverity] || !confidence[rule.ConfidenceBelow] || rule.Fallback == "" {
			return errors.New("invalid second-adjudication rule")
		}
	}
	return nil
}

// InputField carries a value and the sensitivity the caller assigned it.
type InputField struct {
	Value       string
	Sensitivity Sensitivity
}

// Request is the complete driver input. It intentionally has no workspace,
// tool, environment, or executable field.
type Request struct {
	SiteID          string            `json:"site_id"`
	InputDigest     string            `json:"input_digest"`
	Fields          map[string]string `json:"fields"`
	MaxOutput       int               `json:"max_output_bytes"`
	MaxComputeUnits int64             `json:"max_compute_units"`
}

// Response is untrusted driver output plus its measured compute proxy.
type Response struct {
	Output       []byte
	ComputeUnits int64
}

// Driver performs one structured completion without tools or a workspace.
// Implementations must stop provider work and return when ctx is canceled;
// Client additionally prevents a non-conforming driver from accumulating
// more than one orphaned call per site.
type Driver interface {
	Complete(context.Context, Request, Secret) (Response, error)
}

// Binding pins the non-choosable provider identity and credential.
type Binding struct {
	Provider   string
	Model      string
	Credential Secret
	Driver     Driver
}

func (b Binding) producer() string { return b.Provider + "/" + b.Model }

// CallResult is a schema-validated, producer-labeled output.
type CallResult struct {
	Output      []byte
	Producer    string
	InputDigest string
	Fallback    bool
	Reason      string
}

// ErrUnavailable is the fail-safe inference-down condition.
var ErrUnavailable = errors.New("inference unavailable")

func cloneStrings(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}
