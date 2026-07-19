package publish_test

import (
	"context"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/golden"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
)

// memoryLedger is the in-memory IntentLedger fake: a map of committed
// intents plus injectable failure, mirroring the store outbox's
// insert-or-converge contract.
type memoryLedger struct {
	rows  map[string][]byte
	kinds map[string]string
	keys  []string // insertion order, for before-dispatch assertions
	err   error
}

func newMemoryLedger() *memoryLedger {
	return &memoryLedger{rows: map[string][]byte{}, kinds: map[string]string{}}
}

func (l *memoryLedger) Record(_ context.Context, key, kind string, payload []byte) ([]byte, bool, error) {
	if l.err != nil {
		return nil, false, l.err
	}
	if prior, ok := l.rows[key]; ok {
		return prior, false, nil
	}
	stored := append([]byte(nil), payload...)
	l.rows[key] = stored
	l.kinds[key] = kind
	l.keys = append(l.keys, key)
	return stored, true, nil
}

func fixtureIntent() publish.Intent {
	return publish.Intent{
		Identity:        "sha256:01c663f9a986e10d214b2c31c75fa5088e2995674a8e8f2ba959111e06a23fb8",
		InvocationID:    "inv-0001",
		Repo:            "freeside-ai/evidence-repo",
		BaseRef:         "main",
		SourceHeadSHA:   "6dcb09b5b57875f334f61aebed695e2e4193db5e",
		AuthorizationID: "sha256:02c663f9a986e10d214b2c31c75fa5088e2995674a8e8f2ba959111e06a23fb8",
	}
}

// TestIntentGolden pins the encoded ledger payload: the outbox row
// outlives any single daemon build, so a recovery scan after an
// upgrade must decode what an older build recorded.
func TestIntentGolden(t *testing.T) {
	payload, err := fixtureIntent().Encode()
	if err != nil {
		t.Fatal(err)
	}
	golden.Assert(t, "publication-intent", append(payload, '\n'))
}

// TestIntentRoundTrip: Encode then DecodeIntent returns the same
// intent.
func TestIntentRoundTrip(t *testing.T) {
	payload, err := fixtureIntent().Encode()
	if err != nil {
		t.Fatal(err)
	}
	got, err := publish.DecodeIntent(payload)
	if err != nil {
		t.Fatal(err)
	}
	if got != fixtureIntent() {
		t.Errorf("round trip = %+v, want %+v", got, fixtureIntent())
	}
}

// TestIntentValidation: a malformed intent neither encodes nor
// decodes; a decoded outbox row is a reconstructed value, so decode
// re-validates rather than trusting it.
func TestIntentValidation(t *testing.T) {
	cases := map[string]func(*publish.Intent){
		"empty identity":      func(i *publish.Intent) { i.Identity = "" },
		"non-digest identity": func(i *publish.Intent) { i.Identity = "freeside/publish/abcd" },
		"truncated identity":  func(i *publish.Intent) { i.Identity = "sha256:abcd" },
		"empty invocation":    func(i *publish.Intent) { i.InvocationID = "" },
		"empty repo":          func(i *publish.Intent) { i.Repo = "" },
		"empty base ref":      func(i *publish.Intent) { i.BaseRef = "" },
		"empty source head":   func(i *publish.Intent) { i.SourceHeadSHA = "" },
	}
	for name, mutate := range cases {
		i := fixtureIntent()
		mutate(&i)
		if _, err := i.Encode(); err == nil {
			t.Errorf("%s: Encode accepted, want error", name)
		}
	}

	if _, err := publish.DecodeIntent([]byte(`{"identity":"","invocation_id":"","repo":"","base_ref":"","source_head_sha":""}`)); err == nil {
		t.Error("DecodeIntent accepted an empty intent, want error")
	}
	if _, err := publish.DecodeIntent([]byte(`not json`)); err == nil {
		t.Error("DecodeIntent accepted non-JSON, want error")
	}
	// Unknown fields fail closed: a payload this build cannot fully
	// interpret must not drive convergence.
	payload, err := fixtureIntent().Encode()
	if err != nil {
		t.Fatal(err)
	}
	widened := strings.Replace(string(payload), "{", `{"force":true,`, 1)
	if _, err := publish.DecodeIntent([]byte(widened)); err == nil {
		t.Error("DecodeIntent accepted an unknown field, want error")
	}
	// Trailing data fails closed the same way: a valid intent followed
	// by anything else is a payload this package cannot fully
	// interpret, so it must not drive convergence.
	for name, trailer := range map[string]string{
		"second JSON value": ` {"other":true}`,
		"garbage":           `garbage`,
	} {
		if _, err := publish.DecodeIntent([]byte(string(payload) + trailer)); err == nil {
			t.Errorf("DecodeIntent accepted trailing %s, want error", name)
		}
	}
}

// TestIntentKey pins the key shape and its fail-fast checks: an empty
// component would compose a key that can collide across invocations.
func TestIntentKey(t *testing.T) {
	key, err := publish.IntentKey("inv-0001", publish.IntentKindPublication)
	if err != nil {
		t.Fatal(err)
	}
	if key != "publish/inv-0001/publish.publication" {
		t.Errorf("IntentKey = %q", key)
	}
	if _, err := publish.IntentKey("", publish.IntentKindPublication); err == nil {
		t.Error("empty invocation id accepted, want error")
	}
	if _, err := publish.IntentKey("inv-0001", ""); err == nil {
		t.Error("empty kind accepted, want error")
	}
}

// TestMemoryLedgerConverges: the fake honors the port contract its
// consumers rely on — a second Record under the same key returns the
// original payload, not the new one.
func TestMemoryLedgerConverges(t *testing.T) {
	l := newMemoryLedger()
	first, recorded, err := l.Record(context.Background(), "k", "kind", []byte("original"))
	if err != nil || !recorded || string(first) != "original" {
		t.Fatalf("first Record = (%q, %t, %v)", first, recorded, err)
	}
	prior, recorded, err := l.Record(context.Background(), "k", "kind", []byte("retry"))
	if err != nil || recorded || string(prior) != "original" {
		t.Errorf("second Record = (%q, %t, %v), want original payload, not recorded", prior, recorded, err)
	}
}

// memoryLedger must satisfy the port it fakes.
var _ publish.IntentLedger = (*memoryLedger)(nil)
