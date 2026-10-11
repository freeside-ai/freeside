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

    /// The writer's convention as an agent hard-wraps it (#1461): a
    /// paragraph broken mid-sentence, and concerns as a list whose first
    /// item is the one the Wave 8 ready card buried in a paragraph (#1929).
    static let wrappedLastConcern = "The fixture covers a file exactly at the limit but not one byte over."
    static let wrapped = """
        ## Change

        The upload command now refuses an oversized image before reading it,
        with a message that names the limit and the file. The limit comes from
        the existing configuration file.

        ## Remaining concerns

        - The project's own lint and format commands could not be run in the
          workspace, so style conformance is **unchecked**.
        - \(wrappedLastConcern)
        """

    /// Sanitized reconstruction of the one-paragraph specification summary
    /// the Wave 8 exit run showed at the approval gate (#1461), not captured
    /// production content. Its shape is the point: a root cause, a
    /// recommended design choice, and a caveat in one unbroken paragraph,
    /// with the open questions after the first 800 characters.
    static let specificationLastConcern =
        "A reviewer of the source issue also asked whether animated images should be exempt, and this specification does not decide that."
    static let specificationLegacy =
        "The upload command fails on images over the host's size limit because it reads the whole file before checking its size, so the user sees a timeout instead of a clear refusal. This specification proposes checking the file size from its metadata before any bytes are read and refusing an oversized image with a message that names the limit and the file. The recommended design keeps the limit in the existing configuration file rather than adding a flag, because every other bound the command enforces already lives there and a flag would give two sources for one number. The check runs once per file, before the caption step, so a batch stops at the first oversized image and uploads nothing; the alternative, skipping the oversized file and continuing, was rejected because a partial batch leaves the pull request with captions that point at missing images. The specification also covers the error text, the exit status, and a fixture for a file exactly at the limit. One caveat remains open: the host's documented limit and the limit observed in the failing run differ, and nothing in the supplied evidence says which one the host enforces, so the specification uses the documented value and names the observed one as an assumption the implementer must not treat as settled. \(specificationLastConcern)"

    /// The same specification summary under the headings the specifier
    /// prompt now asks for, hard-wrapped, with a material last concern.
    static let specificationStructuredLastConcern =
        "The host's documented limit and the limit observed in the failing run differ. The specification uses the documented value as an assumption."
    static let specificationStructured = """
        ## Change

        Refuse an oversized image before reading it. The upload command checks
        each file's size from its metadata and stops the batch with a message
        naming the limit and the file, so the user gets a clear refusal instead
        of a timeout. The limit stays in the existing configuration file.

        ## Remaining concerns

        - The batch stops at the first oversized image. Skipping it instead was
          rejected, and may surprise users with large batches.
        - Whether animated images should be exempt is **not decided**.
        - \(specificationStructuredLastConcern)
        """

    /// A card of `type` whose summary claim carries `content`. A
    /// specification approval's reason repeats the summary, as
    /// `acceptSpecification` writes both from the same bytes.
    static func snapshot(
        content: String?, plain: Bool = false,
        type: Components.Schemas.AttentionType = .ready_for_final_review
    ) -> Components.Schemas.AttentionItemSnapshot {
        var snapshot = AttentionFixtures.fixture(type: type)
        guard let index = snapshot.item.agent_claims.firstIndex(where: { $0.label == "freeside.summary" }) else {
            preconditionFailure("Fixture needs a summary claim")
        }
        if type == .spec_approval, let content {
            snapshot.item.reason = content
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
