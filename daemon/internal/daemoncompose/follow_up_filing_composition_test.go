package daemoncompose

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
)

type filingNoNetworkTransport struct{ t *testing.T }

func (r filingNoNetworkTransport) RoundTrip(*http.Request) (*http.Response, error) {
	r.t.Error("composition made a network call")
	return nil, errors.New("unexpected network call")
}

func TestFollowUpFilerCompositionAndBinding(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	st := storetest.Open(t, filepath.Join(root, "store.db"), store.Options{})
	ks, err := publish.NewKeystore(filepath.Join(root, "credentials"), filepath.Join(root, "state"))
	if err != nil {
		t.Fatal(err)
	}
	recorder, err := publish.NewStoreRecorder(st)
	if err != nil {
		t.Fatal(err)
	}
	trust, err := publish.NewStoreTrustSource(st)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: filingNoNetworkTransport{t}}
	minter := publish.NewMinter(ks, client, "https://github.test", recorder, trust, time.Now)
	auditor, err := publish.NewGitHubWorkflowAuditor(
		publish.NewCachedTokenSource(minter, time.Now), client, "https://github.test", time.Now)
	if err != nil {
		t.Fatal(err)
	}
	posters, err := composeFollowUpFiler(st, minter, ks, client, "https://github.test", auditor)
	if err != nil {
		t.Fatal(err)
	}
	filer, comments := posters.filer, posters.comments
	if filer == nil || comments == nil {
		t.Fatal("composition returned no filer or comment dispatcher")
	}
	if got := FollowUpFilerBinding(nil); got != nil {
		t.Fatal("fake-driver binding is not a nil interface")
	}
	if got := FollowUpFilerBinding(&ClaudeComposition{}); got != nil {
		t.Fatal("empty composition binding is not a nil interface")
	}
	got := FollowUpFilerBinding(&ClaudeComposition{FollowUpFiler: filer, IssueCommentDispatcher: comments})
	if wake, ok := got.(followUpWake); !ok || wake.filer != filer || wake.comments != comments {
		t.Fatal("binding did not wake the composed filer and comment dispatcher")
	}
	if err := comments.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := filer.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
}
