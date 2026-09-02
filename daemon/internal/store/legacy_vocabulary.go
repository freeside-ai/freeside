package store

import (
	"bytes"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// Pre-rename vocabulary (#986); the only file in the package that may spell
// it. Stored bytes are never rewritten: outbox payload digests, backup
// manifests, and canonical-body checks cover them. Instead the store
// canonicalizes on read, at the row boundary every consumer shares, so the
// engine, signet, and observe decoders see only the current names:
//
//   - queue kinds map to their current spelling as rows are scanned, and a
//     kind filter in SQL matches both spellings;
//   - the legacy JSON key and the versioned payload names that persisted
//     bodies and payloads carry are substituted in place before decoding.
//     Each substitution swaps one quoted token for the current encoder's, so
//     a payload the pre-rename encoder wrote canonically stays byte-equal to
//     what the current encoder produces from its decoded value, and the
//     engine's canonical-payload checks keep passing.
//
// Identifier prefixes are not translated: a row keeps its identifier family,
// and domain derives the family from the run ID.

var legacyQueueKinds = map[string]string{
	"elaboration_invocation_requested": string(domain.SpecificationInvocationRequestedKind),
	"elaboration_discussion_requested": string(domain.SpecificationDiscussionRequestedKind),
	"elaboration_stage_terminal":       "specification_stage_terminal",
	"elaboration_discussion_terminal":  "specification_discussion_terminal",
	"elaboration_implementation_claim": "specification_implementation_claim",
}

var currentQueueKinds = func() map[string]string {
	m := make(map[string]string, len(legacyQueueKinds))
	for legacy, current := range legacyQueueKinds {
		m[current] = legacy
	}
	return m
}()

var legacyBodyKeys = [][2][]byte{
	{[]byte(`"elaboration_run_id":`), []byte(`"specification_run_id":`)},
	{[]byte(`"freeside.elaboration-request/v1"`), []byte(`"freeside.specification-request/v1"`)},
	{[]byte(`"freeside.elaboration-discussion-request/v1"`), []byte(`"freeside.specification-discussion-request/v1"`)},
	{[]byte(`"freeside.elaboration-prior-artifact/v1"`), []byte(`"freeside.specification-prior-artifact/v1"`)},
}

// canonicalQueueKind maps a stored kind onto its current spelling.
func canonicalQueueKind(kind string) string {
	if current, ok := legacyQueueKinds[kind]; ok {
		return current
	}
	return kind
}

// queueKindAlias returns the legacy spelling a SQL kind filter must also
// match, or the kind itself when it never had one, so a two-slot IN clause
// binds without branching.
func queueKindAlias(kind string) string {
	if legacy, ok := currentQueueKinds[kind]; ok {
		return legacy
	}
	return kind
}

// canonicalizeLegacyBody renames the legacy JSON keys a stored body may
// carry. It returns the input unchanged, without copying, when none appear.
func canonicalizeLegacyBody(body []byte) []byte {
	for _, pair := range legacyBodyKeys {
		if bytes.Contains(body, pair[0]) {
			body = bytes.ReplaceAll(body, pair[0], pair[1])
		}
	}
	return body
}

// canonicalizeLegacyQueueEntry rewrites a scanned queue row's kind and
// payload to the current vocabulary. It runs after the stored payload digest
// has been checked against the stored bytes, so the digest still attests to
// what the row holds.
func canonicalizeLegacyQueueEntry(entry *QueueEntry) {
	entry.Kind = canonicalQueueKind(entry.Kind)
	entry.Payload = canonicalizeLegacyBody(entry.Payload)
}
