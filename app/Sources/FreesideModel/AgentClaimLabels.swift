// Keep in sync with export.SummaryEvidenceLabel and
// export.ReviewGuideEvidenceLabel in
// daemon/internal/export/evidence_source.go, and agentTranscript with the
// transcript label in daemon/internal/exec/claude/spec.go,
// daemon/internal/exec/stage/failure_evidence.go, and
// daemon/internal/signet/invocation_transcript.go.
package enum AgentClaimLabels {
    package static let summary = "freeside.summary"
    package static let reviewGuide = "freeside.review-guide"
    package static let agentTranscript = "agent-transcript"
    static let addressals = "Addressals"
    package static let specification = "Specification"

    package static func isApprovalMaterial(_ label: String) -> Bool {
        label == addressals || label == specification
    }
}
