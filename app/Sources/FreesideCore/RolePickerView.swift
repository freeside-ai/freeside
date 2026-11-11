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
            footer
        }
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
                    ).map {
                        .init(
                            value: $0.id, label: $0.label, consequence: $0.detail, register: .item,
                            isEnabled: $0.isEnabled)
                    },
                    // A closure, not the method itself: Swift 6.3's IR
                    // generation crashes on the thunk that converts the
                    // isolated method to the binding's setter.
                    selection: Binding(get: { current.chosenRow }, set: { choose($0) }))
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
            popover(isPresented: isPresented, arrowEdge: .bottom) {
                picker().frame(width: RolePickerLayout.popoverWidth).frame(maxHeight: 640)
            }
        #else
            sheet(isPresented: isPresented) {
                picker()
                    .presentationDetents([.large])
                    .freesideSheetPresentation()
            }
        #endif
    }
}

enum RolePickerLayout {
    /// The Mac popover's width, which the goldens draw too.
    static let popoverWidth: CGFloat = 420
}
