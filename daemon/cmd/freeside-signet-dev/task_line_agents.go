package main

import (
	"errors"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
)

// convergenceLineAgents is the harness's task line bridge. It has no agent
// tree and runs no agent, so it accepts every agent name a client sends, as
// the harness did when lines held names, and gives each name a selection of
// its own: the name as the enrollment, and a fixed model and effort. The
// selection maps back to that name, so a submitted line shows the agent the
// client chose. Signet checks a name's shape before it reaches the bridge.
type convergenceLineAgents struct{}

const (
	convergenceLineModel  = "convergence-model"
	convergenceLineEffort = "high"
)

var _ signet.TaskLineAgents = convergenceLineAgents{}

func (convergenceLineAgents) SelectionOfAgent(name string) (domain.TaskLineSelection, error) {
	return domain.TaskLineSelection{
		EnrollmentID: domain.ClientEnrollmentID(name), ModelID: convergenceLineModel, Effort: convergenceLineEffort,
	}, nil
}

func (convergenceLineAgents) AgentForSelection(
	_ domain.RoleName, selection domain.TaskLineSelection,
) (string, error) {
	if selection.ModelID != convergenceLineModel || selection.Effort != convergenceLineEffort {
		return "", errors.New("the harness runs no agent for that selection")
	}
	return string(selection.EnrollmentID), nil
}

// AgentDigest gives each name its own definition, since each has its own
// selection.
func (convergenceLineAgents) AgentDigest(name string) (domain.Digest, error) {
	return convergenceDigest("agent:" + name), nil
}
