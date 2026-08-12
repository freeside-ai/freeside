package domain_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/golden"
	"github.com/freeside-ai/freeside/daemon/internal/strictjson"
)

func proposalPolicy(t *testing.T) domain.ResolvedPolicy {
	t.Helper()
	p, err := domain.NewResolvedPolicy("run-policy", []domain.PolicyKey{{
		Key: "paths", Value: "daemon/", Provenance: domain.KeyProvenance{
			Source: domain.ProvenanceOverride,
			Digest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func validEffectProposal(t *testing.T) domain.EffectProposal {
	t.Helper()
	p, err := domain.NewEffectProposal(domain.EffectRunProposal, domain.RunProposalParameters{
		SubjectHandle: "subject-opaque-1", Intent: domain.RunProposalIntentImplement,
		ExpectedCostUnits: 10,
		Scope:             domain.RunProposalScope{ComponentCount: 1, DeclaredPathCount: 4},
	}, proposalPolicy(t))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestEffectProposalGoldenAndRoundTrip(t *testing.T) {
	proposal := validEffectProposal(t)
	body, err := proposal.Encode()
	if err != nil {
		t.Fatal(err)
	}
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, body, "", "  "); err != nil {
		t.Fatal(err)
	}
	pretty.WriteByte('\n')
	golden.Assert(t, "effect_proposal", pretty.Bytes())
	decoded, err := domain.DecodeEffectProposal(body)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Digest != proposal.Digest || decoded.RunProposal.SubjectHandle != "subject-opaque-1" {
		t.Fatalf("decoded = %#v", decoded)
	}
}

func TestEffectProposalRegistryAndGateFailClosed(t *testing.T) {
	policy := proposalPolicy(t)
	if _, err := domain.NewEffectProposal("unknown", domain.RunProposalParameters{}, policy); !errors.Is(err, domain.ErrInvalidEffectKind) {
		t.Fatalf("unknown kind error = %v", err)
	}
	if _, err := domain.NewEffectProposal(domain.EffectRunProposal, "wrong type", policy); !errors.Is(err, domain.ErrEffectProposalInconsistent) {
		t.Fatalf("wrong type error = %v", err)
	}
	proposal := validEffectProposal(t)
	otherPolicy, err := domain.NewResolvedPolicy("other-run", []domain.PolicyKey{{
		Key: "paths", Value: "app/", Provenance: domain.KeyProvenance{Source: domain.ProvenanceOverride, Digest: policy.Keys[0].Provenance.Digest},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := domain.GateEffectProposal(proposal, otherPolicy); !errors.Is(err, domain.ErrProposalPolicyMismatch) {
		t.Fatalf("policy mismatch error = %v", err)
	}
}

func TestDecodeEffectProposalRejectsUntrustedShapes(t *testing.T) {
	body, err := validEffectProposal(t).Encode()
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		body []byte
		want error
	}{
		{"unknown field", bytes.Replace(body, []byte(`"digest"`), []byte(`"unknown":true,"digest"`), 1), nil},
		{"trailing value", append(append([]byte{}, body...), []byte(` {}`)...), strictjson.ErrTrailingData},
		{"invalid utf8", append([]byte{'"', 0xff, '"'}, body...), strictjson.ErrInvalidUTF8},
		{"oversized", []byte(`{"padding":"` + strings.Repeat("x", domain.MaxEffectProposalBytes) + `"}`), strictjson.ErrLimitExceeded},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, got := domain.DecodeEffectProposal(tc.body)
			if got == nil || (tc.want != nil && !errors.Is(got, tc.want)) {
				t.Fatalf("error = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestProposalAdmissionKeyEnumeratesIdentitySpace(t *testing.T) {
	if len(domain.AllEffectKinds) != 1 || domain.AllEffectKinds[0] != domain.EffectRunProposal {
		t.Fatalf("effect registry = %v", domain.AllEffectKinds)
	}
	if len(domain.AllProposalAdmissionSources) != 3 || len(domain.AllRunProposalIntents) != 1 {
		t.Fatalf("admission source registry = %v", domain.AllProposalAdmissionSources)
	}
	export := domain.Digest("sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	valid := []domain.ProposalAdmissionKey{
		{Source: domain.ProposalSourceUpstreamEvent, UpstreamEventID: "github:event:1"},
		{Source: domain.ProposalSourceClientCommand, SubmissionCommandID: "command-1"},
		{Source: domain.ProposalSourceRunEmission, InvocationID: "inv-1", EmissionOrdinal: 1},
		{Source: domain.ProposalSourceRunEmission, ExportIdentity: export, EmissionOrdinal: 2},
	}
	seen := map[string]bool{}
	for _, key := range valid {
		got, err := key.String()
		if err != nil {
			t.Fatalf("key %#v: %v", key, err)
		}
		if seen[got] {
			t.Fatalf("distinct key %#v collided as %q", key, got)
		}
		seen[got] = true
	}
	invalid := []domain.ProposalAdmissionKey{
		{},
		{Source: domain.ProposalSourceUpstreamEvent},
		{Source: domain.ProposalSourceUpstreamEvent, UpstreamEventID: "e", SubmissionCommandID: "c"},
		{Source: domain.ProposalSourceClientCommand, SubmissionCommandID: "c", EmissionOrdinal: 1},
		{Source: domain.ProposalSourceRunEmission, InvocationID: "i"},
		{Source: domain.ProposalSourceRunEmission, InvocationID: "i", ExportIdentity: export, EmissionOrdinal: 1},
		{Source: domain.ProposalSourceRunEmission, ExportIdentity: "not-a-digest", EmissionOrdinal: 1},
		{Source: domain.ProposalSourceUpstreamEvent, UpstreamEventID: " event"},
		{Source: domain.ProposalSourceClientCommand, SubmissionCommandID: strings.Repeat("c", domain.MaxProposalOccurrenceIDBytes+1)},
		{Source: domain.ProposalSourceRunEmission, InvocationID: domain.InvocationID(string([]byte{'i', 0xff})), EmissionOrdinal: 1},
	}
	for _, key := range invalid {
		if _, err := key.String(); err == nil {
			t.Errorf("invalid key %#v accepted", key)
		}
	}
}
