import FreesideCore
import SwiftUI

@main
struct FreesideMacApp: App {
    var body: some Scene {
        WindowGroup {
            FreesideRootView()
        }
        .defaultSize(width: 960, height: 640)
    }
}
