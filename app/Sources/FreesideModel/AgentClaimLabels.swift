// Keep in sync with export.SummaryEvidenceLabel and
// export.ReviewGuideEvidenceLabel in
// daemon/internal/export/evidence_source.go.
package enum AgentClaimLabels {
    package static let summary = "freeside.summary"
    package static let reviewGuide = "freeside.review-guide"
    static let addressals = "Addressals"
    package static let specification = "Specification"

    package static func isApprovalMaterial(_ label: String) -> Bool {
        label == addressals || label == specification
    }
}
