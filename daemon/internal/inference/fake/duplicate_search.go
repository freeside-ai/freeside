package fake

// Scripted duplicate-search response bodies, one per answer and one per
// refusal the site must make. They are exported so a caller's tests script
// the same bodies the site's own tests pin:
//
//	driver.Script(inference.DuplicateSearchSiteID,
//		fake.Script{Response: inference.Response{Output: []byte(fake.DuplicateSearchMatch)}})
const (
	// DuplicateSearchMatchHandle is the handle DuplicateSearchMatch names. The
	// search's input must list it.
	DuplicateSearchMatchHandle = "candidate-1"

	DuplicateSearchNew = `{"duplicate_of":null,"confidence":"high","reason":"No listed issue tracks this finding."}`

	DuplicateSearchMatch = `{"duplicate_of":"candidate-1","confidence":"high","reason":"candidate-1 already tracks this work."}`

	// DuplicateSearchUnknownHandle names a handle no input lists, so the
	// site returns no answer.
	DuplicateSearchUnknownHandle = `{"duplicate_of":"candidate-999","confidence":"high","reason":"That issue tracks it."}`

	// DuplicateSearchMalformed carries a field outside the schema, so the
	// site returns no answer.
	DuplicateSearchMalformed = `{"duplicate_of":null,"confidence":"high","reason":"none","number":17}`
)
