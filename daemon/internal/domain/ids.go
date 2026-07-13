package domain

// Identifier newtypes. Each is a distinct named string so signatures document
// which identifier they take and a mis-ordered argument fails to compile
// rather than silently swapping two IDs. They carry no validation beyond
// non-emptiness (checked where they are required); the daemon assigns their
// values, and this types-only package never generates them.
type (
	ItemID          string
	ProjectID       string
	SubjectID       string
	DeviceID        string
	ConversationID  string
	MessageID       string
	RunID           string
	StageID         string
	AttemptID       string
	InvocationID    string
	FindingID       string
	ArtifactID      string
	ProposalBatchID string
)

// Digest is a content address (e.g. "sha256:..."); artifacts, specs, recipes,
// and resolved policy are all digest-addressed (plan §5.9, §5.12, §5.15).
type Digest string
