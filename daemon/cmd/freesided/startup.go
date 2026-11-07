package main

import (
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
)

// startupBindings holds the values startup composes after the attention
// service, whose options read them later. Each field is assigned at the
// point startup composes its value and stays nil in a composition that
// has none, so an option that runs earlier, or in another composition,
// takes its unavailable answer. Options read a field through the
// pointer at call time; one that copied a field at construction would
// keep nil forever.
type startupBindings struct {
	workflow       *engine.Engine
	claudeWiring   *claudeComposition
	agentCatalog   *signet.AgentCatalog
	upkeepTrigger  *imageUpkeepTrigger
	scenarioWiring *scenarioComposition
}
