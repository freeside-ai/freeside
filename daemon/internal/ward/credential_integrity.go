package ward

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/strictjson"
)

// The credential-integrity observations (plan §10, issue #1630). Each reads
// one stored credential and reports digests and a length verdict, never the
// bytes. Neither takes a lease or a read hold: the caller holds the
// identity's shared read hold around the call (plan §5.4), and neither
// writes to the store it reads.

// ErrCredentialStoreAbsent reports that an integrity observation found no
// store to read: no volume of that name, no token file in it, or no file at
// the host path. It is not a finding. A store the probe cannot observe is
// reported as not checked.
var ErrCredentialStoreAbsent = errors.New("credential store is absent")

// SetupTokenIntegrity is what the integrity observer saw in a setup-token
// volume.
type SetupTokenIntegrity struct {
	// TreeDigest is the content address of the volume's complete tree, the
	// digest AdoptClaudeAuth records as a generation's StoreManifestDigest.
	TreeDigest domain.Digest
	// TokenDigest is the content address of the token file's bytes, the
	// digest EnrollClaudeSetupToken records.
	TokenDigest domain.Digest
	// Truncated reports a token file shorter than MinSetupTokenBytes, the
	// lower bound enrollment enforces on an operator-entered token.
	Truncated bool
}

// ObserveSetupTokenIntegrity observes an existing setup-token volume through
// the networkless, read-only exporter observer InspectCredentialVolumeManifest
// uses, running the integrity script. The exporter image must be
// digest-pinned, as for ObserveSetupTokenVolume: the observer runs it with
// the credential volume mounted.
//
// A missing volume or token file is ErrCredentialStoreAbsent. The volume
// list is read first so the observer never mounts a name the runtime would
// have to create.
func ObserveSetupTokenIntegrity(
	ctx context.Context,
	runtime Runtime,
	exporterImage, volume string,
	authorize RuntimeResourceAuthorizer,
) (SetupTokenIntegrity, error) {
	if runtime == nil || volume == "" {
		return SetupTokenIntegrity{}, errors.New("setup-token integrity observation requires a runtime and a volume")
	}
	if !digestPinnedImagePattern.MatchString(exporterImage) {
		return SetupTokenIntegrity{}, errors.New("exporter image is not digest-pinned")
	}
	volumes, err := runtime.ListVolumes(ctx)
	if err != nil {
		return SetupTokenIntegrity{}, fmt.Errorf("list volumes: %w", err)
	}
	present := false
	for _, v := range volumes {
		if v.Name == volume {
			present = true
			break
		}
	}
	if !present {
		return SetupTokenIntegrity{}, fmt.Errorf("credential volume %q: %w", volume, ErrCredentialStoreAbsent)
	}
	proof, err := observeCredentialVolumeProof(
		ctx, runtime, exporterImage, volume, CredentialManifestOpaque, authorize, true,
	)
	if err != nil {
		return SetupTokenIntegrity{}, err
	}
	if proof.tokenDigest == "" {
		return SetupTokenIntegrity{}, fmt.Errorf(
			"credential volume %q holds no token file: %w", volume, ErrCredentialStoreAbsent)
	}
	tree, treeOK := contentaddr.FromHex(proof.tree)
	token, tokenOK := contentaddr.FromHex(proof.tokenDigest)
	if !treeOK || !tokenOK {
		return SetupTokenIntegrity{}, errors.New("credential volume proof carries a malformed digest")
	}
	return SetupTokenIntegrity{
		TreeDigest:  domain.Digest(tree),
		TokenDigest: domain.Digest(token),
		Truncated:   !proof.tokenLengthOK,
	}, nil
}

// CodexStoreIntegrity is what the integrity observation saw in a Codex host
// auth store.
type CodexStoreIntegrity struct {
	// ContentDigest is the content address of the file's bytes, the digest
	// Codex enrollment and adoption record as StoreManifestDigest.
	ContentDigest domain.Digest
	// Truncated reports a file that is empty or ends before its JSON
	// document closes.
	Truncated bool
}

// ObserveCodexStoreIntegrity reads the Codex auth store at path with the
// bounded private-file reader the review lifecycle reads it with, under the
// same private root, and requires the path to resolve to itself, as the
// refresh does before it trusts an identity's store binding. A missing file
// is ErrCredentialStoreAbsent. No error it returns carries file content.
func ObserveCodexStoreIntegrity(root, path string) (CodexStoreIntegrity, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return CodexStoreIntegrity{}, errors.New("auth-store path is not a clean absolute path")
	}
	if _, err := os.Lstat(path); errors.Is(err, fs.ErrNotExist) {
		return CodexStoreIntegrity{}, fmt.Errorf("auth store %q: %w", path, ErrCredentialStoreAbsent)
	}
	resolved, body, err := readCodexReviewInput(root, path, maxCodexAuthSnapshotBytes)
	if err != nil {
		return CodexStoreIntegrity{}, fmt.Errorf("read auth store: %w", err)
	}
	if resolved != path {
		return CodexStoreIntegrity{}, errors.New("auth-store path does not resolve to itself")
	}
	return CodexStoreIntegrity{
		ContentDigest: domain.Digest(contentaddr.Sum(body)),
		Truncated:     jsonDocumentTruncated(body),
	}, nil
}

// jsonDocumentTruncated reports whether body ends before its first JSON
// value closes: empty, whitespace only, or cut off mid-document. Bytes that
// are not JSON at all, or that follow a document that did close, are not
// truncation. That is corruption, which a length check must not claim.
func jsonDocumentTruncated(body []byte) bool {
	var document json.RawMessage
	err := strictjson.DecodeAllowingUnknownFields(
		body, &document, strictjson.TolerateInvalidUTF8, strictjson.NoLimit)
	if errors.Is(err, strictjson.ErrTrailingData) {
		return false
	}
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
}
