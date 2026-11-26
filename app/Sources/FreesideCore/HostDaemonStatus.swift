import Foundation
import SwiftUI

/// Host-side status of the Mac's supervised daemon, passed through the
/// environment so the shared list view can explain why a first load hasn't
/// landed without coupling itself to `DaemonMenuModel`. Nil on iPhone, on
/// ephemeral Mac runs that supervise no LaunchAgent, and when the session is
/// connected to a deployment other than the local supervised daemon.
public struct HostDaemonStatus: Equatable, Sendable {
    public let headline: String
    public let failure: DaemonStartFailure?

    public init(headline: String, failure: DaemonStartFailure? = nil) {
        self.headline = headline
        self.failure = failure
    }
}

extension EnvironmentValues {
    /// Host-side LaunchAgent status for the Mac's supervised daemon when the
    /// session is connected to it; nil on iPhone, on unsupervised tiers, and
    /// when connected to another deployment.
    @Entry public var hostDaemonStatus: HostDaemonStatus? = nil
}

#if os(macOS)
    extension HostDaemonStatus {
        /// Maps the supervised daemon's polled menu state and latest start
        /// error into the host status shown in the list column. Returns nil
        /// while the daemon is running, still checking, or unsupervised.
        public init?(state: DaemonMenuState, startFailure: DaemonStartFailure?) {
            let headline: String
            let failure: DaemonStartFailure?
            switch state {
            case .stopped:
                headline = "The daemon is stopped"
                failure = startFailure
            case .needsApproval:
                headline = "The daemon is waiting for Login Items approval"
                failure = nil
            case .unavailable:
                headline = "The daemon's LaunchAgent is unavailable"
                failure = nil
            case .unreachable:
                headline = "The daemon isn't answering"
                failure = startFailure
            case .running, .checking, .unsupervised:
                return nil
            }
            self.init(headline: headline, failure: failure)
        }

        /// Host status for `connectedURL` only when it names the same
        /// deployment as `localDaemonURL`; nil otherwise, so a session talking
        /// to another daemon never shows the local LaunchAgent's state or log.
        /// A nil `connectedURL` (not yet connected, mock, or pairing demo)
        /// returns nil because there is no connected deployment whose failure
        /// the local LaunchAgent can explain.
        public static func forConnectedDeployment(
            state: DaemonMenuState,
            startFailure: DaemonStartFailure?,
            connectedURL: URL?,
            localDaemonURL: URL
        ) -> HostDaemonStatus? {
            guard let connectedURL,
                AppSession.deploymentKey(for: connectedURL)
                    == AppSession.deploymentKey(for: localDaemonURL)
            else {
                return nil
            }
            return HostDaemonStatus(state: state, startFailure: startFailure)
        }
    }
#endif
