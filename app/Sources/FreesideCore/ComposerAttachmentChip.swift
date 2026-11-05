import CoreGraphics
import FreesideModel
import SwiftUI

/// One attachment in the New Task composer (#2590), on R33's composer-chip
/// pill: a thumbnail for an image or a document glyph for a file, then the
/// name, its size in dim, and a remove control. The name truncates in the
/// middle so its extension stays visible.
struct ComposerAttachmentChip: View {
    let attachment: DraftAttachment
    /// The image's decoded thumbnail; nil for a file or bytes that don't
    /// decode, which draw the document glyph.
    let thumbnail: CGImage?
    let onRemove: () -> Void

    static func size(_ bytes: Int) -> String {
        ByteCountFormatter.string(fromByteCount: Int64(bytes), countStyle: .file)
    }

    var body: some View {
        HStack(spacing: 6) {
            Group {
                if let thumbnail {
                    Image(decorative: thumbnail, scale: 1)
                        .resizable()
                        .scaledToFill()
                        .frame(width: 18, height: 18)
                        .clipShape(RoundedRectangle(cornerRadius: 3))
                } else {
                    Image(systemName: "doc")
                        .font(.system(size: 11))
                        .foregroundStyle(Color.inkDim)
                        .frame(width: 18, height: 18)
                }
            }
            .accessibilityHidden(true)
            Text(attachment.name)
                .font(FreesideFont.trailingSummary)
                .foregroundStyle(Color.ink)
                .lineLimit(1)
                .truncationMode(.middle)
            Text(Self.size(attachment.sizeBytes))
                .font(FreesideFont.trailingSummary)
                .foregroundStyle(Color.inkDim)
                .lineLimit(1)
                .fixedSize()
            Button(action: onRemove) {
                Image(systemName: "xmark")
                    .font(.system(size: 9, weight: .semibold))
                    .foregroundStyle(Color.inkDim)
                    .frame(width: 14, height: 14)
                    .contentShape(Rectangle())
            }
            .buttonStyle(.plain)
            .accessibilityLabel("Remove \(attachment.name)")
        }
        .padding(.vertical, 3)
        .padding(.leading, 4)
        .padding(.trailing, 8)
        .composerPill(isActive: false)
        .accessibilityElement(children: .contain)
        .accessibilityLabel("\(attachment.name), \(Self.size(attachment.sizeBytes))")
    }
}
