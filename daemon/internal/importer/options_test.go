package importer

import (
	"errors"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/export"
)

const testBaseSHA = "0123456789abcdef0123456789abcdef01234567"

func TestOptionsDefaults(t *testing.T) {
	o := Options{BaseSHA: testBaseSHA}.withDefaults()
	if o.CommitMessage != DefaultCommitMessage || o.AuthorName != DefaultAuthorName ||
		o.AuthorEmail != DefaultAuthorEmail || o.GitPath != "git" {
		t.Fatalf("defaults not applied: %+v", o)
	}
	p := o.Policy
	if p.MaxManifestBytes != DefaultMaxManifestBytes || p.MaxEntries != DefaultMaxEntries ||
		p.MaxBlobBytes != DefaultMaxBlobBytes || p.MaxTotalBytes != DefaultMaxTotalBytes ||
		p.SecretMaxScanBytes != DefaultSecretMaxScan || p.MaxPathBytes != DefaultMaxPathBytes ||
		p.MaxPathDepth != DefaultMaxPathDepth || p.CommitPlan != domain.CommitPlanSingleCommit ||
		p.MessageRuleset != domain.MessageRulesetGitHub1 || p.MaxCommitPlanBytes != export.DefaultMaxCommitPlanBytes ||
		p.MaxCommitPlanGroups != DefaultMaxCommitPlanGroups || p.MaxCommitMessageBytes != DefaultMaxCommitMessageBytes {
		t.Fatalf("policy defaults not applied: %+v", p)
	}
}

func TestOptionsValidate(t *testing.T) {
	cases := []struct {
		name string
		opts Options
		ok   bool
	}{
		{"valid", Options{BaseSHA: testBaseSHA}, true},
		{"valid with ref", Options{BaseSHA: testBaseSHA, ImportRef: "refs/freeside/imports/x"}, true},
		{"missing base", Options{}, false},
		{"short base", Options{BaseSHA: "abc123"}, false},
		{"uppercase base", Options{BaseSHA: strings.ToUpper(testBaseSHA)}, false},
		{"non-hex base", Options{BaseSHA: strings.Repeat("g", 40)}, false},
		{"unqualified ref", Options{BaseSHA: testBaseSHA, ImportRef: "main"}, false},
		{"negative cap", Options{BaseSHA: testBaseSHA, Policy: Policy{MaxEntries: -1}}, false},
		{"invalid commit plan", Options{BaseSHA: testBaseSHA, Policy: Policy{CommitPlan: "plan_required"}}, false},
		{"invalid message ruleset", Options{BaseSHA: testBaseSHA, Policy: Policy{MessageRuleset: "github/2"}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.opts.validate()
			if tc.ok && err != nil {
				t.Fatalf("validate() = %v, want nil", err)
			}
			if !tc.ok && !errors.Is(err, ErrInvalidOptions) {
				t.Fatalf("validate() = %v, want %v", err, ErrInvalidOptions)
			}
		})
	}
}
