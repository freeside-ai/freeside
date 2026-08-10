package publish

import (
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/freeside-ai/freeside/daemon/internal/atomicfile"
)

// The keystore is the protected storage of docs/plan.md §10: the App's
// private key lands here directly at registration and never leaves.
// Containment is structural, not procedural: the credentials directory
// must be disjoint from the state directory, which is the surface the
// future checkpoint/backup unit will capture (§5.10 excludes App keys
// from checkpoints) and the surface workspace mounts are built from
// (§5.4: no GitHub write credential ever enters any workspace). The
// future checkpoint unit excludes Keystore.Dir(); until it exists, the
// disjointness check plus the tests over every written path are the
// asserted invariant (issue #80 acceptance 2, residual gap recorded in
// the work unit's decision note).

const (
	appDirName               = "github-app"
	keyFileName              = "app.pem"
	metaFileName             = "app.json"
	authorityPendingFileName = "authority.pending"

	// quarantineDirSuffix names the sibling directory holding records the
	// operator has withdrawn from this host. It is deliberately not one of
	// the legacy-singleton journal suffixes (.legacy/.old/.staging), which
	// migration treats as credential sources.
	quarantineDirSuffix = ".quarantine"
)

// UnreadableRegistrationError names the single registration whose record
// failed to load, so a caller can route the operator to it. OwnerID is
// the record's directory key: safe numeric routing information, never
// credential material.
type UnreadableRegistrationError struct {
	OwnerID int64
	Err     error
}

func (e *UnreadableRegistrationError) Error() string {
	return fmt.Sprintf("keystore: registration for owner %d is unreadable: %v", e.OwnerID, e.Err)
}

// Unwrap exposes the sentinel and the underlying cause together, so a
// caller that already distinguishes a specific cause (widened
// permissions, an absent key) keeps matching it, while a caller that only
// needs "this record is unusable" matches the sentinel.
func (e *UnreadableRegistrationError) Unwrap() []error {
	return []error{ErrUnreadableRegistration, e.Err}
}

// Keystore stores the GitHub App's credentials (private key and
// registration metadata) under a dedicated credentials directory with
// owner-only permissions: directories 0700, files 0600, re-asserted on
// every load so a widened file fails closed rather than being trusted.
type Keystore struct {
	root string // the credentials directory
	dir  string // root/github-app, whose children are numeric-owner-keyed registrations
	mu   *sync.Mutex
}

// NewKeystore roots a keystore at credentialsDir after proving it is
// disjoint from stateDir. Overlap in either direction is rejected: a
// credentials directory inside the state directory would land the key
// in checkpoints, and a state directory inside the credentials
// directory would complect the two surfaces the containment invariant
// keeps separate. Symlinks are resolved through the deepest existing
// ancestor of each path, so a symlinked credentials directory cannot
// tunnel back inside the state directory; any ambiguity fails closed.
func NewKeystore(credentialsDir, stateDir string) (*Keystore, error) {
	if credentialsDir == "" || stateDir == "" {
		return nil, fmt.Errorf("keystore: empty directory: %w", ErrCredentialsInsideStateDir)
	}
	cred, err := resolveExisting(credentialsDir)
	if err != nil {
		return nil, fmt.Errorf("keystore: resolve credentials dir: %w", err)
	}
	state, err := resolveExisting(stateDir)
	if err != nil {
		return nil, fmt.Errorf("keystore: resolve state dir: %w", err)
	}
	// The comparison is case-folded: filepath.Rel is lexical and
	// case-sensitive, so on a case-insensitive filesystem (macOS APFS
	// default) paths differing only in case would pass while physically
	// nesting. Folding over-rejects on case-sensitive volumes, which is
	// the fail-closed direction.
	credFold, stateFold := strings.ToLower(cred), strings.ToLower(state)
	if pathContains(credFold, stateFold) || pathContains(stateFold, credFold) {
		return nil, fmt.Errorf("keystore: credentials dir %s and state dir %s: %w",
			credentialsDir, stateDir, ErrCredentialsInsideStateDir)
	}
	return &Keystore{root: cred, dir: filepath.Join(cred, appDirName), mu: &sync.Mutex{}}, nil
}

// Dir returns the credentials root: the single authoritative path the
// checkpoint unit excludes from backup manifests and the workspace
// units keep out of every mount.
func (k *Keystore) Dir() string { return k.root }

// SaveApp persists one numeric owner's App credentials: the private key as
// PKCS#1 PEM and the registration metadata (including the display login,
// webhook, and client
// secrets, which are unrecoverable after the one-time manifest
// conversion) as JSON. Both files are written owner-only and their
// permissions re-asserted after the write, so a pre-existing wider file
// fails closed instead of silently keeping its mode.
func (k *Keystore) SaveApp(creds AppCredentials) error {
	k.mu.Lock()
	defer k.mu.Unlock()

	return k.saveAppLocked(creds, false, false)
}

// SaveAppPendingAuthority atomically binds the converted credentials to a
// recovery marker. Ordinary keystore consumers refuse the record until setup
// commits the matching authority and removes the marker.
func (k *Keystore) SaveAppPendingAuthority(creds AppCredentials) error {
	k.mu.Lock()
	defer k.mu.Unlock()

	return k.saveAppLocked(creds, false, true)
}

func (k *Keystore) saveAppLocked(
	creds AppCredentials,
	allowLegacy, authorityPending bool,
) error {
	if creds.Key == nil {
		return errors.New("keystore: refusing to save credentials without a private key")
	}
	// The same identity gate as the conversion boundary, held here too
	// because SaveApp is exported: an issuer-0 identity would overwrite
	// working credentials and fail every later mint.
	if creds.AppID <= 0 {
		return fmt.Errorf("keystore: refusing to save credentials with app id %d", creds.AppID)
	}
	if err := validateOwnerLogin(creds.Owner); err != nil {
		return err
	}
	ownerKey, err := appOwnerKey(creds.OwnerID)
	if err != nil {
		return err
	}
	if !creds.Visibility.valid() {
		return fmt.Errorf("keystore: refusing to save credentials with visibility %q", creds.Visibility)
	}
	creds.KeyID, err = appKeyID(creds.Key)
	if err != nil {
		return err
	}
	// Converge the target to a secure state before any secret bytes
	// land: the root chain is proven symlink-free (it was resolved at
	// construction, so a link appearing since is tampering), created
	// with every level fsynced, and narrowed. MkdirAll would otherwise
	// follow a planted symlink ancestor into the state surface, and a
	// pre-existing directory would keep its wider mode.
	if err := rejectNonDir(k.root); err != nil {
		return err
	}
	if err := mkdirAllSync(k.root); err != nil {
		return fmt.Errorf("keystore: create %s: %w", k.root, err)
	}
	if err := narrowDir(k.root); err != nil {
		return err
	}
	if err := rejectNonDir(k.dir); err != nil {
		return err
	}
	if err := mkdirAllSync(k.dir); err != nil {
		return fmt.Errorf("keystore: create %s: %w", k.dir, err)
	}
	if err := narrowDir(k.dir); err != nil {
		return err
	}
	if !allowLegacy {
		legacy, err := k.hasLegacyLayout()
		if err != nil {
			return err
		}
		if legacy {
			return ErrLegacyAppMigrationRequired
		}
	}

	appDir := filepath.Join(k.dir, ownerKey)
	if err := k.recoverSwap(appDir, false); err != nil {
		return err
	}
	if err := rejectNonDir(appDir); err != nil {
		return err
	}

	// Replacement is a recoverable journaled swap: the credential files and
	// optional recovery marker land in a fresh staging directory (each write
	// fsynced), the old directory is renamed aside, and the staged directory
	// is activated. An activation failure rolls the old directory back
	// immediately; a crash between the two renames is recovered by LoadApp
	// or the next SaveApp before any caller observes the store. The mutex
	// serializes loads with that short rename window in this daemon process.
	staging, old := appDir+".staging", appDir+".old"
	for _, leftover := range []string{staging, old} {
		if _, err := removeSwapLeftover(leftover); err != nil {
			return fmt.Errorf("keystore: clear leftover %s: %w", leftover, err)
		}
	}
	if err := os.Mkdir(staging, 0o700); err != nil {
		return fmt.Errorf("keystore: create staging: %w", err)
	}
	if err := atomicfile.SyncDir(k.dir); err != nil {
		return fmt.Errorf("keystore: sync staging entry: %w", err)
	}

	// The recovery marker lands first. A crash before it leaves an empty
	// incomplete stage; a crash after it can never leave complete credentials
	// that recovery mistakes for an ordinary finalized SaveApp.
	if authorityPending {
		if err := writeFileExclSync(
			filepath.Join(staging, authorityPendingFileName), nil,
		); err != nil {
			return fmt.Errorf("keystore: write pending authority marker: %w", err)
		}
	}

	keyPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(creds.Key),
	})
	if err := writeFileExclSync(filepath.Join(staging, keyFileName), keyPEM); err != nil {
		return fmt.Errorf("keystore: write key: %w", err)
	}

	// The inline struct is the one place the secrets are deliberately
	// revealed: real values persist only inside the protected
	// directory, and no named type ever holds them as plain strings.
	meta, err := json.MarshalIndent(struct { //nolint:gosec // the keystore's protected-storage write is the one sanctioned secret persistence
		Owner         string        `json:"owner"`
		OwnerID       int64         `json:"owner_id"`
		Visibility    AppVisibility `json:"visibility"`
		KeyID         string        `json:"key_id"`
		AppID         int64         `json:"app_id"`
		Name          string        `json:"name"`
		Slug          string        `json:"slug"`
		ClientID      string        `json:"client_id"`
		WebhookSecret string        `json:"webhook_secret"`
		ClientSecret  string        `json:"client_secret"`
	}{
		creds.Owner,
		creds.OwnerID,
		creds.Visibility,
		creds.KeyID,
		creds.AppID,
		creds.Name,
		creds.Slug,
		creds.ClientID,
		creds.WebhookSecret.Reveal(),
		creds.ClientSecret.Reveal(),
	}, "", "  ")
	if err != nil {
		return fmt.Errorf("keystore: encode metadata: %w", err)
	}
	if err := writeFileExclSync(filepath.Join(staging, metaFileName), meta); err != nil {
		return fmt.Errorf("keystore: write metadata: %w", err)
	}
	hadOld := false
	if _, err := os.Lstat(appDir); err == nil {
		if err := os.Rename(appDir, old); err != nil {
			return fmt.Errorf("keystore: set aside previous credentials: %w", err)
		}
		hadOld = true
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("keystore: lstat %s: %w", appDir, err)
	}
	if err := os.Rename(staging, appDir); err != nil {
		activateErr := fmt.Errorf("keystore: activate credentials: %w", err)
		if !hadOld {
			return activateErr
		}
		if rollbackErr := os.Rename(old, appDir); rollbackErr != nil {
			return errors.Join(activateErr, fmt.Errorf("keystore: restore previous credentials: %w", rollbackErr))
		}
		if rollbackErr := atomicfile.SyncDir(k.dir); rollbackErr != nil {
			return errors.Join(activateErr, fmt.Errorf("keystore: sync restored credentials: %w", rollbackErr))
		}
		return activateErr
	}
	if err := atomicfile.SyncDir(k.dir); err != nil {
		return fmt.Errorf("keystore: sync %s: %w", k.dir, err)
	}
	if _, err := removeSwapLeftover(old); err != nil {
		return fmt.Errorf("keystore: remove previous credentials: %w", err)
	}
	if err := atomicfile.SyncDir(k.dir); err != nil {
		return fmt.Errorf("keystore: sync previous-credential removal: %w", err)
	}

	return k.assertPermissionsAt(appDir)
}

// LoadApp reads one stable numeric owner's persisted credentials back,
// re-asserting the
// owner-only permissions first: a key reachable by group or other must
// be treated as exposed, so the load fails closed rather than trusting
// it. An unknown owner returns ErrNoAppRegistration. A legacy singleton
// returns ErrLegacyAppMigrationRequired until MigrateLegacyApp receives
// explicit owner login, numeric ID, and visibility attribution.
func (k *Keystore) LoadApp(ownerID int64) (AppCredentials, error) {
	k.mu.Lock()
	defer k.mu.Unlock()

	legacy, err := k.hasLegacyLayout()
	if err != nil {
		return AppCredentials{}, err
	}
	if legacy {
		return AppCredentials{}, ErrLegacyAppMigrationRequired
	}
	ownerKey, err := appOwnerKey(ownerID)
	if err != nil {
		return AppCredentials{}, err
	}
	appDir := filepath.Join(k.dir, ownerKey)
	exists, err := realDirExists(appDir)
	if err != nil {
		return AppCredentials{}, err
	}
	if !exists {
		staged, stageErr := realDirExists(appDir + ".staging")
		old, oldErr := realDirExists(appDir + ".old")
		if stageErr != nil {
			return AppCredentials{}, stageErr
		}
		if oldErr != nil {
			return AppCredentials{}, oldErr
		}
		if !staged && !old {
			return AppCredentials{}, ErrNoAppRegistration
		}
	}
	if err := k.recoverSwap(appDir, true); err != nil {
		return AppCredentials{}, err
	}
	creds, err := k.loadAppFrom(appDir)
	if err != nil {
		if errors.Is(err, ErrNoAppCredentials) {
			active, existsErr := realDirExists(appDir)
			if existsErr != nil {
				return AppCredentials{}, existsErr
			}
			if active {
				return AppCredentials{}, fmt.Errorf("keystore: registration %q is incomplete: %w", ownerKey, err)
			}
			return AppCredentials{}, ErrNoAppRegistration
		}
		return AppCredentials{}, err
	}
	if creds.OwnerID != ownerID {
		return AppCredentials{}, fmt.Errorf("keystore: registration owner id %d does not match lookup %d", creds.OwnerID, ownerID)
	}
	if creds.AuthorityPending {
		return AppCredentials{}, fmt.Errorf(
			"keystore: registration %d: %w", creds.AppID, ErrPendingAppAuthority,
		)
	}
	if err := k.clearSwapLeftovers(appDir); err != nil {
		return AppCredentials{}, err
	}
	return creds, nil
}

// ListApps enumerates every numeric-owner-keyed registration in stable ID order.
// A legacy singleton, and any entry that occupies a registration's name without
// being a real directory, fail closed rather than being skipped, since omission
// could make callers operate with an incomplete view. Only an entry whose name
// could never be a registration, and whose kind an operating system writes on
// its own (a regular file), is skipped; the legacy singleton's own file names
// are excluded from that skip whatever their kind. skippableEntry holds the
// rule, and UnexpectedEntries reports what it passed over.
func (k *Keystore) ListApps() ([]AppCredentials, error) {
	apps, err := k.listApps()
	if err != nil {
		return nil, err
	}
	for _, app := range apps {
		if app.AuthorityPending {
			return nil, fmt.Errorf(
				"keystore: registration %d: %w", app.AppID, ErrPendingAppAuthority,
			)
		}
	}
	return apps, nil
}

// ListAppsIncludingPendingAuthority is setup's recovery-only enumeration.
// Runtime consumers use ListApps, which fails closed on an unfinalized record.
func (k *Keystore) ListAppsIncludingPendingAuthority() ([]AppCredentials, error) {
	return k.listApps()
}

// FinalizeAppAuthority removes setup's recovery marker only after the caller
// has committed authority for the exact canonical registration.
func (k *Keystore) FinalizeAppAuthority(registration AppRegistration) error {
	if err := registration.validate(); err != nil {
		return err
	}
	k.mu.Lock()
	defer k.mu.Unlock()

	ownerKey, err := appOwnerKey(registration.OwnerID)
	if err != nil {
		return err
	}
	appDir := filepath.Join(k.dir, ownerKey)
	if err := k.recoverSwap(appDir, true); err != nil {
		return err
	}
	creds, err := k.loadRegistrationAt(appDir, registration.OwnerID)
	if err != nil {
		return err
	}
	if creds.Registration() != registration {
		return errors.New("keystore: pending authority registration identity changed")
	}
	if !creds.AuthorityPending {
		return nil
	}
	if err := os.Remove(filepath.Join(appDir, authorityPendingFileName)); err != nil {
		return fmt.Errorf("keystore: finalize pending authority: %w", err)
	}
	if err := atomicfile.SyncDir(appDir); err != nil {
		return fmt.Errorf("keystore: sync finalized authority marker: %w", err)
	}
	return nil
}

func (k *Keystore) listApps() ([]AppCredentials, error) {
	k.mu.Lock()
	defer k.mu.Unlock()

	legacy, err := k.hasLegacyLayout()
	if err != nil {
		return nil, err
	}
	if legacy {
		return nil, ErrLegacyAppMigrationRequired
	}
	return k.listAppsLocked()
}

func (k *Keystore) listAppsLocked() ([]AppCredentials, error) {
	exists, err := realDirExists(k.dir)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, nil
	}
	if err := assertMode(k.root, true); err != nil {
		return nil, err
	}
	if err := assertMode(k.dir, true); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(k.dir)
	if err != nil {
		return nil, fmt.Errorf("keystore: enumerate registrations: %w", err)
	}
	owners := make(map[int64]struct{})
	for _, entry := range entries {
		ownerID, named := registrationOwnerID(entry.Name())
		if !named {
			if skippableEntry(entry) {
				continue
			}
			return nil, fmt.Errorf("keystore: unexpected registration entry %s: %w", entry.Name(), ErrCredentialPermissions)
		}
		if entry.Type()&os.ModeSymlink != 0 || !entry.IsDir() {
			return nil, fmt.Errorf("keystore: registration %s is not a directory: %w", entry.Name(), ErrCredentialPermissions)
		}
		owners[ownerID] = struct{}{}
	}
	keys := make([]int64, 0, len(owners))
	for ownerID := range owners {
		keys = append(keys, ownerID)
	}
	slices.Sort(keys)

	apps := make([]AppCredentials, 0, len(keys))
	for _, ownerID := range keys {
		ownerKey, err := appOwnerKey(ownerID)
		if err != nil {
			return nil, err
		}
		appDir := filepath.Join(k.dir, ownerKey)
		// Failures from here on belong to one record, so each is attributed
		// to its owner: an unattributed error is what made a single damaged
		// record read as a broken keystore (#271). Attribution is not
		// automatic, though. It claims the record is unreadable, which routes
		// the operator to withdrawal, so it must hold only where withdrawal
		// can actually help: recovery both promotes a record and clears the
		// journals afterwards, and a cleanup failure after a successful
		// promotion leaves a registration that loads fine and would be
		// refused. Ask the record itself rather than trusting the error's
		// shape.
		if err := k.recoverSwap(appDir, true); err != nil {
			if _, loadErr := k.loadRegistrationAt(appDir, ownerID); loadErr == nil {
				return nil, err
			}
			return nil, k.attributeRecord(ownerID, appDir, err)
		}
		creds, err := k.loadRegistrationAt(appDir, ownerID)
		if err != nil {
			if errors.Is(err, ErrNoAppCredentials) {
				active, existsErr := realDirExists(appDir)
				if existsErr != nil {
					return nil, existsErr
				}
				if !active {
					// recoverSwap discarded an incomplete first-save stage;
					// there is no registration to enumerate.
					continue
				}
				return nil, k.attributeRecord(ownerID, appDir,
					fmt.Errorf("keystore: registration %q is incomplete: %w", ownerKey, err))
			}
			return nil, k.attributeRecord(ownerID, appDir, err)
		}
		// Deliberately not attributed as an unreadable record: the
		// registration loaded and bound, so withdrawal is the wrong remedy
		// for a leftover that will not clear, and routing the operator to it
		// would drop a working registration out of janitor coverage. The
		// error already names the leftover path.
		if err := k.clearSwapLeftovers(appDir); err != nil {
			return nil, err
		}
		apps = append(apps, creds)
	}
	return apps, nil
}

// registrationOwnerID reports the owner a keystore entry name registers, and
// false for a name that could never be one. The name carries the whole
// registration contract (a canonical numeric owner ID, plus at most one swap
// journal suffix), so failing it is not a damaged registration: it is not a
// registration at all.
//
// That distinction is what lets enumeration split by name rather than by entry
// kind. Skipping a registration narrows the set resolution picks among, which
// can resolve to the wrong App or miss an ambiguity, so an unreadable record
// still fails closed (#279). A name that could never be a registration was
// never in that set, so skipping it narrows nothing (#284).
func registrationOwnerID(name string) (int64, bool) {
	base := name
	switch {
	case strings.HasSuffix(base, ".staging"):
		base = strings.TrimSuffix(base, ".staging")
	case strings.HasSuffix(base, ".old"):
		base = strings.TrimSuffix(base, ".old")
	}
	if base == "" || strings.Contains(base, ".") {
		return 0, false
	}
	ownerID, err := strconv.ParseInt(base, 10, 64)
	if err != nil {
		return 0, false
	}
	ownerKey, err := appOwnerKey(ownerID)
	if err != nil || ownerKey != base {
		return 0, false
	}
	return ownerID, true
}

// skippableEntry reports whether an entry whose name could never be a
// registration may be passed over. Only a regular file qualifies, since that is
// the class an operating system writes on its own; a directory or symlink there
// is operator error, a half-finished migration, or tampering, and stays a hard
// error.
//
// The legacy singleton's own file names never qualify, whatever their kind.
// They are keystore state rather than foreign noise, and the legacy migration
// enumerates without the gate ListApps runs first, so skipping them would let a
// migration complete while a credential file it must refuse sits in the
// registration root.
// The name test is case-folded because the legacy gate's is: legacyFilesExist
// lstats app.pem, which resolves APP.PEM on the reference platform's
// case-insensitive filesystem while the directory entry keeps its own spelling.
// An exact comparison would skip that entry while the gate still saw a
// singleton, letting a migration complete over stranded key material and leave
// every later enumeration denied. Folding over-refuses on a case-sensitive
// volume, which is the fail-closed direction.
func skippableEntry(entry os.DirEntry) bool {
	for _, legacy := range []string{keyFileName, metaFileName} {
		if strings.EqualFold(entry.Name(), legacy) {
			return false
		}
	}
	return entry.Type().IsRegular()
}

// UnexpectedEntries reports the entries enumeration skips: regular files whose
// names could never be a registration. That is the class an operating system
// writes into a directory it merely displays (.DS_Store and its kin), which is
// why enumeration no longer fails on them; reporting them keeps a skipped entry
// visible to an operator rather than silently swallowed (#284). Names come back
// sorted, as os.ReadDir returns them, and carry no path: a caller diagnosing the
// keystore has its directory already.
func (k *Keystore) UnexpectedEntries() ([]string, error) {
	k.mu.Lock()
	defer k.mu.Unlock()

	exists, err := realDirExists(k.dir)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, nil
	}
	if err := assertMode(k.root, true); err != nil {
		return nil, err
	}
	if err := assertMode(k.dir, true); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(k.dir)
	if err != nil {
		return nil, fmt.Errorf("keystore: enumerate registrations: %w", err)
	}
	var skipped []string
	for _, entry := range entries {
		if _, named := registrationOwnerID(entry.Name()); named {
			continue
		}
		if !skippableEntry(entry) {
			continue
		}
		skipped = append(skipped, entry.Name())
	}
	return skipped, nil
}

// attributeRecord names the owner a per-record failure belongs to, unless
// the roots make every record unwithdrawable. Attribution is a promise
// that withdrawal is the remedy, so it must not be made where withdrawal
// is refused: routing an operator to a remedy that declines leaves
// enumeration blocked with nowhere to go. Every attribution in the
// enumeration loop passes through here, so the two cannot disagree about
// when the promise holds.
func (k *Keystore) attributeRecord(ownerID int64, appDir string, err error) error {
	// Ask the withdrawal's own preconditions rather than re-deriving them.
	// Every recurrence of this defect has been a condition present in one
	// side and absent from the other, so the two sides now run the same
	// checks and a new precondition is inherited by both.
	if reason := k.withdrawalUnavailable(appDir); reason != nil {
		return errors.Join(err, reason)
	}
	return &UnreadableRegistrationError{OwnerID: ownerID, Err: err}
}

// withdrawalUnavailable returns why a withdrawal of the record at appDir
// would be refused, or nil when it would be accepted. It is the preflight
// half of QuarantineApp: everything that method checks before it mutates
// anything.
func (k *Keystore) withdrawalUnavailable(appDir string) error {
	if err := k.assertWithdrawalDirs(); err != nil {
		return err
	}
	return k.assertWithdrawalTargets(appDir)
}

// QuarantineApp withdraws one unreadable registration from the enumerated
// set, so a record this host cannot load stops denying resolution for
// every other registration. It is deliberately an explicit operator act,
// not a silent skip inside enumeration: the record's installations remain
// live on GitHub and simply leave this host's view, which is a decision
// only the operator can make.
//
// Quarantine is not deletion. The key and metadata move intact to a
// sibling directory under the same credentials root, keeping them
// available for repair or forensics and inside the containment boundary
// that keeps App keys out of checkpoints and workspaces.
//
// It reaches every state that blocks enumeration, including a record
// whose persisted identity does not bind to its directory key and one
// left as an invalid recovery journal with no active directory; refusing
// either would route the operator to a remedy that declines the record
// enumeration rejected.
//
// A readable registration is refused, judged by the same gate enumeration
// applies: withdrawing a working registration would drop it from janitor
// coverage while its installations keep running, which is the fail-open
// direction. An existing quarantine record whose source is still present
// is a distinct earlier withdrawal and is refused rather than
// overwritten; one with nothing left to move is this owner's own
// withdrawal, which completes idempotently.
func (k *Keystore) QuarantineApp(ownerID int64) error {
	k.mu.Lock()
	defer k.mu.Unlock()

	// The directories gate everything below, so they are checked first: a
	// mode that blocks reads, traversal, or renames disables this remedy for
	// every record alike, and the legacy and record checks cannot even be
	// evaluated through an unsearchable root.
	if err := k.assertWithdrawalDirs(); err != nil {
		return err
	}
	quarantineDir := k.dir + quarantineDirSuffix

	legacy, err := k.hasLegacyLayout()
	if err != nil {
		return err
	}
	if legacy {
		return ErrLegacyAppMigrationRequired
	}
	ownerKey, err := appOwnerKey(ownerID)
	if err != nil {
		return err
	}
	appDir := filepath.Join(k.dir, ownerKey)
	// Settle the swap journals first, so quarantine acts on the same state
	// enumeration would have loaded rather than on a half-finished save. A
	// recovery that itself fails is not a reason to refuse: an invalid
	// journal is one of the states that blocks enumeration, and withdrawal
	// is precisely its remedy.
	recoverErr := k.recoverSwap(appDir, true)
	sources, err := k.quarantineSources(appDir)
	if err != nil {
		return err
	}
	if len(sources) == 0 {
		if recoverErr != nil {
			return recoverErr
		}
		// Nothing left to move means either this owner was never here or an
		// earlier attempt moved everything and then failed at the durability
		// barrier. A failing sync cannot be journalled past, since the
		// journal write carries the same barrier, so the tractable guarantee
		// is that a retry finishes the sequence rather than reporting a
		// missing record over a withdrawal that already happened. Re-issuing
		// both syncs is idempotent, and reporting success is honest: the
		// postcondition, withdrawn and preserved, holds.
		withdrawn, err := k.quarantineHoldsRecord(quarantineDir, ownerKey)
		if err != nil {
			return err
		}
		if !withdrawn {
			return ErrNoAppRegistration
		}
		return k.syncWithdrawal(quarantineDir)
	}
	if _, err := k.loadRegistrationAt(appDir, ownerID); err == nil {
		return fmt.Errorf(
			"keystore: registration for owner %d is readable; quarantine withdraws only unreadable records",
			ownerID,
		)
	}

	if err := k.assertWithdrawalTargets(appDir); err != nil {
		return err
	}
	if err := mkdirAllSync(quarantineDir); err != nil {
		return fmt.Errorf("keystore: create quarantine directory: %w", err)
	}
	activePresent := slices.Contains(sources, appDir)
	for _, source := range sources {
		if source == appDir {
			continue
		}
		target := filepath.Join(quarantineDir, filepath.Base(source))
		if err := os.Rename(source, target); err != nil {
			return fmt.Errorf("keystore: quarantine registration for owner %d: %w", ownerID, err)
		}
		// Persist each journal removal before the next rename. Rename order
		// alone does not survive a crash, and a journal left on the source
		// side is promotion-capable: with no active directory, recovery
		// adopts a valid .old or .staging and resurrects the registration
		// the operator withdrew. Syncing here bounds the exposure to the one
		// rename in flight rather than any earlier one, and subsumes the
		// barrier the active record would otherwise need.
		if err := k.syncWithdrawal(quarantineDir); err != nil {
			return err
		}
	}
	if activePresent {
		target := filepath.Join(quarantineDir, filepath.Base(appDir))
		if err := os.Rename(appDir, target); err != nil {
			return fmt.Errorf("keystore: quarantine registration for owner %d: %w", ownerID, err)
		}
	}
	return k.syncWithdrawal(quarantineDir)
}

// syncWithdrawal persists the withdrawal's two directory changes.
// Destination first: this is a cross-directory rename, so persisting the
// source's removal before the destination's new entry would make a crash
// between them lose the credential outright. In this order the only
// crash-side divergence is a stale source entry, which still holds the
// record and is refused rather than silently overwritten on the retry.
// Re-issuing both is idempotent, so a retry after a transient sync failure
// completes the sequence.
func (k *Keystore) syncWithdrawal(quarantineDir string) error {
	if err := atomicfile.SyncDir(quarantineDir); err != nil {
		return fmt.Errorf("keystore: sync quarantine directory: %w", err)
	}
	if err := atomicfile.SyncDir(k.dir); err != nil {
		return fmt.Errorf("keystore: sync quarantined registration: %w", err)
	}
	return nil
}

// quarantineHoldsRecord reports whether any piece of one owner's record is
// already withdrawn, which distinguishes a completed-but-unsynced
// withdrawal from an owner that was never present.
func (k *Keystore) quarantineHoldsRecord(quarantineDir, ownerKey string) (bool, error) {
	for _, suffix := range []string{"", ".staging", ".old"} {
		exists, err := realDirExists(filepath.Join(quarantineDir, ownerKey+suffix))
		if err != nil {
			return false, err
		}
		if exists {
			return true, nil
		}
	}
	return false, nil
}

// assertWithdrawalRoot rejects a root the withdrawal cannot operate
// through, in either direction: too permissive to trust, or missing an
// owner bit it needs.
func (k *Keystore) assertWithdrawalRoot(dir string) error {
	if err := assertMode(dir, true); err != nil {
		return err
	}
	usable, err := rootUsable(dir)
	if err != nil {
		return err
	}
	if !usable {
		return fmt.Errorf("keystore: %s lacks owner access: %w", dir, ErrCredentialPermissions)
	}
	return nil
}

// quarantineSources lists any swap journals recoverSwap left behind and
// the active registration directory, in that order. All of them move
// together: leaving a .staging or .old sibling would let the next
// enumeration promote the record back into the active set, undoing the
// withdrawal.
//
// The order is load-bearing, not cosmetic. Moving the active directory
// last means a failure part-way through always leaves the active
// directory in place, so recovery never sees the journals-without-active
// shape it would promote, and the retry finds free destinations for
// whatever is left. Moving it first would let a later failure strand the
// record: recovery would promote a journal into the active slot the retry
// then finds already occupied in quarantine, wedging the only remedy.
func (k *Keystore) quarantineSources(appDir string) ([]string, error) {
	var sources []string
	for _, candidate := range []string{appDir + ".staging", appDir + ".old", appDir} {
		exists, err := realDirExists(candidate)
		if err != nil {
			return nil, err
		}
		if exists {
			sources = append(sources, candidate)
		}
	}
	return sources, nil
}

// MigrateLegacyApp relocates the former singleton only after the caller
// explicitly supplies the owner login, numeric ID, and visibility that its
// metadata could not record. Repeating the migration after a partial failure
// is safe.
func (k *Keystore) MigrateLegacyApp(owner string, ownerID int64, visibility AppVisibility) (AppCredentials, error) {
	k.mu.Lock()
	defer k.mu.Unlock()

	legacy, err := k.hasLegacyLayout()
	if err != nil {
		return AppCredentials{}, err
	}
	if !legacy {
		return AppCredentials{}, ErrNoAppCredentials
	}
	if err := validateOwnerLogin(owner); err != nil {
		return AppCredentials{}, err
	}
	if _, err := appOwnerKey(ownerID); err != nil {
		return AppCredentials{}, err
	}
	if !visibility.valid() {
		return AppCredentials{}, fmt.Errorf("keystore: invalid legacy registration visibility %q", visibility)
	}
	legacyDir := k.dir + ".legacy"
	sourceDir, err := k.legacySourceDir()
	if err != nil {
		return AppCredentials{}, err
	}
	creds, err := k.loadLegacyAppFrom(sourceDir)
	if err != nil {
		if sourceDir == k.dir+".staging" {
			if _, clearErr := removeSwapLeftover(sourceDir); clearErr != nil {
				return AppCredentials{}, errors.Join(
					fmt.Errorf("keystore: validate legacy staging credentials: %w", err),
					fmt.Errorf("keystore: discard incomplete legacy staging credentials: %w", clearErr),
				)
			}
			if syncErr := atomicfile.SyncDir(k.root); syncErr != nil {
				return AppCredentials{}, fmt.Errorf("keystore: sync discarded legacy staging credentials: %w", syncErr)
			}
			return AppCredentials{}, ErrNoAppCredentials
		}
		return AppCredentials{}, err
	}
	creds.Owner = owner
	creds.OwnerID = ownerID
	creds.Visibility = visibility
	creds.Name = creds.Slug
	creds.KeyID, err = appKeyID(creds.Key)
	if err != nil {
		return AppCredentials{}, err
	}
	if sourceDir != legacyDir {
		if err := os.Rename(sourceDir, legacyDir); err != nil {
			return AppCredentials{}, fmt.Errorf("keystore: journal legacy credentials: %w", err)
		}
		if err := atomicfile.SyncDir(k.root); err != nil {
			return AppCredentials{}, fmt.Errorf("keystore: sync legacy journal: %w", err)
		}
	}
	apps, err := k.listAppsLocked()
	if err != nil {
		return AppCredentials{}, err
	}
	if len(apps) > 1 {
		return AppCredentials{}, errors.New("keystore: legacy migration journal conflicts with multiple registrations")
	}
	if len(apps) == 1 {
		existing := apps[0]
		if existing.AuthorityPending {
			return AppCredentials{}, fmt.Errorf(
				"keystore: registration %d: %w",
				existing.AppID,
				ErrPendingAppAuthority,
			)
		}
		if existing.AppID != creds.AppID || existing.KeyID != creds.KeyID {
			return AppCredentials{}, errors.New("keystore: legacy migration journal conflicts with an existing registration")
		}
		if existing.OwnerID != ownerID || !strings.EqualFold(existing.Owner, owner) || existing.Visibility != visibility {
			return AppCredentials{}, fmt.Errorf(
				"keystore: legacy migration already attributed to owner %q (%d) with visibility %q",
				existing.Owner,
				existing.OwnerID,
				existing.Visibility,
			)
		}
		if err := k.clearLegacyJournals(); err != nil {
			return AppCredentials{}, err
		}
		return existing, nil
	}
	if err := k.saveAppLocked(creds, true, false); err != nil {
		return AppCredentials{}, err
	}
	if err := k.clearLegacyJournals(); err != nil {
		return AppCredentials{}, err
	}
	return creds, nil
}

// loadAppFrom validates and reconstructs one complete credential
// directory. It is shared by the active store and crash recovery so a
// staged directory is promoted only when it passes the same trust gate
// as ordinary persisted state.
func (k *Keystore) loadAppFrom(dir string) (AppCredentials, error) {
	keyPath := filepath.Join(dir, keyFileName)
	if _, err := os.Lstat(keyPath); errors.Is(err, fs.ErrNotExist) {
		return AppCredentials{}, ErrNoAppCredentials
	} else if err != nil {
		return AppCredentials{}, fmt.Errorf("keystore: lstat key: %w", err)
	}
	if err := k.assertPermissionsAt(dir); err != nil {
		return AppCredentials{}, err
	}

	// G304: both paths are composed from the keystore's own validated
	// root, never from external input.
	keyPEM, err := os.ReadFile(keyPath) //nolint:gosec // path is keystore-internal, derived from the validated credentials root
	if err != nil {
		return AppCredentials{}, fmt.Errorf("keystore: read key: %w", err)
	}
	block, _ := pem.Decode(keyPEM)
	if block == nil || block.Type != "RSA PRIVATE KEY" {
		return AppCredentials{}, errors.New("keystore: key file is not an RSA PRIVATE KEY PEM")
	}
	key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		return AppCredentials{}, fmt.Errorf("keystore: parse key: %w", err)
	}

	metaRaw, err := os.ReadFile(filepath.Join(dir, metaFileName)) //nolint:gosec // path is keystore-internal, derived from the validated credentials root
	if err != nil {
		return AppCredentials{}, fmt.Errorf("keystore: read metadata: %w", err)
	}
	var meta appMetadata
	if err := json.Unmarshal(metaRaw, &meta); err != nil {
		return AppCredentials{}, fmt.Errorf("keystore: decode metadata: %w", err)
	}
	if meta.AppID <= 0 {
		return AppCredentials{}, fmt.Errorf("keystore: persisted credentials have invalid app id %d", meta.AppID)
	}
	if err := validateOwnerLogin(meta.Owner); err != nil {
		return AppCredentials{}, fmt.Errorf("keystore: persisted credentials: %w", err)
	}
	if _, err := appOwnerKey(meta.OwnerID); err != nil {
		return AppCredentials{}, fmt.Errorf("keystore: persisted credentials: %w", err)
	}
	if !meta.Visibility.valid() {
		return AppCredentials{}, fmt.Errorf("keystore: persisted credentials have invalid visibility %q", meta.Visibility)
	}
	keyID, err := appKeyID(key)
	if err != nil {
		return AppCredentials{}, err
	}
	if meta.KeyID == "" || meta.KeyID != keyID {
		return AppCredentials{}, errors.New("keystore: persisted key id does not match the private key")
	}
	authorityPending := false
	pendingPath := filepath.Join(dir, authorityPendingFileName)
	if info, err := os.Lstat(pendingPath); err == nil {
		if !info.Mode().IsRegular() || info.Size() != 0 {
			return AppCredentials{}, fmt.Errorf(
				"keystore: invalid pending authority marker: %w", ErrCredentialPermissions,
			)
		}
		authorityPending = true
	} else if !errors.Is(err, fs.ErrNotExist) {
		return AppCredentials{}, fmt.Errorf("keystore: inspect pending authority marker: %w", err)
	}

	return AppCredentials{
		Owner:            meta.Owner,
		OwnerID:          meta.OwnerID,
		Visibility:       meta.Visibility,
		KeyID:            meta.KeyID,
		AppID:            meta.AppID,
		Name:             meta.Name,
		Slug:             meta.Slug,
		ClientID:         meta.ClientID,
		Key:              key,
		WebhookSecret:    meta.WebhookSecret,
		ClientSecret:     meta.ClientSecret,
		AuthorityPending: authorityPending,
	}, nil
}

// loadRegistrationAt is the keystore's one readability gate: the record
// must load, and its persisted identity must bind to the directory key it
// was found under. Enumeration and quarantine share it so they cannot
// disagree about which records are usable; when they did, the doctor
// could route an operator to quarantine a record quarantine then refused,
// leaving resolution blocked with no remedy.
func (k *Keystore) loadRegistrationAt(appDir string, ownerID int64) (AppCredentials, error) {
	creds, err := k.loadAppFrom(appDir)
	if err != nil {
		return AppCredentials{}, err
	}
	if creds.OwnerID != ownerID {
		return AppCredentials{}, fmt.Errorf(
			"keystore: registration directory %q does not match persisted owner id %d",
			filepath.Base(appDir),
			creds.OwnerID,
		)
	}
	return creds, nil
}

// recoverSwap repairs the only two crash states in which no active
// directory is visible. A previous directory wins over a staged one;
// without a previous version, a staged directory is promoted only after
// its key, metadata, permissions, and identity all validate.
func (k *Keystore) recoverSwap(appDir string, promoteStaging bool) error {
	active, err := realDirExists(appDir)
	if err != nil || active {
		return err
	}
	old, err := realDirExists(appDir + ".old")
	if err != nil {
		return err
	}
	staging, err := realDirExists(appDir + ".staging")
	if err != nil {
		return err
	}

	source := ""
	if old {
		source = appDir + ".old"
	} else if staging {
		// LoadApp has no replacement value and must salvage a complete
		// first-registration stage. SaveApp already holds a fresh value;
		// discarding an incomplete prior stage lets the new registration
		// replace it without manual filesystem cleanup.
		if !promoteStaging {
			return k.clearSwapLeftovers(appDir)
		}
		source = appDir + ".staging"
	} else {
		return nil
	}
	if _, err := k.loadAppFrom(source); err != nil {
		if source == appDir+".staging" {
			if clearErr := k.clearSwapLeftovers(appDir); clearErr != nil {
				return errors.Join(fmt.Errorf("keystore: validate recoverable credentials: %w", err), clearErr)
			}
			return nil
		}
		return fmt.Errorf("keystore: validate recoverable credentials: %w", err)
	}
	if err := os.Rename(source, appDir); err != nil {
		return fmt.Errorf("keystore: recover active credentials: %w", err)
	}
	if err := atomicfile.SyncDir(filepath.Dir(appDir)); err != nil {
		return fmt.Errorf("keystore: sync recovered credentials: %w", err)
	}
	return k.clearSwapLeftovers(appDir)
}

func (k *Keystore) clearSwapLeftovers(appDir string) error {
	removed := false
	for _, leftover := range []string{appDir + ".staging", appDir + ".old"} {
		removedOne, err := removeSwapLeftover(leftover)
		if err != nil {
			return fmt.Errorf("keystore: clear leftover %s: %w", leftover, err)
		}
		removed = removed || removedOne
	}
	if removed {
		if err := atomicfile.SyncDir(filepath.Dir(appDir)); err != nil {
			return fmt.Errorf("keystore: sync leftover removal: %w", err)
		}
	}
	return nil
}

// removeSwapLeftover removes one journal entry without following a planted
// symlink. A real directory may be owner-only but non-writable after a restore;
// restore owner access before walking it so cleanup cannot fail after activation.
func removeSwapLeftover(path string) (bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if info.Mode()&os.ModeSymlink == 0 && info.IsDir() {
		if err := os.Chmod(path, 0o700); err != nil { //nolint:gosec // directory needs owner traversal for safe recursive cleanup
			return false, err
		}
	}
	if err := os.RemoveAll(path); err != nil {
		return false, err
	}
	return true, nil
}

func realDirExists(path string) (bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("keystore: lstat %s: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return false, fmt.Errorf("keystore: %s is not a real directory: %w", path, ErrCredentialPermissions)
	}
	return true, nil
}

func (k *Keystore) assertPermissionsAt(appDir string) error {
	for _, dir := range []string{k.root, k.dir, appDir} {
		if err := assertMode(dir, true); err != nil {
			return err
		}
	}
	for _, name := range []string{keyFileName, metaFileName, authorityPendingFileName} {
		path := filepath.Join(appDir, name)
		if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err := assertMode(path, false); err != nil {
			return err
		}
	}
	return nil
}

func (k *Keystore) hasLegacyLayout() (bool, error) {
	for _, journal := range []string{k.dir + ".legacy", k.dir + ".old", k.dir + ".staging"} {
		if exists, err := realDirExists(journal); err != nil {
			return false, err
		} else if exists {
			return true, nil
		}
	}
	return legacyFilesExist(k.dir)
}

func (k *Keystore) legacySourceDir() (string, error) {
	legacyDir := k.dir + ".legacy"
	// A migration journal is already the selected source. Otherwise the
	// active singleton wins over its old SaveApp journals, preserving the
	// pre-upgrade recovery rule that an activated registration is current.
	if exists, err := realDirExists(legacyDir); err != nil {
		return "", err
	} else if exists {
		return legacyDir, nil
	}
	if exists, err := legacyFilesExist(k.dir); err != nil {
		return "", err
	} else if exists {
		return k.dir, nil
	}
	for _, journal := range []string{k.dir + ".old", k.dir + ".staging"} {
		if exists, err := realDirExists(journal); err != nil {
			return "", err
		} else if exists {
			return journal, nil
		}
	}
	return "", ErrNoAppCredentials
}

func legacyFilesExist(dir string) (bool, error) {
	info, err := os.Lstat(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("keystore: inspect registration root: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return false, fmt.Errorf("keystore: %s is not a real directory: %w", dir, ErrCredentialPermissions)
	}
	for _, name := range []string{keyFileName, metaFileName} {
		_, err := os.Lstat(filepath.Join(dir, name))
		if err == nil {
			return true, nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return false, fmt.Errorf("keystore: inspect legacy %s: %w", name, err)
		}
	}
	return false, nil
}

func (k *Keystore) clearLegacyJournals() error {
	removed := false
	for _, journal := range []string{k.dir + ".legacy", k.dir + ".old", k.dir + ".staging"} {
		removedOne, err := removeSwapLeftover(journal)
		if err != nil {
			return fmt.Errorf("keystore: clear legacy journal %s: %w", journal, err)
		}
		removed = removed || removedOne
	}
	if removed {
		if err := atomicfile.SyncDir(k.root); err != nil {
			return fmt.Errorf("keystore: sync legacy migration: %w", err)
		}
	}
	return nil
}

func (k *Keystore) loadLegacyAppFrom(dir string) (AppCredentials, error) {
	if err := assertMode(k.root, true); err != nil {
		return AppCredentials{}, err
	}
	if err := assertMode(dir, true); err != nil {
		return AppCredentials{}, err
	}
	for _, name := range []string{keyFileName, metaFileName} {
		if err := assertMode(filepath.Join(dir, name), false); err != nil {
			return AppCredentials{}, err
		}
	}
	keyPEM, err := os.ReadFile(filepath.Join(dir, keyFileName)) //nolint:gosec // legacy path is fixed beneath the validated credentials root
	if err != nil {
		return AppCredentials{}, fmt.Errorf("keystore: read legacy key: %w", err)
	}
	block, _ := pem.Decode(keyPEM)
	if block == nil || block.Type != "RSA PRIVATE KEY" {
		return AppCredentials{}, errors.New("keystore: legacy key file is not an RSA PRIVATE KEY PEM")
	}
	key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		return AppCredentials{}, fmt.Errorf("keystore: parse legacy key: %w", err)
	}
	metaRaw, err := os.ReadFile(filepath.Join(dir, metaFileName)) //nolint:gosec // legacy path is fixed beneath the validated credentials root
	if err != nil {
		return AppCredentials{}, fmt.Errorf("keystore: read legacy metadata: %w", err)
	}
	var meta legacyAppMetadata
	if err := json.Unmarshal(metaRaw, &meta); err != nil {
		return AppCredentials{}, fmt.Errorf("keystore: decode legacy metadata: %w", err)
	}
	if meta.AppID <= 0 {
		return AppCredentials{}, fmt.Errorf("keystore: legacy credentials have invalid app id %d", meta.AppID)
	}
	return AppCredentials{
		AppID:         meta.AppID,
		Slug:          meta.Slug,
		ClientID:      meta.ClientID,
		Key:           key,
		WebhookSecret: meta.WebhookSecret,
		ClientSecret:  meta.ClientSecret,
	}, nil
}

// withdrawalAvailable reports whether a withdrawal would be accepted at
// all: every directory it operates through, judged by the same gate the
// withdrawal itself applies. A mode deviation in either direction
// disqualifies a directory — too permissive to trust with credential
// material, or missing an owner bit the withdrawal needs — and either way
// it disables the remedy for every record alike, so a failure under it is
// keystore-wide and must not be attributed to whichever record met it.
//
// narrowDir deliberately lets a caller keep a directory tighter than 0700,
// so an under-permissive one is a supported state rather than corruption;
// it simply means the remedy is unavailable until the operator restores
// access.
func (k *Keystore) assertWithdrawalDirs() error {
	if err := k.assertWithdrawalRoot(k.root); err != nil {
		return err
	}
	// Ordered deliberately: the registration root cannot be examined through
	// an unsearchable credentials root, and neither can the quarantine
	// directory beside it.
	exists, err := realDirExists(k.dir)
	if err != nil {
		return err
	}
	if !exists {
		return ErrNoAppRegistration
	}
	if err := k.assertWithdrawalRoot(k.dir); err != nil {
		return err
	}
	quarantineDir := k.dir + quarantineDirSuffix
	quarantineExists, err := realDirExists(quarantineDir)
	if err != nil {
		return err
	}
	if !quarantineExists {
		// Absent is fine: it is created through the credentials root, which
		// this function already accepted.
		return nil
	}
	// The destination is as load-bearing as the sources: one the owner
	// cannot write into fails every rename, and one that is too permissive
	// would hold credential material exposed.
	return k.assertWithdrawalRoot(quarantineDir)
}

// assertWithdrawalTargets rejects the destinations a withdrawal of the
// record at appDir would occupy. It runs after recovery has settled the
// swap journals, since recovery changes which pieces exist. A destination
// whose source still exists is a distinct earlier withdrawal for the same
// owner, and overwriting it would be the loss this path exists to avoid; a
// piece an interrupted attempt already moved is absent from the source
// list entirely, so its destination is never examined and the retry
// resumes. A malformed destination — a file or symlink where a directory
// belongs — fails the same way.
func (k *Keystore) assertWithdrawalTargets(appDir string) error {
	sources, err := k.quarantineSources(appDir)
	if err != nil {
		return err
	}
	quarantineDir := k.dir + quarantineDirSuffix
	for _, source := range sources {
		target := filepath.Join(quarantineDir, filepath.Base(source))
		exists, err := realDirExists(target)
		if err != nil {
			return err
		}
		if exists {
			return fmt.Errorf("keystore: quarantine record already exists at %s: %w", target, ErrCredentialPermissions)
		}
	}
	return nil
}

func rootUsable(dir string) (bool, error) {
	info, err := os.Lstat(dir)
	if err != nil {
		return false, fmt.Errorf("keystore: lstat %s: %w", dir, err)
	}
	return info.Mode().Perm()&0o700 == 0o700, nil
}

func assertMode(path string, dir bool) error {
	// Lstat, not Stat: a symlink in the keystore would carry reads and
	// writes outside the validated credentials root, so it must fail
	// the kind check rather than be followed to its target's mode.
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("keystore: lstat %s: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || info.IsDir() != dir {
		return fmt.Errorf("keystore: %s is not the expected kind: %w", path, ErrCredentialPermissions)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("keystore: %s is mode %04o: %w", path, info.Mode().Perm(), ErrCredentialPermissions)
	}
	return nil
}

// narrowDir strips any group/other bits from an existing directory
// without touching owner bits: it removes exposure (the round-1
// containment fix) but never re-widens a mode a caller set tighter,
// so a deliberately restricted root stays restricted.
func narrowDir(dir string) error {
	info, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("keystore: stat %s: %w", dir, err)
	}
	if info.Mode().Perm()&0o077 == 0 {
		return nil
	}
	if err := os.Chmod(dir, info.Mode().Perm()&^0o077); err != nil {
		return fmt.Errorf("keystore: narrow %s: %w", dir, err)
	}
	return nil
}

// rejectNonDir fails closed when path exists but is not a real
// directory: a symlink here would relocate the keystore's writes onto
// the checkpoint or workspace surfaces the containment invariant
// excludes.
func rejectNonDir(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("keystore: lstat %s: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("keystore: %s is not a real directory: %w", path, ErrCredentialPermissions)
	}
	return nil
}

// writeFileExclSync creates path exclusively (no pre-existing inode of
// any kind is written through), owner-only, and syncs both the file
// and its directory entry so the credential survives a crash.
func writeFileExclSync(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // keystore-internal path under the validated credentials root
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return atomicfile.SyncDir(filepath.Dir(path))
}

// mkdirAllSync creates dir owner-only and fsyncs every newly created
// level plus the pre-existing ancestor, so the whole chain of
// directory entries is durable: losing a freshly created keystore
// ancestor on a crash would lose the key it contains. The chain is
// first proven symlink-free: the keystore's paths were resolved at
// construction, so a link that has appeared in any component since is
// tampering aimed at relocating the writes, and MkdirAll would follow
// it.
func mkdirAllSync(dir string) error {
	prefix := string(filepath.Separator)
	for part := range strings.SplitSeq(strings.TrimPrefix(filepath.Clean(dir), string(filepath.Separator)), string(filepath.Separator)) {
		prefix = filepath.Join(prefix, part)
		info, err := os.Lstat(prefix)
		if errors.Is(err, fs.ErrNotExist) {
			break // the rest is ours to create
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("keystore: %s is a symlink: %w", prefix, ErrCredentialPermissions)
		}
	}

	base := dir
	for {
		if _, err := os.Lstat(base); err == nil {
			break
		} else if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		parent := filepath.Dir(base)
		if parent == base {
			break
		}
		base = parent
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	for cur := dir; ; cur = filepath.Dir(cur) {
		if err := atomicfile.SyncDir(cur); err != nil {
			return err
		}
		if cur == base || filepath.Dir(cur) == cur {
			return nil
		}
	}
}

// AppVisibility records whether GitHub permits installations outside the
// registration owner. It is metadata, not an authority decision.
type AppVisibility string

const (
	AppVisibilityPrivate AppVisibility = "private"
	AppVisibilityPublic  AppVisibility = "public"
)

// AllAppVisibilities is the single registration point for valid visibility
// values.
var AllAppVisibilities = []AppVisibility{AppVisibilityPrivate, AppVisibilityPublic}

func (v AppVisibility) valid() bool {
	switch v {
	case AppVisibilityPrivate, AppVisibilityPublic:
		return true
	default:
		return false
	}
}

// AppCredentials is the registered GitHub App's identity, stable numeric
// owner, display login, and key
// material, produced by the manifest conversion and round-tripped
// through the keystore. The secrets are Secret-typed, so they redact
// everywhere except the keystore's deliberate persistence writes; the
// struct carries its own Format/String/GoString/MarshalJSON because
// *rsa.PrivateKey has exported fields that fmt and encoding/json would
// otherwise print.
type AppCredentials struct {
	Owner         string
	OwnerID       int64
	Visibility    AppVisibility
	KeyID         string
	AppID         int64
	Name          string
	Slug          string
	ClientID      string
	Key           *rsa.PrivateKey
	WebhookSecret Secret
	ClientSecret  Secret
	// AuthorityPending is a durable setup recovery marker. Runtime consumers
	// receive ErrPendingAppAuthority instead of credentials while it is true.
	AuthorityPending bool
}

// String renders the public identity only; the key and secrets redact.
func (c AppCredentials) String() string {
	return fmt.Sprintf("publish.AppCredentials{Owner:%q, OwnerID:%d, Visibility:%q, KeyID:%q, AppID:%d, Name:%q, Slug:%q, ClientID:%q, Key:%s, WebhookSecret:%s, ClientSecret:%s}",
		c.Owner, c.OwnerID, c.Visibility, c.KeyID, c.AppID, c.Name, c.Slug, c.ClientID, redacted, redacted, redacted)
}

// GoString keeps %#v as redacted as %v.
func (c AppCredentials) GoString() string { return c.String() }

// Format covers every fmt verb, since a non-string verb like %x would
// otherwise walk the struct fields — including the RSA key's exported
// big integers — without consulting String.
func (c AppCredentials) Format(f fmt.State, _ rune) {
	io.WriteString(f, c.String()) //nolint:errcheck,gosec // fmt.State writes cannot be usefully handled
}

// MarshalJSON emits the identity with every sensitive field redacted;
// real persistence is the keystore's explicit appMetadata write, never
// a marshal of this struct.
func (c AppCredentials) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Owner         string        `json:"owner"`
		OwnerID       int64         `json:"owner_id"`
		Visibility    AppVisibility `json:"visibility"`
		KeyID         string        `json:"key_id"`
		AppID         int64         `json:"app_id"`
		Name          string        `json:"name"`
		Slug          string        `json:"slug"`
		ClientID      string        `json:"client_id"`
		Key           string        `json:"key"`
		WebhookSecret string        `json:"webhook_secret"`
		ClientSecret  string        `json:"client_secret"`
	}{
		c.Owner,
		c.OwnerID,
		c.Visibility,
		c.KeyID,
		c.AppID,
		c.Name,
		c.Slug,
		c.ClientID,
		redacted,
		redacted,
		redacted,
	})
}

// appMetadata is the decoded shape of the on-disk credential metadata.
// Its secret fields are Secret-typed so an in-memory copy redacts like
// every other; the persistence write in SaveApp uses its own inline
// struct with explicit Reveal calls instead.
type appMetadata struct {
	Owner         string        `json:"owner"`
	OwnerID       int64         `json:"owner_id"`
	Visibility    AppVisibility `json:"visibility"`
	KeyID         string        `json:"key_id"`
	AppID         int64         `json:"app_id"`
	Name          string        `json:"name"`
	Slug          string        `json:"slug"`
	ClientID      string        `json:"client_id"`
	WebhookSecret Secret        `json:"webhook_secret"`
	ClientSecret  Secret        `json:"client_secret"`
}

type legacyAppMetadata struct {
	AppID         int64  `json:"app_id"`
	Slug          string `json:"slug"`
	ClientID      string `json:"client_id"`
	WebhookSecret Secret `json:"webhook_secret"`
	ClientSecret  Secret `json:"client_secret"`
}

func validateOwnerLogin(owner string) error {
	if owner == "" || strings.TrimSpace(owner) != owner {
		return errors.New("keystore: registration owner is empty or has surrounding whitespace")
	}
	if len(owner) > 39 {
		return fmt.Errorf("keystore: registration owner %q exceeds GitHub's login limit", owner)
	}
	for i, char := range owner {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') {
			continue
		}
		if char == '-' && i > 0 && i < len(owner)-1 {
			continue
		}
		return fmt.Errorf("keystore: registration owner %q is not a GitHub login", owner)
	}
	return nil
}

func appOwnerKey(ownerID int64) (string, error) {
	if ownerID <= 0 {
		return "", fmt.Errorf("keystore: registration owner id %d is invalid", ownerID)
	}
	return strconv.FormatInt(ownerID, 10), nil
}

func appKeyID(key *rsa.PrivateKey) (string, error) {
	if key == nil {
		return "", errors.New("keystore: cannot fingerprint a nil private key")
	}
	publicDER, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return "", fmt.Errorf("keystore: encode public key for fingerprint: %w", err)
	}
	digest := sha256.Sum256(publicDER)
	return "SHA256:" + base64.StdEncoding.EncodeToString(digest[:]), nil
}

// resolveExisting makes path absolute and resolves symlinks through its
// deepest existing ancestor, rejoining any not-yet-created remainder,
// so containment comparisons see the real filesystem locations even
// before the directories exist. Any error other than non-existence
// fails the resolution (and so the construction) closed.
func resolveExisting(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	rest := ""
	for cur := filepath.Clean(abs); ; {
		resolved, err := filepath.EvalSymlinks(cur)
		if err == nil {
			return filepath.Join(resolved, rest), nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return "", fmt.Errorf("no existing ancestor for %s", abs)
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = parent
	}
}

// pathContains reports whether inner is outer itself or nested anywhere
// beneath it; both arguments must already be absolute and resolved.
func pathContains(outer, inner string) bool {
	rel, err := filepath.Rel(outer, inner)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}
