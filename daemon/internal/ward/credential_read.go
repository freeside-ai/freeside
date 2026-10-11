package ward

import (
	"bytes"
	"context"
	"io"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// SetupTokenRead is a setup token read out of its credential volume, with the
// two digests the same observation attested for the store it came from. A
// caller compares them with the digest the enrollment generation recorded
// before it uses the token.
type SetupTokenRead struct {
	Token SetupToken
	// TreeDigest and TokenDigest are SetupTokenIntegrity's.
	TreeDigest  domain.Digest
	TokenDigest domain.Digest
}

// ReadSetupToken reads the token out of an existing setup-token volume. It is
// the one place a stored token leaves its volume for the daemon process (plan
// §5.4, issue #1426): a judgment call authenticates with it.
//
// The read is ObserveSetupTokenIntegrity's observation with one more step:
// the same digest-pinned exporter image, with no network and the volume
// mounted read-only, the same allowlist check before the observer runs, and
// the same deletion and absence check after. The observer also copies the
// token file into its own root filesystem, and the daemon takes the copy out
// of the exported archive. Like the integrity observation it takes no lease
// or read hold; the caller holds the identity's shared read hold.
//
// A missing volume or token file is ErrCredentialStoreAbsent. A copy that
// does not hash to the digest the proof attests, or that is not a setup
// token, is an error and returns no token. No error carries a byte of the
// store.
func ReadSetupToken(
	ctx context.Context,
	runtime Runtime,
	exporterImage, volume string,
	authorize RuntimeResourceAuthorizer,
) (SetupTokenRead, error) {
	seen, token, err := observeSetupTokenStore(ctx, runtime, exporterImage, volume, authorize, credObserverTokenRead)
	if err != nil {
		return SetupTokenRead{}, err
	}
	return SetupTokenRead{Token: token, TreeDigest: seen.TreeDigest, TokenDigest: seen.TokenDigest}, nil
}

// readCredTokenCopy reads the token-read observer's copy of the token file
// out of its archive and returns it as the token a launch would load. The
// copy must hash to the digest the proof attests for the file in the volume.
// A line ending is dropped, as the shell substitution that loads the token
// into a stage launch drops it; what is left must be a setup token.
func readCredTokenCopy(archive io.Reader, copyPath, tokenDigest string) (SetupToken, error) {
	raw, found, err := extractArchiveRegularFile(archive, copyPath, maxStoredSetupTokenBytes)
	if err != nil {
		return SetupToken{}, failf(CheckAuthStoreMutationLease, "read stored token from observer rootfs: %v", err)
	}
	if !found {
		return SetupToken{}, failf(CheckAuthStoreMutationLease, "credential observer produced no token copy")
	}
	if contentaddr.Hex(contentaddr.Sum(raw)) != tokenDigest {
		return SetupToken{}, failf(CheckAuthStoreMutationLease,
			"token copy does not match the token digest the proof attests")
	}
	token, err := NewSetupToken(bytes.TrimRight(raw, "\n"))
	if err != nil {
		return SetupToken{}, failf(CheckAuthStoreMutationLease, "stored credential is not a setup token: %v", err)
	}
	return token, nil
}
