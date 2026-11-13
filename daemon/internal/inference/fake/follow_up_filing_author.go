package fake

// Scripted follow-up authoring response bodies, one full answer and one per
// refusal the site must make. They are exported so a caller's tests script
// the same bodies the site's own tests pin:
//
//	driver.Script(inference.FollowUpFilingAuthorSiteID,
//		fake.Script{Response: inference.Response{Output: []byte(fake.FollowUpFilingAuthorDraft)}})
const (
	// FollowUpFilingAuthorDraft is a full answer whose kind is fix.
	FollowUpFilingAuthorDraft = `{` +
		`"title":"Bound the sync client's retries",` +
		`"objective":"The sync client retries a failed push without limit. Bound the retries so a dead remote stops the push instead of looping.",` +
		`"non_goals":"Changing the push protocol or its backoff curve.",` +
		`"affected_contracts":"none",` +
		`"acceptance":"- A push against a dead remote stops after the retry bound.\n- A push that succeeds on a retry still completes.",` +
		`"scope":"daemon/a.go",` +
		`"kind":"fix"}`

	// FollowUpFilingAuthorBadKind names a kind outside the enum, so the site
	// returns no draft.
	FollowUpFilingAuthorBadKind = `{` +
		`"title":"Bound the sync client's retries","objective":"Bound the retries.","non_goals":"none",` +
		`"affected_contracts":"none","acceptance":"- It stops.","scope":"daemon/a.go","kind":"contract"}`

	// FollowUpFilingAuthorMalformed carries a field outside the schema, so the
	// site returns no draft.
	FollowUpFilingAuthorMalformed = `{` +
		`"title":"Bound the sync client's retries","objective":"Bound the retries.","non_goals":"none",` +
		`"affected_contracts":"none","acceptance":"- It stops.","scope":"daemon/a.go","kind":"fix",` +
		`"labels":["priority:critical"]}`
)
