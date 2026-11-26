#if os(macOS)
    import AppKit
    import FreesideAPI
    import FreesideModel
    import SwiftUI
    import Testing

    @testable import FreesideCore

    /// The Mac popover with a usage meter on every enrollment (#3675): it
    /// keeps its height cap, its sections scroll inside it, and the whole
    /// Enrollment section shows on open, so the chosen row needs no scroll.
    /// The Mac fonts are fixed-size, so one text size covers the Mac.
    @Suite(.serialized) @MainActor struct RolePickerLayoutTests {
        @Test func threeUsedUpEnrollmentsFitAboveTheFoldInsideTheCap() throws {
            // All three enrollments on the used-up pool, the last one staged.
            var pools = UsageFixtures.defaultUsagePools()
            pools[0].pool.enrollments.append(
                .init(
                    enrollment_id: UsageFixtures.unsupportedCollectorID, auth_identity_id: "identity-codex-b",
                    harness_client: .codex_cli))
            pools.remove(at: 1)
            let catalog = UsageFixtures.defaultAgentCatalog()
            #expect(catalog.enrollments.count == 3)
            let now = Date(timeIntervalSince1970: 1_786_502_400)
            for enrollment in catalog.enrollments {
                guard
                    case .window(let capacity, _) = AgentChoiceDisplay.usage(
                        for: enrollment.enrollment_id, pools: pools, now: now)
                else {
                    Issue.record("\(enrollment.enrollment_id) reads no window")
                    continue
                }
                #expect(capacity.level == .usedUp && capacity.left == 0)
            }
            var picker = RolePicker.draft(role: .implementer, chosen: nil, catalog: catalog)
            picker.pick(.enrollment(UsageFixtures.unsupportedCollectorID), catalog: catalog)
            var view = RolePickerView(
                picker: picker, catalog: catalog, policy: UsageFixtures.defaultRoutingPolicy(), pools: pools,
                now: now, onConfirm: { _ in }, onCancel: {})

            _ = FreesideFont.registration
            // Unscrolled, the picker is taller than the cap, so the cap and
            // the scroll are what this test checks.
            let unscrolled = NSHostingView(
                rootView: view.content.frame(width: RolePickerLayout.popoverWidth).pinnedTimeFormat())
            #expect(unscrolled.fittingSize.height > RolePickerLayout.popoverMaxHeight)

            final class Measurement: @unchecked Sendable {
                var frames: [RolePickerLayout.Part: CGRect] = [:]
            }
            let measurement = Measurement()
            view.layoutReport = { measurement.frames[$0] = $1 }
            // The popover's own container (`rolePickerContainer`).
            let host = NSHostingView(
                rootView: view.frame(width: RolePickerLayout.popoverWidth)
                    .frame(maxHeight: RolePickerLayout.popoverMaxHeight).pinnedTimeFormat())
            host.frame = CGRect(origin: .zero, size: host.fittingSize)
            #expect(host.frame.height <= RolePickerLayout.popoverMaxHeight)
            host.layoutSubtreeIfNeeded()

            let (enrollment, scroll) = try settledFrames(host, measurement: { measurement.frames })
            // Measured 2026-11-26: the section ends at 527pt and the scroll
            // view at 531pt, inside a 640pt popover whose sections run 809pt
            // unscrolled. Four points spare: a taller row breaks this first.
            #expect(enrollment.maxY <= scroll.maxY, "the Enrollment section ends below the fold")
        }

        /// Pumps the run loop until both frames have reported and held still
        /// for a settle window, as `DecisionLayoutBudgetTests` does: the
        /// geometry callbacks land on later main-queue turns and can report
        /// intermediate frames while the layout settles.
        private func settledFrames(
            _ host: NSView, measurement: () -> [RolePickerLayout.Part: CGRect]
        ) throws -> (enrollment: CGRect, scroll: CGRect) {
            let deadline = Date().addingTimeInterval(5)
            let settleWindow: TimeInterval = 0.25
            var last: [RolePickerLayout.Part: CGRect] = [:]
            var unchangedSince: Date?
            while Date() < deadline {
                RunLoop.main.run(until: Date().addingTimeInterval(0.01))
                host.layoutSubtreeIfNeeded()
                let observed = measurement()
                if observed != last {
                    last = observed
                    unchangedSince = observed.count == 2 ? Date() : nil
                    continue
                }
                if let unchangedSince, Date().timeIntervalSince(unchangedSince) >= settleWindow { break }
            }
            let enrollment = try #require(last[.enrollment], "the Enrollment section never reported a frame")
            let scroll = try #require(last[.scroll], "the scroll view never reported a frame")
            return (enrollment, scroll)
        }
    }

    extension View {
        /// Pins the readings' words, whose length the fold depends on, to
        /// the model tests' locale and zone rather than the host's.
        fileprivate func pinnedTimeFormat() -> some View {
            environment(\.locale, Locale(identifier: "en_US_POSIX"))
                .environment(\.timeZone, TimeZone(identifier: "UTC")!)
        }
    }
#endif
