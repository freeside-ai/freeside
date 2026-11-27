package domain_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/golden"
)

func admittedAgentBaseInput() domain.AdmittedAgentBaseInput {
	return domain.AdmittedAgentBaseInput{
		Harness:          domain.HarnessClientClaudeCode,
		ImageRef:         domain.ImageRef("127.0.0.1:5100/freeside-agent-claude@sha256:" + strings.Repeat("ab", 32)),
		HarnessBuild:     "claude-code 2.1.319",
		SourceRepoID:     1278475858,
		SourceRepository: "freeside-ai/freeside",
		SourceCommit:     strings.Repeat("1", 40),
		SourceTreeSHA:    strings.Repeat("2", 40),
		AdmittedAt:       time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC),
	}
}

// TestAdmittedAgentBaseGolden pins the record's shape and its content
// address; the fixture is also a validation-positive case.
func TestAdmittedAgentBaseGolden(t *testing.T) {
	t.Parallel()
	base, err := domain.NewAdmittedAgentBase(admittedAgentBaseInput())
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.MarshalIndent(base, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	golden.Assert(t, "admitted_agent_base", append(body, '\n'))
}

func TestAdmittedAgentBaseRefusesWhatItCannotAdmit(t *testing.T) {
	t.Parallel()
	for name, edit := range map[string]func(*domain.AdmittedAgentBaseInput){
		"unknown harness":       func(in *domain.AdmittedAgentBaseInput) { in.Harness = "other" },
		"unpinned image":        func(in *domain.AdmittedAgentBaseInput) { in.ImageRef = "example.test/base:latest" },
		"empty harness build":   func(in *domain.AdmittedAgentBaseInput) { in.HarnessBuild = "" },
		"control harness build": func(in *domain.AdmittedAgentBaseInput) { in.HarnessBuild = "claude-code\n2.1.319" },
		"padded harness build":  func(in *domain.AdmittedAgentBaseInput) { in.HarnessBuild = " claude-code 2.1.319" },
		"no repository id":      func(in *domain.AdmittedAgentBaseInput) { in.SourceRepoID = 0 },
		"bare repository":       func(in *domain.AdmittedAgentBaseInput) { in.SourceRepository = "freeside" },
		"short commit":          func(in *domain.AdmittedAgentBaseInput) { in.SourceCommit = "abc123" },
		"short tree":            func(in *domain.AdmittedAgentBaseInput) { in.SourceTreeSHA = strings.Repeat("2", 39) },
		"no time":               func(in *domain.AdmittedAgentBaseInput) { in.AdmittedAt = time.Time{} },
		"local time": func(in *domain.AdmittedAgentBaseInput) {
			in.AdmittedAt = in.AdmittedAt.In(time.FixedZone("x", 3600))
		},
	} {
		in := admittedAgentBaseInput()
		edit(&in)
		if _, err := domain.NewAdmittedAgentBase(in); err == nil {
			t.Errorf("%s: admitted", name)
		}
	}
}

// A changed fact no longer matches the ID that addressed the record, so a
// row whose columns were edited fails on decode.
func TestAdmittedAgentBaseIDAddressesEveryFactButTheTime(t *testing.T) {
	t.Parallel()
	base, err := domain.NewAdmittedAgentBase(admittedAgentBaseInput())
	if err != nil {
		t.Fatal(err)
	}
	later := base
	later.AdmittedAt = later.AdmittedAt.Add(time.Hour)
	if err := later.Validate(); err != nil {
		t.Fatalf("a later admission of the same base: %v", err)
	}
	moved := base
	moved.SourceTreeSHA = strings.Repeat("3", 40)
	if err := moved.Validate(); !errors.Is(err, domain.ErrAdmittedAgentBaseInvalid) {
		t.Fatalf("a changed tree under the old ID: %v, want ErrAdmittedAgentBaseInvalid", err)
	}
}

func TestAdmittedAgentBasePointerValidate(t *testing.T) {
	t.Parallel()
	valid := domain.AdmittedAgentBasePointer{
		Harness: domain.HarnessClientClaudeCode, BaseID: domain.Digest("sha256:" + strings.Repeat("ab", 32)),
		AdvancedAt: time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC),
	}
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, edit := range map[string]func(*domain.AdmittedAgentBasePointer){
		"unknown harness": func(p *domain.AdmittedAgentBasePointer) { p.Harness = "other" },
		"bad base id":     func(p *domain.AdmittedAgentBasePointer) { p.BaseID = "sha256:abc" },
		"no time":         func(p *domain.AdmittedAgentBasePointer) { p.AdvancedAt = time.Time{} },
	} {
		pointer := valid
		edit(&pointer)
		if err := pointer.Validate(); err == nil {
			t.Errorf("%s: valid", name)
		}
	}
}
