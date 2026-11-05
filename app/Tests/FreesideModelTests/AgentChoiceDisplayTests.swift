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

    @Test func oneAgentForTheWriterRolesAndAReviewerSendsTheOldWriterAndReviewerLines() {
        // The old Writer and Reviewer triggers sent exactly these four lines,
        // in this order, for writer claude-secondary and reviewer codex-primary.
        let choices: [AgentChoiceDisplay.Role: String] = [
            .reviewer: "codex-primary", .remediator: "claude-secondary",
            .implementer: "claude-secondary", .specifier: "claude-secondary",
        ]
        #expect(
            AgentChoiceDisplay.taskLines(choices: choices) == [
                Line(role: .specifier, agent: "claude-secondary"),
                Line(role: .implementer, agent: "claude-secondary"),
                Line(role: .remediator, agent: "claude-secondary"),
                Line(role: .reviewer, agent: "codex-primary"),
            ])
    }

    @Test func eachChosenRoleSendsOneLineInRoleOrder() {
        #expect(
            AgentChoiceDisplay.taskLines(choices: [.reviewer: "codex-primary", .implementer: "claude-primary"]) == [
                Line(role: .implementer, agent: "claude-primary"),
                Line(role: .reviewer, agent: "codex-primary"),
            ])
        #expect(AgentChoiceDisplay.roleOrder == Components.Schemas.TaskLineRole.allCases)
    }

    @Test func noChoiceSendsNoLinesRatherThanAnEmptyList() {
        #expect(AgentChoiceDisplay.taskLines(choices: [:]) == nil)
    }

    // MARK: Stale choices

    @Test func aChoiceTheCatalogDroppedIsMissing() {
        #expect(AgentChoiceDisplay.missingAgents(choices: [.implementer: "claude-primary"], catalog: catalog).isEmpty)
        #expect(
            AgentChoiceDisplay.missingAgents(
                choices: [.implementer: "claude-retired", .reviewer: "codex-primary"], catalog: catalog)
                == ["claude-retired"])
        #expect(
            AgentChoiceDisplay.missingAgents(choices: [.specifier: "gone", .reviewer: "gone"], catalog: catalog)
                == ["gone"])
    }

    @Test func missingAgentsFollowRoleOrder() {
        #expect(
            AgentChoiceDisplay.missingAgents(
                choices: [.reviewer: "old-reviewer", .remediator: "old-remediator", .specifier: "old-specifier"],
                catalog: catalog) == ["old-specifier", "old-remediator", "old-reviewer"])
    }

    @Test func withNoCatalogEveryChoiceIsMissing() {
        #expect(AgentChoiceDisplay.missingAgents(choices: [:], catalog: nil).isEmpty)
        #expect(
            AgentChoiceDisplay.missingAgents(
                choices: [.specifier: "claude-primary", .reviewer: "codex-primary"], catalog: nil)
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

    // MARK: Summary

    @Test func summaryReadsTheWriterOnceWhenTheThreeWriterLinesAgree() {
        #expect(
            AgentChoiceDisplay.summary(
                AgentChoiceDisplay.taskLines(choices: [
                    .specifier: "claude-opus", .implementer: "claude-opus", .remediator: "claude-opus",
                    .reviewer: "codex",
                ])) == "claude-opus writes · codex reviews")
        #expect(
            AgentChoiceDisplay.summary(
                AgentChoiceDisplay.taskLines(choices: [
                    .specifier: "claude-opus", .implementer: "claude-opus", .remediator: "claude-opus",
                ])) == "claude-opus writes")
    }

    @Test func summaryReadsEachRoleWhenTheWriterLinesDiffer() {
        #expect(
            AgentChoiceDisplay.summary([
                Line(role: .specifier, agent: "claude-opus"), Line(role: .implementer, agent: "codex"),
                Line(role: .remediator, agent: "codex"),
            ]) == "claude-opus specifies · codex implements and remediates")
        // Saved lines may come in any order; the summary reads role order.
        #expect(
            AgentChoiceDisplay.summary([
                Line(role: .reviewer, agent: "codex-primary"), Line(role: .implementer, agent: "claude-primary"),
            ]) == "claude-primary implements · codex-primary reviews")
        #expect(
            AgentChoiceDisplay.summary([Line(role: .reviewer, agent: "codex-primary")]) == "codex-primary reviews")
    }

    @Test func summaryWithNoLinesIsNilSoTheChipReadsDefaultAgents() {
        #expect(AgentChoiceDisplay.summary(nil) == nil)
        #expect(AgentChoiceDisplay.summary([]) == nil)
        #expect(AgentChoiceDisplay.summary(AgentChoiceDisplay.taskLines(choices: [:])) == nil)
    }

    @Test func titlesNameEachRole() {
        #expect(
            AgentChoiceDisplay.roleOrder.map(AgentChoiceDisplay.title(for:)) == [
                "Specifier", "Implementer", "Remediator", "Reviewer",
            ])
    }
}
