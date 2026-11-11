package main

import (
	"errors"
	"fmt"
)

// validateRunConfig checks the flag combinations that no default can
// repair. It runs before startup acquires anything, so a refused
// configuration leaves no lock, listener, or store behind.
func validateRunConfig(cfg config) error {
	if !cfg.Environment.valid() {
		return fmt.Errorf("environment %q is not prod, dev, or ephemeral", cfg.Environment)
	}
	if cfg.DBPath == "" {
		return errors.New("-db is required")
	}
	if cfg.SeedWalkingSkeleton && !cfg.FakeDriverEnabled {
		return errors.New("-seed-walking-skeleton requires -driver fake")
	}
	if cfg.Scenario != nil {
		// Before the lock or the store: a non-ephemeral store must never
		// receive the fake backend's conformance claim.
		if cfg.Environment != environmentEphemeral {
			return fmt.Errorf("-driver scenario requires -environment %s, not %q",
				environmentEphemeral, cfg.Environment)
		}
		if cfg.FakeDriverEnabled || cfg.Claude != nil {
			return errors.New("-driver scenario cannot run beside another driver")
		}
	}
	return nil
}
