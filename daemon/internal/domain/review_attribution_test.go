package domain_test

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec/fake"
)

// reviewAttributionCase is one record kind seen through the attribution
// fields both kinds share, so the rule is exercised once for each.
type reviewAttributionCase struct {
	name string
	role domain.RoleName
	// build returns a record of this kind's validation and labels under the
	// given attribution.
	build func(admission *domain.ReviewAdmission, provider, model, payer string) (func() error, string, string)
}

func reviewAttributionCases() []reviewAttributionCase {
	return []reviewAttributionCase{
		{
			name: "review record", role: domain.RoleReviewer,
			build: func(admission *domain.ReviewAdmission, provider, model, payer string) (func() error, string, string) {
				record := validReviewRecord()
				record.Admission = admission
				record.LegacyProvider, record.LegacyModelConfiguration, record.LegacyCostOwner = provider, model, payer
				return record.Validate, record.Provider(), record.ModelConfiguration()
			},
		},
		{
			name: "shadow review record", role: domain.RoleShadowReviewer,
			build: func(admission *domain.ReviewAdmission, provider, model, payer string) (func() error, string, string) {
				record := validShadowReviewRecord()
				record.Admission = admission
				record.LegacyProvider, record.LegacyModelConfiguration, record.LegacyCostOwner = provider, model, payer
				return record.Validate, record.Provider(), record.ModelConfiguration()
			},
		},
	}
}

// TestReviewRecordAttributionIsSnapshotOrLegacy covers the rule both record
// kinds share: a record names its reviewer through an admission snapshot or
// through the three strings a record written before the snapshot stored,
// never both and never neither.
func TestReviewRecordAttributionIsSnapshotOrLegacy(t *testing.T) {
	t.Parallel()
	for _, kind := range reviewAttributionCases() {
		t.Run(kind.name, func(t *testing.T) {
			t.Parallel()
			runID, id := domain.RunID("run-1"), domain.InvocationID("review-run-1-1")
			if kind.role == domain.RoleShadowReviewer {
				id = "shadow-run-1-1"
			}
			snapshot := func(mutate func(*domain.ReviewAdmission)) *domain.ReviewAdmission {
				admission := fake.RecordAdmission(kind.role, runID, id)
				if mutate != nil {
					mutate(admission)
				}
				return admission
			}
			otherRole := domain.RoleShadowReviewer
			if kind.role == domain.RoleShadowReviewer {
				otherRole = domain.RoleReviewer
			}
			for _, tc := range []struct {
				name                   string
				admission              *domain.ReviewAdmission
				provider, model, payer string
				want                   error
			}{
				{name: "snapshot", admission: snapshot(nil)},
				{name: "legacy", provider: "openai", model: "gpt-codex/high", payer: "owner"},
				{name: "neither", want: domain.ErrEmptyField},
				{name: "legacy without provider", model: "gpt-codex/high", payer: "owner", want: domain.ErrEmptyField},
				{name: "legacy without model configuration", provider: "openai", payer: "owner", want: domain.ErrEmptyField},
				{name: "legacy without cost owner", provider: "openai", model: "gpt-codex/high", want: domain.ErrEmptyField},
				{
					name: "both", admission: snapshot(nil), provider: "openai", model: "gpt-codex/high", payer: "owner",
					want: domain.ErrReviewAttribution,
				},
				{name: "snapshot and provider", admission: snapshot(nil), provider: "openai", want: domain.ErrReviewAttribution},
				{
					name: "snapshot and model configuration", admission: snapshot(nil), model: "gpt-codex/high",
					want: domain.ErrReviewAttribution,
				},
				{name: "snapshot and cost owner", admission: snapshot(nil), payer: "owner", want: domain.ErrReviewAttribution},
				{
					name:      "snapshot of another invocation",
					admission: snapshot(func(a *domain.ReviewAdmission) { a.InvocationID = "another" }),
					want:      domain.ErrParentKeyMismatch,
				},
				{
					name:      "snapshot of another run",
					admission: snapshot(func(a *domain.ReviewAdmission) { a.RunID = "another" }),
					want:      domain.ErrParentKeyMismatch,
				},
				{
					name:      "snapshot of the other arm",
					admission: snapshot(func(a *domain.ReviewAdmission) { a.Role = otherRole }),
					want:      domain.ErrReviewAdmissionInvalid,
				},
				{
					name:      "invalid snapshot",
					admission: snapshot(func(a *domain.ReviewAdmission) { a.AuthIdentityID = "" }),
					want:      domain.ErrEmptyID,
				},
			} {
				validate, _, _ := kind.build(tc.admission, tc.provider, tc.model, tc.payer)
				if err := validate(); !errors.Is(err, tc.want) || (tc.want == nil && err != nil) {
					t.Errorf("%s: Validate = %v, want %v", tc.name, err, tc.want)
				}
			}
		})
	}
}

// TestReviewRecordLabelsComeFromItsAttribution pins what a reader shows: a
// legacy record's strings exactly as stored, and a snapshot's lineage group
// and admitted model and effort, with an unrecorded lineage shown as unknown.
func TestReviewRecordLabelsComeFromItsAttribution(t *testing.T) {
	t.Parallel()
	for _, kind := range reviewAttributionCases() {
		t.Run(kind.name, func(t *testing.T) {
			t.Parallel()
			_, provider, model := kind.build(nil, "codex <frontier>", "any/string at all", "owner")
			if provider != "codex <frontier>" || model != "any/string at all" {
				t.Errorf("legacy labels = %q, %q", provider, model)
			}
			admission := fake.RecordAdmission(kind.role, "run-1", "review-run-1-1")
			admission.LineageGroup = "anthropic"
			admission.AgentBinding.RouteModelID, admission.AgentBinding.NativeEffort = "claude-test", "max"
			_, provider, model = kind.build(admission, "", "", "")
			if provider != "anthropic" || model != "claude-test/max" {
				t.Errorf("snapshot labels = %q, %q", provider, model)
			}
			admission.LineageGroup = ""
			if _, provider, _ = kind.build(admission, "", "", ""); provider != domain.ReviewProviderUnknown {
				t.Errorf("unrecorded lineage shown as %q", provider)
			}
		})
	}
}

// TestLegacyReviewRecordEncodesToItsStoredShape guards the read boundary's
// byte check: a legacy record re-encodes with its three strings under their
// original keys and no admission key, so a stored body still matches.
func TestLegacyReviewRecordEncodesToItsStoredShape(t *testing.T) {
	t.Parallel()
	record := validReviewRecord()
	record.Admission = nil
	record.LegacyProvider, record.LegacyModelConfiguration, record.LegacyCostOwner = "openai", "gpt-codex/high", "owner"
	shadow := validShadowReviewRecord()
	shadow.Admission = nil
	shadow.LegacyProvider, shadow.LegacyModelConfiguration, shadow.LegacyCostOwner = "anthropic", "claude/max", "owner"
	for name, value := range map[string]any{"review record": record, "shadow review record": shadow} {
		body, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(body, &fields); err != nil {
			t.Fatal(err)
		}
		for _, key := range []string{"provider", "model_configuration", "cost_owner"} {
			if _, ok := fields[key]; !ok {
				t.Errorf("%s: legacy key %q missing from %s", name, key, body)
			}
		}
		if strings.Contains(string(body), `"admission"`) {
			t.Errorf("%s: legacy body carries an admission key: %s", name, body)
		}
	}
	body, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	var decoded domain.ReviewRecord
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decoded, record) {
		t.Fatalf("legacy record round trip = %#v, want %#v", decoded, record)
	}
}

// TestReviewRecordSharesSegment pins which records convergence reads as one
// reviewer's consecutive passes: the same review configuration and the same
// reviewer agent. Two legacy records are told apart by configuration alone, and
// a legacy record never shares a segment with one that carries a snapshot.
func TestReviewRecordSharesSegment(t *testing.T) {
	t.Parallel()
	configuration := domain.Digest("sha256:" + strings.Repeat("c", 64))
	otherConfiguration := domain.Digest("sha256:" + strings.Repeat("d", 64))
	admitted := func(configuration domain.Digest, agent string, mutate func(*domain.ReviewAdmission)) domain.ReviewRecord {
		admission := fake.RecordAdmission(domain.RoleReviewer, "run-1", "review-run-1-1")
		admission.AgentBinding.AgentDigest = domain.Digest("sha256:" + strings.Repeat(agent, 64))
		if mutate != nil {
			mutate(admission)
		}
		return domain.ReviewRecord{ConfigurationDigest: configuration, Admission: admission}
	}
	legacy := func(configuration domain.Digest, provider string) domain.ReviewRecord {
		return domain.ReviewRecord{
			ConfigurationDigest: configuration, LegacyProvider: provider,
			LegacyModelConfiguration: "gpt-codex/high", LegacyCostOwner: "owner",
		}
	}
	for _, tc := range []struct {
		name        string
		left, right domain.ReviewRecord
		want        bool
	}{
		{name: "same reviewer", left: admitted(configuration, "a", nil), right: admitted(configuration, "a", nil), want: true},
		{
			name: "same reviewer admitted again", left: admitted(configuration, "a", nil),
			right: admitted(configuration, "a", func(a *domain.ReviewAdmission) {
				a.InvocationID, a.AuthIdentityID = "review-run-1-2", "another-identity"
				a.AgentBinding.LaunchDigest = domain.Digest("sha256:" + strings.Repeat("f", 64))
				a.AgentBinding.EnrollmentGeneration++
			}),
			want: true,
		},
		{name: "another reviewer", left: admitted(configuration, "a", nil), right: admitted(configuration, "b", nil)},
		{
			name: "another configuration", left: admitted(configuration, "a", nil),
			right: admitted(otherConfiguration, "a", nil),
		},
		{name: "two legacy records", left: legacy(configuration, "openai"), right: legacy(configuration, "openai"), want: true},
		{
			name: "two legacy records under other labels", left: legacy(configuration, "openai"),
			right: legacy(configuration, "anthropic"), want: true,
		},
		{
			name: "two legacy records under another configuration", left: legacy(configuration, "openai"),
			right: legacy(otherConfiguration, "openai"),
		},
		{name: "legacy then snapshot", left: legacy(configuration, "openai"), right: admitted(configuration, "a", nil)},
	} {
		if got := tc.left.SharesSegment(tc.right); got != tc.want {
			t.Errorf("%s: SharesSegment = %v, want %v", tc.name, got, tc.want)
		}
		if got := tc.right.SharesSegment(tc.left); got != tc.want {
			t.Errorf("%s reversed: SharesSegment = %v, want %v", tc.name, got, tc.want)
		}
	}
}
