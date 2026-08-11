package domain_test

import (
	"errors"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

func TestInitiatorConfigValidate(t *testing.T) {
	t.Parallel()
	for _, valid := range []domain.InitiatorConfig{
		{Type: domain.InitiatorTypeManual},
		{Type: domain.InitiatorTypeLabel, Label: "freeside", Mode: domain.InitiatorModePropose},
		{Type: domain.InitiatorTypeLabel, Label: "freeside", Mode: domain.InitiatorModeAutoStart},
	} {
		if err := valid.Validate(); err != nil {
			t.Errorf("valid initiator %#v: %v", valid, err)
		}
	}

	tests := []struct {
		name   string
		config domain.InitiatorConfig
		want   error
	}{
		{"unknown type", domain.InitiatorConfig{Type: "scan"}, domain.ErrInvalidInitiatorType},
		{"manual label", domain.InitiatorConfig{Type: domain.InitiatorTypeManual, Label: "freeside"}, domain.ErrInitiatorInconsistent},
		{"manual mode", domain.InitiatorConfig{Type: domain.InitiatorTypeManual, Mode: domain.InitiatorModePropose}, domain.ErrInitiatorInconsistent},
		{"label missing label", domain.InitiatorConfig{Type: domain.InitiatorTypeLabel, Mode: domain.InitiatorModePropose}, domain.ErrEmptyField},
		{"label missing mode", domain.InitiatorConfig{Type: domain.InitiatorTypeLabel, Label: "freeside"}, domain.ErrInvalidInitiatorMode},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.config.Validate(); !errors.Is(err, tt.want) {
				t.Fatalf("Validate() = %v, want %v", err, tt.want)
			}
		})
	}
}
