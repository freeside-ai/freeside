package main

import (
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"

	"github.com/freeside-ai/freeside/daemon/internal/specify"
)

// scenario is one scripted run the ephemeral scenario daemon (-driver
// scenario) plays on the production engine composition: the task it submits,
// the repository the task runs against, and what each stage returns. Every
// stage is answered by a fake, so a run costs nothing and gives the same
// sequence of states every time. The format is private to the ephemeral tier.
type scenario struct {
	Name        string
	Description string
	// Task is the task source `freesided submit` registers.
	Task string
	// BaseFiles is the scenario repository's one commit on main.
	BaseFiles map[string]string
	// Specification is what the specifier returns for the task.
	Specification specify.Specification
	// Delivery is the files the implementer writes over BaseFiles. Their
	// paths are the run's path allowlist.
	Delivery map[string]string
}

var scenarios = []scenario{cleanScenario()}

func cleanScenario() scenario {
	title := "Describe the scenario repository"
	return scenario{
		Name:        "clean",
		Description: "one task runs specification through publication with no findings",
		Task:        "Describe what the scenario repository is for in its README.\n",
		BaseFiles:   map[string]string{"README.md": "# Scenario repository\n"},
		Specification: specify.Specification{
			Title:   &title,
			Summary: "Say in the README what the scenario repository is for.",
			Body: "## Change\n\n" +
				"Add a paragraph to README.md saying the repository exists for scripted " +
				"Freeside runs and holds nothing else.\n\n" +
				"## Acceptance\n\n- README.md states the repository's purpose.",
			Addressals: []specify.Addressal{},
		},
		Delivery: map[string]string{
			"README.md": "# Scenario repository\n\n" +
				"This repository exists for scripted Freeside runs and holds nothing else.\n",
		},
	}
}

func lookupScenario(name string) (scenario, error) {
	for _, s := range scenarios {
		if s.Name == name {
			return s, nil
		}
	}
	names := make([]string, len(scenarios))
	for i, s := range scenarios {
		names[i] = s.Name
	}
	return scenario{}, fmt.Errorf("-scenario %q is not one of: %s", name, strings.Join(names, ", "))
}

// scenarioFromFlags resolves -scenario against -driver: the scenario to play
// under -driver scenario, or nil under any other driver. Each refusal is a
// usage error main reports with exit status 2.
func scenarioFromFlags(execution executionMode, name string) (*scenario, error) {
	if execution != executionScenario {
		if name != "" {
			return nil, errors.New("-scenario requires -driver scenario")
		}
		return nil, nil
	}
	if name == "" {
		return nil, errors.New("-driver scenario requires -scenario NAME (see -list-scenarios)")
	}
	played, err := lookupScenario(name)
	if err != nil {
		return nil, err
	}
	return &played, nil
}

// listScenarios prints each scenario's name and description, one per line,
// for -list-scenarios.
func listScenarios(w io.Writer) error {
	for _, s := range scenarios {
		if _, err := fmt.Fprintf(w, "%s\t%s\n", s.Name, s.Description); err != nil {
			return err
		}
	}
	return nil
}

// allowedPaths is the run's path policy: exactly the files the implementer
// delivers, in sorted order.
func (s scenario) allowedPaths() []string {
	return slices.Sorted(maps.Keys(s.Delivery))
}

// workspace is the candidate's whole tree: the base with the delivery
// written over it.
func (s scenario) workspace() map[string]string {
	tree := maps.Clone(s.BaseFiles)
	maps.Copy(tree, s.Delivery)
	return tree
}
