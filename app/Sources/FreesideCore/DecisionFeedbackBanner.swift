import SwiftUI

struct DecisionFeedbackBanner: View {
    let feedback: DecisionFeedbackModel
    let onView: (String) -> Void

    var body: some View {
        if let conclusion = feedback.conclusion {
            HStack(spacing: 10) {
                Image(systemName: "checkmark")
                    .accessibilityHidden(true)
                Text("\(conclusion.actionLabel) applied")
                    .font(FreesideFont.callout)
                Spacer()
                if conclusion.hasQueryableItem {
                    Button("View") { onView(conclusion.itemID) }
                }
            }
            .foregroundStyle(Color.inkDim)
            .padding(.horizontal, 12)
            .padding(.vertical, 8)
            .frame(maxWidth: .infinity, alignment: .leading)
            .background(Color.neutralWash)
            .accessibilityElement(children: .contain)
        }
    }
}
