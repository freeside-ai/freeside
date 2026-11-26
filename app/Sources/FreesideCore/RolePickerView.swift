import FreesideAPI
import FreesideModel
import SwiftUI

/// One role's agent picker (#3027), the same view on both clients and from
/// both entry points: New Task's role chips and the task page's Change
/// Agent. Enrollment, Model, and Effort scroll together; the footer under
/// them stays put and names the tuple the confirm writes.
///
/// A pick only stages (`RolePicker`). The view reports through `onConfirm`,
/// with the selection to write, and `onCancel`, and through nothing else, so
/// nothing reaches a draft or the daemon until the operator confirms. Its
/// rows keep the order they opened in while a sync changes the catalog.
struct RolePickerView: View {
    typealias Selection = AgentChoiceDisplay.Selection

    let catalog: AgentChoiceDisplay.Catalog?
    let policy: Components.Schemas.RoutingPolicy?
    let pools: [Components.Schemas.UsagePoolSnapshot]
    /// A running task's live line for the role, read on every draw so the
    /// footer's `Now` and the confirm follow a line that changes while the
    /// picker is open. Nil in New Task.
    var liveMode: RolePicker.Mode? = nil
    /// Why the client can't send a change now (stale sync, another change
    /// waiting), which turns the confirm off with the reason shown.
    var unavailableReason: String? = nil
    let onConfirm: (Selection?) -> Void
    let onCancel: () -> Void

    /// The clock the readings and discovery note are read against; nil
    /// reads the system clock at each draw. The goldens pin it here, since
    /// `content` is drawn outside a view graph that would carry an
    /// environment value.
    var now: Date? = nil

    /// Reports where the Enrollment section and the scroll view end, in the
    /// picker's own space, for the popover layout test (#3675); nil reports
    /// nothing.
    var layoutReport: ((RolePickerLayout.Part, CGRect) -> Void)? = nil

    @State private var picker: RolePicker

    init(
        picker: RolePicker, catalog: AgentChoiceDisplay.Catalog?, policy: Components.Schemas.RoutingPolicy?,
        pools: [Components.Schemas.UsagePoolSnapshot], liveMode: RolePicker.Mode? = nil,
        unavailableReason: String? = nil, now: Date? = nil, onConfirm: @escaping (Selection?) -> Void,
        onCancel: @escaping () -> Void
    ) {
        _picker = State(initialValue: picker)
        self.catalog = catalog
        self.policy = policy
        self.pools = pools
        self.liveMode = liveMode
        self.unavailableReason = unavailableReason
        self.now = now
        self.onConfirm = onConfirm
        self.onCancel = onCancel
    }

    /// The staged picker against the live catalog and line. `absorb` also
    /// runs on a catalog change, which keeps a dropped part's slot across
    /// later syncs; here it covers the draw before that change lands.
    private var current: RolePicker {
        var current = picker
        current.absorb(catalog: catalog)
        if let liveMode { current.mode = liveMode }
        return current
    }

    private var title: String { AgentChoiceDisplay.title(for: picker.role) }
    private var isDraft: Bool { picker.mode == .draft }

    var body: some View {
        VStack(spacing: 0) {
            header
            ScrollView { sections }
                .onGeometryChange(for: CGRect.self) {
                    $0.frame(in: .named(RolePickerLayout.space))
                } action: {
                    layoutReport?(.scroll, $0)
                }
            footer
        }
        .coordinateSpace(.named(RolePickerLayout.space))
        .background(Color.ground2)
        .onChange(of: catalog) { picker.absorb(catalog: catalog) }
    }

    /// The picker laid out in place of its scroll view, for the screenshot
    /// suite.
    var content: some View {
        VStack(spacing: 0) {
            header
            sections
            footer
        }
        .background(Color.ground2)
    }

    private var header: some View {
        FreesideSheetHeader(eyebrow: isDraft ? "Agent" : "Change agent", ask: title, askLineLimit: nil)
    }

    /// Readings and the discovery note format in the locale and zone
    /// installed where the picker draws, which a pinned screenshot sets.
    private var sections: some View {
        TimeFormatEnvironment { locale, timeZone in
            sections(locale: locale, timeZone: timeZone)
        }
    }

    private func sections(locale: Locale, timeZone: TimeZone) -> some View {
        let current = current
        let now = now ?? Date()
        return VStack(alignment: .leading, spacing: 18) {
            section("Enrollment") {
                ChoiceList(
                    accessibilityLabel: "\(title) enrollment",
                    options: current.enrollmentRows(
                        catalog: catalog, policy: policy, pools: pools, now: now, locale: locale, timeZone: timeZone
                    ).map { row in
                        .init(
                            value: row.id, label: row.label, consequence: row.detail, register: .item,
                            isEnabled: row.isEnabled,
                            detail: row.usage.map {
                                AnyView(EnrollmentUsageLine(usage: $0, isEnabled: row.isEnabled))
                            },
                            accessibilityValue: row.usage?.spoken)
                    },
                    // A closure, not the method itself: Swift 6.3's IR
                    // generation crashes on the thunk that converts the
                    // isolated method to the binding's setter.
                    selection: Binding(get: { current.chosenRow }, set: { choose($0) }))
            }
            .onGeometryChange(for: CGRect.self) {
                $0.frame(in: .named(RolePickerLayout.space))
            } action: {
                layoutReport?(.enrollment, $0)
            }
            section("Model") {
                pills(current.modelPills(catalog: catalog), named: "model") {
                    picker.pick(.model($0), catalog: catalog)
                }
            }
            section("Effort") {
                pills(current.effortPills(catalog: catalog), named: "effort") {
                    picker.pick(.effort($0), catalog: catalog)
                }
            }
            if let enrollment = current.shownEnrollment(catalog: catalog),
                let note = AgentChoiceDisplay.discoveryNote(
                    on: enrollment, catalog: catalog, now: now, locale: locale, timeZone: timeZone)
            {
                Text(note)
                    .font(FreesideFont.cardBody)
                    .foregroundStyle(Color.inkDim)
                    .fixedSize(horizontal: false, vertical: true)
            }
        }
        .frame(maxWidth: .infinity, alignment: .leading)
        .padding(.horizontal, 16)
        .padding(.bottom, 16)
    }

    private func choose(_ row: RolePicker.EnrollmentRow.ID) {
        switch row {
        case .lineup: picker.pick(.enrollment(nil), catalog: catalog)
        case .byName: break
        case .enrollment(let id): picker.pick(.enrollment(id), catalog: catalog)
        }
    }

    private func section(_ keyword: String, @ViewBuilder content: () -> some View) -> some View {
        VStack(alignment: .leading, spacing: 8) {
            KeywordLabel(text: keyword)
                .accessibilityAddTraits(.isHeader)
            content()
        }
    }

    /// A row of pills, or, while nothing is staged that has any, a dim line
    /// saying where they come from, so the section keeps its place.
    @ViewBuilder
    private func pills(
        _ pills: [RolePicker.Pill], named noun: String, pick: @escaping (String) -> Void
    ) -> some View {
        if pills.isEmpty {
            Text("Choose an enrollment to choose its \(noun).")
                .font(FreesideFont.cardBody)
                .foregroundStyle(Color.inkDim)
        } else {
            WrappingHStack(horizontalSpacing: 6, verticalSpacing: 6) {
                ForEach(pills) { pill in
                    RolePickerPill(pill: pill) { pick(pill.id) }
                }
            }
            .accessibilityElement(children: .contain)
            .accessibilityLabel("\(title) \(noun)")
        }
    }

    private var footer: some View {
        let current = current
        let confirmation = current.confirmation(catalog: catalog)
        let reason: String? =
            switch confirmation {
            case .ready: unavailableReason
            case .blocked(let reason): reason
            }
        return VStack(alignment: .leading, spacing: 0) {
            Divider()
            VStack(alignment: .leading, spacing: 4) {
                ForEach(current.footer(catalog: catalog, policy: policy), id: \.keyword) { line in
                    ViewThatFits(in: .horizontal) {
                        HStack(alignment: .firstTextBaseline, spacing: 8) {
                            KeywordLabel(text: line.keyword)
                            footerText(line.text)
                        }
                        VStack(alignment: .leading, spacing: 2) {
                            KeywordLabel(text: line.keyword)
                            footerText(line.text)
                        }
                    }
                    .accessibilityElement(children: .combine)
                }
                if let reason {
                    Text(reason)
                        .font(FreesideFont.cardBody)
                        .foregroundStyle(Color.inkDim)
                        .fixedSize(horizontal: false, vertical: true)
                }
            }
            .frame(maxWidth: .infinity, alignment: .leading)
            .padding(.horizontal, 16)
            .padding(.vertical, 12)
            FreesideSheetActionRow(
                submitLabel: isDraft ? "Done" : "Change \(title)",
                isSubmitEnabled: reason == nil,
                submit: {
                    if case .ready(let selection) = confirmation, unavailableReason == nil { onConfirm(selection) }
                },
                cancel: onCancel)
        }
        .background(Color.ground2)
    }

    private func footerText(_ text: String) -> some View {
        Text(text)
            .font(FreesideFont.trailingSummary)
            .foregroundStyle(Color.ink)
            .fixedSize(horizontal: false, vertical: true)
    }
}

/// An enrollment row's usage line (#3675): the tightest window's name
/// leading and its reading trailing, the Usage pane's meter under them
/// where the reading has a percent, then the reset and read time on a
/// faint mono line. With no window reading it draws the provider's
/// sentence or the collector's last attempt alone. Hidden from VoiceOver:
/// the row speaks `usage.spoken` as its value.
private struct EnrollmentUsageLine: View {
    let usage: AgentChoiceDisplay.EnrollmentUsage
    let isEnabled: Bool

    var body: some View {
        VStack(alignment: .leading, spacing: 5) {
            switch usage {
            case .window(let capacity, _):
                ViewThatFits(in: .horizontal) {
                    HStack(alignment: .firstTextBaseline, spacing: 12) {
                        name(capacity)
                        Spacer(minLength: 0)
                        value(capacity)
                    }
                    VStack(alignment: .leading, spacing: 3) {
                        name(capacity)
                        value(capacity)
                    }
                }
                if let left = capacity.left {
                    CapacityMeter(left: left, level: capacity.level)
                }
            case .unsupported:
                Text(AgentChoiceDisplay.EnrollmentUsage.unsupportedSentence)
                    .font(FreesideFont.cardBody)
                    .foregroundStyle(isEnabled ? Color.inkDim : Color.inkFaint)
                    .fixedSize(horizontal: false, vertical: true)
            case .attempt:
                EmptyView()
            }
            if let timeLine = usage.timeLine {
                Text(timeLine)
                    .font(FreesideFont.trailingSummary)
                    .foregroundStyle(Color.inkFaint)
                    .fixedSize(horizontal: false, vertical: true)
            }
        }
        .padding(.top, 3)
        .frame(maxWidth: .infinity, alignment: .leading)
        .accessibilityHidden(true)
    }

    private func name(_ capacity: UsageDisplay.WindowCapacity) -> some View {
        Text(capacity.name)
            .font(capacity.nameIsWindowID ? FreesideFont.monoValue : FreesideFont.factLabel)
            .foregroundStyle(isEnabled ? Color.inkDim : Color.inkFaint)
    }

    /// The reading in `CapacityBlock`'s colours, so the colour always
    /// carries a word; used up also reads semibold.
    private func value(_ capacity: UsageDisplay.WindowCapacity) -> some View {
        Text(capacity.value)
            .font(capacity.level == .usedUp ? FreesideFont.strongMonoValue : FreesideFont.monoValue)
            .foregroundStyle(isEnabled ? valueColor(capacity.level) : Color.inkFaint)
    }

    private func valueColor(_ level: UsageDisplay.WindowCapacity.Level) -> Color {
        switch level {
        case .low, .usedUp: .waxText
        case .noReading: .inkDim
        case .fine: .ink
        }
    }
}

/// A Model or Effort pill: R33's pill, marked with the accent while it is
/// the pick, its text faint while it can't be picked. On the phone it
/// takes a 44pt touch height around the drawn pill.
private struct RolePickerPill: View {
    let pill: RolePicker.Pill
    let pick: () -> Void

    var body: some View {
        Button(action: pick) {
            Text(pill.label)
                .font(FreesideFont.trailingSummary)
                .foregroundStyle(pill.isEnabled ? Color.ink : Color.inkFaint)
                .padding(.vertical, 3)
                .padding(.horizontal, 10)
                .composerPill(isActive: pill.isChosen)
                #if os(iOS)
                    .frame(minHeight: 44)
                #endif
                .contentShape(Rectangle())
        }
        .buttonStyle(.plain)
        .disabled(!pill.isEnabled)
        .accessibilityAddTraits(pill.isChosen ? .isSelected : [])
    }
}

extension View {
    /// Presents a role picker: a popover on the Mac, anchored on the view
    /// this modifies, and a full sheet on the iPhone. The iPhone gets a
    /// sheet outright rather than a popover that adapts, since a wide
    /// iPhone in landscape would keep the popover. A dismissal (Escape, a
    /// click outside, a swipe down) only sets `isPresented` false; the
    /// picker's own confirm is the one thing that writes.
    func rolePickerPresentation(
        isPresented: Binding<Bool>, @ViewBuilder picker: @escaping () -> some View
    ) -> some View {
        #if os(macOS)
            popover(isPresented: isPresented, arrowEdge: .bottom) { picker().rolePickerContainer() }
        #else
            sheet(isPresented: isPresented) { picker().rolePickerContainer() }
        #endif
    }

    /// The same, presented while `item` holds a value, which the picker
    /// keeps while it closes: a picker whose content reads the value that
    /// opened it uses this, so it doesn't draw empty on its way out.
    func rolePickerPresentation<Item: Identifiable>(
        item: Binding<Item?>, @ViewBuilder picker: @escaping (Item) -> some View
    ) -> some View {
        #if os(macOS)
            popover(item: item, arrowEdge: .bottom) { picker($0).rolePickerContainer() }
        #else
            sheet(item: item) { picker($0).rolePickerContainer() }
        #endif
    }

    private func rolePickerContainer() -> some View {
        #if os(macOS)
            frame(width: RolePickerLayout.popoverWidth).frame(maxHeight: RolePickerLayout.popoverMaxHeight)
        #else
            presentationDetents([.large]).freesideSheetPresentation()
        #endif
    }
}

enum RolePickerLayout {
    /// The Mac popover's width, which the goldens draw too.
    static let popoverWidth: CGFloat = 420
    /// The Mac popover's height cap; past it the sections scroll between
    /// the fixed header and footer.
    static let popoverMaxHeight: CGFloat = 640

    /// The parts `RolePickerView.layoutReport` measures.
    enum Part {
        case enrollment
        case scroll
    }

    /// The picker's own coordinate space, which `layoutReport` measures in.
    static let space = "rolePicker"
}
