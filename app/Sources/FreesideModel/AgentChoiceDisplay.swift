import Foundation
import FreesideAPI

/// The New Task form's agent choices (plan §5.4, task lines): which lines a
/// per-role choice sends, what the catalog offers a role to pick, how a pick
/// fills the rest of the selection, and what picks the agent for a role left
/// unchosen. Pure: the sheet passes the synced catalog, routing policy, and
/// usage pools in.
///
/// A choice is a selection (enrollment, model, effort), which is what a line
/// records. The operator picks an enrollment, then a model from that
/// enrollment's discovery record, then an effort that model reports. An
/// enrollment the daemon hasn't discovered yet offers only the selections
/// the lineup runs on it, since nothing else says what it can run.
///
/// A role the operator leaves unchosen gets no line, so the daemon picks its
/// agent at first admission. Today that is the lineup's agent; once the
/// daemon routes by policy (#2393) it may be any agent the routing policy
/// orders for the role.
public enum AgentChoiceDisplay {
    public typealias Role = Components.Schemas.TaskLineRole
    public typealias Selection = Components.Schemas.TaskLineSelection
    public typealias Catalog = Components.Schemas.AgentCatalog

    /// The effort that sends no effort option, so the harness uses its own
    /// default. Every discovered model accepts it.
    public static let harnessDefault = "harness_default"

    /// An alternate-agent offer as a picker names it: the enrollment, its
    /// model, and the usage pool it draws on, so the operator sees what
    /// changes.
    public static func offerLabel(_ offer: Components.Schemas.AlternateAgentOffer) -> String {
        "\(offer.enrollment) · \(offer.model) · \(offer.pool)"
    }

    /// The choice a picker sends for an offer: the enrollment alone, so the
    /// daemon runs the implementer lineup's model and effort on it, the
    /// model and effort the offer was made at.
    public static func choice(
        _ offer: Components.Schemas.AlternateAgentOffer
    ) -> Components.Schemas.AlternateAgentChoice {
        .init(enrollment_id: offer.enrollment)
    }

    /// One alternate-agent offer as the retry dialog and the move menu list
    /// it.
    public struct OfferOption: Equatable, Sendable, Identifiable {
        /// The offer's enrollment.
        public let id: String
        /// `offerLabel`, then the enrollment's usage reading when a pool
        /// lists it.
        public let label: String
        /// What picking the entry sends: `choice(offer)`.
        public let choice: Components.Schemas.AlternateAgentChoice
    }

    /// The entries for `offers`, one per offer in the daemon's order. The
    /// reading is looked up by enrollment, as the New Task picker does, so
    /// one enrollment reads the same on every surface. A reading is shown,
    /// never weighed: it doesn't reorder, hide, or disable an offer, and
    /// it doesn't change the choice a pick sends (#1722).
    public static func offerOptions(
        _ offers: [Components.Schemas.AlternateAgentOffer], pools: [Components.Schemas.UsagePoolSnapshot],
        now: Date, locale: Locale = .current, timeZone: TimeZone = .current
    ) -> [OfferOption] {
        offers.map { offer in
            let reading = reading(for: offer.enrollment, pools: pools, now: now, locale: locale, timeZone: timeZone)
            return OfferOption(
                id: offer.enrollment,
                label: ([offerLabel(offer)] + [reading].compactMap(\.self)).joined(separator: " · "),
                choice: choice(offer))
        }
    }

    /// The roles in the order the form shows them and a submission sends
    /// their lines. The generated `TaskLineRole` declares the same order;
    /// naming it here keeps the line order explicit.
    public static let roleOrder: [Role] = [.specifier, .implementer, .remediator, .reviewer]

    /// The roles that write the change, which the summary reads as one
    /// writer when they name one selection.
    public static let writerRoles: [Role] = [.specifier, .implementer, .remediator]

    /// The lines a submission carries: one per chosen role, in `roleOrder`.
    /// With no choice it returns nil, never an empty list, which the daemon
    /// rejects.
    public static func taskLines(choices: [Role: Selection]) -> [Components.Schemas.TaskLineInput]? {
        let lines = roleOrder.compactMap { role in
            choices[role].map { Components.Schemas.TaskLineInput(role: role, selection: $0) }
        }
        return lines.isEmpty ? nil : lines
    }

    // MARK: What the catalog offers

    /// One model a role's menu lists for an enrollment.
    public struct ModelOption: Equatable, Sendable, Identifiable {
        /// The model id a selection sends.
        public let id: String
        /// The discovery record's display name, or the id on an enrollment
        /// not discovered yet.
        public let name: String
        /// The efforts the model takes, in the harness's order: on a
        /// discovered enrollment `harnessDefault` and then the model's
        /// `effort_levels`; otherwise those the lineup runs it at.
        public let efforts: [String]
    }

    /// The models `enrollmentID` offers, in the harness's order: its
    /// discovery record's, or, while it has none, those the lineup runs on
    /// it in lineup order. Empty for an enrollment the catalog doesn't
    /// list.
    public static func models(on enrollmentID: String, catalog: Catalog?) -> [ModelOption] {
        guard let catalog, let enrollment = catalog.enrollments.first(where: { $0.enrollment_id == enrollmentID })
        else { return [] }
        if let discovery = enrollment.discovery?.value1 {
            return discovery.models.map { model in
                ModelOption(
                    id: model.id, name: model.display_name,
                    efforts: unique([harnessDefault] + model.effort_levels))
            }
        }
        let carried = catalog.lineup.map(\.selection).filter { $0.enrollment_id == enrollmentID }
        return unique(carried.map(\.model_id)).map { model in
            ModelOption(
                id: model, name: model, efforts: unique(carried.filter { $0.model_id == model }.map(\.effort)))
        }
    }

    /// The model option a selection names, as the daemon finds it
    /// (`DiscoveredModelCatalog.Serves`): the entry with the model's id or,
    /// when none has it, the entries that resolve to it, whose efforts are
    /// pooled. The tree may name a model by the provider model an alias
    /// resolves to. Nil when the enrollment offers no such model.
    public static func modelOption(for selection: Selection, catalog: Catalog?) -> ModelOption? {
        let options = models(on: selection.enrollment_id, catalog: catalog)
        if let byID = options.first(where: { $0.id == selection.model_id }) { return byID }
        guard
            let discovery = catalog?.enrollments.first(where: { $0.enrollment_id == selection.enrollment_id })?
                .discovery?.value1
        else { return nil }
        let resolved = discovery.models.filter { $0.resolved_model == selection.model_id }
        guard let first = resolved.first else { return nil }
        return ModelOption(
            id: first.id, name: first.display_name,
            efforts: unique([harnessDefault] + resolved.flatMap(\.effort_levels)))
    }

    /// The part of a selection a newer catalog dropped.
    public enum DroppedPart: Equatable, Sendable {
        case enrollment
        /// The enrollment is still listed but offers no model at all: one
        /// not discovered yet whose last lineup selection went away.
        case everyModel
        case model
        case effort
    }

    /// The first part of `selection` the catalog doesn't offer, or nil when
    /// it offers the whole selection. With no catalog, no enrollment is
    /// offered.
    public static func droppedPart(_ selection: Selection, catalog: Catalog?) -> DroppedPart? {
        guard catalog?.enrollments.contains(where: { $0.enrollment_id == selection.enrollment_id }) == true else {
            return .enrollment
        }
        guard let model = modelOption(for: selection, catalog: catalog) else {
            return models(on: selection.enrollment_id, catalog: catalog).isEmpty ? .everyModel : .model
        }
        return model.efforts.contains(selection.effort) ? nil : .effort
    }

    /// Whether the catalog offers the whole selection.
    public static func offers(_ catalog: Catalog?, _ selection: Selection) -> Bool {
        droppedPart(selection, catalog: catalog) == nil
    }

    /// A chosen selection the catalog no longer offers, and which part it
    /// dropped.
    public struct MissingChoice: Equatable, Sendable {
        public let selection: Selection
        public let part: DroppedPart

        /// One sentence that names the dropped part and how to submit.
        public var sentence: String {
            let enrollment = selection.enrollment_id
            switch part {
            case .enrollment:
                return "The daemon no longer holds \(enrollment); choose another enrollment or Default to submit."
            case .everyModel:
                return "\(enrollment) no longer offers any model; choose another enrollment or Default to submit."
            case .model:
                return
                    "\(enrollment) no longer offers \(selection.model_id); choose another model or Default to submit."
            case .effort:
                return "\(selection.model_id) on \(enrollment) no longer offers \(effortLabel(selection.effort)) "
                    + "effort; choose another effort or Default to submit."
            }
        }
    }

    /// The chosen selections the catalog no longer offers, in role order,
    /// each once. A newer catalog can drop a selection's enrollment, model,
    /// or effort; the form keeps the choice and blocks Submit instead of
    /// swapping it silently.
    public static func missingChoices(choices: [Role: Selection], catalog: Catalog?) -> [MissingChoice] {
        unique(roleOrder.compactMap { choices[$0] }).compactMap { selection in
            droppedPart(selection, catalog: catalog).map { MissingChoice(selection: selection, part: $0) }
        }
    }

    // MARK: Picking

    /// The selection picking `enrollmentID` gives `role`: the role's lineup
    /// model when the enrollment offers it, otherwise the enrollment's first
    /// model, at `harnessDefault`. Discovery names no default model, so the
    /// first stands in for one. An enrollment not discovered yet may offer
    /// no `harnessDefault`; the model's first carried effort stands in. Nil
    /// when the enrollment offers no model.
    ///
    /// The selection records the catalog entry's id, as a model pick does,
    /// even when the lineup names the model by what an alias resolves to,
    /// so one model never reaches a line under two spellings.
    public static func choosing(enrollment enrollmentID: String, role: Role, catalog: Catalog?) -> Selection? {
        let lineup = catalog?.lineup.first { $0.role == role }?.selection
        let atLineupModel = lineup.flatMap { lineup in
            modelOption(
                for: Selection(enrollment_id: enrollmentID, model_id: lineup.model_id, effort: harnessDefault),
                catalog: catalog)
        }
        guard let model = atLineupModel ?? models(on: enrollmentID, catalog: catalog).first else { return nil }
        return Selection(enrollment_id: enrollmentID, model_id: model.id, effort: resetEffort(model))
    }

    /// The selection picking `modelID` makes of `current`: the same
    /// enrollment and effort when the new model reports that effort,
    /// otherwise `harnessDefault`. Picking the model `current` already names
    /// leaves it unchanged.
    public static func choosing(model modelID: String, from current: Selection, catalog: Catalog?) -> Selection {
        guard modelOption(for: current, catalog: catalog)?.id != modelID,
            let model = models(on: current.enrollment_id, catalog: catalog).first(where: { $0.id == modelID })
        else { return current }
        let effort = model.efforts.contains(current.effort) ? current.effort : resetEffort(model)
        return Selection(enrollment_id: current.enrollment_id, model_id: modelID, effort: effort)
    }

    /// One pick in a role's menu.
    public enum Pick: Equatable, Sendable {
        /// An enrollment, or nil for the entry that leaves the role to the
        /// lineup (New Task's Default, the task page's Clear).
        case enrollment(String?)
        case model(String)
        case effort(String)
    }

    /// The selection a role holds after `pick`, from `current`. Another
    /// enrollment resets the model and effort (`choosing(enrollment:)`), a
    /// model keeps an effort it reports (`choosing(model:)`), and an effort
    /// replaces the effort alone. Returns `current` for a pick that changes
    /// nothing: the value already chosen, a model or effort with no
    /// selection, or an enrollment that offers no model. Both role menus,
    /// New Task's and the task page's, pick through this one rule.
    public static func picking(_ pick: Pick, from current: Selection?, role: Role, catalog: Catalog?) -> Selection? {
        switch pick {
        case .enrollment(nil):
            return nil
        case .enrollment(let enrollmentID?):
            guard enrollmentID != current?.enrollment_id else { return current }
            return choosing(enrollment: enrollmentID, role: role, catalog: catalog) ?? current
        case .model(let modelID):
            return current.map { choosing(model: modelID, from: $0, catalog: catalog) }
        case .effort(let effort):
            return current.map { Selection(enrollment_id: $0.enrollment_id, model_id: $0.model_id, effort: effort) }
        }
    }

    private static func resetEffort(_ model: ModelOption) -> String {
        model.efforts.contains(harnessDefault) ? harnessDefault : model.efforts.first ?? harnessDefault
    }

    /// One enrollment as a role's menu lists it.
    public struct EnrollmentOption: Equatable, Sendable, Identifiable {
        public let id: String
        /// False while the daemon has no discovery record for it.
        public let isDiscovered: Bool
        /// False when it offers no model, so its entry shows disabled.
        public let isPickable: Bool
        /// Its pool's latest reading and when it was observed, or nil when
        /// no pool lists the enrollment.
        public let reading: String?

        /// The entry's text: the enrollment, `not discovered yet` when it
        /// has no discovery record, and its pool's reading.
        public var label: String {
            ([id] + [isDiscovered ? nil : "not discovered yet", reading].compactMap(\.self))
                .joined(separator: " · ")
        }
    }

    /// Every enrollment the catalog lists, in catalog order. A usage reading
    /// is shown, never weighed: it doesn't reorder, hide, or disable an
    /// enrollment (#1722).
    public static func enrollments(
        _ catalog: Catalog?, pools: [Components.Schemas.UsagePoolSnapshot], now: Date,
        locale: Locale = .current, timeZone: TimeZone = .current
    ) -> [EnrollmentOption] {
        (catalog?.enrollments ?? []).map { enrollment in
            EnrollmentOption(
                id: enrollment.enrollment_id, isDiscovered: enrollment.discovery != nil,
                isPickable: !models(on: enrollment.enrollment_id, catalog: catalog).isEmpty,
                reading: reading(
                    for: enrollment.enrollment_id, pools: pools, now: now, locale: locale, timeZone: timeZone))
        }
    }

    /// The reading of the pool that lists `enrollmentID`, in the usage
    /// view's words: the window with the least left, then when that
    /// window's reading was observed (`read 10:15 PM`), or `stale since`
    /// once the client's clock passes its `stale_at`. A later failed
    /// refresh leaves the reading's own time standing. With no reading
    /// behind the window, the collector's last attempt stands in
    /// (`unsupported`, `refresh failed`, `no reading`). Nil when no pool
    /// lists the enrollment.
    public static func reading(
        for enrollmentID: String, pools: [Components.Schemas.UsagePoolSnapshot], now: Date,
        locale: Locale = .current, timeZone: TimeZone = .current
    ) -> String? {
        guard
            let pool = pools.first(where: { snapshot in
                snapshot.pool.enrollments.contains { $0.enrollment_id == enrollmentID }
            })?.pool
        else { return nil }
        let tightest = UsageDisplay.windows(pool)
            .map { UsageDisplay.capacity(window: $0, in: pool, now: now) }
            .min { tightness($0) < tightness($1) }
        let shown = tightest.flatMap { capacity in pool.windows.first { $0.window == capacity.window } }
        func time(_ date: Date) -> String {
            FreesideFormat.shortTime(date, now: now, locale: locale, timeZone: timeZone)
        }
        let when =
            if let shown, let staleAt = shown.stale_at, UsageDisplay.isStale(shown, now: now) {
                "stale since \(time(staleAt))"
            } else if let shown {
                "read \(time(shown.observed_at))"
            } else {
                UsageDisplay.attemptLine(pool, now: now, locale: locale, timeZone: timeZone).text
            }
        return ([tightest.map { "\($0.name): \($0.value)" }, when].compactMap(\.self))
            .joined(separator: " · ")
    }

    /// Orders windows by how little is left: used up first, then by percent
    /// left, and a window with no reading last.
    private static func tightness(_ capacity: UsageDisplay.WindowCapacity) -> Int {
        switch capacity.level {
        case .usedUp: -1
        case .low, .fine: capacity.left ?? 100
        case .noReading: 101
        }
    }

    /// The menu's footnote for an enrollment: the harness build that
    /// reported its models and when, or that it isn't discovered yet. Nil
    /// for an enrollment the catalog doesn't list.
    public static func discoveryNote(
        on enrollmentID: String, catalog: Catalog?, now: Date, locale: Locale = .current,
        timeZone: TimeZone = .current
    ) -> String? {
        guard let enrollment = catalog?.enrollments.first(where: { $0.enrollment_id == enrollmentID }) else {
            return nil
        }
        guard let discovery = enrollment.discovery?.value1 else {
            return "Not discovered yet; lists what the lineup runs on it"
        }
        let observed = FreesideFormat.shortTime(discovery.observed_at, now: now, locale: locale, timeZone: timeZone)
        return "\(discovery.harness_build) · discovered \(observed)"
    }

    // MARK: Labels

    /// A selection as one label: `enrollment · model · effort`, the raw
    /// values a line records.
    public static func selectionLabel(_ selection: Selection) -> String {
        "\(selection.enrollment_id) · \(selection.model_id) · \(selection.effort)"
    }

    /// A selection's full short form: `enrollment · model · effort`, with
    /// the model's display name where discovery has one. The harness
    /// default sends no effort, so the form names none for it
    /// (`enrollment · model`), which keeps a summary short.
    public static func shortLabel(_ selection: Selection, catalog: Catalog?) -> String {
        let model = modelOption(for: selection, catalog: catalog)?.name ?? selection.model_id
        let effort = selection.effort == harnessDefault ? [] : [selection.effort]
        return ([selection.enrollment_id, model] + effort).joined(separator: " · ")
    }

    /// A selection as a confirm states it: enrollment, model by its display
    /// name, and effort, the harness default included, so nothing the line
    /// will record is left unsaid.
    public static func fullLabel(_ selection: Selection, catalog: Catalog?) -> String {
        let model = modelOption(for: selection, catalog: catalog)?.name ?? selection.model_id
        return "\(selection.enrollment_id) · \(model) · \(effortLabel(selection.effort))"
    }

    /// An effort as the form names it: the harness's own string, or
    /// `harness default`.
    public static func effortLabel(_ effort: String) -> String {
        effort == harnessDefault ? "harness default" : effort
    }

    /// A choice as `roles` name it: the enrollment alone when its model and
    /// effort are the lineup's for every one of those roles, so the label
    /// says only what the choice changes; otherwise the full short form.
    public static func choiceLabel(_ selection: Selection, roles: [Role], catalog: Catalog?) -> String {
        let matchesLineup =
            !roles.isEmpty
            && roles.allSatisfy { role in
                catalog?.lineup.first { $0.role == role }.map {
                    sameModel($0.selection.model_id, as: selection, catalog: catalog)
                        && $0.selection.effort == selection.effort
                } == true
            }
        return matchesLineup ? selection.enrollment_id : shortLabel(selection, catalog: catalog)
    }

    /// Whether `modelID` names the model `selection` runs, on the
    /// selection's enrollment: by string, or through the catalog entry both
    /// resolve to, since the lineup may name a model by what an alias
    /// resolves to while a pick records the entry's id.
    static func sameModel(_ modelID: String, as selection: Selection, catalog: Catalog?) -> Bool {
        if modelID == selection.model_id { return true }
        let probe = Selection(enrollment_id: selection.enrollment_id, model_id: modelID, effort: selection.effort)
        guard let named = modelOption(for: probe, catalog: catalog) else { return false }
        return modelOption(for: selection, catalog: catalog)?.id == named.id
    }

    /// The default entry's label: what picks the agent when the operator
    /// chooses none for `role`. It names the lineup's selection for the role
    /// in the full short form, since the enrollment alone says nothing
    /// against the lineup itself; "or routing policy" follows when the
    /// policy lists the role.
    public static func defaultLabel(
        role: Role, policy: Components.Schemas.RoutingPolicy?, catalog: Catalog?
    ) -> String {
        "Default · " + lineupLabel(role: role, policy: policy, catalog: catalog)
    }

    /// The task page's entry that clears a role's line (#2651): what then
    /// picks the agent, as `defaultLabel` names it, after `Clear`.
    public static func clearLabel(
        role: Role, policy: Components.Schemas.RoutingPolicy?, catalog: Catalog?
    ) -> String {
        "Clear · " + lineupLabel(role: role, policy: policy, catalog: catalog)
    }

    /// What an unchosen role runs on, as its chip names it: the lineup's
    /// selection for `role` in the short form, then "or routing policy" when
    /// the policy lists the role. Nil when the catalog has no lineup entry
    /// for the role, so the caller says `Default`.
    public static func defaultSettingsLabel(
        role: Role, policy: Components.Schemas.RoutingPolicy?, catalog: Catalog?
    ) -> String? {
        catalog?.lineup.first { $0.role == role }.map {
            shortLabel($0.selection, catalog: catalog) + policySuffix(role: role, policy: policy)
        }
    }

    /// What picks the agent for a role left to the lineup:
    /// `lineup` and `defaultSettingsLabel`, or `lineup` alone (with the
    /// policy's part) when the catalog has no lineup entry for the role.
    static func lineupLabel(
        role: Role, policy: Components.Schemas.RoutingPolicy?, catalog: Catalog?
    ) -> String {
        defaultSettingsLabel(role: role, policy: policy, catalog: catalog).map { "lineup \($0)" }
            ?? "lineup" + policySuffix(role: role, policy: policy)
    }

    static func policySuffix(role: Role, policy: Components.Schemas.RoutingPolicy?) -> String {
        policy?.roles.contains { $0.role == role } == true ? " or routing policy" : ""
    }

    private static func unique<T: Hashable>(_ values: [T]) -> [T] {
        var seen = Set<T>()
        return values.filter { seen.insert($0).inserted }
    }

    /// Lines as one sentence of verbs, as the Agents chip and a saved
    /// request read them: `claude-a writes · codex-b reviews` when the three
    /// writer lines carry one selection, otherwise each distinct writer
    /// selection with its roles' verbs (`claude-a specifies · codex-b
    /// implements and remediates`). Each selection reads as its
    /// `choiceLabel` for the roles it is named for, unless another selection
    /// in the sentence would read the same; then both read in the full
    /// short form, so two choices never share a name. Nil when there are no
    /// lines.
    public static func summary(_ lines: [Components.Schemas.TaskLineInput]?, catalog: Catalog? = nil) -> String? {
        guard let lines, !lines.isEmpty else { return nil }
        let byRole = Dictionary(lines.map { ($0.role, $0.selection) }, uniquingKeysWith: { first, _ in first })
        let named = unique(roleOrder.compactMap { byRole[$0] }).map { selection in
            (selection, choiceLabel(selection, roles: roleOrder.filter { byRole[$0] == selection }, catalog: catalog))
        }
        let labelCounts = Dictionary(named.map { ($0.1, 1) }, uniquingKeysWith: +)
        let labels = Dictionary(
            uniqueKeysWithValues: named.map { selection, label in
                (selection, labelCounts[label] == 1 ? label : shortLabel(selection, catalog: catalog))
            })
        func label(_ selection: Selection) -> String { labels[selection] ?? shortLabel(selection, catalog: catalog) }
        var parts: [String] = []
        let writers = writerRoles.compactMap { byRole[$0] }
        if writers.count == writerRoles.count, Set(writers).count == 1 {
            parts.append("\(label(writers[0])) writes")
        } else {
            for writer in unique(writers) {
                let verbs = writerRoles.filter { byRole[$0] == writer }.map(verb)
                parts.append("\(label(writer)) \(verbs.joined(separator: " and "))")
            }
        }
        if let reviewer = byRole[.reviewer] { parts.append("\(label(reviewer)) \(verb(.reviewer))") }
        return parts.joined(separator: " · ")
    }

    /// A role's name as the form shows it, such as the role chip's keyword.
    public static func title(for role: Role) -> String {
        switch role {
        case .specifier: "Specifier"
        case .implementer: "Implementer"
        case .remediator: "Remediator"
        case .reviewer: "Reviewer"
        case .unknown: "Unknown role"
        }
    }

    private static func verb(_ role: Role) -> String {
        switch role {
        case .specifier: "specifies"
        case .implementer: "implements"
        case .remediator: "remediates"
        case .reviewer: "reviews"
        case .unknown: "works"
        }
    }
}
