import SwiftUI

/// A compact leading-aligned flow for fixed-width chips. Each child keeps its
/// intrinsic width; the layout moves whole children to the next line instead
/// of compressing or truncating them. A child wider than a whole line is the
/// one exception: it is measured at the line's width, so a title set in an
/// accessibility size wraps instead of running past the edge.
struct WrappingHStack: Layout {
    var horizontalSpacing: CGFloat = 6
    var verticalSpacing: CGFloat = 6

    func sizeThatFits(
        proposal: ProposedViewSize,
        subviews: Subviews,
        cache: inout ()
    ) -> CGSize {
        let maximumWidth = proposal.width ?? .infinity
        return fittingSize(for: sizes(of: subviews, maximumWidth: maximumWidth), proposedWidth: proposal.width)
    }

    func fittingSize(for sizes: [CGSize], proposedWidth: CGFloat?) -> CGSize {
        arrange(sizes, maximumWidth: proposedWidth ?? .infinity).size
    }

    func placeSubviews(
        in bounds: CGRect,
        proposal: ProposedViewSize,
        subviews: Subviews,
        cache: inout ()
    ) {
        let sizes = sizes(of: subviews, maximumWidth: bounds.width)
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

    private func sizes(of subviews: Subviews, maximumWidth: CGFloat) -> [CGSize] {
        subviews.map { subview in
            let ideal = subview.sizeThatFits(.unspecified)
            guard ideal.width > maximumWidth else { return ideal }
            return subview.sizeThatFits(ProposedViewSize(width: maximumWidth, height: nil))
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
