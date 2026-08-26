import SwiftUI

/// A compact leading-aligned flow for fixed-width chips. Each child keeps its
/// intrinsic width; the layout moves whole children to the next line instead
/// of compressing or truncating them.
struct WrappingHStack: Layout {
    var horizontalSpacing: CGFloat = 6
    var verticalSpacing: CGFloat = 6

    func sizeThatFits(
        proposal: ProposedViewSize,
        subviews: Subviews,
        cache: inout ()
    ) -> CGSize {
        let width = proposal.width ?? .infinity
        let arrangement = arrange(
            subviews.map { $0.sizeThatFits(.unspecified) }, maximumWidth: width)
        return CGSize(width: proposal.width ?? arrangement.size.width, height: arrangement.size.height)
    }

    func placeSubviews(
        in bounds: CGRect,
        proposal: ProposedViewSize,
        subviews: Subviews,
        cache: inout ()
    ) {
        let sizes = subviews.map { $0.sizeThatFits(.unspecified) }
        let arrangement = arrange(sizes, maximumWidth: bounds.width)
        for (index, subview) in subviews.enumerated() {
            subview.place(
                at: CGPoint(
                    x: bounds.minX + arrangement.offsets[index].x,
                    y: bounds.minY + arrangement.offsets[index].y),
                anchor: .topLeading,
                proposal: ProposedViewSize(sizes[index])
            )
        }
    }

    private func arrange(_ sizes: [CGSize], maximumWidth: CGFloat) -> Arrangement {
        var offsets: [CGPoint] = []
        var x: CGFloat = 0
        var y: CGFloat = 0
        var rowHeight: CGFloat = 0
        var usedWidth: CGFloat = 0

        for size in sizes {
            let proposedX = x == 0 ? 0 : x + horizontalSpacing
            if proposedX + size.width > maximumWidth, x > 0 {
                x = 0
                y += rowHeight + verticalSpacing
                rowHeight = 0
            } else {
                x = proposedX
            }
            offsets.append(CGPoint(x: x, y: y))
            x += size.width
            rowHeight = max(rowHeight, size.height)
            usedWidth = max(usedWidth, x)
        }

        return Arrangement(
            size: CGSize(width: usedWidth, height: sizes.isEmpty ? 0 : y + rowHeight),
            offsets: offsets)
    }

    private struct Arrangement {
        let size: CGSize
        let offsets: [CGPoint]
    }
}
