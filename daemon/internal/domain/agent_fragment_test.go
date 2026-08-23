package domain_test

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

func routeFragment(t *testing.T) domain.RouteFragment {
	t.Helper()
	f := domain.RouteFragment{
		EncodingVersion: domain.AgentFragmentEncodingVersion,
		ServiceOperator: "openai", Protocol: "chatgpt_backend",
		InferenceAuthorities: []string{"chatgpt.com"},
		BillingMode:          "subscription", FallbackPolicy: "fail_closed",
		TermsBasisDate: "2026-08-22",
	}
	digest, err := f.ComputeDigest()
	if err != nil {
		t.Fatal(err)
	}
	f.Digest = digest
	return f
}

func adapterFragment(t *testing.T) domain.AdapterFragment {
	t.Helper()
	f := domain.AdapterFragment{
		EncodingVersion: domain.AgentFragmentEncodingVersion,
		AdapterBuild:    "codex_proto_v1@build-7", HarnessBuild: "codex-cli 0.29.0",
		ClientKind: domain.HarnessClientCodexCLI, Vendor: domain.AgentVendorCodex,
		LaunchCapabilities: domain.NewLaunchCapabilitySet(
			domain.LaunchCapReadTools, domain.LaunchCapMutationTools,
			domain.LaunchCapInstructionDelivery, domain.LaunchCapStructuredOutput,
			domain.LaunchCapContextSeverance, domain.LaunchCapRouteStoreContract,
		),
		SendableEfforts: []domain.EffortLevel{domain.EffortMedium, domain.EffortHigh, domain.EffortMax},
	}
	digest, err := f.ComputeDigest()
	if err != nil {
		t.Fatal(err)
	}
	f.Digest = digest
	return f
}

func offerFragment(t *testing.T) domain.OfferFragment {
	t.Helper()
	f := domain.OfferFragment{
		EncodingVersion: domain.AgentFragmentEncodingVersion,
		RouteModelID:    "gpt-5.6-sol", LineageGroup: "openai",
		IdentityStability: domain.IdentityPinned,
		AllowedEfforts:    []domain.EffortLevel{domain.EffortHigh, domain.EffortMax},
		PricingRevision:   "2026-08",
		NotAfter:          time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	digest, err := f.ComputeDigest()
	if err != nil {
		t.Fatal(err)
	}
	f.Digest = digest
	return f
}

func TestRouteFragmentValidate(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*domain.RouteFragment)
		wantErr error
	}{
		{"valid", func(*domain.RouteFragment) {}, nil},
		{"wrong encoding version", func(f *domain.RouteFragment) { f.EncodingVersion = 2 }, domain.ErrAgentEncodingVersion},
		{"no operator", func(f *domain.RouteFragment) { f.ServiceOperator = "" }, domain.ErrEmptyField},
		{"no protocol", func(f *domain.RouteFragment) { f.Protocol = "" }, domain.ErrEmptyField},
		{"no authorities", func(f *domain.RouteFragment) { f.InferenceAuthorities = nil }, domain.ErrEmptyField},
		{"empty authority", func(f *domain.RouteFragment) { f.InferenceAuthorities = []string{""} }, domain.ErrEmptyField},
		{"no billing mode", func(f *domain.RouteFragment) { f.BillingMode = "" }, domain.ErrEmptyField},
		{"no fallback policy", func(f *domain.RouteFragment) { f.FallbackPolicy = "" }, domain.ErrEmptyField},
		{"undated terms basis", func(f *domain.RouteFragment) { f.TermsBasisDate = "recently" }, domain.ErrMissingTimestamp},
		{"digest is a name", func(f *domain.RouteFragment) { f.Digest = "openai_chatgpt_codex" }, domain.ErrInvalidDigest},
		{
			"digest of different content",
			func(f *domain.RouteFragment) { f.Protocol = "responses_api" },
			domain.ErrAgentDigestMismatch,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := routeFragment(t)
			tc.mutate(&f)
			if err := f.Validate(); !errors.Is(err, tc.wantErr) {
				t.Fatalf("Validate() = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func TestAdapterFragmentValidate(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*domain.AdapterFragment)
		wantErr error
	}{
		{"valid", func(*domain.AdapterFragment) {}, nil},
		{"no adapter build", func(f *domain.AdapterFragment) { f.AdapterBuild = "" }, domain.ErrEmptyField},
		{"no harness build", func(f *domain.AdapterFragment) { f.HarnessBuild = "" }, domain.ErrEmptyField},
		{"unknown client kind", func(f *domain.AdapterFragment) { f.ClientKind = "netscape" }, domain.ErrInvalidHarnessClientKind},
		{"unknown vendor", func(f *domain.AdapterFragment) { f.Vendor = "acme" }, domain.ErrInvalidAgentVendor},
		{"unknown capability", func(f *domain.AdapterFragment) {
			f.LaunchCapabilities = domain.LaunchCapabilitySet{"telepathy"}
		}, domain.ErrInvalidLaunchCapability},
		{"non-canonical capability order", func(f *domain.AdapterFragment) {
			f.LaunchCapabilities = domain.LaunchCapabilitySet{
				domain.LaunchCapReadTools, domain.LaunchCapMutationTools,
			}
		}, domain.ErrKeysNotCanonical},
		{"no sendable efforts", func(f *domain.AdapterFragment) { f.SendableEfforts = nil }, domain.ErrEmptyField},
		{"unknown sendable effort", func(f *domain.AdapterFragment) {
			f.SendableEfforts = []domain.EffortLevel{"ultra"}
		}, domain.ErrInvalidEffortLevel},
		{"duplicate sendable effort", func(f *domain.AdapterFragment) {
			f.SendableEfforts = []domain.EffortLevel{domain.EffortMax, domain.EffortMax}
		}, domain.ErrDuplicate},
		{
			"digest of different content",
			func(f *domain.AdapterFragment) { f.HarnessBuild = "codex-cli 0.30.0" },
			domain.ErrAgentDigestMismatch,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := adapterFragment(t)
			tc.mutate(&f)
			if err := f.Validate(); !errors.Is(err, tc.wantErr) {
				t.Fatalf("Validate() = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func TestOfferFragmentValidate(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*domain.OfferFragment)
		wantErr error
	}{
		{"valid", func(*domain.OfferFragment) {}, nil},
		{
			// Unknown lineage is a legal authored state the §7 independence
			// rule fails closed on, not a malformed document.
			"unknown lineage", func(f *domain.OfferFragment) {
				f.LineageGroup = ""
				digest, err := f.ComputeDigest()
				if err != nil {
					t.Fatal(err)
				}
				f.Digest = digest
			}, nil,
		},
		{"no model id", func(f *domain.OfferFragment) { f.RouteModelID = "" }, domain.ErrEmptyField},
		{"unknown stability", func(f *domain.OfferFragment) { f.IdentityStability = "wobbly" }, domain.ErrInvalidIdentityStability},
		{"no efforts", func(f *domain.OfferFragment) { f.AllowedEfforts = nil }, domain.ErrEmptyField},
		{"no pricing revision", func(f *domain.OfferFragment) { f.PricingRevision = "" }, domain.ErrEmptyField},
		{"no expiry", func(f *domain.OfferFragment) { f.NotAfter = time.Time{} }, domain.ErrMissingTimestamp},
		{"non-UTC expiry", func(f *domain.OfferFragment) {
			f.NotAfter = f.NotAfter.In(time.FixedZone("PDT", -7*3600))
		}, domain.ErrTimestampNotUTC},
		{
			"digest of different content",
			func(f *domain.OfferFragment) { f.RouteModelID = "gpt-5.7" },
			domain.ErrAgentDigestMismatch,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := offerFragment(t)
			tc.mutate(&f)
			if err := f.Validate(); !errors.Is(err, tc.wantErr) {
				t.Fatalf("Validate() = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func TestFragmentRoundTrips(t *testing.T) {
	route := routeFragment(t)
	routeBody, err := route.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if decoded, err := domain.DecodeRouteFragment(routeBody); err != nil || decoded.Digest != route.Digest {
		t.Fatalf("route round-trip = %+v, %v", decoded, err)
	}
	adapter := adapterFragment(t)
	adapterBody, err := adapter.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if decoded, err := domain.DecodeAdapterFragment(adapterBody); err != nil || decoded.Digest != adapter.Digest {
		t.Fatalf("adapter round-trip = %+v, %v", decoded, err)
	}
	offer := offerFragment(t)
	offerBody, err := offer.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if decoded, err := domain.DecodeOfferFragment(offerBody); err != nil || decoded.Digest != offer.Digest {
		t.Fatalf("offer round-trip = %+v, %v", decoded, err)
	}
	if _, err := domain.DecodeRouteFragment(append(routeBody, "trailing"...)); err == nil {
		t.Fatal("trailing data decoded")
	}
}

func TestLaunchCapabilitySet(t *testing.T) {
	set := domain.NewLaunchCapabilitySet(
		domain.LaunchCapReadTools, domain.LaunchCapReadTools, domain.LaunchCapExactResume,
	)
	want := domain.LaunchCapabilitySet{domain.LaunchCapExactResume, domain.LaunchCapReadTools}
	if !slices.Equal(set, want) {
		t.Fatalf("NewLaunchCapabilitySet = %v, want %v", set, want)
	}
	if !set.Has(domain.LaunchCapReadTools) || set.Has(domain.LaunchCapMutationTools) {
		t.Fatal("Has misreports membership")
	}
	if domain.NewLaunchCapabilitySet() != nil {
		t.Fatal("empty set is not nil")
	}
	missing := domain.MissingLaunchCapabilities(set, []domain.LaunchCapability{
		domain.LaunchCapReadTools, domain.LaunchCapMutationTools, domain.LaunchCapStructuredOutput,
	})
	wantMissing := []domain.LaunchCapability{domain.LaunchCapMutationTools, domain.LaunchCapStructuredOutput}
	if !slices.Equal(missing, wantMissing) {
		t.Fatalf("MissingLaunchCapabilities = %v, want %v", missing, wantMissing)
	}
	for _, capability := range domain.AllLaunchCapabilities {
		if err := domain.NewLaunchCapabilitySet(capability).Validate(); err != nil {
			t.Fatalf("capability %q: %v", capability, err)
		}
	}
	for _, effort := range domain.AllEffortLevels {
		f := adapterFragment(t)
		f.SendableEfforts = []domain.EffortLevel{effort}
		digest, err := f.ComputeDigest()
		if err != nil {
			t.Fatal(err)
		}
		f.Digest = digest
		if err := f.Validate(); err != nil {
			t.Fatalf("effort %q: %v", effort, err)
		}
	}
	for _, stability := range domain.AllIdentityStabilities {
		f := offerFragment(t)
		f.IdentityStability = stability
		digest, err := f.ComputeDigest()
		if err != nil {
			t.Fatal(err)
		}
		f.Digest = digest
		if err := f.Validate(); err != nil {
			t.Fatalf("stability %q: %v", stability, err)
		}
	}
}
