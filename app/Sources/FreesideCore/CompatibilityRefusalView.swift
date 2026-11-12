import FreesideAPI
import SwiftUI

/// The words every incompatibility surface shares (#3109): the refusal
/// screen that replaces the inbox, the freshness banner, and the Mac
/// menu-bar panel, so the three read alike. The update instruction names
/// today's installers and nothing more; how an update reaches the app is
/// #2939's.
struct CompatibilityWords {
    let incompatibility: DaemonIncompatibility
    /// The platform the instruction is for. The Mac updates the app and its
    /// bundled daemon in one install; an iPhone installs from the Mac.
    var platform = Platform.current
    /// This app's level, a parameter so a screenshot fixture stays the same
    /// when the spec's level rises.
    var appLevel = APIContract.compatibilityLevel

    enum Platform {
        case mac
        case phone

        static var current: Platform {
            #if os(macOS)
                .mac
            #else
                .phone
            #endif
        }
    }

    var title: String {
        switch incompatibility {
        case .appBelowFloor: "This app is too old for this daemon"
        case .daemonBelowLevel: "This daemon is too old for this app"
        }
    }

    /// One sentence: what stopped and who updates.
    var sentence: String {
        switch incompatibility {
        case .appBelowFloor:
            "Nothing syncs and no action is sent until the app is updated."
        case .daemonBelowLevel:
            "Nothing syncs and no action is sent until the daemon is updated."
        }
    }

    /// Where the instruction runs, ahead of the command.
    var instructionLead: String {
        switch (incompatibility, platform) {
        case (.appBelowFloor, .mac): "Reinstall the app from a current checkout:"
        case (.appBelowFloor, .phone): "On the Mac, from a current checkout:"
        case (.daemonBelowLevel, _): "On the daemon's Mac, from a current checkout:"
        }
    }

    /// The installer command, drawn in mono.
    var command: String {
        switch (incompatibility, platform) {
        case (.appBelowFloor, .phone): "app/scripts/install-ios-app.sh --device '<name>'"
        case (.appBelowFloor, .mac), (.daemonBelowLevel, _):
            "app/scripts/install-mac-app.sh --daemon-path <freesided>"
        }
    }

    /// The levels behind the verdict and the daemon's build, drawn dim.
    var levels: String {
        let app = "app level \(appLevel)"
        return switch incompatibility {
        case .appBelowFloor(let floor, let version): "\(app) · daemon \(version) serves level \(floor) and up"
        case .daemonBelowLevel(let level, let version): "\(app) · daemon \(version) is at level \(level)"
        }
    }

    /// The banner and the menu panel's one-line form.
    var notice: String { "\(title). \(sentence)" }
}

/// Drawn in place of the whole inbox while the daemon and this app can't
/// talk (`InboxStore.Freshness.incompatible`). The cache stays on disk but
/// isn't shown: one screen, so nothing reads as current or actionable.
/// Sync resumes, and the inbox returns, on the first round whose health
/// read finds the two compatible again; no relaunch.
struct CompatibilityRefusalView: View {
    let incompatibility: DaemonIncompatibility
    var platform = CompatibilityWords.Platform.current
    var appLevel = APIContract.compatibilityLevel

    var body: some View {
        let words = CompatibilityWords(incompatibility: incompatibility, platform: platform, appLevel: appLevel)
        VStack(spacing: 6) {
            EmptyStateBlock(glyph: .incompatible, title: words.title, description: words.sentence)
            VStack(spacing: 4) {
                Text(words.instructionLead)
                    .font(FreesideFont.cardBody)
                    .foregroundStyle(Color.inkDim)
                Text(words.command)
                    .font(FreesideFont.monoValue)
                    .foregroundStyle(Color.ink)
            }
            .padding(.top, 10)
            Text(words.levels)
                .font(FreesideFont.trailingSummary)
                .foregroundStyle(Color.inkFaint)
                .padding(.top, 10)
        }
        .multilineTextAlignment(.center)
        .emptyStateSeat(.pane)
    }
}
