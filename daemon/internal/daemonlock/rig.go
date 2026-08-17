package daemonlock

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/atomicfile"
	"github.com/freeside-ai/freeside/daemon/internal/strictjson"
)

const (
	rigManifestVersion = 3
	rigManifestLimit   = 64 << 10
	rigGlobalLockName  = "production-rig.lock"
	rigGlobalStateName = "production-rig.json"
	rigLockName        = ".freeside-rig.lock"
	rigBindLockName    = ".freeside-rig-bind.lock"
	stateManifestName  = ".freeside-rig.json"
	seedManifestName   = ".freeside-rig-seed.json"
	rigAmendmentDomain = "freeside-production-rig-amendment-v1"
)

var (
	ErrRigHeld                 = errors.New("production rig lease is held")
	ErrRigStale                = errors.New("production rig lease is stale")
	ErrRigNotHeld              = errors.New("production rig lease is not live")
	ErrRigToken                = errors.New("production rig lease token is invalid")
	ErrRigRecoveryConfirmation = errors.New("stale rig recovery requires explicit confirmation")
	rigTokenDigestPattern      = regexp.MustCompile(`^[0-9a-f]{64}$`)
	rigSignaturePattern        = regexp.MustCompile(`^[0-9a-f]{128}$`)
	rigContainerNamePattern    = regexp.MustCompile(`^(freeside-handoff-c[0-9a-f]{31}-(seeder|observer|ins-seed|ins-check|cfg-seed|cfg-check|projects-check|sess-check|writer-check|cred-pre|cred-post|agent|exporter)|freeside-handoff-conf-[0-9a-f]{16}-(seeder|observer|ins-seed|ins-check|agent|exporter)|freeside-handoff-review-[0-9a-f]{24}-(seeder|observer)|freeside-review-review-[0-9a-f]{24}-(ws-obs|workspace-observer|agents-init|agents-obs|agents-observer|snap-init|snap-obs|codex)|freeside-ward-conf-[0-9a-f]{16}-prejob|freeside-ward-conf-conf-[0-9a-f]{16}-(liveness|seed|audit|excl-writer|excl-second|net-live|net|inx-live|inx))$`)
	rigVolumeNamePattern       = regexp.MustCompile(`^(freeside-handoff-c[0-9a-f]{31}-(ws|ins|cfg|projects|session-env)|freeside-handoff-conf-[0-9a-f]{16}-(ws|ins)|freeside-handoff-review-[0-9a-f]{24}-ws|freeside-review-review-[0-9a-f]{24}-(agents|snap)|freeside-ward-conf-conf-[0-9a-f]{16}-(cred|liveness-ws|excl-ws|net-live-ws|inx-ws))$`)
	rigNetworkNamePattern      = regexp.MustCompile(`^(freeside-handoff-(c[0-9a-f]{31}|conf-[0-9a-f]{16})-egress|freeside-review-review-[0-9a-f]{24}-egress)$`)
)

// RigOwner is the deliberately narrow owner identity shown on a refusal. It
// never carries process arguments or environment values.
type RigOwner struct {
	User string `json:"user"`
	Host string `json:"host"`
	PID  int    `json:"pid"`
	Note string `json:"note,omitempty"`
}

// RigResources are the exact host resources one production acceptance
// campaign binds. Container names are admitted only from ward's deterministic
// handoff namespace before cleanup may act on them.
type RigResources struct {
	StateRoot     string   `json:"state_root"`
	DatabasePath  string   `json:"database_path"`
	ListenAddress string   `json:"listen_address"`
	SeedRoot      string   `json:"seed_root"`
	LeaseRoot     string   `json:"lease_root"`
	Containers    []string `json:"containers"`
	Volumes       []string `json:"volumes"`
	Networks      []string `json:"networks"`
}

// RigManifest is the inspectable, non-secret lease record. TokenDigest
// authenticates a cooperating holder without disclosing the token in
// refusals; PID is descriptive and is never used as liveness evidence.
type RigManifest struct {
	Version            int          `json:"version"`
	Owner              RigOwner     `json:"owner"`
	AcquiredAt         time.Time    `json:"acquired_at"`
	Resources          RigResources `json:"resources"`
	TokenDigest        string       `json:"token_sha256"`
	AmendmentPublicKey string       `json:"amendment_ed25519_public_key"`
}

// RigAcquireConfig is the operator binding requested before any production
// state mutation or daemon launch.
type RigAcquireConfig struct {
	Owner         RigOwner
	StateRoot     string
	DatabasePath  string
	ListenAddress string
	SeedRoot      string
	LeaseRoot     string
	Now           func() time.Time
}

// RigConflictError reports the current holder without inspecting its process.
type RigConflictError struct {
	Manifest *RigManifest
}

func (e *RigConflictError) Error() string {
	if e == nil || e.Manifest == nil {
		return ErrRigHeld.Error() + "; owner manifest is not yet readable"
	}
	return fmt.Sprintf("%s by %s@%s pid=%d since %s; %s",
		ErrRigHeld, e.Manifest.Owner.User, e.Manifest.Owner.Host,
		e.Manifest.Owner.PID, e.Manifest.AcquiredAt.Format(time.RFC3339),
		formatRigResources(e.Manifest.Resources))
}

func (*RigConflictError) Unwrap() error { return ErrRigHeld }

// RigStaleError reports an unclean prior release. An old timestamp or a dead
// manifest PID never clears this condition automatically.
type RigStaleError struct {
	Manifest RigManifest
}

func (e *RigStaleError) Error() string {
	return fmt.Sprintf("%s from %s@%s pid=%d acquired %s; %s; run `freesided rig recover`",
		ErrRigStale, e.Manifest.Owner.User, e.Manifest.Owner.Host,
		e.Manifest.Owner.PID, e.Manifest.AcquiredAt.Format(time.RFC3339),
		formatRigResources(e.Manifest.Resources))
}

func (*RigStaleError) Unwrap() error { return ErrRigStale }

func formatRigResources(resources RigResources) string {
	return fmt.Sprintf("state=%q database=%q listen=%q seed=%q containers=%q volumes=%q networks=%q",
		resources.StateRoot, resources.DatabasePath, resources.ListenAddress,
		resources.SeedRoot, resources.Containers, resources.Volumes, resources.Networks)
}

type rigLock struct {
	path string
	f    *os.File
}

// RigLease holds both root locks until Close. Close removes the manifests
// before dropping either flock, so a clean handoff has no manifest-free live
// window and a crash leaves an explicit stale record.
type RigLease struct {
	mu            sync.Mutex
	locks         []rigLock
	gate          rigLock
	manifestPaths []string
	manifest      RigManifest
	token         string
	closed        bool
}

// RigAuthorization holds the amendment gate while a token-authorized
// destructive operation acts on the manifest it authenticated.
type RigAuthorization struct {
	mu       sync.Mutex
	gate     rigLock
	manifest RigManifest
	closed   bool
}

// AcquireRig atomically excludes every other production campaign in the
// coordination domain as well as the canonical roots, database, and listener.
// The fixed lexical lock order prevents overlapping campaigns from acquiring
// shared resources in opposite order.
func AcquireRig(cfg RigAcquireConfig) (*RigLease, error) {
	manifest, err := newRigManifest(cfg)
	if err != nil {
		return nil, err
	}
	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		return nil, fmt.Errorf("mint rig lease token: %w", err)
	}
	token := hex.EncodeToString(tokenBytes)
	digest := sha256.Sum256([]byte(token))
	manifest.TokenDigest = hex.EncodeToString(digest[:])
	privateKey := ed25519.NewKeyFromSeed(tokenBytes)
	manifest.AmendmentPublicKey = hex.EncodeToString(privateKey.Public().(ed25519.PublicKey))
	if err := manifest.validate(); err != nil {
		return nil, err
	}

	coordinationPaths, localPaths, manifestPaths := splitRigPaths(manifest.Resources)
	coordinationLocks, conflictPath, err := acquireRigLocks(coordinationPaths)
	if err != nil {
		if errors.Is(err, ErrRigHeld) {
			candidates := append(manifestPaths, globalRigStatePath(manifest.Resources))
			return nil, &RigConflictError{Manifest: readConflictManifest(conflictPath, candidates)}
		}
		return nil, err
	}
	releaseCoordination := func() { _ = closeRigLocks(coordinationLocks) }
	staleGlobal, err := readRigManifest(globalRigStatePath(manifest.Resources))
	if err == nil {
		releaseCoordination()
		return nil, &RigStaleError{Manifest: staleGlobal}
	}
	if !errors.Is(err, os.ErrNotExist) {
		releaseCoordination()
		return nil, fmt.Errorf("inspect global rig manifest: %w", err)
	}
	for _, root := range []string{manifest.Resources.StateRoot, manifest.Resources.SeedRoot} {
		if err := os.MkdirAll(root, 0o700); err != nil {
			releaseCoordination()
			return nil, fmt.Errorf("create rig resource root %s: %w", root, err)
		}
		resolved, err := canonicalPath(root)
		if err != nil || resolved != root {
			releaseCoordination()
			return nil, fmt.Errorf("rig resource root changed while acquiring: %s", root)
		}
	}
	gate, err := acquireRigGate(manifest.Resources.StateRoot)
	if err != nil {
		releaseCoordination()
		return nil, err
	}
	defer func() { _ = closeRigLock(gate) }()
	localLocks, conflictPath, err := acquireRigLocks(localPaths)
	if err != nil {
		releaseCoordination()
		if errors.Is(err, ErrRigHeld) {
			return nil, &RigConflictError{Manifest: readConflictManifest(conflictPath, manifestPaths)}
		}
		return nil, err
	}
	locks := append(coordinationLocks, localLocks...)
	releaseOnError := func() { _ = closeRigLocks(locks) }
	for _, path := range manifestPaths {
		prior, readErr := readRigManifest(path)
		switch {
		case readErr == nil:
			releaseOnError()
			return nil, &RigStaleError{Manifest: prior}
		case errors.Is(readErr, os.ErrNotExist):
		default:
			releaseOnError()
			return nil, fmt.Errorf("inspect prior rig manifest %s: %w", path, readErr)
		}
	}
	if err := writeRigLockMetadata(locks, manifest, token); err != nil {
		releaseOnError()
		return nil, err
	}
	writeOrder := []string{manifestPaths[0], manifestPaths[1]}
	for _, path := range writeOrder {
		if err := writeRigManifest(path, manifest); err != nil {
			// Publication errors are ambiguous after rename. Preserve any
			// visible authoritative manifest for explicit stale recovery.
			releaseOnError()
			return nil, err
		}
	}
	if err := writeRigManifest(globalRigStatePath(manifest.Resources), manifest); err != nil {
		// The per-root manifests are already durable, so a failed global
		// publication remains recoverable from the authoritative state root.
		releaseOnError()
		return nil, fmt.Errorf("publish global rig manifest: %w", err)
	}
	return &RigLease{
		locks: locks, manifestPaths: manifestPaths, manifest: manifest, token: token,
	}, nil
}

func newRigManifest(cfg RigAcquireConfig) (RigManifest, error) {
	for _, required := range []struct {
		name  string
		value string
	}{
		{"owner user", cfg.Owner.User},
		{"owner host", cfg.Owner.Host},
		{"state root", cfg.StateRoot},
		{"database path", cfg.DatabasePath},
		{"listen address", cfg.ListenAddress},
		{"seed root", cfg.SeedRoot},
	} {
		if strings.TrimSpace(required.value) == "" {
			return RigManifest{}, fmt.Errorf("rig lease %s is required", required.name)
		}
	}
	if cfg.Owner.PID <= 0 {
		return RigManifest{}, errors.New("rig lease owner PID must be positive")
	}
	if len(cfg.Owner.User) > 256 || len(cfg.Owner.Host) > 256 || len(cfg.Owner.Note) > 1024 {
		return RigManifest{}, errors.New("rig lease owner metadata exceeds its size limit")
	}
	stateRoot, err := canonicalProspectivePath(cfg.StateRoot)
	if err != nil {
		return RigManifest{}, fmt.Errorf("canonicalize rig state root: %w", err)
	}
	databasePath, err := canonicalProspectivePath(cfg.DatabasePath)
	if err != nil {
		return RigManifest{}, fmt.Errorf("canonicalize rig database path: %w", err)
	}
	if err := rejectMultipleLinks(databasePath); err != nil {
		return RigManifest{}, err
	}
	seedRoot, err := canonicalProspectivePath(cfg.SeedRoot)
	if err != nil {
		return RigManifest{}, fmt.Errorf("canonicalize rig seed root: %w", err)
	}
	now := time.Now
	if cfg.Now != nil {
		now = cfg.Now
	}
	listenAddress, err := canonicalListenAddress(cfg.ListenAddress)
	if err != nil {
		return RigManifest{}, err
	}
	leaseRoot := cfg.LeaseRoot
	if leaseRoot == "" {
		leaseRoot, err = defaultRigLeaseRoot()
		if err != nil {
			return RigManifest{}, err
		}
	}
	if err := os.MkdirAll(leaseRoot, 0o700); err != nil {
		return RigManifest{}, fmt.Errorf("create rig coordination root: %w", err)
	}
	if err := validateRigLeaseRoot(leaseRoot); err != nil {
		return RigManifest{}, err
	}
	leaseRoot, err = canonicalPath(leaseRoot)
	if err != nil {
		return RigManifest{}, fmt.Errorf("canonicalize rig coordination root: %w", err)
	}
	manifest := RigManifest{
		Version: rigManifestVersion, Owner: cfg.Owner, AcquiredAt: now().UTC(),
		Resources: RigResources{
			StateRoot: stateRoot, DatabasePath: databasePath,
			ListenAddress: listenAddress, SeedRoot: seedRoot, LeaseRoot: leaseRoot,
			Containers: []string{}, Volumes: []string{}, Networks: []string{},
		},
	}
	if databasePath != filepath.Join(stateRoot, "freeside.db") {
		return RigManifest{}, errors.New("rig database must be the canonical freeside.db under the state root")
	}
	return manifest, nil
}

func rigPaths(resources RigResources) (lockPaths, manifestPaths []string) {
	coordination, local, manifestPaths := splitRigPaths(resources)
	lockPaths = append(coordination, local...)
	sort.Strings(lockPaths)
	lockPaths = slices.Compact(lockPaths)
	return lockPaths, manifestPaths
}

func globalRigLockPath(resources RigResources) string {
	return filepath.Join(resources.LeaseRoot, rigGlobalLockName)
}

func globalRigStatePath(resources RigResources) string {
	return filepath.Join(resources.LeaseRoot, rigGlobalStateName)
}

func splitRigPaths(resources RigResources) (coordination, local, manifestPaths []string) {
	coordination = []string{
		globalRigLockPath(resources),
		rigResourceLockPath(resources.LeaseRoot, "root", resources.StateRoot),
		rigResourceLockPath(resources.LeaseRoot, "root", resources.SeedRoot),
		rigResourceLockPath(resources.LeaseRoot, "database", resources.DatabasePath),
		rigResourceLockPath(resources.LeaseRoot, "listen", resources.ListenAddress),
	}
	local = []string{
		filepath.Join(resources.StateRoot, rigLockName),
		filepath.Join(resources.SeedRoot, rigLockName),
	}
	manifestPaths = []string{
		filepath.Join(resources.StateRoot, stateManifestName),
		filepath.Join(resources.SeedRoot, seedManifestName),
	}
	sort.Strings(coordination)
	coordination = slices.Compact(coordination)
	sort.Strings(local)
	local = slices.Compact(local)
	return coordination, local, manifestPaths
}

func canonicalProspectivePath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	current := abs
	var suffix []string
	for {
		_, err := os.Lstat(current)
		if err == nil {
			resolved, err := filepath.EvalSymlinks(current)
			if err != nil {
				return "", err
			}
			return filepath.Join(append([]string{resolved}, suffix...)...), nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", fmt.Errorf("canonicalize prospective path %q: no existing ancestor", path)
		}
		suffix = append([]string{filepath.Base(current)}, suffix...)
		current = parent
	}
}

func rigResourceLockPath(root, kind, value string) string {
	digest := sha256.Sum256([]byte(value))
	return filepath.Join(root, kind+"-"+hex.EncodeToString(digest[:])+".lock")
}

func canonicalListenAddress(value string) (string, error) {
	resolved, err := net.ResolveTCPAddr("tcp", value)
	if err != nil {
		return "", fmt.Errorf("resolve rig listen address %q: %w", value, err)
	}
	if resolved.IP == nil || resolved.IP.IsUnspecified() || resolved.Zone != "" || resolved.Port == 0 {
		return "", fmt.Errorf("rig listen address %q resolved to unsupported address %q", value, resolved)
	}
	return net.JoinHostPort(resolved.IP.String(), strconv.Itoa(resolved.Port)), nil
}

func defaultRigLeaseRoot() (string, error) {
	current, err := user.Current()
	if err != nil {
		return "", fmt.Errorf("identify current user for rig coordination: %w", err)
	}
	if current.HomeDir == "" {
		return "", errors.New("current user has no home directory for rig coordination")
	}
	return filepath.Join(current.HomeDir, ".freeside", "rig-locks"), nil
}

func validateRigLeaseRoot(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect rig coordination root: %w", err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int64(stat.Uid) != int64(os.Geteuid()) {
		return errors.New("rig coordination root is not owned by the current OS user")
	}
	if !info.IsDir() || info.Mode()&0o077 != 0 {
		return errors.New("rig coordination root must be a private directory")
	}
	return nil
}

func acquireRigGate(stateRoot string) (rigLock, error) {
	path := filepath.Join(stateRoot, rigBindLockName)
	// #nosec G304 -- the path is a fixed basename below a canonical state root.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return rigLock{}, fmt.Errorf("open rig amendment lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return rigLock{}, fmt.Errorf("acquire rig amendment lock: %w", err)
	}
	return rigLock{path: path, f: f}, nil
}

func closeRigLock(lock rigLock) error {
	if lock.f == nil {
		return nil
	}
	return errors.Join(
		syscall.Flock(int(lock.f.Fd()), syscall.LOCK_UN),
		lock.f.Close(),
	)
}

func acquireRigLocks(paths []string) ([]rigLock, string, error) {
	locks := make([]rigLock, 0, len(paths))
	for _, path := range paths {
		// #nosec G304 -- every path is a fixed basename below a canonical operator root.
		f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			_ = closeRigLocks(locks)
			return nil, "", fmt.Errorf("open rig lock %s: %w", path, err)
		}
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			_ = f.Close()
			_ = closeRigLocks(locks)
			if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
				return nil, path, ErrRigHeld
			}
			return nil, "", fmt.Errorf("acquire rig lock %s: %w", path, err)
		}
		locks = append(locks, rigLock{path: path, f: f})
	}
	return locks, "", nil
}

func closeRigLocks(locks []rigLock) error {
	var result error
	for index := len(locks) - 1; index >= 0; index-- {
		result = errors.Join(result,
			syscall.Flock(int(locks[index].f.Fd()), syscall.LOCK_UN),
			locks[index].f.Close(),
		)
	}
	return result
}

type rigLockMetadataRecord struct {
	Sequence  uint64      `json:"sequence"`
	Manifest  RigManifest `json:"manifest"`
	Signature string      `json:"signature"`
}

type rigUnsignedLockMetadataRecord struct {
	Domain   string      `json:"domain"`
	Sequence uint64      `json:"sequence"`
	Manifest RigManifest `json:"manifest"`
}

func writeRigLockMetadata(locks []rigLock, manifest RigManifest, token string) error {
	body, err := encodeRigLockMetadataRecord(0, manifest, token)
	if err != nil {
		return err
	}
	for _, lock := range locks {
		if lock.path == globalRigLockPath(manifest.Resources) {
			continue
		}
		if err := writeRigLockMetadataFile(lock.f, lock.path, body); err != nil {
			return err
		}
	}
	return nil
}

func appendRigLockMetadata(path string, manifest RigManifest, token string) error {
	_, sequence, partial, err := readRigLockMetadataLog(path)
	if err != nil {
		return err
	}
	if partial {
		return fmt.Errorf("rig lock metadata %s ends with an incomplete record", path)
	}
	body, err := encodeRigLockMetadataRecord(sequence+1, manifest, token)
	if err != nil {
		return err
	}
	// #nosec G304 -- callers use the fixed local lock basename under a canonical state root.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("open rig lock metadata %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Write(body); err != nil {
		return fmt.Errorf("append rig lock metadata %s: %w", path, err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("sync rig lock metadata %s: %w", path, err)
	}
	return nil
}

func writeRigLockMetadataFile(f *os.File, path string, body []byte) error {
	if err := f.Truncate(0); err != nil {
		return fmt.Errorf("truncate rig lock metadata %s: %w", path, err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("seek rig lock metadata %s: %w", path, err)
	}
	if _, err := f.Write(body); err != nil {
		return fmt.Errorf("write rig lock metadata %s: %w", path, err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("sync rig lock metadata %s: %w", path, err)
	}
	return nil
}

func encodeRigLockMetadataRecord(sequence uint64, manifest RigManifest, token string) ([]byte, error) {
	seed, err := hex.DecodeString(token)
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, ErrRigToken
	}
	privateKey := ed25519.NewKeyFromSeed(seed)
	publicKey := privateKey.Public().(ed25519.PublicKey)
	if subtle.ConstantTimeCompare(
		[]byte(hex.EncodeToString(publicKey)), []byte(manifest.AmendmentPublicKey),
	) != 1 {
		return nil, ErrRigToken
	}
	unsigned := rigUnsignedLockMetadataRecord{
		Domain: rigAmendmentDomain, Sequence: sequence, Manifest: manifest,
	}
	unsignedPayload, err := json.Marshal(unsigned)
	if err != nil {
		return nil, fmt.Errorf("encode rig lock metadata: %w", err)
	}
	record := rigLockMetadataRecord{
		Sequence: sequence, Manifest: manifest,
		Signature: hex.EncodeToString(ed25519.Sign(privateKey, unsignedPayload)),
	}
	payload, err := json.Marshal(record)
	if err != nil {
		return nil, fmt.Errorf("encode rig lock metadata: %w", err)
	}
	digest := sha256.Sum256(payload)
	return fmt.Appendf(nil, "%d %x %s\n", len(payload), digest, payload), nil
}

func readRigLockMetadata(path string) (*RigManifest, error) {
	manifest, _, _, err := readRigLockMetadataLog(path)
	return manifest, err
}

func readRigLockMetadataLog(path string) (*RigManifest, uint64, bool, error) {
	// #nosec G304 -- callers derive fixed lock paths from validated manifest roots.
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, 0, false, err
	}
	if len(body) == 0 {
		return nil, 0, false, nil
	}
	lines := bytes.Split(body, []byte{'\n'})
	partial := len(lines[len(lines)-1]) != 0
	complete := lines[:len(lines)-1]
	if len(complete) == 0 {
		return nil, 0, partial, errors.New("rig lock metadata has no complete record")
	}
	var first, previous, current RigManifest
	var sequence uint64
	for index, line := range complete {
		record, err := decodeRigLockMetadataRecord(line)
		if err != nil {
			return nil, 0, partial, fmt.Errorf("decode rig lock metadata record %d: %w", index, err)
		}
		if record.Sequence != uint64(index) {
			return nil, 0, partial, fmt.Errorf("rig lock metadata record %d has sequence %d", index, record.Sequence)
		}
		current = record.Manifest
		sequence = record.Sequence
		if index == 0 {
			first = current
		} else if !rigManifestIdentityEqual(first, current) || !rigManifestResourceSubset(previous, current) {
			return nil, 0, partial, fmt.Errorf("rig lock metadata record %d violates append-only authority", index)
		}
		previous = current
	}
	return &current, sequence, partial, nil
}

func decodeRigLockMetadataRecord(line []byte) (rigLockMetadataRecord, error) {
	lengthText, remainder, ok := bytes.Cut(line, []byte{' '})
	if !ok {
		return rigLockMetadataRecord{}, errors.New("missing record length")
	}
	digestText, payload, ok := bytes.Cut(remainder, []byte{' '})
	if !ok {
		return rigLockMetadataRecord{}, errors.New("missing record digest")
	}
	length, err := strconv.ParseUint(string(lengthText), 10, 64)
	if err != nil || length != uint64(len(payload)) {
		return rigLockMetadataRecord{}, errors.New("record length is invalid")
	}
	digest := sha256.Sum256(payload)
	if subtle.ConstantTimeCompare([]byte(hex.EncodeToString(digest[:])), digestText) != 1 {
		return rigLockMetadataRecord{}, errors.New("record digest is invalid")
	}
	var record rigLockMetadataRecord
	if err := strictjson.Decode(
		payload, &record, strictjson.RejectInvalidUTF8, strictjson.Limit(rigManifestLimit+1024),
	); err != nil {
		return rigLockMetadataRecord{}, err
	}
	if err := record.Manifest.validate(); err != nil {
		return rigLockMetadataRecord{}, err
	}
	if !rigSignaturePattern.MatchString(record.Signature) {
		return rigLockMetadataRecord{}, errors.New("record signature is invalid")
	}
	publicKey, err := hex.DecodeString(record.Manifest.AmendmentPublicKey)
	if err != nil || len(publicKey) != ed25519.PublicKeySize {
		return rigLockMetadataRecord{}, errors.New("record public key is invalid")
	}
	signature, err := hex.DecodeString(record.Signature)
	if err != nil {
		return rigLockMetadataRecord{}, errors.New("record signature is invalid")
	}
	unsignedPayload, err := json.Marshal(rigUnsignedLockMetadataRecord{
		Domain: rigAmendmentDomain, Sequence: record.Sequence, Manifest: record.Manifest,
	})
	if err != nil {
		return rigLockMetadataRecord{}, err
	}
	if !ed25519.Verify(ed25519.PublicKey(publicKey), unsignedPayload, signature) {
		return rigLockMetadataRecord{}, errors.New("record signature is invalid")
	}
	canonical, err := json.Marshal(record)
	if err != nil {
		return rigLockMetadataRecord{}, err
	}
	if !bytes.Equal(payload, canonical) {
		return rigLockMetadataRecord{}, errors.New("record is not canonical")
	}
	return record, nil
}

func readConflictManifest(conflictPath string, manifestPaths []string) *RigManifest {
	candidates := append([]string{conflictPath}, manifestPaths...)
	for range 20 {
		for _, path := range candidates {
			manifest, err := readRigManifest(path)
			if err == nil {
				return &manifest
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	return nil
}

// Token returns the one-time secret used by cooperating rig commands. It is
// never stored in either manifest.
func (l *RigLease) Token() string {
	if l == nil {
		return ""
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.token
}

func (l *RigLease) Manifest() RigManifest {
	if l == nil {
		return RigManifest{}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return cloneRigManifest(l.manifest)
}

func cloneRigManifest(manifest RigManifest) RigManifest {
	manifest.Resources.Containers = slices.Clone(manifest.Resources.Containers)
	manifest.Resources.Volumes = slices.Clone(manifest.Resources.Volumes)
	manifest.Resources.Networks = slices.Clone(manifest.Resources.Networks)
	return manifest
}

// Close performs the clean release. A crash does not call it, leaving the
// manifest as the explicit stale-recovery gate.
func (l *RigLease) Close() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil
	}
	if l.gate.f == nil {
		gate, err := acquireRigGate(l.manifest.Resources.StateRoot)
		if err != nil {
			return err
		}
		l.gate = gate
	}
	if err := removeRigManifest(globalRigStatePath(l.manifest.Resources)); err != nil {
		result := errors.Join(err, closeRigLocks(l.locks), closeRigLock(l.gate))
		l.closed = true
		l.token = ""
		return result
	}
	var secondaryResult error
	for _, path := range l.manifestPaths[1:] {
		if err := removeRigManifest(path); err != nil {
			secondaryResult = errors.Join(secondaryResult, fmt.Errorf("remove rig manifest %s: %w", path, err))
		}
	}
	result := secondaryResult
	if secondaryResult == nil {
		if err := removeRigManifest(l.manifestPaths[0]); err != nil {
			result = errors.Join(result, fmt.Errorf("remove rig manifest %s: %w", l.manifestPaths[0], err))
		}
	}
	result = errors.Join(result, closeRigLocks(l.locks), closeRigLock(l.gate))
	l.closed = true
	l.token = ""
	return result
}

// ReadRigManifest loads the authoritative state-root manifest for inspection
// or recovery. It does not establish liveness.
func ReadRigManifest(stateRoot string) (RigManifest, error) {
	canonical, err := canonicalPath(stateRoot)
	if err != nil {
		return RigManifest{}, err
	}
	manifest, err := readRigManifest(filepath.Join(canonical, stateManifestName))
	if err != nil {
		return RigManifest{}, err
	}
	if manifest.Resources.StateRoot != canonical {
		return RigManifest{}, errors.New("rig manifest state root differs from its storage root")
	}
	acquired, err := readRigLockMetadata(filepath.Join(canonical, rigLockName))
	if err != nil {
		return RigManifest{}, fmt.Errorf("read rig acquisition metadata: %w", err)
	}
	if acquired == nil || !rigManifestIdentityEqual(manifest, *acquired) {
		return RigManifest{}, errors.New("rig manifest authority differs from its acquisition metadata")
	}
	authority := *acquired
	global, globalErr := readRigManifest(globalRigStatePath(manifest.Resources))
	switch {
	case globalErr == nil:
		if !rigManifestIdentityEqual(manifest, global) {
			return RigManifest{}, errors.New("state-root and global rig manifests disagree")
		}
		if !rigManifestResourceSubset(global, authority) {
			return RigManifest{}, errors.New("global rig resources are not authorized by durable amendment metadata")
		}
	case errors.Is(globalErr, os.ErrNotExist):
	default:
		return RigManifest{}, fmt.Errorf("read global rig amendment authority: %w", globalErr)
	}
	if !rigManifestResourceSubset(manifest, authority) {
		return RigManifest{}, errors.New("rig manifest resources are not authorized by durable amendment metadata")
	}
	manifest.Resources.Containers = slices.Clone(authority.Resources.Containers)
	manifest.Resources.Volumes = slices.Clone(authority.Resources.Volumes)
	manifest.Resources.Networks = slices.Clone(authority.Resources.Networks)
	canonicalDatabase, err := canonicalPath(manifest.Resources.DatabasePath)
	if err != nil {
		return RigManifest{}, fmt.Errorf("canonicalize recorded rig database: %w", err)
	}
	if canonicalDatabase != manifest.Resources.DatabasePath {
		return RigManifest{}, errors.New("recorded rig database no longer resolves to the leased path")
	}
	if err := rejectMultipleLinks(canonicalDatabase); err != nil {
		return RigManifest{}, fmt.Errorf("validate recorded rig database: %w", err)
	}
	return manifest, nil
}

func readRigManifest(path string) (RigManifest, error) {
	// #nosec G304 -- callers derive the path from a canonical root and fixed basename.
	body, err := os.ReadFile(path)
	if err != nil {
		return RigManifest{}, err
	}
	return decodeRigManifest(body)
}

func decodeRigManifest(body []byte) (RigManifest, error) {
	var manifest RigManifest
	if err := strictjson.Decode(
		body, &manifest, strictjson.RejectInvalidUTF8, strictjson.Limit(rigManifestLimit),
	); err != nil {
		return RigManifest{}, fmt.Errorf("decode rig manifest: %w", err)
	}
	if err := manifest.validate(); err != nil {
		return RigManifest{}, err
	}
	canonical, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return RigManifest{}, fmt.Errorf("re-encode rig manifest: %w", err)
	}
	canonical = append(canonical, '\n')
	if !bytes.Equal(body, canonical) {
		return RigManifest{}, errors.New("rig manifest is not in canonical form")
	}
	return manifest, nil
}

func (m RigManifest) validate() error {
	switch {
	case m.Version != rigManifestVersion:
		return fmt.Errorf("rig manifest version %d is unsupported", m.Version)
	case m.Owner.User == "" || m.Owner.Host == "" || m.Owner.PID <= 0:
		return errors.New("rig manifest owner is incomplete")
	case m.AcquiredAt.IsZero():
		return errors.New("rig manifest acquisition time is missing")
	case !rigTokenDigestPattern.MatchString(m.TokenDigest):
		return errors.New("rig manifest token digest is invalid")
	case !rigTokenDigestPattern.MatchString(m.AmendmentPublicKey):
		return errors.New("rig manifest amendment public key is invalid")
	case !filepath.IsAbs(m.Resources.StateRoot) || filepath.Clean(m.Resources.StateRoot) != m.Resources.StateRoot:
		return errors.New("rig manifest state root is not canonical")
	case !filepath.IsAbs(m.Resources.DatabasePath) || filepath.Clean(m.Resources.DatabasePath) != m.Resources.DatabasePath:
		return errors.New("rig manifest database path is not canonical")
	case !filepath.IsAbs(m.Resources.SeedRoot) || filepath.Clean(m.Resources.SeedRoot) != m.Resources.SeedRoot:
		return errors.New("rig manifest seed root is not canonical")
	case !filepath.IsAbs(m.Resources.LeaseRoot) || filepath.Clean(m.Resources.LeaseRoot) != m.Resources.LeaseRoot:
		return errors.New("rig manifest coordination root is not canonical")
	case m.Resources.ListenAddress == "":
		return errors.New("rig manifest listen address is missing")
	}
	if m.Resources.DatabasePath != filepath.Join(m.Resources.StateRoot, "freeside.db") {
		return errors.New("rig manifest database is not the canonical freeside.db under the state root")
	}
	listenAddress, err := canonicalListenAddress(m.Resources.ListenAddress)
	if err != nil {
		return fmt.Errorf("validate rig manifest listen address: %w", err)
	}
	if listenAddress != m.Resources.ListenAddress {
		return errors.New("rig manifest listen address is not canonical")
	}
	for _, resource := range []string{
		m.Resources.StateRoot, m.Resources.DatabasePath,
		m.Resources.ListenAddress, m.Resources.SeedRoot, m.Resources.LeaseRoot,
	} {
		if strings.ContainsAny(resource, "\r\n") {
			return errors.New("rig manifest resource contains a line break")
		}
	}
	if !slices.IsSorted(m.Resources.Containers) {
		return errors.New("rig manifest container names are not sorted")
	}
	for index, name := range m.Resources.Containers {
		if !rigContainerNamePattern.MatchString(name) {
			return fmt.Errorf("rig manifest container name %q is outside the owned namespace", name)
		}
		if index > 0 && name == m.Resources.Containers[index-1] {
			return fmt.Errorf("rig manifest container name %q is duplicated", name)
		}
	}
	for kind, values := range map[string]struct {
		names   []string
		pattern *regexp.Regexp
	}{
		"volume":  {m.Resources.Volumes, rigVolumeNamePattern},
		"network": {m.Resources.Networks, rigNetworkNamePattern},
	} {
		if !slices.IsSorted(values.names) {
			return fmt.Errorf("rig manifest %s names are not sorted", kind)
		}
		for index, name := range values.names {
			if !values.pattern.MatchString(name) {
				return fmt.Errorf("rig manifest %s name %q is outside the owned namespace", kind, name)
			}
			if index > 0 && name == values.names[index-1] {
				return fmt.Errorf("rig manifest %s name %q is duplicated", kind, name)
			}
		}
	}
	return nil
}

func writeRigManifest(path string, manifest RigManifest) error {
	body, err := encodeRigManifest(manifest)
	if err != nil {
		return err
	}
	if err := atomicfile.WriteFile(path, body, 0o600); err != nil {
		return fmt.Errorf("publish rig manifest: %w", err)
	}
	return nil
}

func encodeRigManifest(manifest RigManifest) ([]byte, error) {
	body, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode rig manifest: %w", err)
	}
	body = append(body, '\n')
	if len(body) > rigManifestLimit {
		return nil, fmt.Errorf("rig manifest exceeds the %d-byte decoding limit", rigManifestLimit)
	}
	return body, nil
}

func removeRigManifest(path string) error {
	err := os.Remove(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return atomicfile.SyncDir(filepath.Dir(path))
}

func tokenMatches(manifest RigManifest, token string) bool {
	digest := sha256.Sum256([]byte(token))
	return token != "" && subtle.ConstantTimeCompare(
		digest[:], decodeRigTokenDigest(manifest.TokenDigest),
	) == 1
}

func decodeRigTokenDigest(value string) []byte {
	digest, err := hex.DecodeString(value)
	if err != nil {
		return nil
	}
	return digest
}

// BindRigRuntimeResources atomically amends both manifest copies only while the
// token matches and another process demonstrably holds both flocks.
func BindRigRuntimeResources(
	stateRoot, token string, containers, volumes, networks []string,
) (RigManifest, error) {
	canonicalStateRoot, err := canonicalPath(stateRoot)
	if err != nil {
		return RigManifest{}, err
	}
	gate, err := acquireRigGate(canonicalStateRoot)
	if err != nil {
		return RigManifest{}, err
	}
	defer func() { _ = closeRigLock(gate) }()
	manifest, err := AuthenticateRig(canonicalStateRoot, token)
	if err != nil {
		return RigManifest{}, err
	}
	_, manifestPaths := rigPaths(manifest.Resources)
	manifest.Resources.Containers = append(manifest.Resources.Containers, containers...)
	sort.Strings(manifest.Resources.Containers)
	manifest.Resources.Containers = slices.Compact(manifest.Resources.Containers)
	manifest.Resources.Volumes = append(manifest.Resources.Volumes, volumes...)
	sort.Strings(manifest.Resources.Volumes)
	manifest.Resources.Volumes = slices.Compact(manifest.Resources.Volumes)
	manifest.Resources.Networks = append(manifest.Resources.Networks, networks...)
	sort.Strings(manifest.Resources.Networks)
	manifest.Resources.Networks = slices.Compact(manifest.Resources.Networks)
	if err := manifest.validate(); err != nil {
		return RigManifest{}, err
	}
	// Reject an amendment the public manifest decoder could not read before the
	// signed authority log changes. A failed oversized bind therefore leaves the
	// prior log and every mirror intact and usable for cleanup or recovery.
	if _, err := encodeRigManifest(manifest); err != nil {
		return RigManifest{}, err
	}
	// Append authority before publishing any mirror. An interrupted append is
	// ignored as an incomplete final record, while a completed append may safely
	// lead mirrors because the bind has not yet returned permission to create
	// those resources.
	if err := appendRigLockMetadata(filepath.Join(canonicalStateRoot, rigLockName), manifest, token); err != nil {
		return RigManifest{}, err
	}
	if err := writeRigManifest(globalRigStatePath(manifest.Resources), manifest); err != nil {
		return RigManifest{}, fmt.Errorf("publish global rig amendment: %w", err)
	}
	prior, err := readRigManifest(manifestPaths[1])
	if err != nil {
		return RigManifest{}, fmt.Errorf("read seed-root rig manifest before bind: %w", err)
	}
	if err := writeRigManifest(manifestPaths[1], manifest); err != nil {
		return RigManifest{}, err
	}
	if err := writeRigManifest(manifestPaths[0], manifest); err != nil {
		rollbackErr := writeRigManifest(manifestPaths[1], prior)
		return RigManifest{}, errors.Join(err, rollbackErr)
	}
	return manifest, nil
}

// AuthenticateRig returns the manifest only when the caller has the live
// holder's token and both resource flocks are still held.
func AuthenticateRig(stateRoot, token string) (RigManifest, error) {
	manifest, err := ReadRigManifest(stateRoot)
	if err != nil {
		return RigManifest{}, err
	}
	if !tokenMatches(manifest, token) {
		return RigManifest{}, ErrRigToken
	}
	lockPaths, _ := rigPaths(manifest.Resources)
	if err := requireRigLocksHeld(lockPaths); err != nil {
		return RigManifest{}, err
	}
	return manifest, nil
}

// AuthorizeRig authenticates a live holder and prevents clean release or a
// successor acquisition until the caller closes the authorization.
func AuthorizeRig(stateRoot, token string) (*RigAuthorization, error) {
	canonicalStateRoot, err := canonicalPath(stateRoot)
	if err != nil {
		return nil, err
	}
	gate, err := acquireRigGate(canonicalStateRoot)
	if err != nil {
		return nil, err
	}
	manifest, err := AuthenticateRig(canonicalStateRoot, token)
	if err != nil {
		_ = closeRigLock(gate)
		return nil, err
	}
	return &RigAuthorization{gate: gate, manifest: manifest}, nil
}

func (a *RigAuthorization) Manifest() RigManifest {
	if a == nil {
		return RigManifest{}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return cloneRigManifest(a.manifest)
}

func (a *RigAuthorization) Close() error {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return nil
	}
	a.closed = true
	return closeRigLock(a.gate)
}

func requireRigLocksHeld(paths []string) error {
	for _, path := range paths {
		// #nosec G304 -- every path is a fixed basename below a validated manifest root.
		f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			return err
		}
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
			_ = f.Close()
			return ErrRigNotHeld
		}
		_ = f.Close()
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			return fmt.Errorf("probe rig lock %s: %w", path, err)
		}
	}
	return nil
}

// AcquireStaleRig proves the holder is gone by acquiring every recorded
// resource flock. It leaves all manifests in place until explicit recovery
// finishes.
func AcquireStaleRig(stateRoot string) (*RigLease, error) {
	return acquireStaleRig(stateRoot, nil)
}

func acquireStaleRig(stateRoot string, afterInitialRead func()) (*RigLease, error) {
	manifest, err := ReadRigManifest(stateRoot)
	if err != nil {
		return nil, err
	}
	lockPaths, manifestPaths := rigPaths(manifest.Resources)
	canonicalSeedRoot, err := canonicalPath(manifest.Resources.SeedRoot)
	if err != nil {
		return nil, fmt.Errorf("canonicalize recorded rig seed root: %w", err)
	}
	if canonicalSeedRoot != manifest.Resources.SeedRoot {
		return nil, errors.New("recorded rig seed root no longer resolves to the leased root")
	}
	canonicalLeaseRoot, err := canonicalPath(manifest.Resources.LeaseRoot)
	if err != nil {
		return nil, fmt.Errorf("canonicalize recorded rig coordination root: %w", err)
	}
	if canonicalLeaseRoot != manifest.Resources.LeaseRoot {
		return nil, errors.New("recorded rig coordination root no longer resolves to the leased root")
	}
	if afterInitialRead != nil {
		afterInitialRead()
	}
	gate, err := acquireRigGate(manifest.Resources.StateRoot)
	if err != nil {
		return nil, err
	}
	locks, conflictPath, err := acquireRigLocks(lockPaths)
	if errors.Is(err, ErrRigHeld) {
		_ = closeRigLock(gate)
		candidates := append(manifestPaths, globalRigStatePath(manifest.Resources))
		return nil, &RigConflictError{Manifest: readConflictManifest(conflictPath, candidates)}
	}
	if err != nil {
		_ = closeRigLock(gate)
		return nil, err
	}
	currentManifest, err := ReadRigManifest(manifest.Resources.StateRoot)
	if err != nil {
		_ = closeRigLocks(locks)
		_ = closeRigLock(gate)
		return nil, fmt.Errorf("re-read authoritative rig manifest after exclusion: %w", err)
	}
	if !rigManifestIdentityEqual(manifest, currentManifest) {
		_ = closeRigLocks(locks)
		_ = closeRigLock(gate)
		return nil, errors.New("authoritative rig manifest changed while acquiring recovery")
	}
	manifest = currentManifest
	globalManifest, err := readRigManifest(globalRigStatePath(manifest.Resources))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		_ = closeRigLocks(locks)
		_ = closeRigLock(gate)
		return nil, fmt.Errorf("read global rig manifest: %w", err)
	}
	if err == nil && !rigManifestIdentityEqual(manifest, globalManifest) {
		_ = closeRigLocks(locks)
		_ = closeRigLock(gate)
		return nil, errors.New("state-root and global rig manifests disagree")
	}
	seedManifest, err := readRigManifest(manifestPaths[1])
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		_ = closeRigLocks(locks)
		_ = closeRigLock(gate)
		return nil, fmt.Errorf("read seed-root rig manifest: %w", err)
	}
	if err == nil {
		if !rigManifestIdentityEqual(manifest, seedManifest) {
			_ = closeRigLocks(locks)
			_ = closeRigLock(gate)
			return nil, errors.New("state-root and seed-root rig manifests disagree")
		}
		if !rigManifestResourceSubset(seedManifest, manifest) {
			_ = closeRigLocks(locks)
			_ = closeRigLock(gate)
			return nil, errors.New("seed-root rig resources are not authorized by durable amendment metadata")
		}
		if err := writeRigManifest(manifestPaths[1], manifest); err != nil {
			_ = closeRigLocks(locks)
			_ = closeRigLock(gate)
			return nil, err
		}
		if err := writeRigManifest(manifestPaths[0], manifest); err != nil {
			_ = closeRigLocks(locks)
			_ = closeRigLock(gate)
			return nil, err
		}
	}
	return &RigLease{
		locks: locks, manifestPaths: manifestPaths, manifest: manifest, gate: gate,
	}, nil
}

func rigManifestIdentityEqual(left, right RigManifest) bool {
	return left.Version == right.Version && left.Owner == right.Owner &&
		left.AcquiredAt.Equal(right.AcquiredAt) && left.TokenDigest == right.TokenDigest &&
		left.AmendmentPublicKey == right.AmendmentPublicKey &&
		left.Resources.StateRoot == right.Resources.StateRoot &&
		left.Resources.DatabasePath == right.Resources.DatabasePath &&
		left.Resources.ListenAddress == right.Resources.ListenAddress &&
		left.Resources.SeedRoot == right.Resources.SeedRoot &&
		left.Resources.LeaseRoot == right.Resources.LeaseRoot
}

func rigManifestResourceSubset(candidate, authority RigManifest) bool {
	return sortedStringsSubset(candidate.Resources.Containers, authority.Resources.Containers) &&
		sortedStringsSubset(candidate.Resources.Volumes, authority.Resources.Volumes) &&
		sortedStringsSubset(candidate.Resources.Networks, authority.Resources.Networks)
}

func sortedStringsSubset(candidate, authority []string) bool {
	for _, value := range candidate {
		_, found := slices.BinarySearch(authority, value)
		if !found {
			return false
		}
	}
	return true
}

// Abandon drops stale-recovery flocks without clearing either manifest. It is
// used when confirmation is absent or any cleanup/liveness check fails.
func (l *RigLease) Abandon() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil
	}
	err := errors.Join(closeRigLocks(l.locks), closeRigLock(l.gate))
	l.closed = true
	return err
}
