package main

import (
	"errors"

	"github.com/freeside-ai/freeside/daemon/internal/daemoncompose"
)

// scenarioFromFlags resolves -scenario against -driver: the scenario to play
// under -driver scenario, or nil under any other driver. Each refusal is a
// usage error main reports with exit status 2.
func scenarioFromFlags(execution executionMode, name string) (*daemoncompose.Scenario, error) {
	if execution != executionScenario {
		if name != "" {
			return nil, errors.New("-scenario requires -driver scenario")
		}
		return nil, nil
	}
	if name == "" {
		return nil, errors.New("-driver scenario requires -scenario NAME (see -list-scenarios)")
	}
	played, err := daemoncompose.LookupScenario(name)
	if err != nil {
		return nil, err
	}
	return &played, nil
}
