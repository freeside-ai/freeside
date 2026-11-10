package daemoncompose

import (
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/specify"
)

// Scenario is one scripted run the ephemeral Scenario daemon (-driver
// Scenario) plays on the production engine composition: the task it submits,
// the repository the task runs against, and what each stage returns. Every
// stage is answered by a fake, so a run costs nothing and gives the same
// sequence of states every time. The format is private to the ephemeral tier.
type Scenario struct {
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
	// QuotaRefusal, when set, makes the provider refuse the first
	// implementation attempt on a quota window. The task parks until the
	// window resets, then the next attempt delivers.
	QuotaRefusal *ScenarioQuotaRefusal
}

// ScenarioQuotaRefusal is the refusal a quota scenario's first
// implementation attempt fails with.
type ScenarioQuotaRefusal struct {
	Window string
	// ResetAfter is how long after the refusal the window resets: long
	// enough to watch the park, short enough to wait out the resume.
	ResetAfter time.Duration
}

var scenarios = []Scenario{CleanScenario(), QuotaParkScenario()}

func CleanScenario() Scenario {
	title := "Describe the scenario repository"
	return Scenario{
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

// QuotaParkScenario is clean with a provider quota refusal on the first
// implementation attempt (#2421).
func QuotaParkScenario() Scenario {
	s := CleanScenario()
	s.Name = "quota-park"
	s.Description = "the provider refuses the first implementation on a quota window; " +
		"the task parks until the reset, then publishes"
	s.QuotaRefusal = &ScenarioQuotaRefusal{Window: "five_hour", ResetAfter: 2 * time.Minute}
	return s
}

func LookupScenario(name string) (Scenario, error) {
	for _, s := range scenarios {
		if s.Name == name {
			return s, nil
		}
	}
	names := make([]string, len(scenarios))
	for i, s := range scenarios {
		names[i] = s.Name
	}
	return Scenario{}, fmt.Errorf("-scenario %q is not one of: %s", name, strings.Join(names, ", "))
}

// ListScenarios prints each scenario's name and description, one per line,
// for -list-scenarios.
func ListScenarios(w io.Writer) error {
	for _, s := range scenarios {
		if _, err := fmt.Fprintf(w, "%s\t%s\n", s.Name, s.Description); err != nil {
			return err
		}
	}
	return nil
}

// allowedPaths is the run's path policy: exactly the files the implementer
// delivers, in sorted order.
func (s Scenario) allowedPaths() []string {
	return slices.Sorted(maps.Keys(s.Delivery))
}

// workspace is the candidate's whole tree: the base with the delivery
// written over it.
func (s Scenario) workspace() map[string]string {
	tree := maps.Clone(s.BaseFiles)
	maps.Copy(tree, s.Delivery)
	return tree
}
