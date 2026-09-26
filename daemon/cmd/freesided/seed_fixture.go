package main

import (
	"errors"
	"fmt"

	"github.com/freeside-ai/freeside/daemon/internal/seedfixture"
)

// parseSeedFixture resolves -seed-fixture and refuses every configuration in
// which fixture state could reach a supervised store or an engine: fixtures
// are development data for an ephemeral, driverless daemon only (#1503).
func parseSeedFixture(cfg config) (seedfixture.Name, error) {
	name, err := seedfixture.Parse(cfg.SeedFixture)
	if err != nil {
		return "", fmt.Errorf("-seed-fixture: %w", err)
	}
	if cfg.Environment != environmentEphemeral {
		return "", fmt.Errorf("-seed-fixture requires -environment %s, not %q", environmentEphemeral, cfg.Environment)
	}
	if cfg.FakeDriverEnabled || cfg.Claude != nil {
		return "", errors.New("-seed-fixture requires -driver disabled")
	}
	return name, nil
}
