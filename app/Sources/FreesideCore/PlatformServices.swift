import Foundation
import Network
import SwiftUI

#if canImport(AppKit)
    import AppKit
#elseif canImport(UIKit)
    import UIKit
#endif

// The Apple platform services the portable models take as parameters,
// supplied here as their defaults so call sites compile unchanged.

extension SyncCoordinator {
    /// Watches the network path with `NWPathMonitor`.
    public func startReachabilityMonitoring() {
        startReachabilityMonitoring { report in
            let monitor = NWPathMonitor()
            monitor.pathUpdateHandler = { path in report(path.status == .satisfied) }
            monitor.start(queue: DispatchQueue(label: "ai.freeside.reachability"))
            return { monitor.cancel() }
        }
    }
}

extension DecisionModel {
    public convenience init(store: InboxStore, itemID: String) {
        self.init(store: store, itemID: itemID, openURL: Self.openExternalURL)
    }

    convenience init(
        store: InboxStore,
        itemID: String,
        onConclusion: @escaping @MainActor (DecisionConclusion) -> Void
    ) {
        self.init(store: store, itemID: itemID, openURL: Self.openExternalURL, onConclusion: onConclusion)
    }

    private static func openExternalURL(_ url: URL) async -> Bool {
        #if canImport(AppKit)
            return NSWorkspace.shared.open(url)
        #elseif canImport(UIKit)
            guard UIApplication.shared.canOpenURL(url) else { return false }
            return await withCheckedContinuation { continuation in
                UIApplication.shared.open(url, options: [:]) { opened in
                    continuation.resume(returning: opened)
                }
            }
        #endif
    }
}

extension DecisionFeedbackModel {
    convenience init() {
        self.init(announce: AccessibilityAnnouncer.announce)
    }
}

private enum AccessibilityAnnouncer {
    @MainActor
    static func announce(_ message: String) {
        AccessibilityNotification.Announcement(message).post()
    }
}
