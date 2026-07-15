import FreesideCore
import SwiftUI

@main
struct FreesideMacApp: App {
    var body: some Scene {
        WindowGroup {
            FreesideRootView()
        }
        .defaultSize(width: 480, height: 320)
    }
}
