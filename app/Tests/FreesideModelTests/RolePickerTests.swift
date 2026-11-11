import Foundation
import FreesideAPI
import Testing

@testable import FreesideModel

/// #3027: one role's picker stages a pick without writing it, keeps its
/// controls in place while the catalog changes, names the whole tuple
/// before the confirm, and turns the confirm off when it would do nothing
/// or can't work. The default catalog is `AgentChoiceDisplayTests`'s.
@Suite struct RolePickerTests {
    typealias Selection = AgentChoiceDisplay.Selection
    let catalog = UsageFixtures.defaultAgentCatalog()
    let policy = UsageFixtures.defaultRoutingPolicy()
    private static let mac = UsageFixtures.readingCollectorID
    private static let mini = UsageFixtures.readingSecondEnrollmentID
    private static let codex = UsageFixtures.unsupportedCollectorID
    private static let now = Date(timeIntervalSince1970: 1_786_502_400)

    private func rows(_ picker: RolePicker, catalog: AgentChoiceDisplay.Catalog?) -> [RolePicker.EnrollmentRow] {
        picker.enrollmentRows(
            catalog: catalog, policy: policy, pools: UsageFixtures.defaultUsagePools(), now: Self.now,
            locale: Locale(identifier: "en_US_POSIX"), timeZone: TimeZone(identifier: "UTC")!)
    }

    // MARK: Staging

    @Test func aPickStagesThroughTheSharedRuleAndWritesNothingElse() {
        var picker = RolePicker.draft(role: .implementer, chosen: nil, catalog: catalog)
        #expect(picker.staged == .lineup && picker.chosenRow == .lineup)

        picker.pick(.enrollment(Self.codex), catalog: catalog)
        let codex = AgentChoiceDisplay.choosing(enrollment: Self.codex, role: .implementer, catalog: catalog)
        #expect(picker.staged.selection == codex && picker.chosenRow == .enrollment(Self.codex))
        picker.pick(.model("gpt-mini"), catalog: catalog)
        #expect(picker.staged.selection == UsageFixtures.selection(Self.codex, "gpt-mini", "harness_default"))
        picker.pick(.model("gpt-codex"), catalog: catalog)
        picker.pick(.effort("high"), catalog: catalog)
        #expect(picker.staged.selection == UsageFixtures.selection(Self.codex, "gpt-codex", "high"))
        #expect(picker.confirmation(catalog: catalog) == .ready(picker.staged.selection))

        picker.pick(.enrollment(nil), catalog: catalog)
        #expect(picker.staged == .lineup)
        #expect(picker.confirmation(catalog: catalog) == .ready(nil))
        // With nothing staged, a model or effort pick has nothing to change.
        picker.pick(.model("gpt-codex"), catalog: catalog)
        #expect(picker.staged == .lineup)
    }

    /// The task page's cases that `RoleSelectionMenu.choice` held, as
    /// confirm outcomes: an enrollment pick applies New Task's reset rule,
    /// and staging what the line holds leaves the confirm off.
    @Test func aRunningTasksConfirmIsOffWhileTheStagedPickIsItsLine() {
        let line = UsageFixtures.agentSelection("codex-primary")
        var reviewer = RolePicker.change(role: .reviewer, current: line, byName: nil, catalog: catalog)
        let unchanged = RolePicker.Confirmation.blocked("The reviewer runs on this now.")
        #expect(reviewer.confirmation(catalog: catalog) == unchanged)
        reviewer.pick(.effort(line.effort), catalog: catalog)
        #expect(reviewer.confirmation(catalog: catalog) == unchanged)

        reviewer.pick(.enrollment(Self.mac), catalog: catalog)
        #expect(
            reviewer.confirmation(catalog: catalog)
                == .ready(AgentChoiceDisplay.choosing(enrollment: Self.mac, role: .reviewer, catalog: catalog)))
        // Back to the line's enrollment: the reset rule gives the line again.
        reviewer.pick(.enrollment(Self.codex), catalog: catalog)
        #expect(reviewer.confirmation(catalog: catalog) == unchanged)
        reviewer.pick(.enrollment(nil), catalog: catalog)
        #expect(reviewer.confirmation(catalog: catalog) == .ready(nil))

        // A role on the lineup has nothing to clear.
        var specifier = RolePicker.change(role: .specifier, current: nil, byName: nil, catalog: catalog)
        #expect(specifier.confirmation(catalog: catalog) == .blocked("The specifier runs on this now."))
        specifier.pick(.enrollment(nil), catalog: catalog)
        #expect(specifier.confirmation(catalog: catalog) == .blocked("The specifier runs on this now."))
    }

    /// A line recorded by agent name before #2597 holds no selection: the
    /// picker shows its agent as the chosen row, disabled, and any pick,
    /// Clear included, replaces it.
    @Test func aLineSetByNameShowsItsAgentAndAnyPickReplacesIt() {
        var picker = RolePicker.change(role: .implementer, current: nil, byName: "claude-primary", catalog: catalog)
        #expect(picker.chosenRow == .byName)
        let shown = rows(picker, catalog: catalog)
        #expect(shown.prefix(2).map(\.id) == [.lineup, .byName])
        #expect(shown[1] == .init(id: .byName, label: "claude-primary", detail: "set by name", isEnabled: false))
        #expect(picker.modelPills(catalog: catalog).isEmpty && picker.effortPills(catalog: catalog).isEmpty)
        #expect(picker.confirmation(catalog: catalog) == .blocked("The implementer runs on this now."))

        picker.pick(.model("claude-opus"), catalog: catalog)
        #expect(picker.staged == .byName("claude-primary"))
        picker.pick(.enrollment(nil), catalog: catalog)
        #expect(picker.confirmation(catalog: catalog) == .ready(nil))
        picker.pick(.enrollment(Self.codex), catalog: catalog)
        #expect(
            picker.confirmation(catalog: catalog)
                == .ready(AgentChoiceDisplay.choosing(enrollment: Self.codex, role: .implementer, catalog: catalog)))
    }

    /// A line written before #3027 can name the model an alias resolves
    /// to. Staging that model again, under the catalog's own id, is the
    /// line already, so the confirm stays off.
    @Test func aLineNamingTheResolvedModelIsUnchangedByThatModelsPick() throws {
        var resolved = catalog
        resolved.enrollments[0].discovery?.value1.models[0].resolved_model = "claude-opus-5-5"
        resolved.lineup[1].selection.model_id = "claude-opus-5-5"
        let restaged = try #require(
            AgentChoiceDisplay.choosing(enrollment: Self.mac, role: .implementer, catalog: resolved))
        #expect(restaged.model_id == "claude-opus")
        let line = Selection(enrollment_id: Self.mac, model_id: "claude-opus-5-5", effort: restaged.effort)

        var picker = RolePicker.change(role: .implementer, current: line, byName: nil, catalog: resolved)
        picker.pick(.enrollment(Self.codex), catalog: resolved)
        picker.pick(.enrollment(Self.mac), catalog: resolved)
        #expect(picker.staged == .selection(restaged))
        #expect(picker.confirmation(catalog: resolved) == .blocked("The implementer runs on this now."))
    }

    /// A line set by name that another device changes while the picker is
    /// open: the untouched pick writes nothing and says why, rather than
    /// reading as the line it no longer is.
    @Test func aByNameLineChangedWhileOpenAsksForAPick() {
        var picker = RolePicker.change(role: .implementer, current: nil, byName: "claude-primary", catalog: catalog)
        picker.mode = .change(current: UsageFixtures.agentSelection("codex-primary"), byName: nil)
        #expect(
            picker.confirmation(catalog: catalog)
                == .blocked("The implementer changed while this was open. Pick what it should run on."))
    }

    @Test func theConfirmIsOffWhenTheLiveCatalogNoLongerOffersTheStagedPick() {
        let retired = UsageFixtures.selection(Self.mac, "claude-retired", "high")
        #expect(
            RolePicker.draft(role: .implementer, chosen: retired, catalog: catalog).confirmation(catalog: catalog)
                == .blocked("\(Self.mac) no longer offers claude-retired."))
        let sonnetMax = UsageFixtures.selection(Self.mac, "claude-sonnet", "max")
        #expect(
            RolePicker.draft(role: .reviewer, chosen: sonnetMax, catalog: catalog).confirmation(catalog: catalog)
                == .blocked("claude-sonnet on \(Self.mac) no longer offers max effort."))

        // Staged against one catalog, confirmed against a newer one.
        var picker = RolePicker.change(
            role: .reviewer, current: UsageFixtures.agentSelection("codex-primary"), byName: nil, catalog: catalog)
        picker.pick(.enrollment(Self.mac), catalog: catalog)
        var dropped = catalog
        dropped.enrollments.removeAll { $0.enrollment_id == Self.mac }
        #expect(picker.confirmation(catalog: dropped) == .blocked("The daemon no longer holds \(Self.mac)."))
    }

    // MARK: Rows

    @Test func theEnrollmentRowsNameTheDefaultAndEachEnrollmentsReading() {
        let picker = RolePicker.draft(role: .implementer, chosen: nil, catalog: catalog)
        let shown = rows(picker, catalog: catalog)
        #expect(shown.map(\.id) == [.lineup, .enrollment(Self.mac), .enrollment(Self.mini), .enrollment(Self.codex)])
        #expect(shown[0].label == "Default")
        #expect(shown[0].detail == "lineup \(Self.mac) · Opus · high or routing policy")
        #expect(shown[2].detail?.hasPrefix("not discovered yet") == true)
        #expect(
            shown.dropFirst().map(\.detail)
                == AgentChoiceDisplay.enrollments(
                    catalog, pools: UsageFixtures.defaultUsagePools(), now: Self.now,
                    locale: Locale(identifier: "en_US_POSIX"), timeZone: TimeZone(identifier: "UTC")!
                ).map { option in
                    let parts = [option.isDiscovered ? nil : "not discovered yet", option.reading].compactMap(\.self)
                    return parts.isEmpty ? nil : parts.joined(separator: " · ")
                })
        let change = RolePicker.change(role: .implementer, current: nil, byName: nil, catalog: catalog)
        #expect(rows(change, catalog: catalog).first?.label == "Clear")
    }

    /// With Default chosen the Model and Effort sections show the lineup's
    /// model and effort, marked and read-only; with no lineup, nothing.
    @Test func theDefaultShowsTheLineupsModelAndEffortReadOnly() {
        let picker = RolePicker.draft(role: .implementer, chosen: nil, catalog: catalog)
        let models = picker.modelPills(catalog: catalog)
        #expect(models.map(\.id) == ["claude-opus", "claude-sonnet"])
        #expect(models.map(\.isChosen) == [true, false])
        #expect(models.allSatisfy { !$0.isEnabled })
        let efforts = picker.effortPills(catalog: catalog)
        #expect(efforts.map(\.label) == ["harness default", "low", "medium", "high", "xhigh", "max"])
        #expect(efforts.filter(\.isChosen).map(\.id) == ["high"])
        #expect(efforts.allSatisfy { !$0.isEnabled })
        #expect(picker.shownEnrollment(catalog: catalog) == Self.mac)

        let treeless = UsageFixtures.treelessAgentCatalog()
        let bare = RolePicker.draft(role: .implementer, chosen: nil, catalog: treeless)
        #expect(bare.modelPills(catalog: treeless).isEmpty && bare.effortPills(catalog: treeless).isEmpty)
        #expect(rows(bare, catalog: treeless)[0].detail == "lineup or routing policy")

        var chosen = picker
        chosen.pick(.enrollment(Self.mac), catalog: catalog)
        #expect(chosen.modelPills(catalog: catalog).allSatisfy { $0.isEnabled })
        #expect(chosen.effortPills(catalog: catalog).filter(\.isChosen).map(\.id) == ["harness_default"])
    }

    /// A sync while the picker is open rewrites text in place: a dropped
    /// enrollment, model, or effort keeps its slot, disabled, and a new one
    /// joins at the end, so no tap lands on a row a refresh moved.
    @Test func aNewerCatalogKeepsEveryControlInTheSlotItOpenedIn() throws {
        var picker = RolePicker.draft(role: .implementer, chosen: nil, catalog: catalog)
        picker.pick(.enrollment(Self.mac), catalog: catalog)
        picker.pick(.model("claude-opus"), catalog: catalog)

        var newer = catalog
        newer.enrollments.removeAll { $0.enrollment_id == Self.mini }
        var added = newer.enrollments[1]
        added.enrollment_id = "enrollment-new"
        newer.enrollments.insert(added, at: 0)
        var models = try #require(newer.enrollments[1].discovery?.value1.models)
        #expect(newer.enrollments[1].enrollment_id == Self.mac)
        models.removeAll { $0.id == "claude-sonnet" }
        models[0].effort_levels = ["max", "high", "turbo"]
        models.insert(
            .init(id: "claude-haiku", resolved_model: nil, display_name: "Haiku", effort_levels: ["low"]), at: 0)
        newer.enrollments[1].discovery?.value1.models = models
        picker.absorb(catalog: newer)

        let shown = rows(picker, catalog: newer)
        #expect(
            shown.map(\.id) == [
                .lineup, .enrollment(Self.mac), .enrollment(Self.mini), .enrollment(Self.codex),
                .enrollment("enrollment-new"),
            ])
        #expect(
            shown[2] == .init(id: .enrollment(Self.mini), label: Self.mini, detail: "not offered", isEnabled: false))
        let pills = picker.modelPills(catalog: newer)
        #expect(pills.map(\.id) == ["claude-opus", "claude-sonnet", "claude-haiku"])
        #expect(pills.map(\.isEnabled) == [true, false, true])
        #expect(pills[1].label == "claude-sonnet · not offered")
        let efforts = picker.effortPills(catalog: newer)
        #expect(efforts.map(\.id) == ["harness_default", "low", "medium", "high", "xhigh", "max", "turbo"])
        #expect(efforts.map(\.isEnabled) == [true, false, false, true, false, true, true])
    }

    /// A choice whose enrollment the catalog had already dropped when the
    /// picker opened still shows, chosen, in a slot of its own.
    @Test func aChoiceDroppedBeforeThePickerOpenedKeepsARow() {
        let gone = UsageFixtures.selection("enrollment-gone", "claude-opus", "high")
        let picker = RolePicker.draft(role: .implementer, chosen: gone, catalog: catalog)
        #expect(picker.chosenRow == .enrollment("enrollment-gone"))
        #expect(
            rows(picker, catalog: catalog).last
                == .init(
                    id: .enrollment("enrollment-gone"), label: "enrollment-gone", detail: "not offered",
                    isEnabled: false))
        #expect(
            picker.modelPills(catalog: catalog) == [
                .init(id: "claude-opus", label: "claude-opus · not offered", isChosen: true, isEnabled: false)
            ])
    }

    // MARK: Footer

    @Test func theFooterNamesTheWholeTupleTheConfirmWrites() {
        var draft = RolePicker.draft(role: .implementer, chosen: nil, catalog: catalog)
        #expect(
            draft.footer(catalog: catalog, policy: policy) == [
                .init(keyword: "Runs on", text: "lineup \(Self.mac) · Opus · high or routing policy")
            ])
        draft.pick(.enrollment(Self.codex), catalog: catalog)
        #expect(
            draft.footer(catalog: catalog, policy: policy) == [
                .init(keyword: "Runs on", text: "\(Self.codex) · GPT Codex · harness default")
            ])
        #expect(
            RolePicker.draft(role: .implementer, chosen: nil, catalog: nil).footer(catalog: nil, policy: policy) == [
                .init(keyword: "Runs on", text: "Default")
            ])

        var change = RolePicker.change(
            role: .reviewer, current: UsageFixtures.agentSelection("codex-primary"), byName: nil, catalog: catalog)
        change.pick(.enrollment(Self.mac), catalog: catalog)
        #expect(
            change.footer(catalog: catalog, policy: policy) == [
                .init(keyword: "Now", text: "\(Self.codex) · GPT Codex · harness default"),
                .init(keyword: "New", text: "\(Self.mac) · Opus · harness default"),
            ])
        // The caller refreshes the line; Now reads the live one.
        change.mode = .change(current: nil, byName: "claude-primary")
        #expect(
            change.footer(catalog: catalog, policy: nil).first
                == .init(keyword: "Now", text: "claude-primary · set by name"))
    }
}
