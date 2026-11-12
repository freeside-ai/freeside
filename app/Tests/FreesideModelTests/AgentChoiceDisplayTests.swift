import Foundation
import FreesideAPI
import Testing

@testable import FreesideModel

/// The New Task form's agent choices: the lines they send, what each
/// enrollment offers and how a pick fills the selection, the readings beside
/// each enrollment, the stale-choice check, and the labels. The default
/// catalog lists three enrollments: claude-a-mac and codex-b-mac discovered
/// with different models, and claude-a-mini not discovered yet, where only
/// claude-secondary runs. Its lineup binds specifier and implementer to
/// claude-primary (claude-a-mac, claude-opus, high), remediator to
/// claude-secondary (claude-a-mini, claude-opus, max), and reviewer to
/// codex-primary (codex-b-mac, gpt-codex, harness_default).
@Suite struct AgentChoiceDisplayTests {
    typealias Line = Components.Schemas.TaskLineInput
    typealias Selection = AgentChoiceDisplay.Selection
    let catalog = UsageFixtures.defaultAgentCatalog()

    private func selection(_ agent: String) -> Selection { UsageFixtures.agentSelection(agent) }

    /// A selection on claude-a-mac whose model its discovery doesn't list.
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
        #expect(AgentChoiceDisplay.roleOrder == Components.Schemas.TaskLineRole.allCases.filter { $0 != .unknown })
    }

    @Test func noChoiceSendsNoLinesRatherThanAnEmptyList() {
        #expect(AgentChoiceDisplay.taskLines(choices: [:]) == nil)
    }

    // MARK: Picking

    @Test func eachEnrollmentOffersItsDiscoveredModelsOrItsTreeAgentsSelections() {
        let claude = AgentChoiceDisplay.models(on: UsageFixtures.readingCollectorID, catalog: catalog)
        #expect(claude.map(\.id) == ["claude-opus", "claude-sonnet"])
        #expect(claude.map(\.name) == ["Opus", "Sonnet"])
        #expect(claude[1].efforts == ["harness_default", "low", "medium", "high"])
        let codex = AgentChoiceDisplay.models(on: UsageFixtures.unsupportedCollectorID, catalog: catalog)
        #expect(codex.map(\.id) == ["gpt-codex", "gpt-mini"])
        // A model with no effort levels takes only the harness default.
        #expect(codex[1].efforts == ["harness_default"])
        // Not discovered yet: what the lineup runs on it, by id.
        #expect(
            AgentChoiceDisplay.models(on: UsageFixtures.readingSecondEnrollmentID, catalog: catalog) == [
                .init(id: "claude-opus", name: "claude-opus", efforts: ["max"])
            ])
        #expect(
            AgentChoiceDisplay.models(
                on: UsageFixtures.readingSecondEnrollmentID, catalog: UsageFixtures.treelessAgentCatalog()
            ).isEmpty)
        #expect(AgentChoiceDisplay.models(on: "enrollment-unknown", catalog: catalog).isEmpty)
        #expect(AgentChoiceDisplay.models(on: UsageFixtures.readingCollectorID, catalog: nil).isEmpty)
    }

    @Test func pickingAnEnrollmentTakesTheLineupModelWhenItIsOfferedAtTheHarnessDefault() {
        // The implementer's lineup runs claude-opus; the enrollment reports it.
        #expect(
            AgentChoiceDisplay.choosing(
                enrollment: UsageFixtures.readingCollectorID, role: .remediator, catalog: catalog)
                == UsageFixtures.selection(UsageFixtures.readingCollectorID, "claude-opus", "harness_default"))
        // codex doesn't report claude-opus, so its first model stands in.
        #expect(
            AgentChoiceDisplay.choosing(
                enrollment: UsageFixtures.unsupportedCollectorID, role: .implementer, catalog: catalog)
                == UsageFixtures.selection(UsageFixtures.unsupportedCollectorID, "gpt-codex", "harness_default"))
        #expect(
            AgentChoiceDisplay.choosing(enrollment: UsageFixtures.readingCollectorID, role: .reviewer, catalog: catalog)
                == UsageFixtures.selection(UsageFixtures.readingCollectorID, "claude-opus", "harness_default"))
        // Not discovered yet, the enrollment offers only what the tree runs
        // on it, which has no harness default.
        #expect(
            AgentChoiceDisplay.choosing(
                enrollment: UsageFixtures.readingSecondEnrollmentID, role: .implementer, catalog: catalog)
                == UsageFixtures.selection(UsageFixtures.readingSecondEnrollmentID, "claude-opus", "max"))
        #expect(
            AgentChoiceDisplay.choosing(
                enrollment: UsageFixtures.readingSecondEnrollmentID, role: .implementer,
                catalog: UsageFixtures.treelessAgentCatalog()) == nil)
        #expect(AgentChoiceDisplay.choosing(enrollment: "enrollment-unknown", role: .reviewer, catalog: catalog) == nil)
    }

    @Test func aLineupModelNamedByWhatAnAliasResolvesToIsOffered() {
        // The tree names the provider model; discovery lists it under an
        // alias that resolves to it, which the daemon accepts.
        var resolved = catalog
        resolved.enrollments[0].discovery?.value1.models[0].resolved_model = "claude-opus-5-5"
        resolved.lineup[1].selection.model_id = "claude-opus-5-5"
        let chosen = AgentChoiceDisplay.choosing(
            enrollment: UsageFixtures.readingCollectorID, role: .implementer, catalog: resolved)
        // The pick records the catalog entry's id, not the lineup's spelling.
        #expect(chosen == UsageFixtures.selection(UsageFixtures.readingCollectorID, "claude-opus", "harness_default"))
        #expect(chosen.map { AgentChoiceDisplay.offers(resolved, $0) } == true)
    }

    /// #3027: New Task recorded `opus` and Change Agent `claude-opus-5-5`
    /// for one model. An enrollment pick and a model pick of the same model
    /// now record the same id, the catalog entry's.
    @Test func anEnrollmentPickAndAModelPickOfOneModelRecordOneID() {
        var resolved = catalog
        resolved.enrollments[0].discovery?.value1.models[0].resolved_model = "claude-opus-5-5"
        resolved.lineup[1].selection.model_id = "claude-opus-5-5"
        let byEnrollment = AgentChoiceDisplay.choosing(
            enrollment: UsageFixtures.readingCollectorID, role: .implementer, catalog: resolved)
        let sonnet = UsageFixtures.selection(UsageFixtures.readingCollectorID, "claude-sonnet", "harness_default")
        let byModel = AgentChoiceDisplay.choosing(model: "claude-opus", from: sonnet, catalog: resolved)
        #expect(byEnrollment == byModel)
    }

    /// A pick that records the entry's id still reads as the lineup's model
    /// when the lineup names what the entry's alias resolves to.
    @Test func aPickOfTheLineupsAliasedModelReadsAsTheEnrollmentAlone() {
        var resolved = catalog
        resolved.enrollments[0].discovery?.value1.models[0].resolved_model = "claude-opus-5-5"
        resolved.lineup[1].selection.model_id = "claude-opus-5-5"
        let pick = UsageFixtures.selection(UsageFixtures.readingCollectorID, "claude-opus", "high")
        #expect(
            AgentChoiceDisplay.choiceLabel(pick, roles: [.implementer], catalog: resolved)
                == UsageFixtures.readingCollectorID)
        // Another model at the lineup's effort still reads in full.
        let sonnet = UsageFixtures.selection(UsageFixtures.readingCollectorID, "claude-sonnet", "high")
        #expect(
            AgentChoiceDisplay.choiceLabel(sonnet, roles: [.implementer], catalog: resolved)
                == AgentChoiceDisplay.shortLabel(sonnet, catalog: resolved))
    }

    @Test func pickingAModelKeepsAnEffortItReportsAndOtherwiseResetsIt() {
        let opusHigh = UsageFixtures.selection(UsageFixtures.readingCollectorID, "claude-opus", "high")
        #expect(
            AgentChoiceDisplay.choosing(model: "claude-sonnet", from: opusHigh, catalog: catalog)
                == UsageFixtures.selection(UsageFixtures.readingCollectorID, "claude-sonnet", "high"))
        let opusMax = UsageFixtures.selection(UsageFixtures.readingCollectorID, "claude-opus", "max")
        #expect(
            AgentChoiceDisplay.choosing(model: "claude-sonnet", from: opusMax, catalog: catalog)
                == UsageFixtures.selection(UsageFixtures.readingCollectorID, "claude-sonnet", "harness_default"))
        let codexHigh = UsageFixtures.selection(UsageFixtures.unsupportedCollectorID, "gpt-codex", "high")
        #expect(
            AgentChoiceDisplay.choosing(model: "gpt-mini", from: codexHigh, catalog: catalog)
                == UsageFixtures.selection(UsageFixtures.unsupportedCollectorID, "gpt-mini", "harness_default"))
        // The model already chosen, or one the enrollment doesn't offer,
        // changes nothing.
        #expect(AgentChoiceDisplay.choosing(model: "claude-opus", from: opusMax, catalog: catalog) == opusMax)
        #expect(AgentChoiceDisplay.choosing(model: "gpt-codex", from: opusMax, catalog: catalog) == opusMax)
    }

    /// The one rule both role menus pick through: an enrollment resets the
    /// model and effort, a model keeps an effort it reports, an effort
    /// replaces the effort alone, and a pick that changes nothing returns the
    /// current selection, so neither menu writes or sends for it.
    @Test func aMenuPickFollowsTheChoosingRulesAndAnIdlePickReturnsTheCurrentSelection() {
        let opusMax = UsageFixtures.selection(UsageFixtures.readingCollectorID, "claude-opus", "max")
        func pick(_ pick: AgentChoiceDisplay.Pick, from current: Selection?) -> Selection? {
            AgentChoiceDisplay.picking(pick, from: current, role: .implementer, catalog: catalog)
        }
        #expect(
            pick(.enrollment(UsageFixtures.unsupportedCollectorID), from: opusMax)
                == AgentChoiceDisplay.choosing(
                    enrollment: UsageFixtures.unsupportedCollectorID, role: .implementer, catalog: catalog))
        #expect(
            pick(.enrollment(UsageFixtures.readingCollectorID), from: nil)
                == UsageFixtures.selection(UsageFixtures.readingCollectorID, "claude-opus", "harness_default"))
        #expect(
            pick(.model("claude-sonnet"), from: opusMax)
                == AgentChoiceDisplay.choosing(model: "claude-sonnet", from: opusMax, catalog: catalog))
        #expect(
            pick(.effort("high"), from: opusMax)
                == UsageFixtures.selection(UsageFixtures.readingCollectorID, "claude-opus", "high"))
        #expect(pick(.enrollment(nil), from: opusMax) == nil)

        // Picking the chosen enrollment keeps its model and effort.
        #expect(pick(.enrollment(UsageFixtures.readingCollectorID), from: opusMax) == opusMax)
        #expect(pick(.model("claude-opus"), from: opusMax) == opusMax)
        #expect(pick(.effort("max"), from: opusMax) == opusMax)
        // A model or effort needs a selection, and an enrollment offering no
        // model offers nothing to pick.
        #expect(pick(.model("claude-opus"), from: nil) == nil)
        #expect(pick(.effort("high"), from: nil) == nil)
        #expect(pick(.enrollment("enrollment-unknown"), from: opusMax) == opusMax)
    }

    // MARK: Usage readings

    private static let base = Date(timeIntervalSince1970: 1_786_502_400)
    private static let utc = TimeZone(identifier: "UTC")!
    private static let locale = Locale(identifier: "en_US_POSIX")

    /// An instant `offset` after the fixtures' epoch, as the form prints it.
    private func time(_ offset: TimeInterval) -> String {
        FreesideFormat.shortTime(
            Self.base.addingTimeInterval(offset), now: Self.base, locale: Self.locale, timeZone: Self.utc)
    }

    private func enrollments(
        pools: [Components.Schemas.UsagePoolSnapshot], at offset: TimeInterval,
        catalog: Components.Schemas.AgentCatalog? = nil
    ) -> [AgentChoiceDisplay.EnrollmentOption] {
        AgentChoiceDisplay.enrollments(
            catalog ?? self.catalog, pools: pools, now: Self.base.addingTimeInterval(offset), locale: Self.locale,
            timeZone: Self.utc)
    }

    @Test func eachEnrollmentShowsItsPoolsReadingAndWhenItWasObserved() {
        let pools = UsageFixtures.defaultUsagePools()
        let current = enrollments(pools: pools, at: 3_000)
        // Both claude enrollments draw on one pool, so they show one reading:
        // its tightest window, then when that window's reading was observed.
        #expect(current[0].reading == "Weekly Opus: Used up · read \(time(2_400))")
        #expect(current[1].reading == current[0].reading)
        #expect(
            current[1].label
                == "\(UsageFixtures.readingSecondEnrollmentID) · not discovered yet · \(current[1].reading!)")
        #expect(current[2].reading == "unsupported \(time(1_800))")
        // Past the readings' stale_at the pool says so instead.
        let stale = enrollments(pools: pools, at: 5_000)
        #expect(stale[0].reading == "Weekly Opus: Used up · stale since \(time(4_200))")
        #expect(enrollments(pools: [], at: 3_000).allSatisfy { $0.reading == nil })
        // A later failed refresh leaves the reading's own time standing.
        var failed = pools
        failed[0].pool.last_attempt = .init(
            value1: .init(status: .failed, at: Self.base.addingTimeInterval(2_700), detail: "timed out"))
        #expect(enrollments(pools: failed, at: 3_000)[0].reading == current[0].reading)
        #expect(enrollments(pools: [], at: 3_000)[0].label == UsageFixtures.readingCollectorID)
    }

    @Test func aUsageReadingNeverReordersHidesOrDisablesAnEnrollment() {
        let pools = UsageFixtures.defaultUsagePools()
        let catalogOrder = catalog.enrollments.map(\.enrollment_id)
        for options in [
            enrollments(pools: pools, at: 3_000), enrollments(pools: pools, at: 5_000), enrollments(pools: [], at: 0),
        ] {
            #expect(options.map(\.id) == catalogOrder)
            #expect(options.map(\.isPickable) == [true, true, true])
            #expect(options.map(\.isDiscovered) == [true, false, true])
        }
        // Only an undiscovered enrollment the lineup doesn't run on is disabled,
        // whatever its reading.
        for pools in [pools, []] {
            #expect(
                enrollments(pools: pools, at: 3_000, catalog: UsageFixtures.treelessAgentCatalog()).map(\.isPickable)
                    == [true, false, true])
        }
    }

    @Test func theFootnoteNamesTheHarnessBuildAndDiscoveryTime() {
        let now = Self.base.addingTimeInterval(3_000)
        #expect(
            AgentChoiceDisplay.discoveryNote(
                on: UsageFixtures.readingCollectorID, catalog: catalog, now: now, locale: Self.locale,
                timeZone: Self.utc) == "claude-code 2.1.0 · discovered \(time(600))")
        #expect(
            AgentChoiceDisplay.discoveryNote(on: UsageFixtures.readingSecondEnrollmentID, catalog: catalog, now: now)
                == "Not discovered yet; lists what the lineup runs on it")
        #expect(AgentChoiceDisplay.discoveryNote(on: "enrollment-unknown", catalog: catalog, now: now) == nil)
    }

    // MARK: Alternate-agent offers

    /// Offers on the claude pool's second enrollment, the unsupported codex
    /// enrollment, and one no pool lists, in an order no pool or reading
    /// gives them.
    private let offers: [Components.Schemas.AlternateAgentOffer] = [
        .init(
            enrollment: UsageFixtures.unsupportedCollectorID, pool: "codex-team-pool", harness: .codex_cli,
            model: "gpt-codex", qualification: .different_pool),
        .init(
            enrollment: "enrollment-unpooled", pool: "spare-pool", harness: .claude_code, model: "claude-opus-5-5",
            qualification: .different_pool),
        .init(
            enrollment: UsageFixtures.readingSecondEnrollmentID, pool: "claude-team-pool", harness: .claude_code,
            model: "claude-opus-5-5", qualification: .different_pool),
    ]

    private func offerOptions(
        pools: [Components.Schemas.UsagePoolSnapshot], at offset: TimeInterval
    ) -> [AgentChoiceDisplay.OfferOption] {
        AgentChoiceDisplay.offerOptions(
            offers, pools: pools, now: Self.base.addingTimeInterval(offset), locale: Self.locale, timeZone: Self.utc)
    }

    @Test func eachOfferShowsItsEnrollmentsReadingAfterItsLabel() {
        let pools = UsageFixtures.defaultUsagePools()
        let current = offerOptions(pools: pools, at: 3_000)
        #expect(
            current.map(\.label) == [
                "\(UsageFixtures.unsupportedCollectorID) · gpt-codex · codex-team-pool · unsupported \(time(1_800))",
                "enrollment-unpooled · claude-opus-5-5 · spare-pool",
                "\(UsageFixtures.readingSecondEnrollmentID) · claude-opus-5-5 · claude-team-pool"
                    + " · Weekly Opus: Used up · read \(time(2_400))",
            ])
        let stale = offerOptions(pools: pools, at: 5_000)
        #expect(
            stale[2].label
                == "\(UsageFixtures.readingSecondEnrollmentID) · claude-opus-5-5 · claude-team-pool"
                + " · Weekly Opus: Used up · stale since \(time(4_200))")
        // An offer whose enrollment no pool lists reads as the offer alone.
        #expect(offerOptions(pools: [], at: 3_000).map(\.label) == offers.map(AgentChoiceDisplay.offerLabel))
    }

    @Test func aUsageReadingNeverReordersDropsOrChangesAnOffersChoice() {
        let pools = UsageFixtures.defaultUsagePools()
        for options in [
            offerOptions(pools: pools, at: 3_000), offerOptions(pools: pools, at: 5_000),
            offerOptions(pools: [], at: 0),
        ] {
            #expect(options.map(\.id) == offers.map(\.enrollment))
            // The choice is the enrollment alone, so the daemon runs the
            // lineup's model and effort on it.
            #expect(
                options.map(\.choice)
                    == offers.map { Components.Schemas.AlternateAgentChoice(enrollment_id: $0.enrollment) })
            #expect(options.allSatisfy { $0.choice.model_id == nil && $0.choice.effort == nil })
        }
    }

    // MARK: Stale choices

    private func missing(
        _ choices: [AgentChoiceDisplay.Role: Selection], catalog: Components.Schemas.AgentCatalog? = nil
    ) -> [AgentChoiceDisplay.MissingChoice] {
        AgentChoiceDisplay.missingChoices(choices: choices, catalog: catalog ?? self.catalog)
    }

    @Test func everyLineupSelectionAndEveryPickIsOffered() {
        #expect(catalog.lineup.allSatisfy { AgentChoiceDisplay.offers(catalog, $0.selection) })
        for role in AgentChoiceDisplay.roleOrder {
            for enrollment in catalog.enrollments {
                let chosen = AgentChoiceDisplay.choosing(
                    enrollment: enrollment.enrollment_id, role: role, catalog: catalog)
                #expect(chosen.map { AgentChoiceDisplay.offers(catalog, $0) } == true)
            }
        }
        #expect(missing([.implementer: selection("claude-primary"), .reviewer: selection("codex-primary")]).isEmpty)
    }

    @Test func aDroppedEnrollmentKeepsTheChoiceAndSaysTheEnrollmentWentAway() {
        var dropped = catalog
        dropped.enrollments.removeAll { $0.enrollment_id == UsageFixtures.unsupportedCollectorID }
        #expect(
            missing([.reviewer: selection("codex-primary")], catalog: dropped) == [
                .init(selection: selection("codex-primary"), part: .enrollment)
            ])
        #expect(
            missing([.reviewer: selection("codex-primary")], catalog: dropped)[0].sentence
                == "The daemon no longer holds \(UsageFixtures.unsupportedCollectorID); choose another enrollment "
                + "or Default to submit.")
    }

    @Test func aDroppedModelKeepsTheChoiceAndSaysTheModelWentAway() {
        let gone = retired("claude-retired")
        #expect(missing([.implementer: gone]) == [.init(selection: gone, part: .model)])
        #expect(
            missing([.implementer: gone])[0].sentence
                == "\(UsageFixtures.readingCollectorID) no longer offers claude-retired; choose another model or "
                + "Default to submit.")
        // On an enrollment not discovered yet, a model the lineup doesn't run.
        let sonnet = UsageFixtures.selection(UsageFixtures.readingSecondEnrollmentID, "claude-sonnet", "max")
        #expect(missing([.implementer: sonnet]).map(\.part) == [.model])
    }

    @Test func aDroppedEffortKeepsTheChoiceAndSaysTheEffortWentAway() {
        let sonnetMax = UsageFixtures.selection(UsageFixtures.readingCollectorID, "claude-sonnet", "max")
        #expect(missing([.implementer: sonnetMax]) == [.init(selection: sonnetMax, part: .effort)])
        #expect(
            missing([.implementer: sonnetMax])[0].sentence
                == "claude-sonnet on \(UsageFixtures.readingCollectorID) no longer offers max effort; choose another "
                + "effort or Default to submit.")
        let miniLow = UsageFixtures.selection(UsageFixtures.unsupportedCollectorID, "gpt-mini", "low")
        #expect(missing([.reviewer: miniLow]).map(\.part) == [.effort])
        // On an enrollment not discovered yet, an effort the lineup doesn't
        // run the model at.
        let opusHigh = UsageFixtures.selection(UsageFixtures.readingSecondEnrollmentID, "claude-opus", "high")
        #expect(missing([.implementer: opusHigh]).map(\.part) == [.effort])
    }

    @Test func anUndiscoveredEnrollmentWithNoLineupSelectionLeftSaysToChooseAnotherEnrollment() {
        var bare = catalog
        bare.lineup.removeAll { $0.selection.enrollment_id == UsageFixtures.readingSecondEnrollmentID }
        let opusMax = UsageFixtures.selection(UsageFixtures.readingSecondEnrollmentID, "claude-opus", "max")
        #expect(missing([.remediator: opusMax], catalog: bare) == [.init(selection: opusMax, part: .everyModel)])
        #expect(
            missing([.remediator: opusMax], catalog: bare)[0].sentence
                == "\(UsageFixtures.readingSecondEnrollmentID) no longer offers any model; choose another "
                + "enrollment or Default to submit.")
    }

    @Test func missingChoicesFollowRoleOrderOnceEach() {
        let choices: [AgentChoiceDisplay.Role: Selection] = [
            .reviewer: retired("old-reviewer"), .remediator: retired("old-remediator"),
            .specifier: retired("old-specifier"), .implementer: retired("old-specifier"),
        ]
        #expect(
            missing(choices).map(\.selection.model_id) == ["old-specifier", "old-remediator", "old-reviewer"])
    }

    @Test func withNoCatalogEveryChoiceIsMissingItsEnrollment() {
        let choices: [AgentChoiceDisplay.Role: Selection] = [
            .specifier: selection("claude-primary"), .reviewer: selection("codex-primary"),
        ]
        #expect(AgentChoiceDisplay.missingChoices(choices: [:], catalog: nil).isEmpty)
        #expect(
            AgentChoiceDisplay.missingChoices(choices: choices, catalog: nil).map(\.part) == [.enrollment, .enrollment])
    }

    // MARK: Labels

    @Test func aSelectionReadsAsItsEnrollmentModelAndEffort() {
        let codex = selection("codex-primary")
        #expect(
            AgentChoiceDisplay.selectionLabel(codex)
                == "\(UsageFixtures.unsupportedCollectorID) · gpt-codex · harness_default")
        // The short form names the model by its display name where
        // discovery has one, and no effort for the harness default.
        #expect(
            AgentChoiceDisplay.shortLabel(codex, catalog: catalog)
                == "\(UsageFixtures.unsupportedCollectorID) · GPT Codex")
        #expect(
            AgentChoiceDisplay.shortLabel(selection("claude-secondary"), catalog: catalog)
                == "\(UsageFixtures.readingSecondEnrollmentID) · claude-opus · max")
        // A choice whose model and effort are its role's lineup's reads as
        // the enrollment alone.
        #expect(
            AgentChoiceDisplay.choiceLabel(codex, roles: [.reviewer], catalog: catalog)
                == UsageFixtures.unsupportedCollectorID)
        #expect(
            AgentChoiceDisplay.choiceLabel(codex, roles: [.implementer], catalog: catalog)
                == AgentChoiceDisplay.shortLabel(codex, catalog: catalog))
        #expect(
            AgentChoiceDisplay.choiceLabel(codex, roles: [.reviewer], catalog: nil)
                == "\(UsageFixtures.unsupportedCollectorID) · gpt-codex")
    }

    @Test func defaultLabelNamesTheLineupSelection() {
        let policy = UsageFixtures.defaultRoutingPolicy()
        #expect(
            AgentChoiceDisplay.defaultLabel(role: .reviewer, policy: policy, catalog: catalog)
                == "Default · lineup \(UsageFixtures.unsupportedCollectorID) · GPT Codex")
        #expect(
            AgentChoiceDisplay.defaultLabel(role: .implementer, policy: policy, catalog: catalog)
                == "Default · lineup \(UsageFixtures.readingCollectorID) · Opus · high or routing policy")
        #expect(
            AgentChoiceDisplay.defaultLabel(role: .remediator, policy: nil, catalog: catalog)
                == "Default · lineup \(UsageFixtures.readingSecondEnrollmentID) · claude-opus · max")
        #expect(AgentChoiceDisplay.defaultLabel(role: .reviewer, policy: nil, catalog: nil) == "Default · lineup")
        #expect(
            AgentChoiceDisplay.defaultLabel(
                role: .reviewer, policy: nil, catalog: UsageFixtures.treelessAgentCatalog()) == "Default · lineup")
        #expect(
            AgentChoiceDisplay.defaultLabel(
                role: .implementer, policy: policy, catalog: UsageFixtures.treelessAgentCatalog())
                == "Default · lineup or routing policy")
    }

    /// #3027: an unchosen role chip names what the role runs on, not a bare
    /// `Default`; the default entry's label is built on the same text.
    @Test func defaultSettingsNameTheLineupSelectionOrNothing() throws {
        let policy = UsageFixtures.defaultRoutingPolicy()
        #expect(
            AgentChoiceDisplay.defaultSettingsLabel(role: .implementer, policy: policy, catalog: catalog)
                == "\(UsageFixtures.readingCollectorID) · Opus · high or routing policy")
        #expect(
            AgentChoiceDisplay.defaultSettingsLabel(role: .reviewer, policy: nil, catalog: catalog)
                == "\(UsageFixtures.unsupportedCollectorID) · GPT Codex")
        for role in AgentChoiceDisplay.roleOrder {
            let settings = try #require(
                AgentChoiceDisplay.defaultSettingsLabel(role: role, policy: policy, catalog: catalog))
            #expect(
                AgentChoiceDisplay.defaultLabel(role: role, policy: policy, catalog: catalog)
                    == "Default · lineup \(settings)")
        }
        #expect(AgentChoiceDisplay.defaultSettingsLabel(role: .reviewer, policy: policy, catalog: nil) == nil)
        #expect(
            AgentChoiceDisplay.defaultSettingsLabel(
                role: .reviewer, policy: policy, catalog: UsageFixtures.treelessAgentCatalog()) == nil)
    }

    /// The task page clears a line where New Task leaves a role unchosen, so
    /// its entry names the same fallback after Clear.
    @Test func clearLabelNamesWhatPicksTheAgentOnceTheLineIsCleared() {
        let policy = UsageFixtures.defaultRoutingPolicy()
        #expect(
            AgentChoiceDisplay.clearLabel(role: .implementer, policy: policy, catalog: catalog)
                == "Clear · lineup \(UsageFixtures.readingCollectorID) · Opus · high or routing policy")
        #expect(AgentChoiceDisplay.clearLabel(role: .reviewer, policy: nil, catalog: nil) == "Clear · lineup")
    }

    // MARK: Summary

    @Test func summaryReadsTheWriterOnceWhenTheThreeWriterLinesAgree() {
        // claude-primary is the specifier's and implementer's lineup line but
        // not the remediator's, so the writer reads in the full form.
        let writer = selection("claude-primary")
        #expect(
            AgentChoiceDisplay.summary(
                AgentChoiceDisplay.taskLines(choices: [
                    .specifier: writer, .implementer: writer, .remediator: writer,
                    .reviewer: selection("codex-primary"),
                ]), catalog: catalog)
                == "\(UsageFixtures.readingCollectorID) · Opus · high writes · \(UsageFixtures.unsupportedCollectorID) reviews"
        )
        #expect(
            AgentChoiceDisplay.summary(
                AgentChoiceDisplay.taskLines(choices: [.specifier: writer, .implementer: writer, .remediator: writer]),
                catalog: catalog) == "\(UsageFixtures.readingCollectorID) · Opus · high writes")
    }

    @Test func summaryReadsAnEnrollmentAloneWhenItRunsTheLineupsModelAndEffort() {
        // The lineup's model and effort on another enrollment: only the
        // enrollment changed.
        let moved = UsageFixtures.selection(UsageFixtures.unsupportedCollectorID, "claude-opus", "high")
        #expect(
            AgentChoiceDisplay.summary(
                [Line(role: .specifier, selection: moved), Line(role: .implementer, selection: moved)],
                catalog: catalog) == "\(UsageFixtures.unsupportedCollectorID) specifies and implements")
        #expect(
            AgentChoiceDisplay.summary(
                [
                    Line(role: .reviewer, selection: selection("codex-primary")),
                    Line(role: .implementer, selection: selection("claude-primary")),
                ], catalog: catalog)
                == "\(UsageFixtures.readingCollectorID) implements · \(UsageFixtures.unsupportedCollectorID) reviews")
    }

    @Test func summaryReadsEachDistinctSelectionWithItsVerbs() {
        let mini = UsageFixtures.selection(UsageFixtures.unsupportedCollectorID, "gpt-mini", "harness_default")
        #expect(
            AgentChoiceDisplay.summary(
                [
                    Line(role: .specifier, selection: selection("claude-primary")),
                    Line(role: .implementer, selection: mini),
                    Line(role: .remediator, selection: mini),
                ], catalog: catalog)
                == "\(UsageFixtures.readingCollectorID) specifies · "
                + "\(UsageFixtures.unsupportedCollectorID) · GPT Mini implements and remediates")
    }

    @Test func twoSelectionsThatWouldShareANameReadInFull() {
        // Each matches its own role's lineup line, so each alone would read
        // as the same enrollment.
        let primary = selection("claude-primary")
        let opusMax = UsageFixtures.selection(UsageFixtures.readingCollectorID, "claude-opus", "max")
        #expect(
            AgentChoiceDisplay.summary(
                [Line(role: .implementer, selection: primary), Line(role: .remediator, selection: opusMax)],
                catalog: catalog)
                == "\(UsageFixtures.readingCollectorID) · Opus · high implements · "
                + "\(UsageFixtures.readingCollectorID) · Opus · max remediates")
    }

    @Test func summaryWithNoCatalogReadsEverySelectionInFull() {
        #expect(
            AgentChoiceDisplay.summary([Line(role: .reviewer, selection: selection("codex-primary"))])
                == "\(UsageFixtures.unsupportedCollectorID) · gpt-codex reviews")
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
