import CryptoKit
import Foundation
import FreesideAPI

/// Sanitized reconstruction of the long completion-report shape from #1378,
/// not captured production content. A material concern ends both variants.
enum DecisionSummaryFixtures {
    static let change =
        "The upload command now keeps image captions with their files, so a reviewer can tell which screenshot shows each state. The change preserves the existing upload destination and command interface."
    static let concern =
        "The final upload still needs a human check on a real account. A reviewer disagrees about caption ordering, and that question remains unresolved. Do not treat the local fixture run as proof of production upload behavior."
    static let details = Array(
        repeating:
            "The implementation follows the existing command path and keeps the preparation step separate from publication. Local fixtures exercise captions, ordering, and missing inputs. The retained result report lists the commands and their observed outputs. This reconstruction includes supporting methodology to reproduce the original card's long-report shape without copying private run data. It does not add a new service or change credentials, account selection, storage, or the destination of uploaded files. Those operations remain outside this focused change.",
        count: 6
    ).joined(separator: "\n\n")
    static let structured = "## Change\n\n\(change)\n\n## Details\n\n\(details)\n\n## Remaining concerns\n\n\(concern)"
    static let legacy = "\(change)\n\n\(details)\n\n\(concern)"

    static func snapshot(content: String?, plain: Bool = false) -> Components.Schemas.AttentionItemSnapshot {
        var snapshot = AttentionFixtures.fixture(type: .ready_for_final_review)
        guard let index = snapshot.item.agent_claims.firstIndex(where: { $0.label == "freeside.summary" }) else {
            preconditionFailure("Ready fixture needs a summary claim")
        }
        let oldDigest = snapshot.item.agent_claims[index].digest
        snapshot.item.agent_claims[index].digest =
            "sha256:"
            + SHA256.hash(data: Data((content ?? legacy).utf8)).map {
                String(format: "%02x", $0)
            }.joined()
        snapshot.item.agent_claims[index].text = content.map {
            .init(media_type: plain ? .text_sol_plain : .text_sol_markdown, content: $0)
        }
        snapshot.item.agent_claims[index].metadata.size_bytes = Int64((content ?? legacy).utf8.count)
        snapshot.item.agent_claims[index].metadata.media_type = plain ? .text_sol_plain : .text_sol_markdown
        snapshot.item.artifact_digests = snapshot.item.artifact_digests.map {
            $0 == oldDigest ? snapshot.item.agent_claims[index].digest : $0
        }.sorted()
        return snapshot
    }
}
