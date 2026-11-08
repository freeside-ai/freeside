import FreesideAPI
import FreesideModel
import SwiftUI

/// A role's agent menu, the one New Task's role chips open: the lineup entry
/// and the enrollments with their usage readings, then, once an enrollment is
/// chosen, its models and the chosen model's efforts, and a footnote naming
/// the harness build that reported them. Each pick closes the menu and goes
/// through `AgentChoiceDisplay.picking`; a pick that changes nothing calls
/// nothing. A model or effort a newer catalog dropped keeps a disabled entry
/// tagged with it, so its picker still shows it chosen, and so does a line
/// recorded by agent name before #2597, which any pick replaces. The caller
/// supplies the menu around it, so only its entries are drawn here.
struct RoleSelectionMenu: View {
    typealias Role = AgentChoiceDisplay.Role
    typealias Selection = AgentChoiceDisplay.Selection

    /// What the menu offers and shows chosen. Each surface builds it from
    /// the synced catalog and usage pools, so two surfaces showing the same
    /// selection offer the same entries.
    struct Offer: Equatable {
        let role: Role
        let catalog: AgentChoiceDisplay.Catalog?
        let pools: [Components.Schemas.UsagePoolSnapshot]
        /// The selection the role holds; nil while it is left to the lineup
        /// or holds a line set by name.
        let current: Selection?
        /// The agent a line recorded before #2597 names in place of a
        /// selection. New Task has no such line.
        var byName: String? = nil
    }

    let offer: Offer
    /// The entry that leaves the role to the lineup.
    let lineupLabel: String
    let onChoose: (Selection?) -> Void

    /// The tag of an entry in the enrollment picker.
    private enum EnrollmentEntry: Hashable {
        case lineup
        case byName
        case enrollment(String)
    }

    var body: some View {
        let catalog = offer.catalog
        let chosen = offer.current
        let now = Date()
        let dropped = chosen.flatMap { AgentChoiceDisplay.droppedPart($0, catalog: catalog) }
        Picker("\(AgentChoiceDisplay.title(for: offer.role)) enrollment", selection: enrollmentChoice) {
            Text(lineupLabel).tag(EnrollmentEntry.lineup)
            ForEach(AgentChoiceDisplay.enrollments(catalog, pools: offer.pools, now: now)) { enrollment in
                Text(enrollment.label).tag(EnrollmentEntry.enrollment(enrollment.id)).disabled(!enrollment.isPickable)
            }
            if let agent = offer.byName {
                Text("\(agent) · set by name").tag(EnrollmentEntry.byName).disabled(true)
            }
            if let chosen, dropped == .enrollment {
                Text("\(chosen.enrollment_id) · no longer offered")
                    .tag(EnrollmentEntry.enrollment(chosen.enrollment_id)).disabled(true)
            }
        }
        .pickerStyle(.inline)
        if let chosen, dropped != .enrollment {
            let option = AgentChoiceDisplay.modelOption(for: chosen, catalog: catalog)
            Section("Model") {
                Picker("Model", selection: modelChoice) {
                    ForEach(AgentChoiceDisplay.models(on: chosen.enrollment_id, catalog: catalog)) { model in
                        Text(model.name).tag(model.id)
                    }
                    if option == nil {
                        Text("\(chosen.model_id) · no longer offered").tag(chosen.model_id).disabled(true)
                    }
                }
                .pickerStyle(.inline)
            }
            if let option {
                Section("Effort") {
                    Picker("Effort", selection: effortChoice) {
                        ForEach(option.efforts, id: \.self) { effort in
                            Text(AgentChoiceDisplay.effortLabel(effort)).tag(effort)
                        }
                        if dropped == .effort {
                            Text("\(AgentChoiceDisplay.effortLabel(chosen.effort)) · no longer offered")
                                .tag(chosen.effort).disabled(true)
                        }
                    }
                    .pickerStyle(.inline)
                }
            }
            if let note = AgentChoiceDisplay.discoveryNote(on: chosen.enrollment_id, catalog: catalog, now: now) {
                Text(note)
            }
        }
    }

    /// What a pick hands `onChoose`: the role's selection after it.
    struct Choice: Equatable {
        let selection: Selection?
    }

    /// The choice `pick` makes in `offer`, or nil when it changes nothing.
    /// A line set by name holds no selection, so leaving it for the lineup
    /// is a change too.
    static func choice(_ pick: AgentChoiceDisplay.Pick, in offer: Offer) -> Choice? {
        let next = AgentChoiceDisplay.picking(pick, from: offer.current, role: offer.role, catalog: offer.catalog)
        guard next != offer.current || (offer.byName != nil && pick == .enrollment(nil)) else { return nil }
        return Choice(selection: next)
    }

    private func choose(_ pick: AgentChoiceDisplay.Pick) {
        if let choice = Self.choice(pick, in: offer) { onChoose(choice.selection) }
    }

    private var enrollmentChoice: Binding<EnrollmentEntry> {
        Binding(
            get: {
                if let current = offer.current { return .enrollment(current.enrollment_id) }
                return offer.byName == nil ? .lineup : .byName
            },
            set: { entry in
                switch entry {
                case .lineup: choose(.enrollment(nil))
                case .byName: break
                case .enrollment(let id): choose(.enrollment(id))
                }
            })
    }

    /// The chosen model as its menu entry's id, which differs from the
    /// recorded id when the tree names the model the entry resolves to.
    private var modelChoice: Binding<String> {
        Binding(
            get: {
                offer.current.map {
                    AgentChoiceDisplay.modelOption(for: $0, catalog: offer.catalog)?.id ?? $0.model_id
                } ?? ""
            },
            set: { choose(.model($0)) })
    }

    private var effortChoice: Binding<String> {
        Binding(get: { offer.current?.effort ?? "" }, set: { choose(.effort($0)) })
    }
}
