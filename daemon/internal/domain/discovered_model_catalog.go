package domain

import (
	"errors"
	"fmt"
	"slices"
	"time"
)

// ErrNoDiscoveredModels is returned for a catalog that lists no model: a
// harness that reports none has told the daemon nothing it can select.
var ErrNoDiscoveredModels = errors.New("discovered model catalog lists no model")

// ErrDiscoveredModelUnserved and ErrDiscoveredEffortUnserved are Serves's
// refusals: the catalog lists no entry for the model, or no entry for it
// reports the effort.
var (
	ErrDiscoveredModelUnserved  = errors.New("the discovered catalog lists no such model")
	ErrDiscoveredEffortUnserved = errors.New("the discovered catalog lists no such effort for the model")
)

// DiscoveredModelCatalog is what one enrollment's pinned harness reported it
// can run, as of one observation (issue #2594). It is observation, never
// authority: it feeds a picker and the selection-tuple check, and grants
// nothing. A later observation replaces it whole.
type DiscoveredModelCatalog struct {
	EnrollmentID ClientEnrollmentID `json:"enrollment_id"`
	// HarnessBuild is the CLI's own --version line, as it printed it.
	HarnessBuild string            `json:"harness_build"`
	ObservedAt   time.Time         `json:"observed_at"`
	Models       []DiscoveredModel `json:"models"`
}

// DiscoveredModel is one model the harness offers.
type DiscoveredModel struct {
	// ID is the value the harness's model option takes.
	ID string `json:"id"`
	// ResolvedModel is the provider model the id resolves to, when the
	// harness said; an alias such as "opus" resolves to a dated model.
	ResolvedModel *string `json:"resolved_model"`
	DisplayName   string  `json:"display_name"`
	// EffortLevels are the harness's own effort strings, in its order. They
	// are not EffortLevel: the harness reports levels that enum lacks. A
	// model without effort support carries an empty list, never null.
	EffortLevels []string `json:"effort_levels"`
}

// Validate reports whether the model is well-formed.
func (m DiscoveredModel) Validate() error {
	if m.ID == "" {
		return fmt.Errorf("discovered model id: %w", ErrEmptyField)
	}
	if m.ResolvedModel != nil && *m.ResolvedModel == "" {
		return fmt.Errorf("discovered model resolved_model: %w", ErrEmptyField)
	}
	if m.DisplayName == "" {
		return fmt.Errorf("discovered model display_name: %w", ErrEmptyField)
	}
	if m.EffortLevels == nil {
		return fmt.Errorf("discovered model effort_levels: %w", ErrEmptyField)
	}
	seen := make(map[string]bool, len(m.EffortLevels))
	for i, level := range m.EffortLevels {
		if level == "" {
			return fmt.Errorf("discovered model effort level %d: %w", i, ErrEmptyField)
		}
		if seen[level] {
			return fmt.Errorf("discovered model effort level %d: %w", i, ErrDuplicate)
		}
		seen[level] = true
	}
	return nil
}

// Validate reports whether the catalog is well-formed: a named enrollment and
// build, a UTC observation time, and at least one model, each with a unique id.
// Models keep the harness's order, which is the order it offers them in.
func (c DiscoveredModelCatalog) Validate() error {
	if c.EnrollmentID == "" {
		return fmt.Errorf("discovered model catalog enrollment_id: %w", ErrEmptyID)
	}
	if c.HarnessBuild == "" {
		return fmt.Errorf("discovered model catalog %s harness_build: %w", c.EnrollmentID, ErrEmptyField)
	}
	if err := validateObservationTime("discovered model catalog "+string(c.EnrollmentID)+" observed_at", c.ObservedAt); err != nil {
		return err
	}
	if err := ValidateDiscoveredModels(c.Models); err != nil {
		return fmt.Errorf("discovered model catalog %s: %w", c.EnrollmentID, err)
	}
	return nil
}

// ValidateDiscoveredModels reports whether models can form a catalog: at
// least one, each well-formed, with unique ids. A decoder checks a harness's
// answer with it before the answer is bound to an enrollment. Errors name a
// model by its index, never by its harness-supplied strings, so an error
// carries no harness output into a log.
func ValidateDiscoveredModels(models []DiscoveredModel) error {
	if len(models) == 0 {
		return ErrNoDiscoveredModels
	}
	seen := make(map[string]bool, len(models))
	for i, model := range models {
		if err := model.Validate(); err != nil {
			return fmt.Errorf("model %d: %w", i, err)
		}
		if seen[model.ID] {
			return fmt.Errorf("model %d id: %w", i, ErrDuplicate)
		}
		seen[model.ID] = true
	}
	return nil
}

// Serves reports whether the catalog offers model at effort, the check a
// selection's model and effort meet once a catalog is recorded. The model is
// found by entry id or, failing that, by the model entries resolve to
// (catalogEntries). EffortHarnessDefault passes for any listed model, since
// it sends no effort. The error is ErrDiscoveredModelUnserved or
// ErrDiscoveredEffortUnserved and quotes neither the caller's strings nor the
// catalog's, so a caller can name the field it refuses.
func (c DiscoveredModelCatalog) Serves(model, effort string) error {
	entries := catalogEntries(c, model)
	if len(entries) == 0 {
		return ErrDiscoveredModelUnserved
	}
	if effort != string(EffortHarnessDefault) &&
		!slices.ContainsFunc(entries, func(m DiscoveredModel) bool { return slices.Contains(m.EffortLevels, effort) }) {
		return ErrDiscoveredEffortUnserved
	}
	return nil
}
