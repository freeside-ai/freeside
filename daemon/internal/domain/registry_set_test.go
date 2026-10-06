package domain_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// initialRegistryHosts is the plan §5.4 initial registry set, in canonical
// order.
var initialRegistryHosts = []string{
	"files.pythonhosted.org", "proxy.golang.org", "pypi.org",
	"registry.npmjs.org", "sum.golang.org",
}

const initialRegistryPolicyValue = `["files.pythonhosted.org","proxy.golang.org","pypi.org","registry.npmjs.org","sum.golang.org"]`

func egressPolicy(t *testing.T, values map[string]string) domain.ResolvedPolicy {
	t.Helper()
	keys := []domain.PolicyKey{{
		Key: "rein", Value: "tight",
		Provenance: domain.KeyProvenance{Source: domain.ProvenancePreset, Digest: "sha256:preset"},
	}}
	for key, value := range values {
		keys = append(keys, domain.PolicyKey{
			Key: key, Value: value,
			Provenance: domain.KeyProvenance{Source: domain.ProvenanceOverride, Digest: "sha256:project-policy"},
		})
	}
	policy, err := domain.NewResolvedPolicy("run-1", keys)
	if err != nil {
		t.Fatal(err)
	}
	return policy
}

func TestRegistrySetFromPolicyReadsTheCanonicalDeclaration(t *testing.T) {
	set, declared, err := domain.RegistrySetFromPolicy(egressPolicy(t, map[string]string{
		domain.RegistrySetPolicyKey: initialRegistryPolicyValue,
	}))
	if err != nil || !declared {
		t.Fatalf("registry set = declared %v, err %v; want a declared set", declared, err)
	}
	want, err := domain.NewRegistrySet(initialRegistryHosts)
	if err != nil {
		t.Fatal(err)
	}
	if set.EncodingVersion != want.EncodingVersion || strings.Join(set.Hosts, ",") != strings.Join(want.Hosts, ",") {
		t.Fatalf("registry set = %#v, want %#v", set, want)
	}
	gotDigest, err := set.Digest()
	if err != nil {
		t.Fatal(err)
	}
	wantDigest, err := want.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if gotDigest != wantDigest {
		t.Fatalf("digest = %q, want %q", gotDigest, wantDigest)
	}

	_, declared, err = domain.RegistrySetFromPolicy(egressPolicy(t, nil))
	if err != nil || declared {
		t.Fatalf("absent key = declared %v, err %v; want undeclared without error", declared, err)
	}
}

func TestRegistrySetFromPolicyRejectsEverythingButTheCanonicalForm(t *testing.T) {
	for name, value := range map[string]string{
		"empty value":       ``,
		"empty array":       `[]`,
		"null":              `null`,
		"object":            `{"hosts":["pypi.org"]}`,
		"versioned object":  `{"encoding_version":1,"hosts":["pypi.org"]}`,
		"string":            `"pypi.org"`,
		"non-string member": `["pypi.org",1]`,
		"null member":       `["pypi.org",null]`,
		"whitespace":        `["pypi.org", "sum.golang.org"]`,
		"trailing newline":  "[\"pypi.org\"]\n",
		"trailing data":     `["pypi.org"]["sum.golang.org"]`,
		// A JSON escape for "g": decodes to pypi.org, but is not its canonical
		// spelling.
		"escaped spelling":      `["pypi.or\` + `u0067"]`,
		"unsorted":              `["sum.golang.org","pypi.org"]`,
		"duplicate":             `["pypi.org","pypi.org"]`,
		"empty host":            `[""]`,
		"uppercase":             `["PyPI.org"]`,
		"scheme":                `["https://pypi.org"]`,
		"port":                  `["pypi.org:443"]`,
		"path":                  `["pypi.org/simple"]`,
		"userinfo":              `["user@pypi.org"]`,
		"wildcard":              `["*.pythonhosted.org"]`,
		"underscore":            `["my_registry.example.com"]`,
		"trailing dot":          `["pypi.org."]`,
		"leading dot":           `[".pypi.org"]`,
		"doubled dot":           `["pypi..org"]`,
		"leading hyphen label":  `["-pypi.org"]`,
		"trailing hyphen label": `["pypi-.org"]`,
		"non-ASCII":             `["pypí.org"]`,
		"single label":          `["registry"]`,
		"bare localhost":        `["localhost"]`,
		"localhost suffix":      `["registry.localhost"]`,
		"local suffix":          `["registry.local"]`,
		"internal suffix":       `["registry.corp.internal"]`,
		"test suffix":           `["registry.test"]`,
		"example suffix":        `["registry.example"]`,
		"invalid suffix":        `["registry.invalid"]`,
		"alt suffix":            `["registry.alt"]`,
		"onion suffix":          `["registry.onion"]`,
		"home.arpa suffix":      `["registry.home.arpa"]`,
		"reverse-lookup name":   `["1.0.0.127.in-addr.arpa"]`,
		"corp suffix":           `["registry.corp"]`,
		"home suffix":           `["registry.home"]`,
		"mail suffix":           `["registry.mail"]`,
		"IPv4 literal":          `["10.0.0.1"]`,
		"short IPv4 literal":    `["127.1"]`,
		"hex IPv4 literal":      `["0x7f.0x1"]`,
		"integer IPv4 literal":  `["registry.2130706433"]`,
		"bracketed IPv6":        `["[::1]"]`,
		"bare IPv6":             `["::1"]`,
		"64-byte label":         `["` + strings.Repeat("a", 64) + `.org"]`,
		"254-byte name":         `["` + strings.Repeat("a.", 124) + `bb.org"]`,
	} {
		t.Run(name, func(t *testing.T) {
			_, declared, err := domain.RegistrySetFromPolicy(egressPolicy(t, map[string]string{
				domain.RegistrySetPolicyKey: value,
			}))
			if !errors.Is(err, domain.ErrRegistrySetInvalid) || declared {
				t.Fatalf("value %q = declared %v, err %v; want ErrRegistrySetInvalid", value, declared, err)
			}
		})
	}
}

// The host rules refuse by class, so the names next to each boundary still
// have to pass: the longest label and name DNS carries, digits and hyphens
// inside a label, and a reserved word anywhere but the top label.
func TestRegistrySetAcceptsPublicHostNames(t *testing.T) {
	longestName := strings.Repeat("a.", 124) + "b.org"
	if len(longestName) != 253 {
		t.Fatalf("fixture is %d bytes, want 253", len(longestName))
	}
	for name, host := range map[string]string{
		"two labels":                "pypi.org",
		"63-byte label":             strings.Repeat("a", 63) + ".org",
		"253-byte name":             longestName,
		"digit-leading label":       "3rdparty.registry.io",
		"all-digit lower label":     "registry.123.io",
		"hyphen inside a label":     "my-registry.pkg.dev",
		"digit ending the top":      "registry.co2",
		"reserved word below top":   "local.registry.io",
		"reserved word as registry": "test.pypi.org",
		"punycode label":            "xn--bcher-kva.org",
	} {
		t.Run(name, func(t *testing.T) {
			set, err := domain.NewRegistrySet([]string{host})
			if err != nil {
				t.Fatalf("NewRegistrySet(%q) = %v, want it accepted", host, err)
			}
			if err := set.Validate(); err != nil {
				t.Fatalf("Validate() = %v", err)
			}
		})
	}
}

func TestRegistrySetValidateRejectsAForgedShape(t *testing.T) {
	valid, err := domain.NewRegistrySet(initialRegistryHosts)
	if err != nil {
		t.Fatal(err)
	}
	for name, set := range map[string]domain.RegistrySet{
		"zero value":       {},
		"unknown encoding": {EncodingVersion: 2, Hosts: valid.Hosts},
		"no hosts":         {EncodingVersion: domain.RegistrySetEncodingVersion},
		"unsorted hosts":   {EncodingVersion: domain.RegistrySetEncodingVersion, Hosts: []string{"pypi.org", "files.pythonhosted.org"}},
	} {
		t.Run(name, func(t *testing.T) {
			if err := set.Validate(); !errors.Is(err, domain.ErrRegistrySetInvalid) {
				t.Fatalf("Validate() = %v, want ErrRegistrySetInvalid", err)
			}
			if _, err := set.Digest(); !errors.Is(err, domain.ErrRegistrySetInvalid) {
				t.Fatalf("Digest() = %v, want ErrRegistrySetInvalid", err)
			}
		})
	}
}

func TestEgressProfileFromPolicySelectsOnlyTheWriterProfiles(t *testing.T) {
	for name, tc := range map[string]struct {
		values map[string]string
		want   domain.EgressProfile
	}{
		"absent key defaults to provider_only": {nil, domain.EgressProviderOnly},
		"explicit provider_only": {
			map[string]string{domain.EgressProfilePolicyKey: "provider_only"},
			domain.EgressProviderOnly,
		},
		// The rebuild gate reads the set whatever the writer's profile, so
		// declaring it does not opt the writer in.
		"a declared set alone does not opt in": {
			map[string]string{domain.RegistrySetPolicyKey: initialRegistryPolicyValue},
			domain.EgressProviderOnly,
		},
		"provider_registry with a declared set": {
			map[string]string{
				domain.EgressProfilePolicyKey: "provider_registry",
				domain.RegistrySetPolicyKey:   initialRegistryPolicyValue,
			},
			domain.EgressProviderRegistry,
		},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := domain.EgressProfileFromPolicy(egressPolicy(t, tc.values))
			if err != nil || got != tc.want {
				t.Fatalf("profile = %q, err %v; want %q", got, err, tc.want)
			}
		})
	}

	for name, tc := range map[string]struct {
		values map[string]string
		want   error
	}{
		"empty value":        {map[string]string{domain.EgressProfilePolicyKey: ""}, domain.ErrInvalidEgressProfile},
		"unknown value":      {map[string]string{domain.EgressProfilePolicyKey: "provider_everything"}, domain.ErrInvalidEgressProfile},
		"uppercase":          {map[string]string{domain.EgressProfilePolicyKey: "Provider_Registry"}, domain.ErrInvalidEgressProfile},
		"padded":             {map[string]string{domain.EgressProfilePolicyKey: " provider_registry"}, domain.ErrInvalidEgressProfile},
		"provider_web_read":  {map[string]string{domain.EgressProfilePolicyKey: "provider_web_read"}, domain.ErrInvalidEgressProfile},
		"clean_verification": {map[string]string{domain.EgressProfilePolicyKey: "clean_verification"}, domain.ErrInvalidEgressProfile},
		"opt-in without a set": {
			map[string]string{domain.EgressProfilePolicyKey: "provider_registry"},
			domain.ErrRegistrySetInvalid,
		},
		"opt-in with an empty set": {
			map[string]string{
				domain.EgressProfilePolicyKey: "provider_registry",
				domain.RegistrySetPolicyKey:   `[]`,
			},
			domain.ErrRegistrySetInvalid,
		},
		"opt-in with a malformed set": {
			map[string]string{
				domain.EgressProfilePolicyKey: "provider_registry",
				domain.RegistrySetPolicyKey:   `["https://pypi.org"]`,
			},
			domain.ErrRegistrySetInvalid,
		},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := domain.EgressProfileFromPolicy(egressPolicy(t, tc.values))
			if !errors.Is(err, tc.want) || got != "" {
				t.Fatalf("profile = %q, err %v; want %v and no profile", got, err, tc.want)
			}
		})
	}
}

// A project that declares neither key keeps the policy digest it had before
// the profile existed: the digest covers present keys only, so adding the
// readers changed no existing run's policy identity. The literal is the digest
// testdata/resolved_policy.golden has pinned for this key set since before the
// profile.
func TestPolicyWithoutEgressKeysKeepsItsDigest(t *testing.T) {
	const digestBeforeProviderRegistry = "sha256:dc1af0d4d3c8cdc2f6b59b72234a6988199c4d9dd9c3f2e663452eb58cd97ca7"
	policy := egressPolicy(t, nil)
	if policy.Digest != digestBeforeProviderRegistry {
		t.Fatalf("policy digest = %q, want the unchanged %q", policy.Digest, digestBeforeProviderRegistry)
	}
	if profile, err := domain.EgressProfileFromPolicy(policy); err != nil || profile != domain.EgressProviderOnly {
		t.Fatalf("profile = %q, err %v; want provider_only", profile, err)
	}
	opted := egressPolicy(t, map[string]string{
		domain.EgressProfilePolicyKey: "provider_registry",
		domain.RegistrySetPolicyKey:   initialRegistryPolicyValue,
	})
	if opted.Digest == policy.Digest {
		t.Fatal("a policy that declares the registry set shares the digest of one that does not")
	}
}
