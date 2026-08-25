import FreesideCore
import SwiftUI
import UIKit

@main
struct FreesideIOSApp: App {
    private let launchInputs = LaunchInputs.standard()

    var body: some Scene {
        WindowGroup {
            FreesideRootView(session: .fromEnvironment(), launchInputs: launchInputs)
                .background(
                    AccessibilityContrastOverride(contrast: launchInputs.contrast)
                        .frame(width: 0, height: 0))
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
