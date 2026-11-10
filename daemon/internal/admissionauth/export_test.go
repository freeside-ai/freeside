package admissionauth

// The external tests share the internal tests' engine-seeded submission
// and counting resolver double through these aliases.
var NewSubmissionFixture = newSubmissionFixture

type CountingCommitAuthorResolver = countingProductionCommitAuthorResolver
