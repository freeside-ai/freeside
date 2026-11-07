import FreesideAPI
import Testing

@testable import FreesideModel

/// The New Task form's agent choices: the lines they send, the stale-choice
/// check, each agent's facts, and the default entry's label. A choice is the
/// selection a catalog agent runs. The default catalog's claude-primary and
/// claude-secondary share `anthropic`; codex-primary has no lineage group.
/// Its lineup binds specifier and implementer to claude-primary, remediator
/// to claude-secondary, and reviewer to codex-primary.
@Suite struct AgentChoiceDisplayTests {
    typealias Line = Components.Schemas.TaskLineInput
    typealias Selection = AgentChoiceDisplay.Selection
    let catalog = UsageFixtures.defaultAgentCatalog()

    private func selection(_ agent: String) -> Selection { UsageFixtures.agentSelection(agent) }

    /// A selection no catalog agent carries.
    private func retired(_ model: String) -> Selection {
        UsageFixtures.selection(UsageFixtures.readingCollectorID, model, "high")
    }

    // MARK: Lines

    @Test func oneAgentForTheWriterRolesAndAReviewerSendsTheOldWriterAndReviewerLines() {
        // The old Writer and Reviewer triggers sent these four lines, in this
        // order, for writer claude-secondary and reviewer codex-primary.
        let choices: [AgentChoiceDisplay.Role: Selection] = [
            .reviewer: selection("codex-primary"), .remediator: selection("claude-secondary"),
            .implementer: selection("claude-secondary"), .specifier: selection("claude-secondary"),
        ]
        #expect(
            AgentChoiceDisplay.taskLines(choices: choices) == [
                Line(role: .specifier, selection: selection("claude-secondary")),
                Line(role: .implementer, selection: selection("claude-secondary")),
                Line(role: .remediator, selection: selection("claude-secondary")),
                Line(role: .reviewer, selection: selection("codex-primary")),
            ])
    }

    @Test func eachChosenRoleSendsOneLineInRoleOrder() {
        #expect(
            AgentChoiceDisplay.taskLines(choices: [
                .reviewer: selection("codex-primary"), .implementer: selection("claude-primary"),
            ]) == [
                Line(role: .implementer, selection: selection("claude-primary")),
                Line(role: .reviewer, selection: selection("codex-primary")),
            ])
        #expect(AgentChoiceDisplay.roleOrder == Components.Schemas.TaskLineRole.allCases)
    }

    @Test func noChoiceSendsNoLinesRatherThanAnEmptyList() {
        #expect(AgentChoiceDisplay.taskLines(choices: [:]) == nil)
    }

    // MARK: Stale choices

    @Test func aChoiceTheCatalogNoLongerOffersIsMissing() {
        #expect(
            AgentChoiceDisplay.missingSelections(choices: [.implementer: selection("claude-primary")], catalog: catalog)
                .isEmpty)
        #expect(
            AgentChoiceDisplay.missingSelections(
                choices: [.implementer: retired("claude-retired"), .reviewer: selection("codex-primary")],
                catalog: catalog)
                == ["\(UsageFixtures.readingCollectorID) · claude-retired · high"])
        #expect(
            AgentChoiceDisplay.missingSelections(
                choices: [.specifier: retired("gone"), .reviewer: retired("gone")], catalog: catalog)
                == ["\(UsageFixtures.readingCollectorID) · gone · high"])
    }

    @Test func aChoiceWhoseEnrollmentTheCatalogDroppedIsMissing() {
        var dropped = catalog
        dropped.enrollments.removeAll { $0.enrollment_id == selection("codex-primary").enrollment_id }
        #expect(!AgentChoiceDisplay.offers(dropped, selection("codex-primary")))
        #expect(
            AgentChoiceDisplay.missingSelections(choices: [.reviewer: selection("codex-primary")], catalog: dropped)
                == [AgentChoiceDisplay.selectionLabel(selection("codex-primary"))])
    }

    @Test func thePickerListsEachOfferedSelectionOnceUnderItsFirstAgent() {
        #expect(AgentChoiceDisplay.pickable(catalog).map(\.name) == catalog.agents.map(\.name))
        #expect(AgentChoiceDisplay.pickable(nil).isEmpty)
        // A twin (one definition under a second name) adds no row, and the
        // chip reads the row's name for it.
        var twinned = catalog
        var twin = catalog.agents[0]
        twin.name = "claude-twin"
        twinned.agents.append(twin)
        #expect(AgentChoiceDisplay.pickable(twinned).map(\.name) == catalog.agents.map(\.name))
        #expect(AgentChoiceDisplay.choiceLabel(twin.selection, catalog: twinned) == catalog.agents[0].name)
        // An agent on an enrollment the daemon no longer lists isn't offered.
        var dropped = catalog
        dropped.enrollments.removeAll { $0.enrollment_id == selection("codex-primary").enrollment_id }
        #expect(!AgentChoiceDisplay.pickable(dropped).map(\.name).contains("codex-primary"))
    }

    @Test func missingSelectionsFollowRoleOrder() {
        let missing = AgentChoiceDisplay.missingSelections(
            choices: [
                .reviewer: retired("old-reviewer"), .remediator: retired("old-remediator"),
                .specifier: retired("old-specifier"),
            ],
            catalog: catalog)
        #expect(
            missing
                == ["old-specifier", "old-remediator", "old-reviewer"].map {
                    "\(UsageFixtures.readingCollectorID) · \($0) · high"
                })
    }

    @Test func withNoCatalogOrNoTreeEveryChoiceIsMissing() {
        let choices: [AgentChoiceDisplay.Role: Selection] = [
            .specifier: selection("claude-primary"), .reviewer: selection("codex-primary"),
        ]
        let labels = [selection("claude-primary"), selection("codex-primary")].map(AgentChoiceDisplay.selectionLabel)
        #expect(AgentChoiceDisplay.missingSelections(choices: [:], catalog: nil).isEmpty)
        #expect(AgentChoiceDisplay.missingSelections(choices: choices, catalog: nil) == labels)
        #expect(
            AgentChoiceDisplay.missingSelections(choices: choices, catalog: UsageFixtures.treelessAgentCatalog())
                == labels)
    }

    // MARK: Facts and labels

    @Test func factsNameModelEffortAndLineageGroup() {
        #expect(AgentChoiceDisplay.facts(catalog.agents[1]) == "claude-opus · Max · anthropic")
        #expect(AgentChoiceDisplay.facts(catalog.agents[2]) == "gpt-codex · Harness default · No lineage group")
    }

    @Test func aSelectionReadsAsItsEnrollmentModelAndEffort() {
        let codex = selection("codex-primary")
        #expect(
            AgentChoiceDisplay.selectionLabel(codex)
                == "\(UsageFixtures.unsupportedCollectorID) · gpt-codex · harness_default")
        // A choice reads as the tree agent that carries it, else as itself.
        #expect(AgentChoiceDisplay.choiceLabel(codex, catalog: catalog) == "codex-primary")
        #expect(
            AgentChoiceDisplay.choiceLabel(retired("gone"), catalog: catalog)
                == "\(UsageFixtures.readingCollectorID) · gone · high")
        #expect(AgentChoiceDisplay.choiceLabel(codex, catalog: nil) == AgentChoiceDisplay.selectionLabel(codex))
    }

    @Test func defaultLabelSaysWhatPicksTheRolesAgent() {
        let policy = UsageFixtures.defaultRoutingPolicy()
        let codex = AgentChoiceDisplay.selectionLabel(selection("codex-primary"))
        let claude = AgentChoiceDisplay.selectionLabel(selection("claude-primary"))
        let secondary = AgentChoiceDisplay.selectionLabel(selection("claude-secondary"))
        #expect(
            AgentChoiceDisplay.defaultLabel(role: .reviewer, policy: policy, catalog: catalog)
                == "Default · lineup codex-primary (\(codex))")
        #expect(
            AgentChoiceDisplay.defaultLabel(role: .implementer, policy: policy, catalog: catalog)
                == "Default · lineup claude-primary (\(claude)) or routing policy")
        #expect(
            AgentChoiceDisplay.defaultLabel(role: .remediator, policy: nil, catalog: catalog)
                == "Default · lineup claude-secondary (\(secondary))")
        #expect(AgentChoiceDisplay.defaultLabel(role: .reviewer, policy: nil, catalog: nil) == "Default · lineup")
        #expect(
            AgentChoiceDisplay.defaultLabel(
                role: .reviewer, policy: nil, catalog: UsageFixtures.treelessAgentCatalog()) == "Default · lineup")
    }

    // MARK: Summary

    @Test func summaryReadsTheWriterOnceWhenTheThreeWriterLinesAgree() {
        let writer = selection("claude-primary")
        #expect(
            AgentChoiceDisplay.summary(
                AgentChoiceDisplay.taskLines(choices: [
                    .specifier: writer, .implementer: writer, .remediator: writer,
                    .reviewer: selection("codex-primary"),
                ]), catalog: catalog) == "claude-primary writes · codex-primary reviews")
        #expect(
            AgentChoiceDisplay.summary(
                AgentChoiceDisplay.taskLines(choices: [.specifier: writer, .implementer: writer, .remediator: writer]),
                catalog: catalog) == "claude-primary writes")
    }

    @Test func summaryReadsEachRoleWhenTheWriterLinesDiffer() {
        #expect(
            AgentChoiceDisplay.summary(
                [
                    Line(role: .specifier, selection: selection("claude-primary")),
                    Line(role: .implementer, selection: selection("codex-primary")),
                    Line(role: .remediator, selection: selection("codex-primary")),
                ], catalog: catalog) == "claude-primary specifies · codex-primary implements and remediates")
        // Saved lines may come in any order; the summary reads role order.
        #expect(
            AgentChoiceDisplay.summary(
                [
                    Line(role: .reviewer, selection: selection("codex-primary")),
                    Line(role: .implementer, selection: selection("claude-primary")),
                ], catalog: catalog) == "claude-primary implements · codex-primary reviews")
    }

    @Test func summaryNamesASelectionNoAgentCarriesByItsLabel() {
        let label = AgentChoiceDisplay.selectionLabel(selection("codex-primary"))
        #expect(
            AgentChoiceDisplay.summary([Line(role: .reviewer, selection: selection("codex-primary"))])
                == "\(label) reviews")
        #expect(
            AgentChoiceDisplay.summary([Line(role: .reviewer, selection: retired("gone"))], catalog: catalog)
                == "\(UsageFixtures.readingCollectorID) · gone · high reviews")
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
