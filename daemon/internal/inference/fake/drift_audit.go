package fake

// Scripted drift-auditor response bodies, one per verdict and one per refusal
// the site must make. They are exported so a caller's tests script the same
// bodies the site's own tests pin:
//
//	driver.Script(inference.DriftAuditorSiteID,
//		fake.Script{Response: inference.Response{Output: []byte(fake.DriftAuditOverHardened)}})
const (
	// DriftAuditFindingID is the finding DriftAuditOverHardened cites. The
	// audit's input must name it in a disposition or an adjudication entry.
	DriftAuditFindingID = "finding-1"

	DriftAuditConverged = `{"verdict":"converged","confidence":"high","reversals":[],"explanation":"the change matches the approved specification"}`

	DriftAuditOverHardened = `{"verdict":"over_hardened","confidence":"high","reversals":[{"finding_id":"finding-1","undo":"remove the retry wrapper around the config read","rationale":"the specification reads the config once at start"}],"explanation":"review added defensive work the specification does not need"}`

	DriftAuditStuck = `{"verdict":"stuck","confidence":"medium","reversals":[],"explanation":"rounds keep producing findings and the change converges neither way"}`

	// DriftAuditMalformed carries a field outside the schema, so the site
	// returns no verdict.
	DriftAuditMalformed = `{"verdict":"converged","confidence":"high","reversals":[],"explanation":"fine","route":"auto"}`

	// DriftAuditUnknownFinding is a well-formed over_hardened body whose
	// reversal cites a finding no input names, so the site returns no verdict.
	DriftAuditUnknownFinding = `{"verdict":"over_hardened","confidence":"high","reversals":[{"finding_id":"finding-unknown","undo":"remove the guard","rationale":"the specification does not need it"}],"explanation":"review added defensive work the specification does not need"}`
)
