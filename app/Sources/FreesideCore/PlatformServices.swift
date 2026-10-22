import Foundation
import Network

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
