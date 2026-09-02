package specify

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

const (
	PolicySpecApproval          = "gates.spec_approval"
	PolicyMaxIterations         = "specification.max_iterations"
	PolicyStageActiveTime       = "budgets.stage_active_time"
	PolicyApprovalWait          = "waiting.spec_approval_attention_after"
	PolicyResearchAllowlist     = "research.allowlist"
	PolicyResearchMaxBytes      = "research.max_response_bytes"
	MaxSpecificationIterations  = 16
	MaxResearchAllowlistEntries = 64
	MaxResearchResponseBytes    = 16 << 20
)

// Policy is the typed engine-side view over the authenticated ResolvedPolicy bag.
type Policy struct {
	SpecApproval      bool
	MaxIterations     int
	StageActiveTime   time.Duration
	ApprovalWait      time.Duration
	ResearchAllowlist []string
	ResearchMaxBytes  int64
}

// ParsePolicy fails closed when a required key is missing or malformed.
func ParsePolicy(resolved domain.ResolvedPolicy) (Policy, error) {
	if err := resolved.Validate(); err != nil {
		return Policy{}, fmt.Errorf("specification policy: %w", err)
	}
	values := make(map[string]string, len(resolved.Keys))
	for _, key := range resolved.Keys {
		values[key.Key] = key.Value
	}
	require := func(key string) (string, error) {
		value, ok := values[key]
		if !ok {
			return "", fmt.Errorf("%w: %s", ErrPolicyMissing, key)
		}
		return value, nil
	}
	boolValue, err := require(PolicySpecApproval)
	if err != nil {
		return Policy{}, err
	}
	specApproval, err := strconv.ParseBool(boolValue)
	if err != nil {
		return Policy{}, fmt.Errorf("policy %s: %w", PolicySpecApproval, err)
	}
	iterationsValue, err := require(PolicyMaxIterations)
	if err != nil {
		return Policy{}, err
	}
	iterations, err := strconv.Atoi(iterationsValue)
	if err != nil || iterations < 1 || iterations > MaxSpecificationIterations {
		return Policy{}, fmt.Errorf("policy %s must be between 1 and %d",
			PolicyMaxIterations, MaxSpecificationIterations)
	}
	active, err := durationPolicy(require, PolicyStageActiveTime)
	if err != nil {
		return Policy{}, err
	}
	wait, err := durationPolicy(require, PolicyApprovalWait)
	if err != nil {
		return Policy{}, err
	}
	allowlistValue, err := require(PolicyResearchAllowlist)
	if err != nil {
		return Policy{}, err
	}
	allowlist := strings.Split(allowlistValue, ",")
	if len(allowlist) > MaxResearchAllowlistEntries {
		return Policy{}, fmt.Errorf("policy %s exceeds %d entries",
			PolicyResearchAllowlist, MaxResearchAllowlistEntries)
	}
	for i := range allowlist {
		allowlist[i] = strings.TrimSpace(allowlist[i])
		if allowlist[i] == "" {
			return Policy{}, fmt.Errorf("policy %s contains an empty entry", PolicyResearchAllowlist)
		}
	}
	maxBytesValue, err := require(PolicyResearchMaxBytes)
	if err != nil {
		return Policy{}, err
	}
	maxBytes, err := strconv.ParseInt(maxBytesValue, 10, 64)
	if err != nil || maxBytes < 1 || maxBytes > MaxResearchResponseBytes {
		return Policy{}, fmt.Errorf("policy %s must be between 1 and %d",
			PolicyResearchMaxBytes, MaxResearchResponseBytes)
	}
	return Policy{
		SpecApproval: specApproval, MaxIterations: iterations,
		StageActiveTime: active, ApprovalWait: wait,
		ResearchAllowlist: allowlist, ResearchMaxBytes: maxBytes,
	}, nil
}

func durationPolicy(require func(string) (string, error), key string) (time.Duration, error) {
	value, err := require(key)
	if err != nil {
		return 0, err
	}
	duration, err := time.ParseDuration(value)
	if err != nil || duration <= 0 {
		return 0, fmt.Errorf("policy %s must be a positive duration", key)
	}
	return duration, nil
}
