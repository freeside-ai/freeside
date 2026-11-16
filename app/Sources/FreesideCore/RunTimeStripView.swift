import FreesideAPI
import SwiftUI

/// Where a run's time went, to scale (#3233): one bar cut into agent
/// working, waiting on you, held (#3237), and everything else, with the
/// elapsed time and each kind's total under it. The bar keeps true scale,
/// so a short segment can be thin; the totals and each segment's own
/// accessible button carry what the eye can't.
struct RunTimeStripView: View {
    let strip: RunTimeStrip.Strip
    /// Brings the milestones rail to a segment. Nil (a screenshot) leaves
    /// each segment a button that does nothing.
    var onSelect: ((RunTimeStrip.Segment) -> Void)? = nil
    @Environment(\.locale) private var locale
    @Environment(\.timeZone) private var timeZone
    @Environment(\.pinnedNow) private var pinnedNow
    /// The legend's swatch grows with its text.
    @ScaledMetric(relativeTo: .callout) private var swatch: CGFloat = 9

    private static let barHeight: CGFloat = 12
    /// Taller than the bar, so a thin segment's button still takes a press
    /// above and below it.
    private static let hitPadding: CGFloat = 8

    var body: some View {
        VStack(alignment: .leading, spacing: 4) {
            HStack(alignment: .firstTextBaseline, spacing: 8) {
                KeywordLabel(text: "Elapsed")
                Text(Self.duration(strip.elapsed, locale: locale))
                    .font(FreesideFont.trailingSummary)
                    .foregroundStyle(Color.ink)
            }
            // The legend says the same in words; the heading reads it once.
            .accessibilityElement(children: .ignore)
            .accessibilityLabel(Self.summary(strip, locale: locale))
            .accessibilityAddTraits(.isHeader)
            bar
            WrappingHStack(horizontalSpacing: 14, verticalSpacing: 4) {
                ForEach(RunTimeStrip.Kind.allCases, id: \.self) { kind in
                    legendEntry(kind)
                }
            }
            .accessibilityHidden(true)
        }
    }

    private var bar: some View {
        GeometryReader { proxy in
            HStack(spacing: 0) {
                ForEach(Array(strip.segments.enumerated()), id: \.offset) { index, segment in
                    let label = segmentLabel(segment)
                    let width = proxy.size.width * CGFloat(segment.duration / strip.elapsed)
                    Button {
                        onSelect?(segment)
                    } label: {
                        // A clear hairline opens each later segment, so
                        // where one ends shows without telling the kinds
                        // apart by hue; a segment too thin to spare it keeps
                        // its colour.
                        Rectangle()
                            .fill(Self.color(segment.kind))
                            .padding(.leading, index > 0 && width > 3 ? 1 : 0)
                            .frame(width: width, height: Self.barHeight)
                            .padding(.vertical, Self.hitPadding)
                            .contentShape(Rectangle())
                    }
                    .buttonStyle(.plain)
                    .accessibilityLabel(label)
                    .help(label)
                }
            }
            .mask {
                RoundedRectangle(cornerRadius: 3).frame(height: Self.barHeight)
            }
        }
        .frame(height: Self.barHeight + 2 * Self.hitPadding)
    }

    private func legendEntry(_ kind: RunTimeStrip.Kind) -> some View {
        HStack(alignment: .firstTextBaseline, spacing: 6) {
            RoundedRectangle(cornerRadius: 2)
                .fill(Self.color(kind))
                .frame(width: swatch, height: swatch)
            Text(Self.label(kind))
                .font(FreesideFont.cardBody)
                .foregroundStyle(Color.inkDim)
            Text(Self.duration(strip.total(kind), locale: locale))
                .font(FreesideFont.trailingSummary)
                .foregroundStyle(Color.ink)
        }
    }

    private func segmentLabel(_ segment: RunTimeStrip.Segment) -> String {
        Self.segmentLabel(segment, now: pinnedNow ?? Date(), locale: locale, timeZone: timeZone)
    }

    /// A segment's kind, a held segment's reason in the hold callout's
    /// words, its length, and its span: the segment's tooltip and
    /// accessible label.
    static func segmentLabel(
        _ segment: RunTimeStrip.Segment, now: Date, locale: Locale, timeZone: TimeZone
    ) -> String {
        let from = FreesideFormat.shortTime(segment.start, now: now, locale: locale, timeZone: timeZone)
        let to = FreesideFormat.shortTime(segment.end, now: now, locale: locale, timeZone: timeZone)
        let name = [label(segment.kind), segment.holdReason.map(RunDisplay.label)].compactMap { $0 }
        return (name + [duration(segment.duration, locale: locale, width: .wide), "\(from) to \(to)"])
            .joined(separator: ", ")
    }

    /// Water for the agent, as a running invocation is; the accent for
    /// waiting on you, as a blocked one is; the dim ink for a hold, apart
    /// from the strong neutral the rest takes. Wax stays the rail's failure
    /// mark.
    static func color(_ kind: RunTimeStrip.Kind) -> Color {
        switch kind {
        case .agentWorking: .waterText
        case .waitingOnYou: .accentBorder
        case .held: .inkDim
        case .other: .ruleStrong
        }
    }

    static func label(_ kind: RunTimeStrip.Kind) -> String {
        switch kind {
        case .agentWorking: "Agent working"
        case .waitingOnYou: "Waiting on you"
        case .held: "Held"
        case .other: "Other"
        }
    }

    /// Hours and minutes, `3h 50m`, or spelled out for a spoken label.
    static func duration(
        _ interval: TimeInterval, locale: Locale, width: Duration.UnitsFormatStyle.UnitWidth = .narrow
    ) -> String {
        Duration.seconds(interval.rounded())
            .formatted(.units(allowed: [.hours, .minutes], width: width).locale(locale))
    }

    /// The strip in one sentence for assistive technology: the elapsed time,
    /// then each kind's total.
    static func summary(_ strip: RunTimeStrip.Strip, locale: Locale) -> String {
        let totals = RunTimeStrip.Kind.allCases.map { kind in
            "\(label(kind).lowercased()) \(duration(strip.total(kind), locale: locale, width: .wide))"
        }
        return "Elapsed \(duration(strip.elapsed, locale: locale, width: .wide)): "
            + totals.joined(separator: ", ") + "."
    }
}
