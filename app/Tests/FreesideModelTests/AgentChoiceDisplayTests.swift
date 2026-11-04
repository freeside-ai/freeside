import FreesideAPI
import Testing

@testable import FreesideModel

/// The New Task form's agent choices: the lines they send, the stale-choice
/// check, each agent's facts, and the default entry's label. The
/// default catalog's claude-primary and claude-secondary share `anthropic`;
/// codex-primary has no lineage group. Its lineup binds specifier and
/// implementer to claude-primary, remediator to claude-secondary, and
/// reviewer to codex-primary.
@Suite struct AgentChoiceDisplayTests {
    typealias Line = Components.Schemas.TaskLineInput
    let catalog = UsageFixtures.defaultAgentCatalog()

    // MARK: Lines

    @Test func writerSetsTheThreeWriterRoles() {
        #expect(
            AgentChoiceDisplay.taskLines(writer: "claude-secondary", reviewer: nil) == [
                Line(role: .specifier, agent: "claude-secondary"),
                Line(role: .implementer, agent: "claude-secondary"),
                Line(role: .remediator, agent: "claude-secondary"),
            ])
    }

    @Test func reviewerSetsTheReviewerRole() {
        #expect(
            AgentChoiceDisplay.taskLines(writer: nil, reviewer: "codex-primary") == [
                Line(role: .reviewer, agent: "codex-primary")
            ])
    }

    @Test func writerAndReviewerSetFourLines() {
        let lines = AgentChoiceDisplay.taskLines(writer: "claude-secondary", reviewer: "codex-primary")
        #expect(lines?.map(\.role) == [.specifier, .implementer, .remediator, .reviewer])
        #expect(lines?.last?.agent == "codex-primary")
    }

    @Test func noChoiceSendsNoLinesRatherThanAnEmptyList() {
        #expect(AgentChoiceDisplay.taskLines(writer: nil, reviewer: nil) == nil)
    }

    // MARK: Stale choices

    @Test func aChoiceTheCatalogDroppedIsMissing() {
        #expect(AgentChoiceDisplay.missingAgents(writer: "claude-primary", reviewer: nil, catalog: catalog).isEmpty)
        #expect(
            AgentChoiceDisplay.missingAgents(writer: "claude-retired", reviewer: "codex-primary", catalog: catalog)
                == ["claude-retired"])
        #expect(
            AgentChoiceDisplay.missingAgents(writer: "gone", reviewer: "gone", catalog: catalog) == ["gone"])
    }

    @Test func withNoCatalogEveryChoiceIsMissing() {
        #expect(AgentChoiceDisplay.missingAgents(writer: nil, reviewer: nil, catalog: nil).isEmpty)
        #expect(
            AgentChoiceDisplay.missingAgents(writer: "claude-primary", reviewer: "codex-primary", catalog: nil)
                == ["claude-primary", "codex-primary"])
    }

    // MARK: Facts and labels

    @Test func factsNameModelEffortAndLineageGroup() {
        #expect(AgentChoiceDisplay.facts(catalog.agents[1]) == "claude-opus · Max · anthropic")
        #expect(AgentChoiceDisplay.facts(catalog.agents[2]) == "gpt-codex · Harness default · No lineage group")
    }

    @Test func defaultLabelSaysWhatPicksTheAgent() {
        let policy = UsageFixtures.defaultRoutingPolicy()
        #expect(
            AgentChoiceDisplay.defaultLabel(roles: [.reviewer], policy: policy, catalog: catalog)
                == "Default · lineup codex-primary")
        #expect(
            AgentChoiceDisplay.defaultLabel(roles: [.implementer], policy: policy, catalog: catalog)
                == "Default · lineup claude-primary or routing policy")
        #expect(
            AgentChoiceDisplay.defaultLabel(roles: AgentChoiceDisplay.writerRoles, policy: policy, catalog: catalog)
                == "Default · lineup or routing policy")
        #expect(
            AgentChoiceDisplay.defaultLabel(roles: AgentChoiceDisplay.writerRoles, policy: nil, catalog: catalog)
                == "Default · lineup")
        #expect(AgentChoiceDisplay.defaultLabel(roles: [.reviewer], policy: nil, catalog: nil) == "Default · lineup")
    }

    // MARK: Saved requests

    @Test func summaryNamesOneWriterOrEachRole() {
        #expect(AgentChoiceDisplay.summary(nil) == nil)
        #expect(
            AgentChoiceDisplay.summary(
                AgentChoiceDisplay.taskLines(writer: "claude-secondary", reviewer: "codex-primary"))
                == "Writer claude-secondary · Reviewer codex-primary")
        #expect(
            AgentChoiceDisplay.summary([
                Line(role: .reviewer, agent: "codex-primary"), Line(role: .implementer, agent: "claude-primary"),
            ]) == "Implementer claude-primary · Reviewer codex-primary")
    }
}
