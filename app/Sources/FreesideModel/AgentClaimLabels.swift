// Keep in sync with export.SummaryEvidenceLabel in
// daemon/internal/export/evidence_source.go.
package enum AgentClaimLabels {
    package static let summary = "freeside.summary"
    static let addressals = "Addressals"
    package static let specification = "Specification"

    package static func isApprovalMaterial(_ label: String) -> Bool {
        label == addressals || label == specification
    }
}
