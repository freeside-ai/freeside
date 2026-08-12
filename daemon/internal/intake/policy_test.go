package intake

import (
	"errors"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// policyKey builds one resolved-policy key with the given value and provenance
// source. The provenance digest is a non-empty placeholder; PolicyKey.Validate
// requires only that it is set, and NewResolvedPolicy computes the bag digest.
func policyKey(key, value string, source domain.ProvenanceSource) domain.PolicyKey {
	return domain.PolicyKey{
		Key:   key,
		Value: value,
		Provenance: domain.KeyProvenance{
			Source: source,
			Digest: domain.Digest("sha256:seed"),
		},
	}
}

// resolvedWith builds a valid resolved policy carrying exactly the given keys.
func resolvedWith(t *testing.T, keys ...domain.PolicyKey) domain.ResolvedPolicy {
	t.Helper()
	resolved, err := domain.NewResolvedPolicy("run-1", keys)
	if err != nil {
		t.Fatalf("build resolved policy: %v", err)
	}
	return resolved
}

func TestParseIntakePolicyValid(t *testing.T) {
	cases := []struct {
		name          string
		capValue      string
		modeValue     string
		modeSource    domain.ProvenanceSource
		wantCap       int
		wantMode      domain.InitiatorMode
		wantAuthd     bool
		wantEffective domain.InitiatorMode
		wantDowngrade bool
	}{
		{
			name: "auto_start override authorized", capValue: "3",
			modeValue: "auto_start", modeSource: domain.ProvenanceOverride,
			wantCap: 3, wantMode: domain.InitiatorModeAutoStart, wantAuthd: true,
			wantEffective: domain.InitiatorModeAutoStart, wantDowngrade: false,
		},
		{
			name: "auto_start preset downgrades", capValue: "5",
			modeValue: "auto_start", modeSource: domain.ProvenancePreset,
			wantCap: 5, wantMode: domain.InitiatorModeAutoStart, wantAuthd: false,
			wantEffective: domain.InitiatorModePropose, wantDowngrade: true,
		},
		{
			name: "propose override is propose", capValue: "1",
			modeValue: "propose", modeSource: domain.ProvenanceOverride,
			wantCap: 1, wantMode: domain.InitiatorModePropose, wantAuthd: false,
			wantEffective: domain.InitiatorModePropose, wantDowngrade: false,
		},
		{
			name: "propose preset is propose", capValue: "1024",
			modeValue: "propose", modeSource: domain.ProvenancePreset,
			wantCap: 1024, wantMode: domain.InitiatorModePropose, wantAuthd: false,
			wantEffective: domain.InitiatorModePropose, wantDowngrade: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resolved := resolvedWith(t,
				policyKey(PolicyRunWIPCap, tc.capValue, domain.ProvenancePreset),
				policyKey(PolicyInitiatorMode, tc.modeValue, tc.modeSource),
			)
			p, err := ParseIntakePolicy(resolved)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if p.WIPCap != tc.wantCap {
				t.Errorf("WIPCap = %d, want %d", p.WIPCap, tc.wantCap)
			}
			if p.Mode != tc.wantMode {
				t.Errorf("Mode = %q, want %q", p.Mode, tc.wantMode)
			}
			if got := p.AutoStartAuthorized(); got != tc.wantAuthd {
				t.Errorf("AutoStartAuthorized = %v, want %v", got, tc.wantAuthd)
			}
			if got := p.EffectiveMode(); got != tc.wantEffective {
				t.Errorf("EffectiveMode = %q, want %q", got, tc.wantEffective)
			}
			if got := p.Downgraded(); got != tc.wantDowngrade {
				t.Errorf("Downgraded = %v, want %v", got, tc.wantDowngrade)
			}
		})
	}
}

func TestParseIntakePolicyWIPCapBounds(t *testing.T) {
	cases := []struct {
		value   string
		wantErr bool
	}{
		{"1", false},
		{"1024", false},
		{"0", true},
		{"-1", true},
		{"1025", true},
		{"abc", true},
		{"", true},
		{"3.5", true},
	}
	for _, tc := range cases {
		t.Run(tc.value, func(t *testing.T) {
			resolved := resolvedWith(t,
				policyKey(PolicyRunWIPCap, tc.value, domain.ProvenancePreset),
				policyKey(PolicyInitiatorMode, "propose", domain.ProvenancePreset),
			)
			_, err := ParseIntakePolicy(resolved)
			if tc.wantErr {
				if !errors.Is(err, ErrIntakePolicyMalformed) {
					t.Fatalf("cap %q: err = %v, want ErrIntakePolicyMalformed", tc.value, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("cap %q: unexpected err %v", tc.value, err)
			}
		})
	}
}

func TestParseIntakePolicyRejectsUnknownMode(t *testing.T) {
	resolved := resolvedWith(t,
		policyKey(PolicyRunWIPCap, "2", domain.ProvenancePreset),
		policyKey(PolicyInitiatorMode, "sprint", domain.ProvenanceOverride),
	)
	if _, err := ParseIntakePolicy(resolved); !errors.Is(err, ErrIntakePolicyMalformed) {
		t.Fatalf("err = %v, want ErrIntakePolicyMalformed", err)
	}
}

func TestParseIntakePolicyRequiresBothKeys(t *testing.T) {
	t.Run("missing cap", func(t *testing.T) {
		resolved := resolvedWith(t, policyKey(PolicyInitiatorMode, "propose", domain.ProvenancePreset))
		if _, err := ParseIntakePolicy(resolved); !errors.Is(err, ErrIntakePolicyMissing) {
			t.Fatalf("err = %v, want ErrIntakePolicyMissing", err)
		}
	})
	t.Run("missing mode", func(t *testing.T) {
		resolved := resolvedWith(t, policyKey(PolicyRunWIPCap, "2", domain.ProvenancePreset))
		if _, err := ParseIntakePolicy(resolved); !errors.Is(err, ErrIntakePolicyMissing) {
			t.Fatalf("err = %v, want ErrIntakePolicyMissing", err)
		}
	})
}

func TestParseIntakePolicyRejectsInvalidBag(t *testing.T) {
	// A zero-value resolved policy fails its own validation (empty run id, bad
	// digest); parsing must surface that, not read values from it.
	if _, err := ParseIntakePolicy(domain.ResolvedPolicy{}); err == nil {
		t.Fatal("expected an error parsing an invalid resolved policy")
	}
}

func TestWIPCapExhausted(t *testing.T) {
	p := IntakePolicy{WIPCap: 3}
	cases := []struct {
		active int
		want   bool
	}{
		{0, false},
		{2, false},
		{3, true},
		{4, true},
	}
	for _, tc := range cases {
		if got := p.WIPCapExhausted(tc.active); got != tc.want {
			t.Errorf("WIPCapExhausted(%d) = %v, want %v", tc.active, got, tc.want)
		}
	}
}
