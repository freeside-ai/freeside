import FreesideAPI
import SwiftUI

/// The seal moment for Stop, Decline, and Dismiss: the one place wax is
/// allowed. Replaces the system confirmation dialog, whose destructive
/// button took the system red fill (devlog
/// 2026-09-12-0945-chrome-under-design-language). The sheet names the
/// action, states its consequence in the daemon's words, and binds the
/// submission to the reviewed item by id and version, so the operator
/// confirms exactly what will be sent.
struct ConsequenceSheet: View {
    let action: Components.Schemas.Action
    /// The reviewed snapshot's item: the consequence and the binding line
    /// read from what will be submitted, never from a later refresh.
    let item: Components.Schemas.AttentionItem
    let submit: () -> Void
    let cancel: () -> Void

    #if !os(macOS)
        @Environment(\.dynamicTypeSize) private var dynamicTypeSize
    #endif

    var body: some View {
        #if os(macOS)
            content.frame(width: 380)
        #else
            // Accessibility text sizes outgrow the medium detent (the 390pt
            // phone render is 448pt tall at accessibility 3), so they take
            // the large one, and content that still does not fit scrolls
            // rather than clipping the buttons.
            ViewThatFits(in: .vertical) {
                content
                ScrollView { content }
            }
            .presentationDetents(dynamicTypeSize.isAccessibilitySize ? [.large] : [.medium])
            .presentationDragIndicator(.visible)
            .presentationBackground(Color.ground2)
        #endif
    }

    private var content: some View {
        VStack(alignment: .leading, spacing: 0) {
            VStack(alignment: .leading, spacing: 10) {
                Text(title)
                    .font(FreesideFont.sectionTitle)
                    .foregroundStyle(Color.ink)
                    .accessibilityAddTraits(.isHeader)
                if let consequence {
                    Text(consequence)
                        .font(FreesideFont.callout)
                        .foregroundStyle(Color.ink)
                }
                Text(bindingLine)
                    .font(FreesideFont.monoCaption)
                    .foregroundStyle(Color.inkDim)
            }
            .fixedSize(horizontal: false, vertical: true)
            .frame(maxWidth: .infinity, alignment: .leading)
            .padding(20)
            .padding(.bottom, -6)
            FreesideSheetActionRow(
                submitLabel: actionLabel,
                tone: .destructive,
                submitHint: consequence,
                submit: submit,
                cancel: cancel)
        }
        .background(Color.ground2)
    }

    private var actionLabel: String {
        AttentionDisplay.label(action)
    }

    /// "Stop this run?", "Decline this proposal?", "Dismiss this item?";
    /// any other consequential action falls back to the plain form.
    private var title: String {
        switch action {
        case .stop: "\(actionLabel) this run?"
        case .decline: "\(actionLabel) this proposal?"
        case .dismiss: "\(actionLabel) this item?"
        default: "Confirm \(actionLabel.lowercased())?"
        }
    }

    private var consequence: String? {
        AttentionDisplay.confirmationConsequence(action, for: item)
    }

    /// The subject id and the item version the decision commits against.
    /// The item carries no attempt number, so none is shown.
    private var bindingLine: String {
        let subjectID: String
        switch item.subject {
        case .run(let scoped), .proposal_batch(let scoped):
            subjectID = scoped.subject_id
        case .project(let unscoped), .system(let unscoped):
            subjectID = unscoped.subject_id
        }
        return "\(subjectID) · item version \(item.item_version)"
    }
}
