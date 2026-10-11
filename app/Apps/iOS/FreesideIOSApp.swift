import FreesideCore
import SwiftUI
import UIKit

@main
struct FreesideIOSApp: App {
    @State private var session: AppSession
    @State private var navigation: NavigationModel
    @State private var flowPreferences = DecisionFlowPreferences()
    private let launchInputs: LaunchInputs

    init() {
        let launchInputs = LaunchInputs.standard()
        self.launchInputs = launchInputs
        _session = State(initialValue: .fromEnvironment())
        _navigation = State(initialValue: NavigationModel(launchInputs: launchInputs))
    }

    var body: some Scene {
        WindowGroup {
            FreesideRootView(
                session: session, launchInputs: launchInputs, navigation: navigation,
                flowPreferences: flowPreferences
            )
            .background(
                AccessibilityContrastOverride(contrast: launchInputs.contrast)
                    .frame(width: 0, height: 0)
            )
            // A notification's tap link (`NotificationLink`). On a cold
            // start this fires while the inbox is still loading; the route
            // it sets waits in the navigation model until the list arrives.
            .onOpenURL { url in
                guard let link = NotificationLink(url) else { return }
                Task { await session.open(link, in: navigation) }
            }
        }
    }
}

private struct AccessibilityContrastOverride: UIViewRepresentable {
    let contrast: LaunchInputs.Contrast?

    func makeUIView(context: Context) -> ContrastView {
        ContrastView(contrast: contrast)
    }

    func updateUIView(_ view: ContrastView, context: Context) {
        view.contrast = contrast
    }

    final class ContrastView: UIView {
        var contrast: LaunchInputs.Contrast? {
            didSet { applyOverride() }
        }

        init(contrast: LaunchInputs.Contrast?) {
            self.contrast = contrast
            super.init(frame: .zero)
            isHidden = true
        }

        @available(*, unavailable)
        required init?(coder: NSCoder) {
            fatalError("init(coder:) is unavailable")
        }

        override func didMoveToWindow() {
            super.didMoveToWindow()
            applyOverride()
        }

        private func applyOverride() {
            window?.traitOverrides.accessibilityContrast =
                switch contrast {
                case .increased: .high
                case .standard: .normal
                case nil: .unspecified
                }
        }
    }
}
