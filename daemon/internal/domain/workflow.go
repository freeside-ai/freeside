package domain

import "fmt"

// InitiatorConfig is one resolved workflow-definition initiator (plan §5.12).
// Label mode defaults are applied by parsing before this domain boundary, so a
// persisted label initiator always records propose or the auto_start override.
type InitiatorConfig struct {
	Type  InitiatorType `json:"type"`
	Label string        `json:"label,omitempty"`
	Mode  InitiatorMode `json:"mode,omitempty"`
}

// Validate reports whether the type-specific fields are exact. Manual intake
// carries neither label nor mode; label intake records both.
func (c InitiatorConfig) Validate() error {
	if !c.Type.valid() {
		return fmt.Errorf("initiator type %q: %w", c.Type, ErrInvalidInitiatorType)
	}
	switch c.Type {
	case InitiatorTypeManual:
		if c.Label != "" || c.Mode != "" {
			return fmt.Errorf("manual initiator carries label or mode: %w", ErrInitiatorInconsistent)
		}
	case InitiatorTypeLabel:
		if c.Label == "" {
			return fmt.Errorf("label initiator label: %w", ErrEmptyField)
		}
		if !c.Mode.valid() {
			return fmt.Errorf("label initiator mode %q: %w", c.Mode, ErrInvalidInitiatorMode)
		}
	}
	return nil
}
