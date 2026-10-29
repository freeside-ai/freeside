package ward

import (
	"bytes"
	"fmt"
	"io"
)

// ReviewCredential is a review credential read for one launch from the
// enrollment generation of an identity the source was not composed with,
// which has no host snapshot (#1981). The body stays in memory for the
// launch: it never reaches the request, the journal, the binding, a log, or
// an error, and only its digest is recorded, as a host snapshot's is.
type ReviewCredential struct {
	body []byte
}

// NewReviewCredential holds a copy of body, so the caller can wipe its own.
func NewReviewCredential(body []byte) ReviewCredential {
	return ReviewCredential{body: bytes.Clone(body)}
}

const redactedReviewBody = "[review credential]"

func (ReviewCredential) String() string   { return redactedReviewBody }
func (ReviewCredential) GoString() string { return redactedReviewBody }

// Format covers every fmt verb, so %d or %x cannot dump the bytes.
func (ReviewCredential) Format(f fmt.State, _ rune) {
	io.WriteString(f, redactedReviewBody) //nolint:errcheck,gosec // fmt.State writes cannot be usefully handled
}

func (ReviewCredential) MarshalText() ([]byte, error) {
	return []byte(redactedReviewBody), nil
}

// readReviewAuth reads the credential a review launch delivers: the body
// read for it from an enrollment generation, or else the host snapshot under
// the input root, re-read at each call so a snapshot that changed between two
// checks is caught. The path is empty for a credential, which has no file.
func readReviewAuth(inputRoot, snapshot string, credential *ReviewCredential) (string, []byte, error) {
	if credential != nil {
		return "", bytes.Clone(credential.body), nil
	}
	return readCodexReviewInput(inputRoot, snapshot, maxCodexAuthSnapshotBytes)
}
