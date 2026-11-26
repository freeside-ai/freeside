import FreesideAPI
import SwiftUI

/// Where a task's time went across its runs, to scale (#3234): one bar
/// from the first run's start to the last run's end, each run's part cut
/// as the run page's strip cuts it, and the time between runs drawn as a
/// thin line. Each run's part is a button to its run history. Over the
/// bar a stage strip names each run's part by its stage (#3455), so the
/// reader sees which stretch was specification and which implementation.
struct TaskTimeStripView: View {
    let strip: TaskTimeStrip.Strip
    /// Each run's title by run id, as the page's run lines name it: the
    /// bar's parts speak it.
    let titles: [String: String]
    /// Each run's stage by run id, as the stage strip names it
    /// (`TaskTimelinePresentation.stageLabel`).
    let stages: [String: String]
    /// Opens a run's history. Nil (a screenshot) leaves each run's part a
    /// button that does nothing.
    var onOpenRun: ((String) -> Void)? = nil
    @Environment(\.locale) private var locale
    @Environment(\.timeZone) private var timeZone
    @Environment(\.pinnedNow) private var pinnedNow
    /// The legend's swatch grows with its text.
    @ScaledMetric(relativeTo: .callout) private var swatch: CGFloat = 9
    /// A stage block grows with its name.
    @ScaledMetric(relativeTo: .caption2) private var stageHeight: CGFloat = 18

    private static let barHeight: CGFloat = 12
    /// Taller than the bar, so a thin part's button still takes a press
    /// above and below it.
    private static let hitPadding: CGFloat = 8
    /// The line drawn across time no run spans: thinner than any run's
    /// part, so it reads as a gap by its shape in every contrast setting,
    /// not only by its shade.
    private static let betweenRunsHeight: CGFloat = 2
    /// The visible gap between a stage block and the bar under it.
    private static let stageGap: CGFloat = 2

    var body: some View {
        VStack(alignment: .leading, spacing: 4) {
            heading
            let items = Self.items(
                strip, titles: titles, now: pinnedNow ?? Date(), locale: locale, timeZone: timeZone)
            VStack(alignment: .leading, spacing: 0) {
                stageStrip(items)
                // The bar's hit padding reaches up under the stage strip,
                // so the visible gap between them stays `stageGap`.
                bar(items)
                    .padding(.top, Self.stageGap - Self.hitPadding)
            }
            .padding(.top, 8)
            WrappingHStack(horizontalSpacing: 14, verticalSpacing: 4) {
                ForEach(Self.legendKinds(strip, locale: locale), id: \.self) { kind in
                    legendEntry(
                        swatch: RoundedRectangle(cornerRadius: 2).fill(RunTimeStripView.color(kind))
                            .frame(width: swatch, height: swatch),
                        label: RunTimeStripView.label(kind), total: strip.total(kind))
                }
                if Self.showsBetweenRuns(strip, locale: locale) {
                    legendEntry(
                        swatch: Capsule().fill(Color.ruleStrong)
                            .frame(width: swatch, height: Self.betweenRunsHeight)
                            .frame(height: swatch),
                        label: Self.betweenRunsLabel, total: strip.betweenRuns)
                }
            }
            .accessibilityHidden(true)
        }
    }

    private var heading: some View {
        HStack(alignment: .firstTextBaseline, spacing: 8) {
            KeywordLabel(text: "Elapsed")
            Text(RunTimeStripView.duration(strip.elapsed, locale: locale))
                .font(FreesideFont.trailingSummary)
                .foregroundStyle(Color.ink)
            Text(Self.runCount(strip.runCount))
                .font(FreesideFont.cardBody)
                .foregroundStyle(Color.inkDim)
        }
        // The legend says the same in words; the heading reads it once.
        .accessibilityElement(children: .ignore)
        .accessibilityLabel(Self.summary(strip, locale: locale))
        .accessibilityAddTraits(.isHeader)
    }

    /// The kinds the legend names, by the run strip's rule
    /// (`RunTimeStripView.legendKinds`).
    static func legendKinds(_ strip: TaskTimeStrip.Strip, locale: Locale) -> [RunTimeStrip.Kind] {
        RunTimeStripView.legendKinds(locale: locale, total: strip.total)
    }

    /// Whether the legend names `Between runs`, by the same rule: one run
    /// has no time between runs, so it names none.
    static func showsBetweenRuns(_ strip: TaskTimeStrip.Strip, locale: Locale) -> Bool {
        RunTimeStripView.showsTime(strip.betweenRuns, locale: locale)
    }

    /// One part of the bar as the reader meets it: its spoken label, and
    /// the run it opens, nil for time between runs.
    struct Item {
        let part: TaskTimeStrip.Part
        let label: String
        let opensRunID: String?
    }

    /// The bar's parts in order, each labelled with its run's title (or
    /// `Between runs`), its length, and its span.
    static func items(
        _ strip: TaskTimeStrip.Strip, titles: [String: String], now: Date, locale: Locale, timeZone: TimeZone
    ) -> [Item] {
        strip.parts.map { part in
            let name: String
            let opens: String?
            switch part {
            case .run(let run):
                name = titles[run.runID] ?? "Run \(ShortIdentifier.short(run.runID))"
                opens = run.runID
            case .betweenRuns:
                name = betweenRunsLabel
                opens = nil
            }
            let from = FreesideFormat.shortTime(part.start, now: now, locale: locale, timeZone: timeZone)
            let to = FreesideFormat.shortTime(part.end, now: now, locale: locale, timeZone: timeZone)
            let length = RunTimeStripView.duration(part.duration, locale: locale, width: .wide)
            return Item(part: part, label: "\(name), \(length), \(from) to \(to)", opensRunID: opens)
        }
    }

    /// A part's width on a line `total` wide, and the gap that opens it:
    /// a clear gap opens each later part, wider than the hairline between
    /// a run's own segments, so runs read apart; a part too thin to spare
    /// it keeps its fill. The stage strip and the bar both cut by it, so
    /// their edges meet.
    private func span(_ index: Int, _ part: TaskTimeStrip.Part, total: CGFloat) -> (width: CGFloat, gap: CGFloat) {
        let width = total * CGFloat(part.duration / strip.elapsed)
        return (width, index > 0 && width > 6 ? 3 : 0)
    }

    /// One block per run part in the page's raised ground, holding its
    /// run's stage name; time between runs has no block. Stages add no
    /// color: the bar's colors keep meaning kind of time. A block too
    /// narrow for its name keeps its width and drops the name. Each bar
    /// part already speaks its run's title, so the strip is not read.
    private func stageStrip(_ items: [Item]) -> some View {
        GeometryReader { proxy in
            HStack(spacing: 0) {
                ForEach(Array(items.enumerated()), id: \.offset) { index, item in
                    let span = span(index, item.part, total: proxy.size.width)
                    Group {
                        if case .run(let run) = item.part {
                            stageBlock(stages[run.runID], width: max(span.width - span.gap, 0))
                        } else {
                            Color.clear
                        }
                    }
                    .padding(.leading, span.gap)
                    .frame(width: span.width, alignment: .leading)
                }
            }
        }
        .frame(height: stageHeight)
        .accessibilityHidden(true)
    }

    private func stageBlock(_ name: String?, width: CGFloat) -> some View {
        RoundedRectangle(cornerRadius: 2)
            .fill(Color.ground3)
            .frame(width: width, height: stageHeight)
            .overlay(alignment: .leading) {
                if let name {
                    ViewThatFits(in: .horizontal) {
                        Text(name)
                            .font(FreesideFont.stageName)
                            .foregroundStyle(Color.ink)
                            .lineLimit(1)
                            .fixedSize()
                            .padding(.horizontal, 6)
                        Color.clear.frame(width: 0)
                    }
                    .frame(width: width, alignment: .leading)
                }
            }
    }

    private func bar(_ items: [Item]) -> some View {
        GeometryReader { proxy in
            HStack(spacing: 0) {
                ForEach(Array(items.enumerated()), id: \.offset) { index, item in
                    let (width, gap) = span(index, item.part, total: proxy.size.width)
                    if case .run(let run) = item.part, let runID = item.opensRunID {
                        Button {
                            onOpenRun?(runID)
                        } label: {
                            segments(run, width: width, gap: gap)
                        }
                        .buttonStyle(.plain)
                        .accessibilityLabel(item.label)
                        .accessibilityHint("Opens its run history.")
                        .help(item.label)
                    } else {
                        Capsule()
                            .fill(Color.ruleStrong)
                            .frame(height: Self.betweenRunsHeight)
                            .padding(.leading, gap)
                            .frame(width: width, height: Self.barHeight + 2 * Self.hitPadding)
                            .accessibilityElement()
                            .accessibilityLabel(item.label)
                            .help(item.label)
                    }
                }
            }
            .mask {
                RoundedRectangle(cornerRadius: 3).frame(height: Self.barHeight)
            }
        }
        .frame(height: Self.barHeight + 2 * Self.hitPadding)
    }

    /// One run's part: its segments in the run strip's colours.
    private func segments(_ run: TaskTimeStrip.RunPart, width: CGFloat, gap: CGFloat) -> some View {
        let fill = max(width - gap, 0)
        return HStack(spacing: 0) {
            ForEach(Array(run.segments.enumerated()), id: \.offset) { index, segment in
                let segmentWidth = fill * CGFloat(segment.duration / run.duration)
                Rectangle()
                    .fill(RunTimeStripView.color(segment.kind))
                    .padding(.leading, index > 0 && segmentWidth > 3 ? 1 : 0)
                    .frame(width: segmentWidth, height: Self.barHeight)
            }
        }
        .padding(.leading, gap)
        .frame(width: width, alignment: .leading)
        .padding(.vertical, Self.hitPadding)
        .contentShape(Rectangle())
    }

    private func legendEntry(swatch: some View, label: String, total: TimeInterval) -> some View {
        HStack(alignment: .firstTextBaseline, spacing: 6) {
            swatch
            Text(label)
                .font(FreesideFont.cardBody)
                .foregroundStyle(Color.inkDim)
            Text(RunTimeStripView.duration(total, locale: locale))
                .font(FreesideFont.trailingSummary)
                .foregroundStyle(Color.ink)
        }
    }

    static let betweenRunsLabel = "Between runs"

    static func runCount(_ count: Int) -> String {
        count == 1 ? "across 1 run" : "across \(count) runs"
    }

    /// The strip in one sentence for assistive technology: the elapsed
    /// time and run count, then each kind's total, between runs only when
    /// there is more than one run.
    static func summary(_ strip: TaskTimeStrip.Strip, locale: Locale) -> String {
        let kinds = RunTimeStrip.Kind.allCases.map { kind in
            (RunTimeStripView.label(kind), strip.total(kind))
        }
        let between = strip.runCount > 1 ? [(betweenRunsLabel, strip.betweenRuns)] : []
        let totals = (kinds + between).map { label, total in
            "\(label.lowercased()) \(RunTimeStripView.duration(total, locale: locale, width: .wide))"
        }
        return "Elapsed \(RunTimeStripView.duration(strip.elapsed, locale: locale, width: .wide)) "
            + "\(runCount(strip.runCount)): " + totals.joined(separator: ", ") + "."
    }
}
