package publish

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
)

const (
	// installationAuthorityFileName is the operator-authored snapshot. The
	// daemon only ever reads it: authoring and promotion belong to onboarding
	// (#238), and a daemon that rewrote it could not be told apart from one
	// whose authority had been tampered with.
	installationAuthorityFileName = "installation-authority.json"

	// installationJanitorJournalFileName is the daemon-owned audit log and
	// quarantine set.
	installationJanitorJournalFileName = "installation-janitor-journal.json"

	// installationJanitorLockFileName serializes the journal's
	// read-modify-write. See lockJournal.
	installationJanitorLockFileName = "installation-janitor-journal.lock"

	// installationAuthorityMaxBytes bounds the operator's file. Exceeding it
	// denies the pass rather than truncating: a partial authority is a narrower
	// one, and a narrower authority is destructive.
	installationAuthorityMaxBytes = 1 << 20

	// installationJournalMaxBytes bounds the daemon's own journal, which grows
	// by one audit entry per destructive request and carries the repository set
	// observed at grant drift. That set is sized by the account being
	// reconciled, so the bound is larger than the operator's file and, unlike
	// it, is enforced on the way in as well: a journal written past the size it
	// can be read at would deny every registration and could not be repaired by
	// the daemon itself.
	installationJournalMaxBytes = 8 << 20
)

// InstallationAuthorityStore backs both janitor ports from a daemon state
// directory on a standalone host. It is the first non-test implementation of
// either: #263 deferred their persistence until a consumer existed, which left
// every registration inoperable behind the janitor gate.
//
// One type implements both ports because quarantine's local invalidation has to
// commit with its audit record (see JanitorRecorder): one file and one rename
// are that transaction, and one mutex keeps a half-written withdrawal
// unobservable by a concurrent load.
//
// The operator's file is a reconstruction trust boundary, so every field is
// re-validated on load and any failure denies the pass. "Fails closed" is
// bounded, though, and the bounds are these. It covers structural tamper:
// truncation, a partial write, a widened mode, a symlinked component, unknown
// or duplicated fields, an over-cap file. It does not cover authorship: both
// files are plaintext under a directory the operator controls, the directory's
// ownership is not checked (only its mode, as the keystore checks its own), and
// deleting the journal outright restores trust in every installation the
// operator's file still names. A withdrawal survives a restart; it does not
// survive an operator discarding the daemon's state.
//
// The composing caller (#236) must pass the same state directory the keystore
// was constructed against, which keeps App credentials structurally outside it.
type InstallationAuthorityStore struct {
	dir string
	mu  sync.Mutex
}

var (
	_ InstallationAuthoritySource = (*InstallationAuthorityStore)(nil)
	_ JanitorRecorder             = (*InstallationAuthorityStore)(nil)
)

// NewInstallationAuthorityStore resolves the state directory once, the way the
// keystore resolves its roots: a symlink at the directory itself is the
// operator's own choice, but nothing that appears inside it afterwards is
// followed.
func NewInstallationAuthorityStore(stateDir string) (*InstallationAuthorityStore, error) {
	if strings.TrimSpace(stateDir) == "" {
		return nil, errors.New("installation authority: empty state directory")
	}
	resolved, err := resolveExisting(stateDir)
	if err != nil {
		return nil, fmt.Errorf("installation authority: %w", err)
	}
	if err := rejectNonDir(resolved); err != nil {
		return nil, fmt.Errorf("installation authority: %w", err)
	}
	return &InstallationAuthorityStore{dir: resolved}, nil
}

// InitializeDocument creates the canonical empty authority document exactly
// once. Replays validate the existing document without replacing it: an
// existing authority is operator policy, and setup must never narrow it back
// to an empty registration set.
func (s *InstallationAuthorityStore) InitializeDocument(ctx context.Context) error {
	if s == nil {
		return errors.New("installation authority: nil store")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	unlock, err := s.lockJournal()
	if err != nil {
		return err
	}
	defer unlock()

	path := filepath.Join(s.dir, installationAuthorityFileName)
	payload, err := (InstallationAuthorityDocument{
		Version:       installationAuthoritySnapshotVersion,
		Registrations: []InstallationAuthorityEntry{},
	}).Encode()
	if err != nil {
		return err
	}
	if err := writeFileExclSync(path, payload); err == nil {
		return nil
	} else if !errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("installation authority: initialize: %w", err)
	}
	existing, err := s.readFile(installationAuthorityFileName, installationAuthorityMaxBytes)
	if err != nil {
		return fmt.Errorf(
			"installation authority: %w: %w", err, ErrInstallationAuthoritySnapshot)
	}
	_, err = DecodeInstallationAuthorityDocument(existing)
	return err
}

// InitializeRegistration adds the fail-closed starting authority for a newly
// converted App. The caller must use the canonical registration returned by
// GitHub's conversion endpoint, never command-line identity.
func (s *InstallationAuthorityStore) InitializeRegistration(
	ctx context.Context,
	registration AppRegistration,
) error {
	if err := registration.validate(); err != nil {
		return fmt.Errorf("installation authority: initialize registration: %w", err)
	}
	return s.UpdateDocument(ctx, func(document *InstallationAuthorityDocument) error {
		for _, entry := range document.Registrations {
			if entry.RegistrationID != registration.AppID {
				continue
			}
			for _, owner := range entry.TrustedOwners {
				if owner.ID == registration.OwnerID &&
					strings.EqualFold(owner.Login, registration.Owner) {
					return nil
				}
			}
			return fmt.Errorf(
				"installation authority: registration %d already has different owner authority",
				registration.AppID,
			)
		}
		document.Registrations = append(document.Registrations, InstallationAuthorityEntry{
			RegistrationID:        registration.AppID,
			ActiveEpoch:           1,
			DurableIntentRevision: 1,
			TrustedOwners: []TrustedOwnerRecord{{
				Login: registration.Owner,
				ID:    registration.OwnerID,
			}},
			TrustedInstallations: []TrustedInstallationRecord{},
			Pending:              nil,
		})
		slices.SortFunc(document.Registrations, func(a, b InstallationAuthorityEntry) int {
			return cmp.Compare(a.RegistrationID, b.RegistrationID)
		})
		return nil
	})
}

// InstallationAuthority serves one registration's authority, with every
// installation this daemon has quarantined withdrawn from it.
//
// Both files are re-read on every call. A registration is reconciled against
// the authority as it stands when its turn comes, and an operator's correction
// takes effect on the next pass without a restart.
func (s *InstallationAuthorityStore) InstallationAuthority(
	ctx context.Context,
	registrationID int64,
) (InstallationAuthority, error) {
	if s == nil {
		return InstallationAuthority{}, errors.New("installation authority: nil store")
	}
	if err := ctx.Err(); err != nil {
		return InstallationAuthority{}, err
	}
	if registrationID <= 0 {
		return InstallationAuthority{}, fmt.Errorf(
			"installation authority: registration id %d is not positive: %w",
			registrationID, ErrInstallationAuthoritySnapshot,
		)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	payload, err := s.readFile(installationAuthorityFileName, installationAuthorityMaxBytes)
	if err != nil {
		return InstallationAuthority{}, fmt.Errorf(
			"installation authority: %w: %w", err, ErrInstallationAuthoritySnapshot,
		)
	}
	document, err := DecodeInstallationAuthorityDocument(payload)
	if err != nil {
		return InstallationAuthority{}, err
	}
	journal, err := s.loadJournalLocked()
	if err != nil {
		return InstallationAuthority{}, err
	}
	entry, err := document.entry(registrationID)
	if err != nil {
		return InstallationAuthority{}, err
	}
	served, err := applyQuarantine(entry, journal.Quarantined)
	if err != nil {
		return InstallationAuthority{}, err
	}
	return served.authority(), nil
}

// Document returns the complete operator-authored authority document through
// the same strict decoder the janitor uses.
func (s *InstallationAuthorityStore) Document(
	ctx context.Context,
) (InstallationAuthorityDocument, error) {
	if s == nil {
		return InstallationAuthorityDocument{}, errors.New("installation authority: nil store")
	}
	if err := ctx.Err(); err != nil {
		return InstallationAuthorityDocument{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	payload, err := s.readFile(installationAuthorityFileName, installationAuthorityMaxBytes)
	if err != nil {
		return InstallationAuthorityDocument{}, fmt.Errorf(
			"installation authority: %w: %w", err, ErrInstallationAuthoritySnapshot)
	}
	return DecodeInstallationAuthorityDocument(payload)
}

// UpdateDocument holds the authority-store lock across strict decode,
// caller-supplied mutation, strict encode, and atomic replacement. The daemon
// runtime never calls this path; setup/onboard owns authoring. Holding one lock
// prevents onboarding from restoring a binding a concurrent quarantine removed.
func (s *InstallationAuthorityStore) UpdateDocument(
	ctx context.Context,
	update func(*InstallationAuthorityDocument) error,
) error {
	if s == nil {
		return errors.New("installation authority: nil store")
	}
	if update == nil {
		return errors.New("installation authority: nil document update")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	unlock, err := s.lockJournal()
	if err != nil {
		return err
	}
	defer unlock()
	payload, err := s.readFile(installationAuthorityFileName, installationAuthorityMaxBytes)
	if err != nil {
		return fmt.Errorf(
			"installation authority: %w: %w", err, ErrInstallationAuthoritySnapshot)
	}
	document, err := DecodeInstallationAuthorityDocument(payload)
	if err != nil {
		return err
	}
	if err := update(&document); err != nil {
		return err
	}
	replacement, err := document.Encode()
	if err != nil {
		return err
	}
	return s.writeFile(installationAuthorityFileName, replacement)
}

// RecordInstallationRemoval commits the audit barrier for a removal. A removal
// withdraws nothing locally: the installation it names was never trusted.
func (s *InstallationAuthorityStore) RecordInstallationRemoval(record InstallationRemovalRecord) error {
	return s.record(janitorActionRemoval, record)
}

// RecordInstallationQuarantine commits the audit barrier and the durable
// withdrawal of trust in one write, before the janitor suspends or deletes
// anything. A process that dies after this returns cannot re-trust the
// installation on restart, because the load path subtracts it from whatever the
// operator's file still says.
func (s *InstallationAuthorityStore) RecordInstallationQuarantine(record InstallationRemovalRecord) error {
	return s.record(janitorActionQuarantine, record)
}

func (s *InstallationAuthorityStore) record(action janitorAction, record InstallationRemovalRecord) error {
	if s == nil {
		return errors.New("installation janitor journal: nil store")
	}
	entry := janitorAuditEntry{
		Action:                action,
		RequestedAt:           record.RequestedAt.UTC(),
		RegistrationID:        record.RegistrationID,
		InstallationID:        record.InstallationID,
		AccountID:             record.AccountID,
		Reason:                record.Reason,
		ObservedRepositoryIDs: slices.Clone(record.ObservedRepositoryIDs),
	}
	if err := entry.validate(); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	unlock, err := s.lockJournal()
	if err != nil {
		return err
	}
	defer unlock()

	journal, err := s.loadJournalLocked()
	if err != nil {
		return err
	}
	payload, err := journal.record(entry).encode()
	if err != nil {
		return err
	}
	if len(payload) > installationJournalMaxBytes {
		return fmt.Errorf(
			"installation janitor journal: recording installation %d would grow the journal to %d bytes, "+
				"over the %d byte limit; trim its audit entries and keep its quarantine set",
			entry.InstallationID, len(payload), installationJournalMaxBytes,
		)
	}
	return s.writeFile(installationJanitorJournalFileName, payload)
}

// lockJournal takes the exclusive advisory lock for authority and journal
// read-modify-write operations. The in-process mutex belongs to one store
// value, and nothing stops a composer from constructing one store per port or
// a second process from sharing the state directory. Serializing both files
// prevents either a journal withdrawal or an authority edit from overwriting
// concurrent state. The kernel releases the lock when the descriptor closes,
// so no crash can leave it held.
func (s *InstallationAuthorityStore) lockJournal() (func(), error) {
	if err := s.assertDirectory(); err != nil {
		return nil, err
	}
	path := filepath.Join(s.dir, installationJanitorLockFileName)
	if err := rejectSymlinkedPath(path); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600) //nolint:gosec // G304: fixed name under the resolved state directory
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, err
	}
	return func() { _ = f.Close() }, nil
}

// loadJournalLocked reads the daemon's own journal. An absent file is the only
// tolerated absence in this store: no destructive request has been recorded
// yet, so nothing has been withdrawn. Every other failure denies the caller.
func (s *InstallationAuthorityStore) loadJournalLocked() (janitorJournal, error) {
	payload, err := s.readFile(installationJanitorJournalFileName, installationJournalMaxBytes)
	if errors.Is(err, fs.ErrNotExist) {
		return janitorJournal{Version: installationJanitorJournalVersion}, nil
	}
	if err != nil {
		return janitorJournal{}, fmt.Errorf("installation janitor journal: %w", err)
	}
	return decodeJanitorJournal(payload)
}

// readFile reads one state file under the store's directory, refusing anything
// that is not a regular, owner-only file reached without following a symlink.
// The descriptor, not the path, answers every question after the open, so a
// swap between the checks and the read cannot change what was read.
func (s *InstallationAuthorityStore) readFile(name string, maxBytes int64) ([]byte, error) {
	if err := s.assertDirectory(); err != nil {
		return nil, err
	}
	path := filepath.Join(s.dir, name)
	if err := rejectSymlinkedPath(path); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0) //nolint:gosec // G304: hardened open of a state path derived from the resolved state directory; the descriptor's kind and mode are verified below
	if err != nil {
		return nil, err
	}
	defer f.Close() //nolint:errcheck // read-only descriptor; the read result is the outcome
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file (mode %s)", path, info.Mode().Type())
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%s is mode %04o, which is not owner-only", path, info.Mode().Perm())
	}
	// Size the file before reading it so an over-cap file is reported as one,
	// rather than as the unexpected EOF a truncated file produces.
	if info.Size() > maxBytes {
		return nil, fmt.Errorf("%s is %d bytes, over the %d byte limit", path, info.Size(), maxBytes)
	}
	payload, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(payload)) > maxBytes {
		return nil, fmt.Errorf("%s grew past the %d byte limit while being read", path, maxBytes)
	}
	return payload, nil
}

// writeFile replaces one state file durably: a fresh owner-only temporary file,
// synced, renamed over the destination, and the directory entry synced. A
// reader therefore sees either the whole previous file or the whole new one,
// and a crash cannot leave a half-written journal that would decode as a
// narrower quarantine set.
func (s *InstallationAuthorityStore) writeFile(name string, payload []byte) (err error) {
	if err := s.assertDirectory(); err != nil {
		return err
	}
	// A crash between the temporary file and the rename leaves an orphan behind.
	// Nothing reads it, but the caller holds the journal lock here, so this is
	// the one safe moment to collect them.
	stale, _ := filepath.Glob(filepath.Join(s.dir, name+".tmp-*"))
	for _, path := range stale {
		_ = os.Remove(path)
	}
	tmp, err := os.CreateTemp(s.dir, name+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() {
		if err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmpPath)
		}
	}()
	if _, err = tmp.Write(payload); err != nil {
		return err
	}
	if err = tmp.Sync(); err != nil {
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	if err = os.Rename(tmpPath, filepath.Join(s.dir, name)); err != nil {
		return err
	}
	return syncDir(s.dir)
}

// assertDirectory refuses a state directory another account can write to.
// Directory write permission is what lets a second account swap the authority
// file; read permission does not, since the files themselves are owner-only.
// A directory that does not exist yet is not a failure: no state has been
// written, and the caller's own open or mkdir decides what happens next.
func (s *InstallationAuthorityStore) assertDirectory() error {
	if err := rejectSymlinkedPath(s.dir); err != nil {
		return err
	}
	info, err := os.Lstat(s.dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", s.dir)
	}
	if info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf(
			"%s is mode %04o, which lets another account replace its contents",
			s.dir, info.Mode().Perm(),
		)
	}
	return nil
}

// rejectSymlinkedPath proves every component of path is a real entry. O_NOFOLLOW
// only refuses a symlink at the final component, so without this walk a link at
// any parent would still redirect the read or the rename. It mirrors the
// component check mkdirAllSync runs before creating the keystore's directories.
func rejectSymlinkedPath(path string) error {
	prefix := string(filepath.Separator)
	trimmed := strings.TrimPrefix(filepath.Clean(path), string(filepath.Separator))
	for part := range strings.SplitSeq(trimmed, string(filepath.Separator)) {
		prefix = filepath.Join(prefix, part)
		info, err := os.Lstat(prefix)
		if errors.Is(err, fs.ErrNotExist) {
			return nil // the rest of the path does not exist yet
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%s is a symlink", prefix)
		}
	}
	return nil
}
