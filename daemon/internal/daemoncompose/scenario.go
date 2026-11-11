package daemoncompose

import (
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/engine"
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
	// implementation attempt on a quota window. With a reset the task parks
	// until the window resets; without one it raises the failure card. Either
	// way the next attempt delivers.
	QuotaRefusal *ScenarioQuotaRefusal
	// SecondSubscription seeds a second Claude subscription on its own pool
	// and a routing policy that orders both for the implementer, so a park
	// or a failure card has another agent to offer.
	SecondSubscription bool
	// PublicationTransitionHook, when set, is the publication lane's
	// TransitionHook (engine.ProductionPublicationConfig). No named scenario
	// sets it: a test sets it to stop a run at a durable boundary, such as
	// before the publication effect, so the stored state it captures holds a
	// publication task with no forge effect.
	PublicationTransitionHook engine.DurableTransitionHook
}

// ScenarioQuotaRefusal is the refusal a quota scenario's first
// implementation attempt fails with.
type ScenarioQuotaRefusal struct {
	Window string
	// ResetAfter is how long after the refusal the window resets: long
	// enough to watch the park, short enough to wait out the resume. Zero
	// means the refusal names no reset.
	ResetAfter time.Duration
}

var scenarios = []Scenario{CleanScenario(), QuotaParkScenario(), QuotaMoveScenario(), QuotaNoResetScenario()}

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

// QuotaMoveScenario is quota-park with a second subscription and a reset far
// enough out that the operator moves the parked task to the second agent
// before it (#2951).
func QuotaMoveScenario() Scenario {
	s := QuotaParkScenario()
	s.Name = "quota-move"
	s.Description = "the provider refuses the first implementation on a quota window; " +
		"the operator moves the parked task to the second subscription, which publishes"
	s.QuotaRefusal.ResetAfter = 30 * time.Minute
	s.SecondSubscription = true
	return s
}

// QuotaNoResetScenario is clean with a second subscription and a quota
// refusal that names no reset, so the task raises the failure card with the
// alternate-agent retry (#2951). The seed gives each pool an allowed reading
// with no reset, which the client shows beside the card's offers.
func QuotaNoResetScenario() Scenario {
	s := CleanScenario()
	s.Name = "quota-no-reset"
	s.Description = "the provider refuses the first implementation on a quota window with no known reset; " +
		"the operator retries on the second subscription from the failure card, which publishes"
	s.QuotaRefusal = &ScenarioQuotaRefusal{Window: "five_hour"}
	s.SecondSubscription = true
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
