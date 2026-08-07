package publish_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/golden"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
)

// authorityFixture is the canonical snapshot the golden pins: one registration
// with a settled binding, and one mid-install registration whose pending
// envelope has no installation ID yet. It doubles as the validation-positive
// case for every rule the table below inverts.
func authorityFixture() publish.InstallationAuthorityDocument {
	return publish.InstallationAuthorityDocument{
		Version: 1,
		Registrations: []publish.InstallationAuthorityEntry{
			{
				RegistrationID:        4385298,
				ActiveEpoch:           1,
				DurableIntentRevision: 1,
				TrustedOwners: []publish.TrustedOwnerRecord{
					{Login: "freeside-ai", ID: 231470451},
				},
				TrustedInstallations: []publish.TrustedInstallationRecord{
					{
						InstallationID: 148770512,
						Account:        "freeside-ai",
						AccountID:      231470451,
						RepositoryIDs:  []int64{1278475858},
					},
				},
			},
			{
				RegistrationID:        7700001,
				ActiveEpoch:           3,
				DurableIntentRevision: 7,
				TrustedOwners: []publish.TrustedOwnerRecord{
					{Login: "example-org", ID: 4242},
				},
				// Empty rather than absent: an operator reading the golden as an
				// authoring example should see the shape of "nothing is trusted
				// here yet", which is also the most destructive one to author.
				TrustedInstallations: []publish.TrustedInstallationRecord{},
				Pending: &publish.PendingEnvelopeRecord{
					ActiveEpoch:            3,
					DurableIntentRevision:  7,
					ExpectedAccount:        "example-org",
					ExpectedAccountID:      4242,
					InstallationID:         new(int64),
					CurrentRepositoryIDs:   []int64{},
					ExpectedRepositoryIDs:  []int64{55, 56},
					RequiredRepositoryMode: "selected",
					ExpiresAt:              time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC),
				},
			},
		},
	}
}

func TestInstallationAuthorityDocumentGolden(t *testing.T) {
	t.Parallel()
	payload, err := authorityFixture().Encode()
	if err != nil {
		t.Fatalf("encode fixture: %v", err)
	}
	golden.Assert(t, "installation-authority", payload)

	// The file is authored by hand, so the golden must be the exact form the
	// decoder accepts and the encoder reproduces: pinning only the marshaller's
	// output would leave the authored form unchecked.
	decoded, err := publish.DecodeInstallationAuthorityDocument(payload)
	if err != nil {
		t.Fatalf("decode golden: %v", err)
	}
	reencoded, err := decoded.Encode()
	if err != nil {
		t.Fatalf("re-encode golden: %v", err)
	}
	if string(reencoded) != string(payload) {
		t.Fatalf("round trip changed the document:\n%s\nwant:\n%s", reencoded, payload)
	}
}

// validAuthorityJSON is the byte form the rejection table mutates. Keeping it
// separate from the fixture above keeps each case a single visible edit.
const validAuthorityJSON = `{
  "version": 1,
  "registrations": [
    {
      "registration_id": 91,
      "active_epoch": 2,
      "durable_intent_revision": 5,
      "trusted_owners": [
        {"login": "example-org", "id": 4242}
      ],
      "trusted_installations": [
        {"installation_id": 77, "account": "example-org", "account_id": 4242,
         "repository_ids": [10, 20]}
      ],
      "pending": null
    }
  ]
}`

func TestDecodeInstallationAuthorityDocumentAcceptsCanonicalPayload(t *testing.T) {
	t.Parallel()
	document, err := publish.DecodeInstallationAuthorityDocument([]byte(validAuthorityJSON))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(document.Registrations) != 1 || document.Registrations[0].RegistrationID != 91 {
		t.Fatalf("decoded %+v, want the single registration 91", document.Registrations)
	}
}

func TestDecodeInstallationAuthorityDocumentRejects(t *testing.T) {
	t.Parallel()
	// Every case must fail: a snapshot this package cannot fully interpret
	// would otherwise serve as a narrower authority, and a narrower authority
	// tells the janitor to delete installations.
	cases := []struct {
		name    string
		payload string
	}{
		{"empty payload", ""},
		{"not json", "installation authority"},
		{"invalid utf8", strings.Replace(validAuthorityJSON, "example-org", "example\xffrg", 1)},
		{"truncated", validAuthorityJSON[:len(validAuthorityJSON)/2]},
		{"trailing data", validAuthorityJSON + "\n{}"},
		{"unknown top level field", strings.Replace(validAuthorityJSON, `"version": 1`, `"version": 1, "notes": "x"`, 1)},
		{"unknown entry field", strings.Replace(validAuthorityJSON, `"registration_id": 91`, `"registration_id": 91, "owner": "x"`, 1)},
		{"unknown owner field", strings.Replace(validAuthorityJSON, `{"login": "example-org", "id": 4242}`, `{"login": "example-org", "id": 4242, "kind": "org"}`, 1)},
		{"unknown binding field", strings.Replace(validAuthorityJSON, `"installation_id": 77`, `"installation_id": 77, "suspended": false`, 1)},
		{"version zero", strings.Replace(validAuthorityJSON, `"version": 1`, `"version": 0`, 1)},
		{"version two", strings.Replace(validAuthorityJSON, `"version": 1`, `"version": 2`, 1)},
		{"duplicate registration", strings.Replace(validAuthorityJSON, `"pending": null
    }`, `"pending": null
    },
    {"registration_id": 91, "active_epoch": 1, "durable_intent_revision": 1,
     "trusted_owners": [], "trusted_installations": [], "pending": null}`, 1)},
		{"non-positive registration id", strings.Replace(validAuthorityJSON, `"registration_id": 91`, `"registration_id": 0`, 1)},
		{"non-positive epoch", strings.Replace(validAuthorityJSON, `"active_epoch": 2`, `"active_epoch": 0`, 1)},
		{"non-positive revision", strings.Replace(validAuthorityJSON, `"durable_intent_revision": 5`, `"durable_intent_revision": 0`, 1)},
		{"non-positive owner id", strings.Replace(validAuthorityJSON, `"id": 4242}`, `"id": 0}`, 1)},
		{"empty owner login", strings.Replace(validAuthorityJSON, `"login": "example-org"`, `"login": ""`, 1)},
		{"owner login with whitespace", strings.Replace(validAuthorityJSON, `"login": "example-org"`, `"login": " example-org"`, 1)},
		{"owner login with a slash", strings.Replace(validAuthorityJSON, `"login": "example-org"`, `"login": "example/org"`, 1)},
		{"duplicate owner id", strings.Replace(validAuthorityJSON, `{"login": "example-org", "id": 4242}`, `{"login": "example-org", "id": 4242}, {"login": "other-org", "id": 4242}`, 1)},
		{"non-positive installation id", strings.Replace(validAuthorityJSON, `"installation_id": 77`, `"installation_id": 0`, 1)},
		{"non-positive account id", strings.Replace(validAuthorityJSON, `"account_id": 4242`, `"account_id": 0`, 1)},
		{"invalid binding account", strings.Replace(validAuthorityJSON, `"account": "example-org"`, `"account": "-example"`, 1)},
		{"empty repository set", strings.Replace(validAuthorityJSON, `"repository_ids": [10, 20]`, `"repository_ids": []`, 1)},
		{"duplicate repository id", strings.Replace(validAuthorityJSON, `"repository_ids": [10, 20]`, `"repository_ids": [10, 10]`, 1)},
		{"non-positive repository id", strings.Replace(validAuthorityJSON, `"repository_ids": [10, 20]`, `"repository_ids": [0, 20]`, 1)},
		{"unsorted repository ids", strings.Replace(validAuthorityJSON, `"repository_ids": [10, 20]`, `"repository_ids": [20, 10]`, 1)},
		{
			"duplicate installation id",
			strings.Replace(validAuthorityJSON,
				`{"installation_id": 77, "account": "example-org", "account_id": 4242,
         "repository_ids": [10, 20]}`,
				`{"installation_id": 77, "account": "example-org", "account_id": 4242, "repository_ids": [10]},
         {"installation_id": 77, "account": "example-org", "account_id": 4242, "repository_ids": [20]}`, 1),
		},
		{
			"account bound twice",
			strings.Replace(validAuthorityJSON,
				`"repository_ids": [10, 20]}`,
				`"repository_ids": [10, 20]},
         {"installation_id": 78, "account": "Example-Org", "account_id": 4242, "repository_ids": [30]}`, 1),
		},
		{
			"repository bound twice",
			strings.Replace(validAuthorityJSON,
				`"repository_ids": [10, 20]}`,
				`"repository_ids": [10, 20]},
         {"installation_id": 78, "account": "other-org", "account_id": 4343, "repository_ids": [20]}`, 1),
		},
		{"pending with a zero epoch", withPending(`"active_epoch": 0, "durable_intent_revision": 5,
      "expected_account": "other-org", "expected_account_id": 4343, "installation_id": 0,
      "current_repository_ids": [], "expected_repository_ids": [90],
      "required_repository_mode": "selected", "expires_at": "2026-07-25T12:00:00Z"`)},
		{"pending with an unknown repository mode", withPending(`"active_epoch": 2, "durable_intent_revision": 5,
      "expected_account": "other-org", "expected_account_id": 4343, "installation_id": 0,
      "current_repository_ids": [], "expected_repository_ids": [90],
      "required_repository_mode": "all", "expires_at": "2026-07-25T12:00:00Z"`)},
		{"pending without an expiry", withPending(`"active_epoch": 2, "durable_intent_revision": 5,
      "expected_account": "other-org", "expected_account_id": 4343, "installation_id": 0,
      "current_repository_ids": [], "expected_repository_ids": [90],
      "required_repository_mode": "selected", "expires_at": "0001-01-01T00:00:00Z"`)},
		{"pending with a negative installation id", withPending(`"active_epoch": 2, "durable_intent_revision": 5,
      "expected_account": "other-org", "expected_account_id": 4343, "installation_id": -1,
      "current_repository_ids": [], "expected_repository_ids": [90],
      "required_repository_mode": "selected", "expires_at": "2026-07-25T12:00:00Z"`)},
		{"pending current set is not a subset", withPending(`"active_epoch": 2, "durable_intent_revision": 5,
      "expected_account": "example-org", "expected_account_id": 4242, "installation_id": 77,
      "current_repository_ids": [10, 20, 30], "expected_repository_ids": [10, 20],
      "required_repository_mode": "selected", "expires_at": "2026-07-25T12:00:00Z"`)},
		{"pending without an installation carries a current set", withPending(`"active_epoch": 2, "durable_intent_revision": 5,
      "expected_account": "other-org", "expected_account_id": 4343, "installation_id": 0,
      "current_repository_ids": [90], "expected_repository_ids": [90, 91],
      "required_repository_mode": "selected", "expires_at": "2026-07-25T12:00:00Z"`)},
		{"pending omits the known installation identity", withPending(`"active_epoch": 2, "durable_intent_revision": 5,
      "expected_account": "example-org", "expected_account_id": 4242, "installation_id": 0,
      "current_repository_ids": [], "expected_repository_ids": [10, 20],
      "required_repository_mode": "selected", "expires_at": "2026-07-25T12:00:00Z"`)},
		{"pending names a second installation for a bound account", withPending(`"active_epoch": 2, "durable_intent_revision": 5,
      "expected_account": "example-org", "expected_account_id": 4242, "installation_id": 78,
      "current_repository_ids": [], "expected_repository_ids": [10, 20],
      "required_repository_mode": "selected", "expires_at": "2026-07-25T12:00:00Z"`)},
		{"pending does not bind the current installation state", withPending(`"active_epoch": 2, "durable_intent_revision": 5,
      "expected_account": "example-org", "expected_account_id": 4242, "installation_id": 77,
      "current_repository_ids": [10], "expected_repository_ids": [10, 20, 30],
      "required_repository_mode": "selected", "expires_at": "2026-07-25T12:00:00Z"`)},
		{"pending carries an unbound current set", withPending(`"active_epoch": 2, "durable_intent_revision": 5,
      "expected_account": "other-org", "expected_account_id": 4343, "installation_id": 79,
      "current_repository_ids": [90], "expected_repository_ids": [90, 91],
      "required_repository_mode": "selected", "expires_at": "2026-07-25T12:00:00Z"`)},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			document, err := publish.DecodeInstallationAuthorityDocument([]byte(testCase.payload))
			if err == nil {
				t.Fatalf("decoded %+v, want an error", document)
			}
			if len(document.Registrations) != 0 {
				t.Fatalf("decode returned %+v alongside its error", document)
			}
		})
	}
}

func TestDecodeInstallationAuthorityDocumentErrorsAreSnapshotErrors(t *testing.T) {
	t.Parallel()
	for _, payload := range []string{
		"",
		strings.Replace(validAuthorityJSON, `"version": 1`, `"version": 2`, 1),
		strings.Replace(validAuthorityJSON, `"installation_id": 77`, `"installation_id": 0`, 1),
	} {
		_, err := publish.DecodeInstallationAuthorityDocument([]byte(payload))
		if !errors.Is(err, publish.ErrInstallationAuthoritySnapshot) {
			t.Fatalf("error %v does not match ErrInstallationAuthoritySnapshot", err)
		}
	}
}

// withPending replaces the base payload's null envelope with an authored one.
func withPending(body string) string {
	return strings.Replace(validAuthorityJSON, `"pending": null`, "\"pending\": {\n      "+body+"\n      }", 1)
}

func FuzzDecodeInstallationAuthorityDocument(f *testing.F) {
	f.Add(validAuthorityJSON)
	f.Add(`{"version":1,"registrations":[]}`)
	f.Add(`{"version":1,"registrations":null}`)
	f.Fuzz(func(t *testing.T, payload string) {
		document, err := publish.DecodeInstallationAuthorityDocument([]byte(payload))
		if err != nil {
			return
		}
		// A value the decoder returns must satisfy the same rules on its way
		// back out: anything else means decode accepted a document Encode would
		// refuse to write, which is the shape a laundering bug takes.
		if _, err := document.Encode(); err != nil {
			t.Fatalf("decoded document fails validation on encode: %v", err)
		}
	})
}

// TestDecodeInstallationAuthorityDocumentRejectsDuplicateKeys pins the class a
// refute-first pass found: encoding/json resolves a repeated key silently, last
// scalar or array winning and nested objects merging, so a hand-edited file with
// a duplicated stanza decodes to an authority no line in it authored. The
// surviving value is routinely the narrower one, and narrower here means the
// janitor deletes.
func TestDecodeInstallationAuthorityDocumentRejectsDuplicateKeys(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"narrowed bindings": strings.Replace(validAuthorityJSON,
			`"pending": null`,
			`"trusted_installations": [], "pending": null`, 1),
		"narrowed repository set": strings.Replace(validAuthorityJSON,
			`"repository_ids": [10, 20]`,
			`"repository_ids": [10, 20], "repository_ids": [10]`, 1),
		"revoked pending envelope": strings.Replace(
			withPending(`"active_epoch": 2, "durable_intent_revision": 5,
      "expected_account": "other-org", "expected_account_id": 4343, "installation_id": 0,
      "current_repository_ids": [], "expected_repository_ids": [90],
      "required_repository_mode": "selected", "expires_at": "2026-07-25T12:00:00Z"`),
			`"registration_id": 91`, `"registration_id": 91, "pending": null`, 1),
		"merged pending envelope": strings.Replace(
			withPending(`"active_epoch": 2, "durable_intent_revision": 5,
      "expected_account": "other-org", "expected_account_id": 4343, "installation_id": 0,
      "current_repository_ids": [], "expected_repository_ids": [90],
      "required_repository_mode": "selected", "expires_at": "2020-01-01T00:00:00Z"`),
			`"expires_at": "2020-01-01T00:00:00Z"`,
			`"expires_at": "2020-01-01T00:00:00Z", "expires_at": "2099-01-01T00:00:00Z"`, 1),
		"duplicated version": strings.Replace(validAuthorityJSON,
			`"version": 1`, `"version": 1, "version": 1`, 1),
		"duplicated registrations": strings.Replace(validAuthorityJSON,
			`"registrations": [`, `"registrations": [], "registrations": [`, 1),
		"duplicated owner field": strings.Replace(validAuthorityJSON,
			`{"login": "example-org", "id": 4242}`,
			`{"login": "example-org", "login": "other-org", "id": 4242}`, 1),
	}

	for name, payload := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := publish.DecodeInstallationAuthorityDocument([]byte(payload)); err == nil {
				t.Fatal("a document naming a key twice was accepted")
			}
		})
	}

	// The check must not reject a document that merely repeats a key in
	// different objects, which every multi-entry file does.
	if _, err := publish.DecodeInstallationAuthorityDocument([]byte(validAuthorityJSON)); err != nil {
		t.Fatalf("the canonical document was rejected: %v", err)
	}
}

// TestDecodeInstallationAuthorityDocumentRequiresExplicitAbsence pins the two
// fields where an omitted key would otherwise mean something dangerous: no
// bindings is a mass-delete instruction, and a zero pending installation ID
// matches any installation on the account.
func TestDecodeInstallationAuthorityDocumentRequiresExplicitAbsence(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"bindings omitted": strings.Replace(validAuthorityJSON,
			`"trusted_installations": [
        {"installation_id": 77, "account": "example-org", "account_id": 4242,
         "repository_ids": [10, 20]}
      ],`, "", 1),
		"bindings null": strings.Replace(validAuthorityJSON,
			`"trusted_installations": [
        {"installation_id": 77, "account": "example-org", "account_id": 4242,
         "repository_ids": [10, 20]}
      ]`, `"trusted_installations": null`, 1),
		"pending installation id omitted": withPending(`"active_epoch": 2, "durable_intent_revision": 5,
      "expected_account": "other-org", "expected_account_id": 4343,
      "current_repository_ids": [], "expected_repository_ids": [90],
      "required_repository_mode": "selected", "expires_at": "2026-07-25T12:00:00Z"`),
		"pending installation id null": withPending(`"active_epoch": 2, "durable_intent_revision": 5,
      "expected_account": "other-org", "expected_account_id": 4343, "installation_id": null,
      "current_repository_ids": [], "expected_repository_ids": [90],
      "required_repository_mode": "selected", "expires_at": "2026-07-25T12:00:00Z"`),
	}

	for name, payload := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := publish.DecodeInstallationAuthorityDocument([]byte(payload)); err == nil {
				t.Fatal("an omitted field was read as its most permissive value")
			}
		})
	}

	// Absence must be refused everywhere it is expressible, not only where it is
	// obviously dangerous: enumerating the format once beats re-litigating which
	// omission is safe each time the shape grows.
	for _, key := range []string{
		`"version": 1,`, `"registration_id": 91,`, `"active_epoch": 2,`,
		`"durable_intent_revision": 5,`, `"pending": null`,
	} {
		t.Run("omitted "+strings.Trim(strings.SplitN(key, ":", 2)[0], `"`), func(t *testing.T) {
			payload := strings.Replace(validAuthorityJSON, key, "", 1)
			if _, err := publish.DecodeInstallationAuthorityDocument([]byte(payload)); err == nil {
				t.Fatal("an omitted key was accepted")
			}
		})
	}
	// An omitted `pending` is the case that deletes a fresh native install; an
	// authored null is the operator saying there is no exception.
	t.Run("pending null is authored absence", func(t *testing.T) {
		if _, err := publish.DecodeInstallationAuthorityDocument([]byte(validAuthorityJSON)); err != nil {
			t.Fatalf("an authored null pending envelope was rejected: %v", err)
		}
	})
	t.Run("omitted envelope field", func(t *testing.T) {
		payload := withPending(`"active_epoch": 2, "durable_intent_revision": 5,
      "expected_account": "other-org", "expected_account_id": 4343, "installation_id": 0,
      "expected_repository_ids": [90],
      "required_repository_mode": "selected", "expires_at": "2026-07-25T12:00:00Z"`)
		if _, err := publish.DecodeInstallationAuthorityDocument([]byte(payload)); err == nil {
			t.Fatal("an envelope omitting current_repository_ids was accepted")
		}
	})

	// Authored explicitly, both are legitimate.
	explicit := strings.Replace(validAuthorityJSON,
		`"trusted_installations": [
        {"installation_id": 77, "account": "example-org", "account_id": 4242,
         "repository_ids": [10, 20]}
      ]`, `"trusted_installations": []`, 1)
	if _, err := publish.DecodeInstallationAuthorityDocument([]byte(explicit)); err != nil {
		t.Fatalf("an explicitly empty binding set was rejected: %v", err)
	}
}
