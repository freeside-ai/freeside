import Foundation
import FreesideAPI

/// One role's agent picker while it is open (#3027): the pick it stages,
/// the rows and pills it draws, its footer, and whether its confirm can
/// write. New Task's role chips and the task page's Change Agent open the
/// same picker; only the mode differs.
///
/// A pick only stages. Nothing reaches the draft or the daemon until the
/// caller takes `confirmation`'s ready selection on the operator's
/// confirm, so a stray tap can't change an agent.
///
/// Controls keep their place while the picker is open. It remembers the
/// order in which it first showed each enrollment, each model of an
/// enrollment, and each effort of a model; a newer catalog rewrites their
/// text in place, keeps a part it dropped in its slot as not offered, and
/// adds a new part at the end (`absorb`). A tap meant for one row can't
/// land on another because a sync moved it.
public struct RolePicker: Equatable, Sendable {
    public typealias Role = AgentChoiceDisplay.Role
    public typealias Selection = AgentChoiceDisplay.Selection
    public typealias Catalog = AgentChoiceDisplay.Catalog
    public typealias Policy = Components.Schemas.RoutingPolicy

    /// Where the picker's confirm writes.
    public enum Mode: Equatable, Sendable {
        /// New Task: Done writes the role's choice to the draft.
        case draft
        /// A running task: the confirm sends one change of the role's line.
        /// `current` is the line's selection, nil while the role has no
        /// line or holds a line set by name; `byName` is the agent such a
        /// line names (before #2597).
        case change(current: Selection?, byName: String?)
    }

    /// What the picker holds for the role.
    public enum Staged: Equatable, Sendable {
        /// The role left to the lineup: New Task's Default, the task
        /// page's Clear.
        case lineup
        /// A running task's line set by name, untouched since the picker
        /// opened.
        case byName(String)
        case selection(Selection)

        public var selection: Selection? {
            if case .selection(let selection) = self { return selection }
            return nil
        }
    }

    public let role: Role
    /// The caller refreshes a running task's line while the picker is open,
    /// so the footer and the confirm read the live line.
    public var mode: Mode
    public private(set) var staged: Staged

    private var enrollmentOrder: [String] = []
    private var modelOrder: [String: [String]] = [:]
    private var effortOrder: [ModelKey: [String]] = [:]

    private struct ModelKey: Hashable, Sendable {
        let enrollment: String
        let model: String
    }

    /// New Task's picker for `role`, holding the draft's choice.
    public static func draft(role: Role, chosen: Selection?, catalog: Catalog?) -> RolePicker {
        RolePicker(role: role, mode: .draft, staged: chosen.map(Staged.selection) ?? .lineup, catalog: catalog)
    }

    /// A running task's picker for `role`, holding the role's line.
    public static func change(role: Role, current: Selection?, byName: String?, catalog: Catalog?) -> RolePicker {
        let staged: Staged = current.map(Staged.selection) ?? byName.map(Staged.byName) ?? .lineup
        return RolePicker(role: role, mode: .change(current: current, byName: byName), staged: staged, catalog: catalog)
    }

    private init(role: Role, mode: Mode, staged: Staged, catalog: Catalog?) {
        self.role = role
        self.mode = mode
        self.staged = staged
        absorb(catalog: catalog)
    }

    // MARK: Order

    /// Records the parts `catalog` shows that the picker hasn't, after
    /// those it has. Call it whenever the catalog changes while the picker
    /// is open. A part `catalog` lacks keeps its slot; the rows mark it.
    public mutating func absorb(catalog: Catalog?) {
        for enrollment in catalog?.enrollments ?? [] {
            let id = enrollment.enrollment_id
            Self.append(id, to: &enrollmentOrder)
            for model in AgentChoiceDisplay.models(on: id, catalog: catalog) {
                Self.append(model.id, to: &modelOrder[id, default: []])
                for effort in model.efforts {
                    Self.append(effort, to: &effortOrder[ModelKey(enrollment: id, model: model.id), default: []])
                }
            }
        }
        // A selection the catalog already dropped still shows, in place,
        // as not offered: the one held, the line, and the lineup's.
        for selection in [staged.selection, currentSelection, lineup(catalog)].compactMap(\.self) {
            let model = modelPillID(selection, catalog: catalog)
            Self.append(selection.enrollment_id, to: &enrollmentOrder)
            Self.append(model, to: &modelOrder[selection.enrollment_id, default: []])
            Self.append(
                selection.effort,
                to: &effortOrder[ModelKey(enrollment: selection.enrollment_id, model: model), default: []])
        }
    }

    private static func append(_ value: String, to order: inout [String]) {
        if !order.contains(value) { order.append(value) }
    }

    // MARK: Picking

    /// Stages `pick` through `AgentChoiceDisplay.picking`, the rule New
    /// Task and Change Agent share. A pick that changes nothing leaves the
    /// staged value as it was, a line set by name included; Default or
    /// Clear replaces it.
    public mutating func pick(_ pick: AgentChoiceDisplay.Pick, catalog: Catalog?) {
        if let next = AgentChoiceDisplay.picking(pick, from: staged.selection, role: role, catalog: catalog) {
            staged = .selection(next)
        } else if pick == .enrollment(nil) {
            staged = .lineup
        }
    }

    // MARK: Rows

    /// One row of the Enrollment section.
    public struct EnrollmentRow: Equatable, Sendable, Identifiable {
        public enum ID: Hashable, Sendable {
            /// Default or Clear.
            case lineup
            /// A line set by name, shown but never picked.
            case byName
            case enrollment(String)
        }

        public let id: ID
        public let label: String
        /// Dim under the label: the lineup's settings, the usage reading,
        /// or why the row can't be picked.
        public let detail: String?
        public let isEnabled: Bool
    }

    /// The Enrollment section: Default or Clear naming what the lineup
    /// runs, a line set by name when the role holds one, then every
    /// enrollment in the order the picker first showed it. A usage reading
    /// is shown, never weighed (#1722).
    public func enrollmentRows(
        catalog: Catalog?, policy: Policy?, pools: [Components.Schemas.UsagePoolSnapshot], now: Date,
        locale: Locale = .current, timeZone: TimeZone = .current
    ) -> [EnrollmentRow] {
        let options = Dictionary(
            AgentChoiceDisplay.enrollments(catalog, pools: pools, now: now, locale: locale, timeZone: timeZone)
                .map { ($0.id, $0) },
            uniquingKeysWith: { first, _ in first })
        let lineupRow = EnrollmentRow(
            id: .lineup, label: mode == .draft ? "Default" : "Clear",
            detail: AgentChoiceDisplay.lineupLabel(role: role, policy: policy, catalog: catalog), isEnabled: true)
        let byNameRow = byName.map {
            EnrollmentRow(id: .byName, label: $0, detail: "set by name", isEnabled: false)
        }
        let enrollmentRows = enrollmentOrder.map { id in
            guard let option = options[id] else {
                return EnrollmentRow(id: .enrollment(id), label: id, detail: "not offered", isEnabled: false)
            }
            let detail = [option.isDiscovered ? nil : "not discovered yet", option.reading].compactMap(\.self)
            return EnrollmentRow(
                id: .enrollment(id), label: id,
                detail: detail.isEmpty ? nil : detail.joined(separator: " · "), isEnabled: option.isPickable)
        }
        return [lineupRow] + [byNameRow].compactMap(\.self) + enrollmentRows
    }

    /// The Enrollment row the staged value marks.
    public var chosenRow: EnrollmentRow.ID {
        switch staged {
        case .lineup: .lineup
        case .byName: .byName
        case .selection(let selection): .enrollment(selection.enrollment_id)
        }
    }

    /// One pill of the Model or Effort section.
    public struct Pill: Equatable, Sendable, Identifiable {
        /// The model id or effort a pick of it stages.
        public let id: String
        public let label: String
        public let isChosen: Bool
        /// False when the catalog no longer offers it, or while Default or
        /// Clear shows the lineup's model and effort, read-only.
        public let isEnabled: Bool
    }

    /// The Model section: the staged enrollment's models, or, with Default
    /// or Clear chosen, the lineup's, read-only with its model marked.
    /// Empty for a line set by name and for a role with no lineup.
    public func modelPills(catalog: Catalog?) -> [Pill] {
        guard let shown = shownSelection(catalog: catalog) else { return [] }
        let offered = AgentChoiceDisplay.models(on: shown.enrollment_id, catalog: catalog)
        let chosen = modelPillID(shown, catalog: catalog)
        return (modelOrder[shown.enrollment_id] ?? []).map { id in
            let option = offered.first { $0.id == id }
            return Pill(
                id: id, label: option?.name ?? "\(id) · not offered", isChosen: id == chosen,
                isEnabled: isEditable && option != nil)
        }
    }

    /// The Effort section: the efforts of the model the Model section
    /// marks, the same way.
    public func effortPills(catalog: Catalog?) -> [Pill] {
        guard let shown = shownSelection(catalog: catalog) else { return [] }
        let offered = AgentChoiceDisplay.modelOption(for: shown, catalog: catalog)?.efforts ?? []
        let key = ModelKey(enrollment: shown.enrollment_id, model: modelPillID(shown, catalog: catalog))
        return (effortOrder[key] ?? []).map { effort in
            let isOffered = offered.contains(effort)
            return Pill(
                id: effort, label: AgentChoiceDisplay.effortLabel(effort) + (isOffered ? "" : " · not offered"),
                isChosen: effort == shown.effort, isEnabled: isEditable && isOffered)
        }
    }

    /// The enrollment the discovery note speaks for: the one the Model
    /// section shows.
    public func shownEnrollment(catalog: Catalog?) -> String? {
        shownSelection(catalog: catalog)?.enrollment_id
    }

    private var isEditable: Bool { staged.selection != nil }

    private func shownSelection(catalog: Catalog?) -> Selection? {
        switch staged {
        case .selection(let selection): selection
        case .lineup: lineup(catalog)
        case .byName: nil
        }
    }

    private func lineup(_ catalog: Catalog?) -> Selection? {
        catalog?.lineup.first { $0.role == role }?.selection
    }

    /// The pill a selection's model marks: the catalog entry it resolves
    /// to, or its own id once the catalog dropped it.
    private func modelPillID(_ selection: Selection, catalog: Catalog?) -> String {
        AgentChoiceDisplay.modelOption(for: selection, catalog: catalog)?.id ?? selection.model_id
    }

    private var currentSelection: Selection? {
        if case .change(let current, _) = mode { return current }
        return nil
    }

    private var byName: String? {
        if case .change(.none, let byName) = mode { return byName }
        return nil
    }

    // MARK: Footer and confirm

    /// One line of the footer: its keyword and the tuple it names.
    public struct FooterLine: Equatable, Sendable {
        public let keyword: String
        public let text: String
    }

    /// The tuple the confirm writes, whole: `Runs on` for New Task, `Now`
    /// and `New` for a running task.
    public func footer(catalog: Catalog?, policy: Policy?) -> [FooterLine] {
        let new = label(staged, catalog: catalog, policy: policy)
        switch mode {
        case .draft:
            return [FooterLine(keyword: "Runs on", text: new)]
        case .change(let current, let byName):
            let now: Staged = current.map(Staged.selection) ?? byName.map(Staged.byName) ?? .lineup
            return [
                FooterLine(keyword: "Now", text: label(now, catalog: catalog, policy: policy)),
                FooterLine(keyword: "New", text: new),
            ]
        }
    }

    private func label(_ value: Staged, catalog: Catalog?, policy: Policy?) -> String {
        switch value {
        case .selection(let selection):
            return AgentChoiceDisplay.fullLabel(selection, catalog: catalog)
        case .byName(let agent):
            return "\(agent) · set by name"
        case .lineup:
            guard let lineup = lineup(catalog) else { return "Default" }
            return "lineup \(AgentChoiceDisplay.fullLabel(lineup, catalog: catalog))"
                + AgentChoiceDisplay.policySuffix(role: role, policy: policy)
        }
    }

    /// Whether the confirm can write, and what.
    public enum Confirmation: Equatable, Sendable {
        /// The selection to write; nil leaves the role to the lineup.
        case ready(Selection?)
        /// Why the confirm is off, shown beside it.
        case blocked(String)
    }

    /// Ready unless the staged value is the running task's line already or
    /// the live catalog no longer offers it. A line set by name holds no
    /// selection, so any pick replaces it, Clear included; left staged, it
    /// writes nothing, even once the live line has changed under it.
    public func confirmation(catalog: Catalog?) -> Confirmation {
        if case .change(let current, let byName) = mode {
            let title = AgentChoiceDisplay.title(for: role).lowercased()
            if case .byName(let name) = staged, byName != name {
                return .blocked("The \(title) changed while this was open. Pick what it should run on.")
            }
            let unchanged =
                switch staged {
                case .byName: true
                case .lineup: current == nil && byName == nil
                case .selection(let selection): current.map { Self.sameLine(selection, $0, catalog: catalog) } ?? false
                }
            if unchanged {
                return .blocked("The \(title) runs on this now.")
            }
        }
        if let selection = staged.selection, let part = AgentChoiceDisplay.droppedPart(selection, catalog: catalog) {
            return .blocked(Self.notOffered(selection, part: part))
        }
        return .ready(staged.selection)
    }

    /// Whether `staged` would write `line` again: the same enrollment and
    /// effort, and the same catalog model however each names it, since a
    /// line written before #3027 can hold the name an alias resolves to.
    private static func sameLine(_ staged: Selection, _ line: Selection, catalog: Catalog?) -> Bool {
        staged.enrollment_id == line.enrollment_id && staged.effort == line.effort
            && AgentChoiceDisplay.sameModel(staged.model_id, as: line, catalog: catalog)
    }

    private static func notOffered(_ selection: Selection, part: AgentChoiceDisplay.DroppedPart) -> String {
        let enrollment = selection.enrollment_id
        switch part {
        case .enrollment: return "The daemon no longer holds \(enrollment)."
        case .everyModel: return "\(enrollment) no longer offers any model."
        case .model: return "\(enrollment) no longer offers \(selection.model_id)."
        case .effort:
            return "\(selection.model_id) on \(enrollment) no longer offers "
                + "\(AgentChoiceDisplay.effortLabel(selection.effort)) effort."
        }
    }
}
