package specify

import "strings"

// Pre-rename vocabulary (#986); the only file in the package that may spell
// it. A resolved policy is digest-addressed, so a stored one keeps its key
// names; ParsePolicy reads the legacy prefix as the current one instead.
const legacyPolicyKeyPrefix = "elaboration."

func canonicalPolicyKey(key string) string {
	if suffix, ok := strings.CutPrefix(key, legacyPolicyKeyPrefix); ok {
		return policyKeyPrefix + suffix
	}
	return key
}
