package domain

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/strictjson"
)

const (
	// RegistrySetEncodingVersion tags the registry set's canonical encoding.
	RegistrySetEncodingVersion = 1
	// EgressProfilePolicyKey names the resolved-policy key that selects the
	// writer's egress profile. An absent key means provider_only.
	EgressProfilePolicyKey = "execution.egress_profile"
	// RegistrySetPolicyKey names the resolved-policy key that declares the
	// project's registry set as a canonical JSON array of host names.
	RegistrySetPolicyKey = "execution.registry_set"

	// registrySetPolicyLimit bounds the policy value. The plan calls the set
	// short; the bound only keeps a hostile value from being decoded at
	// length.
	registrySetPolicyLimit = strictjson.Limit(1 << 16)
)

// reservedRegistrySuffixes are the top-level labels that never name a public
// package registry: the IANA special-use names (alt, arpa, example, invalid,
// local, localhost, onion, test) and the labels ICANN withholds from
// delegation because private networks use them (corp, home, internal, mail).
//
// The list is the reserved registries, not every private convention. A name
// such as "nexus.lan" is refused by nothing here; whether a declared name
// resolves to a public address is the proxy's check at connection time.
var reservedRegistrySuffixes = []string{
	"alt", "arpa", "corp", "example", "home", "internal", "invalid", "local",
	"localhost", "mail", "onion", "test",
}

// RegistrySet is the project policy's declared set of package-registry
// authorities (plan §5.4): the hosts the provider_registry profile exposes to
// the writer and the policy-gated image rebuild reads whatever the writer's
// profile (plan §5.15). The set is control-plane policy; this type carries it
// in the canonical form its content address covers.
type RegistrySet struct {
	EncodingVersion int      `json:"encoding_version"`
	Hosts           []string `json:"hosts"`
}

// NewRegistrySet builds a validated set from hosts already in canonical
// order. It does not sort or deduplicate: a declaration that is not canonical
// is refused rather than repaired, so one set has exactly one policy value.
func NewRegistrySet(hosts []string) (RegistrySet, error) {
	set := RegistrySet{
		EncodingVersion: RegistrySetEncodingVersion,
		Hosts:           slices.Clone(hosts),
	}
	if err := set.Validate(); err != nil {
		return RegistrySet{}, err
	}
	return set, nil
}

// Validate reports whether the set is well-formed: the known encoding, at
// least one host, every host a public DNS name, and the hosts strictly
// ascending (sorted and free of duplicates).
//
// The host check is syntactic. Whether a name is a registry the project's
// dependency manifests resolve against is the operator's reviewed decision
// (plan §5.4); the daemon refuses only what could never be one.
func (s RegistrySet) Validate() error {
	if s.EncodingVersion != RegistrySetEncodingVersion {
		return fmt.Errorf("registry set encoding_version %d: %w",
			s.EncodingVersion, ErrRegistrySetInvalid)
	}
	if len(s.Hosts) == 0 {
		return fmt.Errorf("registry set declares no host: %w", ErrRegistrySetInvalid)
	}
	for index, host := range s.Hosts {
		if err := validateRegistryHost(host); err != nil {
			return fmt.Errorf("registry set hosts[%d]: %w", index, err)
		}
		if index > 0 && host <= s.Hosts[index-1] {
			return fmt.Errorf("registry set host %q after %q is not strictly ascending: %w",
				host, s.Hosts[index-1], ErrRegistrySetInvalid)
		}
	}
	return nil
}

// Digest returns the content address of the set's canonical encoding.
func (s RegistrySet) Digest() (Digest, error) {
	if err := s.Validate(); err != nil {
		return "", err
	}
	body, err := json.Marshal(s)
	if err != nil {
		return "", fmt.Errorf("registry set canonical encoding: %w", err)
	}
	return Digest(contentaddr.Sum(body)), nil
}

// validateRegistryHost admits only a bare, lowercase, public DNS host name.
// A scheme, port, path, userinfo, IPv6 literal, or uppercase letter fails the
// character check; the rules after it refuse what the characters still allow.
func validateRegistryHost(host string) error {
	invalid := func(reason string) error {
		return fmt.Errorf("registry host %q %s: %w", host, reason, ErrRegistrySetInvalid)
	}
	// 253 is the longest name DNS can carry in presentation form.
	if host == "" || len(host) > 253 {
		return invalid("is empty or longer than 253 bytes")
	}
	labels := strings.Split(host, ".")
	if len(labels) < 2 {
		return invalid("is a single label")
	}
	for _, label := range labels {
		// An empty label is a leading, trailing, or doubled dot.
		if label == "" || len(label) > 63 {
			return invalid("has an empty label or one longer than 63 bytes")
		}
		if label[0] == '-' || label[len(label)-1] == '-' {
			return invalid("has a label that starts or ends with a hyphen")
		}
		for index := 0; index < len(label); index++ {
			c := label[index]
			if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
				return invalid("has a character outside lowercase letters, digits, hyphens, and dots")
			}
		}
	}
	// No public top-level domain starts with a digit, and every spelling a
	// resolver reads as an IPv4 address ends in a numeric label: dotted
	// decimal, the short forms ("127.1"), and the hexadecimal ones ("0x7f.0x1").
	top := labels[len(labels)-1]
	if top[0] >= '0' && top[0] <= '9' {
		return invalid("is an IP literal or ends in a numeric label")
	}
	if slices.Contains(reservedRegistrySuffixes, top) {
		return invalid("is under a reserved or local suffix")
	}
	return nil
}

func policyValue(policy ResolvedPolicy, key string) (string, bool) {
	for _, entry := range policy.Keys {
		if entry.Key == key {
			return entry.Value, true
		}
	}
	return "", false
}

// RegistrySetFromPolicy strictly reconstructs the declared registry set from
// resolved policy. The bool reports whether the policy declares one; an absent
// key is not an error, because a project that never opts in declares nothing.
// A present value must be the canonical JSON array of the set's hosts: the
// value is control-plane input, so a form that merely decodes to an acceptable
// set is refused.
func RegistrySetFromPolicy(policy ResolvedPolicy) (RegistrySet, bool, error) {
	value, found := policyValue(policy, RegistrySetPolicyKey)
	if !found {
		return RegistrySet{}, false, nil
	}
	var hosts []string
	if err := strictjson.Decode(
		[]byte(value), &hosts, strictjson.RejectInvalidUTF8, registrySetPolicyLimit,
	); err != nil {
		return RegistrySet{}, false, fmt.Errorf("policy %s: %w",
			RegistrySetPolicyKey, errors.Join(err, ErrRegistrySetInvalid))
	}
	if hosts == nil {
		return RegistrySet{}, false, fmt.Errorf("policy %s must be a JSON array: %w",
			RegistrySetPolicyKey, ErrRegistrySetInvalid)
	}
	canonical, err := json.Marshal(hosts)
	if err != nil || !bytes.Equal(canonical, []byte(value)) {
		return RegistrySet{}, false, fmt.Errorf("policy %s is not canonical JSON: %w",
			RegistrySetPolicyKey, errors.Join(err, ErrRegistrySetInvalid))
	}
	set, err := NewRegistrySet(hosts)
	if err != nil {
		return RegistrySet{}, false, fmt.Errorf("policy %s: %w", RegistrySetPolicyKey, err)
	}
	return set, true, nil
}

// DeclaredRegistrySet returns the registry set the policy declares and refuses
// a policy that declares none. It is the reader for a consumer that cannot
// proceed without a set: a writer admitted under provider_registry, whichever
// of policy or a capability manifest selected the profile.
func DeclaredRegistrySet(policy ResolvedPolicy) (RegistrySet, error) {
	set, declared, err := RegistrySetFromPolicy(policy)
	if err != nil {
		return RegistrySet{}, err
	}
	if !declared {
		return RegistrySet{}, fmt.Errorf("policy declares no %s: %w",
			RegistrySetPolicyKey, ErrRegistrySetInvalid)
	}
	return set, nil
}

// EgressProfileFromPolicy returns the egress profile the resolved policy
// requests for the writer. An absent key means provider_only, the default
// (plan §5.4). The key selects only provider_only or provider_registry:
// provider_web_read is selected through a capability manifest with its own
// wider-exposure record, and clean_verification is never the writer's.
//
// Requesting provider_registry requires the policy to declare a valid
// registry set, so an opt-in can never name an empty or malformed allowlist.
// The result is what the policy requests, not what the composition can
// enforce; admission checks that separately.
func EgressProfileFromPolicy(policy ResolvedPolicy) (EgressProfile, error) {
	value, found := policyValue(policy, EgressProfilePolicyKey)
	if !found {
		return EgressProviderOnly, nil
	}
	profile := EgressProfile(value)
	if profile != EgressProviderOnly && profile != EgressProviderRegistry {
		return "", fmt.Errorf("policy %s %q: %w",
			EgressProfilePolicyKey, value, ErrInvalidEgressProfile)
	}
	if profile == EgressProviderRegistry {
		if _, err := DeclaredRegistrySet(policy); err != nil {
			return "", fmt.Errorf("policy %s %q: %w", EgressProfilePolicyKey, value, err)
		}
	}
	return profile, nil
}
